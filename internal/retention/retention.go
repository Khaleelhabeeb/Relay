// Package retention periodically clears old raw payloads and attempt rows
// according to the configured retention policy.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

// AlertDays is how long queued operator alerts are kept, sent or not.
const AlertDays = 7

type Purger struct {
	Store       *store.Store
	PayloadDays int
	AttemptDays int
	// HeldDays, if positive, dismisses events held (unresolved or
	// ambiguous) for longer, each with an event.expired alert.
	HeldDays int
	Interval time.Duration // default 6h
	Logger   *slog.Logger
	Now      func() time.Time
}

// RunOnce applies retention now.
func (p *Purger) RunOnce(ctx context.Context) (store.PurgeResult, error) {
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	day := 24 * time.Hour
	if p.HeldDays > 0 {
		n, err := p.Store.ExpireHeld(ctx, now.Add(-time.Duration(p.HeldDays)*day), now,
			fmt.Sprintf("held longer than retention.held_days (%d)", p.HeldDays))
		if err != nil {
			return store.PurgeResult{}, err
		}
		if n > 0 && p.Logger != nil {
			p.Logger.Warn("held events expired without an owner", "component", "retention", "events", n, "held_days", p.HeldDays)
		}
	}
	// Alerts are queued whether or not alerting is on; old ones go here so
	// the outbox stays small either way.
	if _, err := p.Store.PruneAlerts(ctx, now.Add(-AlertDays*day)); err != nil {
		return store.PurgeResult{}, err
	}
	return p.Store.Purge(ctx, now.Add(-time.Duration(p.PayloadDays)*day), now.Add(-time.Duration(p.AttemptDays)*day))
}

// Run purges shortly after start and then every Interval until ctx ends.
func (p *Purger) Run(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		res, err := p.RunOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			p.Logger.Error("retention purge failed", "component", "retention", "error", err)
		case res.PayloadsCleared > 0 || res.AttemptsDeleted > 0:
			p.Logger.Info("retention purge", "component", "retention",
				"payloads_cleared", res.PayloadsCleared, "attempts_deleted", res.AttemptsDeleted)
		}
		timer.Reset(interval)
	}
}
