// Package reprocess re-runs routing for unresolved events, e.g. after an
// application registers an identifier whose webhook already arrived, or
// after an operator adds a route.
package reprocess

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

// Window bounds automatic reprocessing after a registration: only events
// received this recently are reconsidered.
const Window = 72 * time.Hour

type Reprocessor struct {
	Store    *store.Store
	Engine   *routing.Engine
	Logger   *slog.Logger
	OnRouted func()           // called after a reprocess creates a delivery; must not block
	Metrics  *metrics.Metrics // optional
	Now      func() time.Time
}

func (r *Reprocessor) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Event re-routes one event if it is still unresolved, using the current
// rules and registrations as of the event's arrival. It changes nothing if
// the event stays unresolved, was already routed, or its payload has been
// purged. It returns the event's routing status afterwards.
func (r *Reprocessor) Event(ctx context.Context, eventID, decidedBy string) (routing.Outcome, error) {
	var outcome routing.Outcome
	err := r.Store.WithTx(ctx, func(tx *store.Tx) error {
		e, err := tx.GetEvent(ctx, eventID)
		if err != nil {
			return err
		}
		outcome = e.RoutingStatus
		// A dismissed event stays closed unless an operator reroutes it.
		if e.RoutingStatus != routing.Unresolved || len(e.RawPayload) == 0 || !e.DismissedAt.IsZero() {
			return nil
		}
		// Stored payloads were verified at ingest; the engine's adapter
		// only re-reads them.
		ad, ok := r.Engine.Adapter(e.Provider)
		if !ok {
			return fmt.Errorf("reprocess: provider %q is not configured", e.Provider)
		}
		evt, err := ad.Parse(e.RawPayload)
		if err != nil {
			return err
		}
		d, err := r.Engine.Resolve(ctx, e.Provider, evt.Type, ad.ExtractIdentifiers(evt), e.ReceivedAt, tx.RegistrationLookup(e.Provider))
		if err != nil {
			return err
		}
		if d.Outcome == routing.Unresolved {
			return nil
		}
		outcome = d.Outcome
		if err := tx.SetEventRouting(ctx, eventID, d.Outcome, d.Application); err != nil {
			return err
		}
		return tx.ApplyDecision(ctx, eventID, d, decidedBy, r.now())
	})
	if err != nil {
		return "", err
	}
	if outcome != routing.Unresolved {
		r.Metrics.Routed(string(outcome))
	}
	if outcome == routing.Matched && r.OnRouted != nil {
		r.OnRouted()
	}
	return outcome, nil
}

// Result summarizes a batch reprocess.
type Result struct {
	Considered int
	Matched    int
	Ambiguous  int
}

// Unresolved reprocesses every unresolved event received at or after since
// (zero: all), e.g. after an operator adds a route.
func (r *Reprocessor) Unresolved(ctx context.Context, since time.Time, decidedBy string) (Result, error) {
	ids, err := r.Store.EventIDsByStatus(ctx, string(routing.Unresolved), since)
	if err != nil {
		return Result{}, err
	}
	return r.batch(ctx, ids, decidedBy)
}

// ErrNotReroutable means the event already has an owner or is unsupported.
var ErrNotReroutable = errors.New("reprocess: only unresolved or ambiguous events can be rerouted")

// Reroute assigns an owner to an unresolved or ambiguous event by explicit
// operator decision, recorded with the reason and the channel it came
// through (via, e.g. "cli"), and creates its delivery.
func (r *Reprocessor) Reroute(ctx context.Context, eventID, application, reason, via string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("reprocess: a reason is required")
	}
	err := r.Store.WithTx(ctx, func(tx *store.Tx) error {
		e, err := tx.GetEvent(ctx, eventID)
		if err != nil {
			return err
		}
		if e.RoutingStatus != routing.Unresolved && e.RoutingStatus != routing.Ambiguous {
			return fmt.Errorf("%w (status is %s)", ErrNotReroutable, e.RoutingStatus)
		}
		if e.Confirmation == store.ConfirmRejected {
			return fmt.Errorf("%w (the provider API rejected it: %s)", ErrNotReroutable, e.ConfirmError)
		}
		if len(e.RawPayload) == 0 {
			return fmt.Errorf("%w; it cannot be delivered", store.ErrPurged)
		}
		d := routing.Decision{
			Outcome:     routing.Matched,
			Application: application,
			Candidates:  []routing.Candidate{{Application: application, Source: "operator", Detail: reason}},
			Reason:      "operator reroute via " + via + ": " + reason,
		}
		if err := tx.SetEventRouting(ctx, eventID, d.Outcome, application); err != nil {
			return err
		}
		return tx.ApplyDecision(ctx, eventID, d, store.DecidedByOperator, r.now())
	})
	if err != nil {
		return err
	}
	r.Logger.Info("event rerouted by operator", "component", "reprocess", "event_id", eventID, "application", application, "via", via)
	if r.OnRouted != nil {
		r.OnRouted()
	}
	return nil
}

// Identifier reprocesses unresolved events of prov carrying an identifier
// that arrived within Window. Errors on individual events are logged and do
// not stop the batch.
func (r *Reprocessor) Identifier(ctx context.Context, prov, identifierType, value, decidedBy string) (Result, error) {
	ids, err := r.Store.UnresolvedEventIDs(ctx, prov, identifierType, value, r.now().Add(-Window))
	if err != nil {
		return Result{}, err
	}
	return r.batch(ctx, ids, decidedBy)
}

func (r *Reprocessor) batch(ctx context.Context, ids []string, decidedBy string) (Result, error) {
	var res Result
	for _, id := range ids {
		res.Considered++
		outcome, err := r.Event(ctx, id, decidedBy)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return res, err
		case err != nil:
			r.Logger.Error("reprocessing event failed", "component", "reprocess", "event_id", id, "error", err)
		case outcome == routing.Matched:
			res.Matched++
			r.Logger.Info("unresolved event routed by reprocess", "component", "reprocess", "event_id", id, "decided_by", decidedBy)
		case outcome == routing.Ambiguous:
			res.Ambiguous++
			r.Logger.Warn("reprocessed event is ambiguous", "component", "reprocess", "event_id", id, "decided_by", decidedBy)
		}
	}
	return res, nil
}
