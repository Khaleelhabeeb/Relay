// Package ops is Relay's operator core: listing and inspecting events and
// deliveries, rerouting held events, reprocessing, and retrying
// confirmations and deliveries. The Go methods on relay.Relay, the HTTP
// admin API and the CLI all call it, so every channel enforces the same
// rules: reroutes need a known application and a reason and are audited,
// only held events can be rerouted, only exhausted deliveries retried.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

// Errors returned by Service. Not-found errors wrap store.ErrNotFound.
var (
	// ErrInvalid means the request failed validation.
	ErrInvalid = errors.New("invalid request")
	// ErrWrongState means the event or delivery is not in a state that
	// allows the action, e.g. rerouting an event that already has an owner.
	ErrWrongState = errors.New("action not allowed in the current state")
)

// Channels an operator action can come through, recorded in the audit
// trail of a reroute.
const (
	ViaCLI   = "cli"
	ViaAPI   = "admin api"
	ViaGoAPI = "go api"
)

const defaultLimit = 50

// Service runs operator actions against one database.
type Service struct {
	Store       *store.Store
	Reprocessor *reprocess.Reprocessor // with Engine set
	// Applications are the configured application IDs; reroutes must name one.
	Applications []string
	// OnDelivery and OnConfirm, when set, wake the delivery and confirmation
	// workers of an in-process Relay. They must not block.
	OnDelivery func()
	OnConfirm  func()
	Now        func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// notFoundError reads "event evt_x not found" and matches store.ErrNotFound.
type notFoundError struct{ kind, id string }

func (e notFoundError) Error() string { return e.kind + " " + e.id + " not found" }
func (e notFoundError) Unwrap() error { return store.ErrNotFound }

func notFound(kind, id string) error { return notFoundError{kind, id} }

// wrongState maps the store's and reprocessor's state errors to
// ErrWrongState, keeping their message.
func wrongState(err error) error {
	if errors.Is(err, reprocess.ErrNotReroutable) || errors.Is(err, store.ErrNotRetryable) || errors.Is(err, store.ErrNotHeld) ||
		errors.Is(err, store.ErrNotPending) || errors.Is(err, store.ErrPurged) {
		return fmt.Errorf("%w: %w", ErrWrongState, err)
	}
	return err
}

func checkLimit(n int) (int, error) {
	switch {
	case n == 0:
		return defaultLimit, nil
	case n < 0 || n > admin.MaxLimit:
		return 0, invalid("limit must be 1-%d", admin.MaxLimit)
	}
	return n, nil
}

func checkOneOf(field, v string, allowed ...string) error {
	if v != "" && !slices.Contains(allowed, v) {
		return invalid("%s must be one of: %s", field, strings.Join(allowed, ", "))
	}
	return nil
}

// ListEvents returns one page of events, newest first.
func (s *Service) ListEvents(ctx context.Context, f admin.EventFilter) (admin.EventPage, error) {
	limit, err := checkLimit(f.Limit)
	if err != nil {
		return admin.EventPage{}, err
	}
	if err := errors.Join(
		checkOneOf("status", f.Status, admin.StatusMatched, admin.StatusUnresolved, admin.StatusAmbiguous, admin.StatusUnsupported),
		checkOneOf("confirmation", f.Confirmation, admin.ConfirmationNone, admin.ConfirmationConfirmed,
			admin.ConfirmationPending, admin.ConfirmationRejected, admin.ConfirmationFailed),
	); err != nil {
		return admin.EventPage{}, err
	}
	if f.After != "" {
		if _, err := s.Store.GetEvent(ctx, f.After); errors.Is(err, store.ErrNotFound) {
			return admin.EventPage{}, invalid("after: unknown event %s", f.After)
		} else if err != nil {
			return admin.EventPage{}, err
		}
	}
	// Fetch one extra to know whether another page exists.
	rows, err := s.Store.ListEvents(ctx, store.EventFilter{
		Status: f.Status, Confirmation: f.Confirmation, EventType: f.EventType, Provider: f.Provider,
		ApplicationID: f.ApplicationID, Held: f.Held, Since: f.Since, Before: f.After, Limit: limit + 1,
	})
	if err != nil {
		return admin.EventPage{}, err
	}
	page := admin.EventPage{Events: make([]admin.Event, 0, len(rows))}
	if len(rows) > limit {
		rows = rows[:limit]
		page.Next = rows[limit-1].ID
	}
	for _, r := range rows {
		page.Events = append(page.Events, admin.Event{
			ID: r.ID, Provider: r.Provider, EventType: r.EventType, RoutingStatus: r.RoutingStatus,
			ApplicationID: r.ApplicationID, Reference: r.Reference, DedicatedAccount: r.DedicatedAccount,
			ReceivedAt: r.ReceivedAt, DuplicateCount: r.DuplicateCount, PayloadBytes: r.PayloadBytes,
			Confirmation: r.Confirmation, DismissedAt: r.DismissedAt,
		})
	}
	return page, nil
}

func toEvent(e *store.Event) admin.Event {
	return admin.Event{
		ID: e.ID, Provider: e.Provider, EventType: e.EventType, RoutingStatus: string(e.RoutingStatus),
		ApplicationID: e.ApplicationID, Reference: e.Reference, DedicatedAccount: e.DedicatedAccount,
		ReceivedAt: e.ReceivedAt, DuplicateCount: e.DuplicateCount, PayloadBytes: len(e.RawPayload),
		Confirmation: string(e.Confirmation), DismissedAt: e.DismissedAt,
	}
}

func toDelivery(d store.Delivery, lastError string) admin.Delivery {
	return admin.Delivery{
		ID: d.ID, EventID: d.EventID, ApplicationID: d.ApplicationID, Status: string(d.Status),
		AttemptCount: d.AttemptCount, NextAttemptAt: d.NextAttemptAt, CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt, LastError: lastError,
	}
}

// payloadJSON returns raw as JSON, or as a JSON string if it is not valid
// JSON, so encoding an EventDetail never fails.
func payloadJSON(raw []byte) json.RawMessage {
	if raw == nil {
		return nil
	}
	if json.Valid(raw) {
		return json.RawMessage(raw)
	}
	b, _ := json.Marshal(string(raw))
	return b
}

// GetEvent returns an event with its routing history, deliveries, attempts
// and conflicting duplicates. Payloads are included only if includePayload.
func (s *Service) GetEvent(ctx context.Context, id string, includePayload bool) (admin.EventDetail, error) {
	e, err := s.Store.GetEvent(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return admin.EventDetail{}, notFound("event", id)
	}
	if err != nil {
		return admin.EventDetail{}, err
	}
	out := admin.EventDetail{
		Event: toEvent(e), DedupKey: e.DedupKey, PayloadSHA256: e.PayloadSHA256, LastSeenAt: e.LastSeenAt,
		ConfirmAttempts: e.ConfirmAttempts, ConfirmNextAt: e.ConfirmNextAt, ConfirmError: e.ConfirmError,
		DismissedBy: e.DismissedBy, DismissReason: e.DismissReason,
		RoutingDecisions: []admin.RoutingDecision{}, Deliveries: []admin.Delivery{}, DedupConflicts: []admin.DedupConflict{},
	}
	if includePayload {
		out.Payload = payloadJSON(e.RawPayload)
	}

	decisions, err := s.Store.RoutingDecisionsForEvent(ctx, id)
	if err != nil {
		return admin.EventDetail{}, err
	}
	for _, d := range decisions {
		rd := admin.RoutingDecision{Outcome: string(d.Outcome), ApplicationID: d.ApplicationID, Reason: d.Reason,
			DecidedBy: d.DecidedBy, DecidedAt: d.DecidedAt, Candidates: []admin.Candidate{}}
		for _, c := range d.Candidates {
			rd.Candidates = append(rd.Candidates, admin.Candidate{Application: c.Application, Source: c.Source, Detail: c.Detail})
		}
		out.RoutingDecisions = append(out.RoutingDecisions, rd)
	}

	// Deliveries with their last error, which only the summary query reads.
	summaries, err := s.Store.ListDeliveries(ctx, store.DeliveryFilter{EventID: id, Limit: admin.MaxLimit})
	if err != nil {
		return admin.EventDetail{}, err
	}
	lastErr := make(map[string]string, len(summaries))
	for _, d := range summaries {
		lastErr[d.ID] = d.LastError
	}
	deliveries, err := s.Store.DeliveriesForEvent(ctx, id)
	if err != nil {
		return admin.EventDetail{}, err
	}
	for _, d := range deliveries {
		ad := toDelivery(d, lastErr[d.ID])
		attempts, err := s.Store.AttemptsForDelivery(ctx, d.ID)
		if err != nil {
			return admin.EventDetail{}, err
		}
		for _, a := range attempts {
			ad.Attempts = append(ad.Attempts, admin.Attempt{AttemptNo: a.AttemptNo, StartedAt: a.StartedAt,
				Finished: a.Finished, HTTPStatus: a.HTTPStatus, ErrorCode: a.ErrorCode, DurationMS: a.DurationMS})
		}
		out.Deliveries = append(out.Deliveries, ad)
	}

	conflicts, err := s.Store.DedupConflictsForEvent(ctx, id)
	if err != nil {
		return admin.EventDetail{}, err
	}
	for _, c := range conflicts {
		dc := admin.DedupConflict{ID: c.ID, Reference: c.Reference, DedicatedAccount: c.DedicatedAccount,
			PayloadSHA256: c.PayloadSHA256, ReceivedAt: c.ReceivedAt}
		if includePayload {
			dc.Payload = payloadJSON(c.RawPayload)
		}
		out.DedupConflicts = append(out.DedupConflicts, dc)
	}
	return out, nil
}

// Reroute assigns application as the owner of an unresolved or ambiguous
// event and queues its delivery. The decision is recorded with reason and
// the channel it came through (a Via constant).
func (s *Service) Reroute(ctx context.Context, id, application, reason, via string) (admin.RerouteResult, error) {
	if !slices.Contains(s.Applications, application) {
		return admin.RerouteResult{}, invalid("unknown application %q", application)
	}
	if strings.TrimSpace(reason) == "" {
		return admin.RerouteResult{}, invalid("a reason is required")
	}
	if err := s.Reprocessor.Reroute(ctx, id, application, strings.TrimSpace(reason), via); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return admin.RerouteResult{}, notFound("event", id)
		}
		return admin.RerouteResult{}, wrongState(err)
	}
	e, err := s.Store.GetEvent(ctx, id)
	if err != nil {
		return admin.RerouteResult{}, err
	}
	return admin.RerouteResult{Event: toEvent(e), AwaitingConfirmation: !e.Confirmation.Deliverable()}, nil
}

// Dismiss closes a held event without an owner: nothing will be delivered
// and it stops counting as held. The reason and channel (a Via constant)
// are kept on the event. Rerouting it later clears the dismissal.
func (s *Service) Dismiss(ctx context.Context, id, reason, via string) (admin.Event, error) {
	if strings.TrimSpace(reason) == "" {
		return admin.Event{}, invalid("a reason is required")
	}
	err := s.Store.DismissEvent(ctx, id, "operator via "+via, strings.TrimSpace(reason), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return admin.Event{}, notFound("event", id)
	}
	if err != nil {
		return admin.Event{}, wrongState(err)
	}
	e, err := s.Store.GetEvent(ctx, id)
	if err != nil {
		return admin.Event{}, err
	}
	return toEvent(e), nil
}

// Reprocess routes unresolved events again with the current rules and
// registrations. A failure on one event is reported in its entry and does
// not stop the others.
func (s *Service) Reprocess(ctx context.Context, req admin.ReprocessRequest) (admin.ReprocessResult, error) {
	ids := req.EventIDs
	if len(ids) > 0 && !req.Since.IsZero() {
		return admin.ReprocessResult{}, invalid("give event_ids or since, not both")
	}
	if len(ids) == 0 {
		var err error
		if ids, err = s.Store.EventIDsByStatus(ctx, string(routing.Unresolved), req.Since); err != nil {
			return admin.ReprocessResult{}, err
		}
	}
	res := admin.ReprocessResult{Events: make([]admin.ReprocessedEvent, 0, len(ids))}
	for _, id := range ids {
		res.Considered++
		outcome, err := s.Reprocessor.Event(ctx, id, store.DecidedByReprocess)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return res, err
		case errors.Is(err, store.ErrNotFound):
			res.Failed++
			res.Events = append(res.Events, admin.ReprocessedEvent{ID: id, Error: "event not found"})
			continue
		case err != nil:
			res.Failed++
			res.Events = append(res.Events, admin.ReprocessedEvent{ID: id, Error: err.Error()})
			continue
		}
		switch outcome {
		case routing.Matched:
			res.Matched++
		case routing.Ambiguous:
			res.Ambiguous++
		case routing.Unresolved:
			res.Unresolved++
		}
		res.Events = append(res.Events, admin.ReprocessedEvent{ID: id, Status: string(outcome)})
	}
	return res, nil
}

// RetryConfirmation makes a pending or failed event due for provider API
// confirmation now, with a fresh retry window.
func (s *Service) RetryConfirmation(ctx context.Context, id string) error {
	err := s.Store.RetryConfirmation(ctx, id, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return notFound("event", id)
	}
	if err != nil {
		return wrongState(err)
	}
	if s.OnConfirm != nil {
		s.OnConfirm()
	}
	return nil
}

// ListDeliveries returns deliveries, most recently updated first.
func (s *Service) ListDeliveries(ctx context.Context, f admin.DeliveryFilter) ([]admin.Delivery, error) {
	limit, err := checkLimit(f.Limit)
	if err != nil {
		return nil, err
	}
	if err := checkOneOf("status", f.Status, admin.DeliveryPending, admin.DeliveryInFlight,
		admin.DeliveryDelivered, admin.DeliveryExhausted); err != nil {
		return nil, err
	}
	rows, err := s.Store.ListDeliveries(ctx, store.DeliveryFilter{Status: f.Status, Application: f.ApplicationID,
		EventID: f.EventID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]admin.Delivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, toDelivery(d.Delivery, d.LastError))
	}
	return out, nil
}

// RetryDelivery gives an exhausted delivery a fresh attempt budget, due
// now, to the same owner.
func (s *Service) RetryDelivery(ctx context.Context, id string) error {
	err := s.Store.RetryDelivery(ctx, id, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return notFound("delivery", id)
	}
	if err != nil {
		return wrongState(err)
	}
	if s.OnDelivery != nil {
		s.OnDelivery()
	}
	return nil
}

// Stats returns the counts an operator watches.
func (s *Service) Stats(ctx context.Context) (admin.Stats, error) {
	st, err := s.Store.Stats(ctx, s.now())
	if err != nil {
		return admin.Stats{}, err
	}
	d := st.DeliveriesByStatus
	return admin.Stats{
		Events: admin.EventStats{
			Unresolved:          st.EventsByStatus[string(routing.Unresolved)],
			Ambiguous:           st.EventsByStatus[string(routing.Ambiguous)],
			OldestHeldAt:        st.OldestHeldAt,
			ConfirmationPending: st.Confirmations[string(store.ConfirmPending)],
			ConfirmationFailed:  st.Confirmations[string(store.ConfirmFailed)],
		},
		Deliveries: admin.DeliveryStats{
			Pending:         d[string(store.DeliveryPending)],
			InFlight:        d[string(store.DeliveryInFlight)],
			Delivered:       d[string(store.DeliveryDelivered)],
			Exhausted:       d[string(store.DeliveryExhausted)],
			OldestPendingAt: st.OldestPendingAt,
		},
	}, nil
}
