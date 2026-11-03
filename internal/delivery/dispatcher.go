// Package delivery sends stored events to their owning applications with
// signed requests, bounded retries and SSRF protection.
package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/khaleelhabeeb/relay/internal/config"
	"github.com/khaleelhabeeb/relay/internal/ids"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/store"
)

const (
	defaultPollInterval = time.Second
	leaseMargin         = 30 * time.Second
	maxResponseDrain    = 4 << 10
	storeWriteTimeout   = 10 * time.Second
)

// App is a delivery destination.
type App struct {
	ID            string
	Endpoint      string
	PayloadFormat string
	Secrets       []string // current first, then previous (rotation)
}

// Dispatcher claims due deliveries from the store and sends them with a
// bounded worker pool. Run it once per process.
type Dispatcher struct {
	Store         *store.Store
	Apps          map[string]App
	Client        *http.Client
	Logger        *slog.Logger
	MaxAttempts   int
	Timeout       time.Duration
	Workers       int
	RetryDelay    func(attempt int) time.Duration // delay before the given attempt (1-based)
	PollInterval  time.Duration
	ShutdownGrace time.Duration // how long in-flight attempts may finish after shutdown starts
	UserAgent     string
	Now           func() time.Time
	Rand          func() float64
	Metrics       *metrics.Metrics // optional

	once  sync.Once
	owner string
	nudge chan struct{}
}

// NewDispatcher builds a dispatcher from configuration.
func NewDispatcher(cfg *config.Config, st *store.Store, logger *slog.Logger, userAgent string) *Dispatcher {
	apps := make(map[string]App, len(cfg.Applications))
	for id, a := range cfg.Applications {
		apps[id] = App{
			ID:            id,
			Endpoint:      a.Endpoint,
			PayloadFormat: a.PayloadFormat,
			Secrets:       []string{a.SigningSecret, a.PreviousSigningSecret},
		}
	}
	return &Dispatcher{
		Store:         st,
		Apps:          apps,
		Client:        NewHTTPClient(cfg.Delivery.AllowInsecureLocal),
		Logger:        logger,
		MaxAttempts:   cfg.Delivery.MaxAttempts,
		Timeout:       cfg.Delivery.Timeout(),
		Workers:       cfg.Delivery.WorkerCount,
		RetryDelay:    cfg.Delivery.RetryDelay,
		ShutdownGrace: cfg.Delivery.Timeout() + 5*time.Second,
		UserAgent:     userAgent,
	}
}

func (d *Dispatcher) init() {
	d.once.Do(func() {
		d.owner = ids.New("wrk")
		d.nudge = make(chan struct{}, 1)
		if d.PollInterval <= 0 {
			d.PollInterval = defaultPollInterval
		}
		if d.Workers <= 0 {
			d.Workers = 1
		}
		if d.Logger == nil {
			d.Logger = slog.New(slog.DiscardHandler)
		}
		d.Logger = d.Logger.With("component", "delivery")
	})
}

// Nudge wakes the dispatcher to look for due work now (e.g. right after
// ingest commits a delivery). It never blocks.
func (d *Dispatcher) Nudge() {
	d.init()
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// Run delivers until ctx is cancelled. On cancellation it stops claiming,
// lets in-flight attempts finish for up to ShutdownGrace, aborts the rest,
// and returns their deliveries to pending.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.init()
	if n, err := d.Store.RecoverOrphanedLeases(ctx, d.now()); err != nil {
		return fmt.Errorf("delivery: recover leases: %w", err)
	} else if n > 0 {
		d.Logger.Warn("recovered deliveries interrupted by a previous shutdown or crash", "count", n)
	}

	// Attempts run on workCtx, which outlives ctx by the shutdown grace.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()

	var (
		wg    sync.WaitGroup
		slots = make(chan struct{}, d.Workers)
		freed = make(chan struct{}, 1)
	)
	ticker := time.NewTicker(d.PollInterval)
	defer ticker.Stop()

	d.Logger.Info("delivery workers started", "workers", d.Workers, "owner", d.owner)
	for ctx.Err() == nil {
		if free := d.Workers - len(slots); free > 0 {
			now := d.now()
			claimed, err := d.Store.ClaimDue(ctx, d.owner, now, now.Add(d.Timeout+leaseMargin), free)
			if err != nil && ctx.Err() == nil {
				d.Logger.Error("claiming deliveries failed", "error", err)
			}
			for _, c := range claimed {
				slots <- struct{}{}
				wg.Add(1)
				go func(c store.ClaimedDelivery) {
					defer wg.Done()
					defer func() {
						<-slots
						select {
						case freed <- struct{}{}:
						default:
						}
					}()
					d.attempt(workCtx, c)
				}(c)
			}
			if len(claimed) == free {
				continue // there may be more due work
			}
		}
		select {
		case <-ctx.Done():
		case <-d.nudge:
		case <-freed:
		case <-ticker.C:
		}
	}

	d.Logger.Info("delivery workers stopping", "in_flight", len(slots))
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d.ShutdownGrace):
		d.Logger.Warn("shutdown grace elapsed; aborting in-flight deliveries")
		cancelWork()
		<-done
	}
	sctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if n, err := d.Store.ReleaseLeases(sctx, d.owner, d.now()); err != nil {
		return fmt.Errorf("delivery: release leases: %w", err)
	} else if n > 0 {
		d.Logger.Warn("released unfinished deliveries", "count", n)
	}
	return nil
}

// attempt sends one claimed delivery and records the outcome.
func (d *Dispatcher) attempt(workCtx context.Context, c store.ClaimedDelivery) {
	log := d.Logger.With("delivery_id", c.ID, "event_id", c.EventID, "application", c.ApplicationID, "attempt", c.AttemptNo)
	start := d.now()
	r := store.AttemptResult{DeliveryID: c.ID, AttemptID: c.AttemptID, Owner: d.owner}

	if app, ok := d.Apps[c.ApplicationID]; !ok {
		r.ErrorCode, r.Error = "unknown_application", "application is not in the current configuration"
	} else if body, err := BuildBody(app.PayloadFormat, c.RawPayload, c.EventID, c.Provider, c.EventType, app.ID, c.ReceivedAt); err != nil {
		r.ErrorCode, r.Error = "payload_error", err.Error()
	} else {
		reqCtx, cancel := context.WithTimeout(workCtx, d.Timeout)
		r.HTTPStatus, r.ErrorCode, r.Error = d.send(reqCtx, app, c, body)
		cancel()
	}

	r.FinishedAt = d.now()
	r.Duration = r.FinishedAt.Sub(start)
	switch {
	case r.ErrorCode == "":
		r.Status = store.DeliveryDelivered
	case r.ErrorCode == "aborted":
		// Interrupted by shutdown, not the destination's fault: retry promptly.
		r.Status, r.NextAttemptAt = store.DeliveryPending, r.FinishedAt
	case c.CycleAttempt >= d.MaxAttempts:
		r.Status = store.DeliveryExhausted
	default:
		r.Status = store.DeliveryPending
		var delay time.Duration
		if d.RetryDelay != nil {
			delay = d.RetryDelay(c.CycleAttempt + 1)
		}
		r.NextAttemptAt = r.FinishedAt.Add(jitter(delay, d.Rand))
	}

	sctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if err := d.Store.CompleteAttempt(sctx, r); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			log.Warn("delivery lease lost before the result was recorded; it will be retried")
		} else {
			log.Error("recording delivery result failed; lease will expire and the delivery will be retried", "error", err)
		}
		return
	}

	d.Metrics.Attempt(c.ApplicationID, r.ErrorCode, r.Duration, r.Status == store.DeliveryExhausted)
	attrs := []any{"status", r.Status, "http_status", r.HTTPStatus, "duration_ms", r.Duration.Milliseconds()}
	switch r.Status {
	case store.DeliveryDelivered:
		log.Info("delivered", attrs...)
	case store.DeliveryExhausted:
		log.Error("delivery exhausted; needs operator replay", append(attrs, "error_code", r.ErrorCode, "error", r.Error)...)
	default:
		log.Warn("delivery attempt failed", append(attrs, "error_code", r.ErrorCode, "error", r.Error, "next_attempt_at", r.NextAttemptAt)...)
	}
}

// send performs the HTTP request. It returns the response status (0 if
// none) and, on failure, an error code and short message.
func (d *Dispatcher) send(ctx context.Context, app App, c store.ClaimedDelivery, body []byte) (int, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "request_error", err.Error()
	}
	ts := strconv.FormatInt(d.now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	if d.UserAgent != "" {
		req.Header.Set("User-Agent", d.UserAgent)
	}
	req.Header.Set(HeaderEventID, c.EventID)
	req.Header.Set(HeaderDeliveryID, c.ID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderAttempt, strconv.Itoa(c.AttemptNo))
	req.Header.Set(HeaderProvider, c.Provider)
	req.Header.Set(HeaderSignature, Sign(app.Secrets, ts, c.EventID, c.ID, body))

	resp, err := d.Client.Do(req)
	if err != nil {
		msg := err.Error()
		var uerr *url.Error
		if errors.As(err, &uerr) {
			msg = uerr.Err.Error() // drop the URL, which may carry credentials in its query
		}
		return 0, classify(ctx, err), msg
	}
	// Drain a bounded amount so the connection can be reused; never store it.
	_, _ = io.CopyN(io.Discard, resp.Body, maxResponseDrain)
	resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, "", ""
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return resp.StatusCode, "redirect_not_followed", fmt.Sprintf("HTTP %d (redirects are not followed)", resp.StatusCode)
	default:
		return resp.StatusCode, "http_status", fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}
