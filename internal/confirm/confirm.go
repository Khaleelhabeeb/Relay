// Package confirm finishes provider API confirmations that could not be
// completed when a webhook arrived. Such events are stored as pending and
// not delivered; the Worker retries the check with backoff and, once the
// provider vouches for the event, creates its delivery. Events the provider
// disowns are rejected and never delivered.
package confirm

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider"
)

const (
	// DefaultTimeout bounds one background API call. It is longer than the
	// ingest check because no provider is waiting on the answer.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxAttempts spends the retry budget in about 72 hours with
	// the default backoff (30s doubling to 30m).
	DefaultMaxAttempts = 150
	baseDelay          = 30 * time.Second
	maxDelay           = 30 * time.Minute
	pollInterval       = 5 * time.Second
	batchSize          = 20
)

// Worker retries pending confirmations. Run it once per database.
type Worker struct {
	Store       *store.Store
	Adapters    map[string]provider.Adapter // the configured adapters, with their secrets
	Logger      *slog.Logger
	Metrics     *metrics.Metrics // optional
	OnDelivery  func()           // called after a confirmation created a delivery; must not block
	Timeout     time.Duration    // default DefaultTimeout
	MaxAttempts int              // default DefaultMaxAttempts
	Now         func() time.Time

	wake chan struct{}
}

// New returns a Worker for the given adapters.
func New(st *store.Store, adapters []provider.Adapter, logger *slog.Logger, m *metrics.Metrics) *Worker {
	w := &Worker{Store: st, Adapters: map[string]provider.Adapter{}, Logger: logger, Metrics: m, wake: make(chan struct{}, 1)}
	for _, a := range adapters {
		w.Adapters[a.Name()] = a
	}
	return w
}

// Nudge wakes the worker to look for due confirmations. It never blocks.
func (w *Worker) Nudge() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes due confirmations until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if w.wake == nil {
		w.wake = make(chan struct{}, 1)
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.Logger.Error("confirmation sweep failed", "component", "confirm", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-w.wake:
		}
	}
}

// RunOnce attempts every confirmation due now, and returns how many it tried.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	tried := 0
	for {
		due, err := w.Store.DueConfirmations(ctx, w.now(), batchSize)
		if err != nil || len(due) == 0 {
			return tried, err
		}
		for _, d := range due {
			if ctx.Err() != nil {
				return tried, nil
			}
			w.attempt(ctx, d)
			tried++
		}
		if len(due) < batchSize {
			return tried, nil
		}
	}
}

func (w *Worker) attempt(ctx context.Context, d store.DueConfirmation) {
	log := w.Logger.With("component", "confirm", "provider", d.Provider, "event_id", d.ID, "attempt", d.Attempts+1)
	result, reason := w.check(ctx, d)
	var err error
	switch result {
	case "confirmed":
		var delivery bool
		delivery, err = w.Store.ConfirmEvent(ctx, d.ID, w.now())
		if err == nil {
			log.Info("event confirmed by the provider API", "delivery_created", delivery)
			if delivery && w.OnDelivery != nil {
				w.OnDelivery()
			}
		}
	case "rejected":
		if err = w.Store.RejectEvent(ctx, d.ID, reason); err == nil {
			log.Error("event rejected: provider API does not confirm it; it will not be delivered", "reason", reason)
		}
	default:
		attempts := d.Attempts + 1
		if result == "failed" || attempts >= w.maxAttempts() {
			result = "failed"
			if err = w.Store.DeferConfirmation(ctx, d.ID, attempts, time.Time{}, reason); err == nil {
				log.Error("event could not be confirmed; giving up until an operator runs `relay events confirm`",
					"error", reason)
			}
			break
		}
		next := w.now().Add(Backoff(attempts))
		if err = w.Store.DeferConfirmation(ctx, d.ID, attempts, next, reason); err == nil {
			log.Warn("provider API confirmation failed; will retry", "error", reason, "next_attempt_at", next)
		}
	}
	switch {
	case errors.Is(err, store.ErrNotPending):
		return // resolved meanwhile (an operator, or a concurrent sweep)
	case err != nil:
		log.Error("recording confirmation result failed", "error", err)
		return
	}
	w.Metrics.Confirmation(d.Provider, result)
}

// check returns "confirmed", "rejected", "retry" or "failed" (cannot ever
// be checked), with a reason.
func (w *Worker) check(ctx context.Context, d store.DueConfirmation) (string, string) {
	a, ok := w.Adapters[d.Provider]
	if !ok {
		return "retry", "provider " + d.Provider + " is not configured"
	}
	c, ok := a.(provider.Confirmer)
	if !ok {
		// Confirmation was turned off since the event arrived; the
		// configuration now accepts the provider's webhooks unconfirmed.
		return "confirmed", ""
	}
	if len(d.RawPayload) == 0 {
		return "failed", "payload purged before it could be confirmed"
	}
	e, err := a.Parse(d.RawPayload)
	if err != nil {
		return "failed", "stored payload no longer parses: " + err.Error()
	}
	ids := a.ExtractIdentifiers(e)
	cctx, cancel := context.WithTimeout(ctx, w.timeout())
	defer cancel()
	switch err := c.Confirm(cctx, e, ids); {
	case err == nil:
		return "confirmed", ""
	case errors.Is(err, provider.ErrNotConfirmed):
		return "rejected", err.Error()
	default:
		return "retry", err.Error()
	}
}

// Backoff is the delay after the given number of failed attempts.
func Backoff(attempts int) time.Duration {
	d := baseDelay
	for i := 1; i < attempts && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *Worker) timeout() time.Duration {
	if w.Timeout > 0 {
		return w.Timeout
	}
	return DefaultTimeout
}

func (w *Worker) maxAttempts() int {
	if w.MaxAttempts > 0 {
		return w.MaxAttempts
	}
	return DefaultMaxAttempts
}
