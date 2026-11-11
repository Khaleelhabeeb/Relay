// Package alert tells operators when something needs them. Every state
// change that needs attention (a held event, an exhausted delivery, a
// failed or rejected confirmation, a conflicting duplicate, an expired
// event) is queued in the store's alert outbox in the same transaction as
// the change itself. The Alerter sends the queued alerts as one digest per
// interval, and a reminder while anything still needs attention.
package alert

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/internal/ids"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

const (
	batchSize      = 1000
	sendTimeout    = 15 * time.Second
	reminderState  = "reminder"
	defaultRefresh = time.Minute
)

// Sink receives digests.
type Sink interface {
	Name() string
	Send(ctx context.Context, d *admin.AlertDigest) error
}

// Func is a Sink backed by a Go function.
type Func func(ctx context.Context, d admin.AlertDigest) error

func (Func) Name() string { return "func" }

func (f Func) Send(ctx context.Context, d *admin.AlertDigest) error { return f(ctx, *d) }

// Alerter sends queued alerts as digests.
type Alerter struct {
	Store    *store.Store
	Sinks    []Sink
	Interval time.Duration // between digests; default 1m
	Reminder time.Duration // 0: no reminders
	// StatsFn returns the current totals included in each digest.
	StatsFn func(ctx context.Context) (admin.Stats, error)
	Logger  *slog.Logger
	Metrics *metrics.Metrics
	Now     func() time.Time
}

func (a *Alerter) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// Run sends digests every Interval until ctx ends.
func (a *Alerter) Run(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = defaultRefresh
	}
	if a.Reminder > 0 {
		// Start the reminder clock at the first run rather than reminding at
		// once on every restart.
		if _, ok, err := a.Store.AlertState(ctx, reminderState); err == nil && !ok {
			_ = a.Store.SetAlertState(ctx, reminderState, a.now())
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.RunOnce(ctx); err != nil && ctx.Err() == nil {
			a.Logger.Error("sending alerts failed; retrying at the next interval", "component", "alert", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// RunOnce sends one digest of queued alerts, or else a reminder if one is
// due. Retention deletes old alerts.
func (a *Alerter) RunOnce(ctx context.Context) error {
	sent, err := a.flush(ctx)
	if err != nil {
		return err
	}
	if !sent {
		if err := a.remind(ctx); err != nil {
			return err
		}
	}
	return nil
}

// stillHolds reports whether an alert still describes the current state.
func stillHolds(p store.PendingAlert) bool {
	switch p.Kind {
	case store.AlertUnresolved, store.AlertAmbiguous:
		held := p.RoutingStatus == string(routing.Unresolved) || p.RoutingStatus == string(routing.Ambiguous)
		return held && !p.Dismissed && p.Confirmation != store.ConfirmRejected
	case store.AlertDeliveryExhausted:
		return p.DeliveryStatus == store.DeliveryExhausted
	case store.AlertConfirmationFailed:
		return p.Confirmation == store.ConfirmFailed
	}
	return true
}

func toAlert(p store.PendingAlert) admin.Alert {
	al := admin.Alert{Kind: p.Kind, EventID: p.EventID, DeliveryID: p.DeliveryID, Provider: p.Provider,
		EventType: p.EventType, Reference: p.Reference, ApplicationID: p.ApplicationID, At: p.CreatedAt}
	switch p.Kind {
	case store.AlertDeliveryExhausted:
		al.ApplicationID, al.Detail = p.DeliveryApp, p.LastError
	case store.AlertConfirmationFailed, store.AlertConfirmationRejects:
		al.Detail = p.ConfirmError
	case store.AlertExpired:
		al.Detail = p.DismissReason
	case store.AlertDedupConflict:
		al.Detail = "a webhook with this event's dedup key carried different ownership identifiers"
	}
	return al
}

func (a *Alerter) flush(ctx context.Context) (bool, error) {
	pending, err := a.Store.PendingAlerts(ctx, batchSize)
	if err != nil || len(pending) == 0 {
		return false, err
	}
	maxID := pending[len(pending)-1].ID
	d := &admin.AlertDigest{Counts: map[string]int{}, Alerts: []admin.Alert{}}
	for _, p := range pending {
		if !stillHolds(p) {
			continue
		}
		d.Counts[p.Kind]++
		if len(d.Alerts) < admin.MaxDigestAlerts {
			d.Alerts = append(d.Alerts, toAlert(p))
		} else {
			d.Omitted++
		}
	}
	if len(d.Alerts) == 0 {
		return false, a.Store.MarkAlertsSent(ctx, maxID, a.now())
	}
	if err := a.fillStats(ctx, d); err != nil {
		return false, err
	}
	if err := a.send(ctx, d); err != nil {
		return false, err
	}
	if err := a.Store.MarkAlertsSent(ctx, maxID, a.now()); err != nil {
		return true, err
	}
	// A digest carries the totals, so it also counts as a reminder.
	if a.Reminder > 0 {
		return true, a.Store.SetAlertState(ctx, reminderState, a.now())
	}
	return true, nil
}

// needsAttention reports whether the totals call for a reminder.
func needsAttention(s admin.Stats) bool {
	return s.Events.Unresolved+s.Events.Ambiguous+s.Events.ConfirmationFailed+s.Deliveries.Exhausted > 0
}

func (a *Alerter) remind(ctx context.Context) error {
	if a.Reminder <= 0 {
		return nil
	}
	last, ok, err := a.Store.AlertState(ctx, reminderState)
	if err != nil {
		return err
	}
	if ok && a.now().Sub(last) < a.Reminder {
		return nil
	}
	d := &admin.AlertDigest{Reminder: true, Counts: map[string]int{}, Alerts: []admin.Alert{}}
	if err := a.fillStats(ctx, d); err != nil {
		return err
	}
	if needsAttention(d.Stats) {
		if err := a.send(ctx, d); err != nil {
			return err
		}
	}
	return a.Store.SetAlertState(ctx, reminderState, a.now())
}

func (a *Alerter) fillStats(ctx context.Context, d *admin.AlertDigest) error {
	if a.StatsFn == nil {
		return nil
	}
	s, err := a.StatsFn(ctx)
	d.Stats = s
	return err
}

// send delivers d to every sink. It fails if any sink fails, so the alerts
// are sent again next time; a sink that succeeded may then see them twice.
func (a *Alerter) send(ctx context.Context, d *admin.AlertDigest) error {
	d.Type, d.ID, d.SentAt = admin.AlertDigestType, ids.New(ids.AlertDigest), a.now()
	var errs []error
	for _, s := range a.Sinks {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := s.Send(sctx, d)
		cancel()
		a.Metrics.AlertDigest(s.Name(), err == nil)
		if err != nil {
			errs = append(errs, err)
			a.Logger.Warn("alert digest not accepted", "component", "alert", "sink", s.Name(), "digest_id", d.ID, "error", err)
			continue
		}
		a.Logger.Info("alert digest sent", "component", "alert", "sink", s.Name(), "digest_id", d.ID,
			"alerts", len(d.Alerts)+d.Omitted, "reminder", d.Reminder)
	}
	return errors.Join(errs...)
}
