// Package retention periodically clears old raw payloads and attempt rows
// according to the configured retention policy.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

type Purger struct {
	Store       *store.Store
	PayloadDays int
	AttemptDays int
	Interval    time.Duration // default 6h
	Logger      *slog.Logger
	Now         func() time.Time
}

// RunOnce applies retention now.
func (p *Purger) RunOnce(ctx context.Context) (store.PurgeResult, error) {
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	day := 24 * time.Hour
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
