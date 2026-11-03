package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"github.com/khaleelhabeeb/relay/internal/ids"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

// StoreAcceptor durably records events. In one transaction it either counts
// a duplicate, or inserts the event, its routing decision and (when matched)
// a pending delivery. The webhook is acknowledged only after commit.
type StoreAcceptor struct {
	Store   *store.Store
	Engine  *routing.Engine
	Logger  *slog.Logger
	Metrics *metrics.Metrics // optional
	// OnRouted, if set, is called after a commit that created a delivery
	// (e.g. to wake the dispatcher). It must not block.
	OnRouted func()
}

func (s *StoreAcceptor) Accept(ctx context.Context, a *Accepted) error {
	prov := a.Event.Provider
	var (
		eventID   string
		duplicate bool
		decision  routing.Decision
	)
	err := s.Store.WithTx(ctx, func(tx *store.Tx) error {
		existing, found, err := tx.EventIDByDedupKey(ctx, prov, a.DedupKey)
		if err != nil {
			return err
		}
		if found {
			eventID, duplicate = existing, true
			return tx.RecordDuplicate(ctx, existing, a.ReceivedAt)
		}

		decision, err = s.Engine.Resolve(ctx, prov, a.Event.Type, a.Identifiers, a.ReceivedAt, tx.RegistrationLookup(prov))
		if err != nil {
			return err
		}

		eventID = ids.New(ids.Event)
		sum := sha256.Sum256(a.Event.Raw)
		e := &store.Event{
			ID:            eventID,
			Provider:      prov,
			EventType:     a.Event.Type,
			DedupKey:      a.DedupKey,
			RawPayload:    a.Event.Raw,
			PayloadSHA256: hex.EncodeToString(sum[:]),
			Reference:     a.Identifiers.Reference,
			ReceivedAt:    a.ReceivedAt,
			RoutingStatus: decision.Outcome,
			ApplicationID: decision.Application,
		}
		if a.Identifiers.DedicatedAccount != nil {
			e.DedicatedAccount = a.Identifiers.DedicatedAccount.Key()
		}
		if err := tx.InsertEvent(ctx, e); err != nil {
			return err
		}
		return tx.ApplyDecision(ctx, eventID, decision, store.DecidedByIngest, a.ReceivedAt)
	})
	if err != nil {
		return err
	}

	log := s.Logger.With("component", "ingest", "provider", prov, "event_id", eventID, "event_type", a.Event.Type)
	if duplicate {
		s.Metrics.Webhook(prov, "duplicate")
	} else {
		s.Metrics.Webhook(prov, "accepted")
		s.Metrics.Routed(string(decision.Outcome))
	}
	switch {
	case duplicate:
		log.Info("duplicate webhook; existing event kept")
	case decision.Outcome == routing.Matched:
		log.Info("event routed", "application", decision.Application)
		if s.OnRouted != nil {
			s.OnRouted()
		}
	case decision.Outcome == routing.Unsupported:
		log.Info("event stored; type not routed", "reason", decision.Reason)
	default:
		log.Warn("event stored without owner", "routing_status", decision.Outcome, "reason", decision.Reason)
	}
	return nil
}
