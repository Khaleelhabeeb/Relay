package alert

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/delivery"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
	"github.com/khaleelhabeeb/relay/receiver"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type capture struct {
	digests []admin.AlertDigest
	fail    error
}

func (c *capture) sink() Func {
	return func(_ context.Context, d admin.AlertDigest) error {
		if c.fail != nil {
			return c.fail
		}
		c.digests = append(c.digests, d)
		return nil
	}
}

var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	st  *store.Store
	acc *ingest.StoreAcceptor
	ids map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	engine := routing.NewEngine(routing.RulesFromConfig([]config.RouteConfig{
		{Application: "app_a", Match: config.MatchConfig{ReferencePrefix: "APPA_"}}}))
	return &env{st: st, acc: &ingest.StoreAcceptor{Store: st, Logger: logger, Engine: engine}, ids: map[string]string{}}
}

func (e *env) ingest(t *testing.T, id int, ref string, at time.Time) string {
	t.Helper()
	ev, _ := paystack.Parse([]byte(`{"event":"charge.success","data":{"id":` + itoa(id) + `,"reference":"` + ref + `"}}`))
	idents := paystack.ExtractIdentifiers(ev)
	a := &ingest.Accepted{Event: ev, Identifiers: idents, DedupKey: paystack.DedupKey(ev, idents), ReceivedAt: at}
	if err := e.acc.Accept(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var eid string
	_ = e.st.WithTx(context.Background(), func(tx *store.Tx) error {
		eid, _, _ = tx.EventIDByDedupKey(context.Background(), "paystack", a.DedupKey)
		return nil
	})
	return eid
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func stats(st *store.Store) func(context.Context) (admin.Stats, error) {
	return func(ctx context.Context) (admin.Stats, error) {
		s, err := st.Stats(ctx, time.Now())
		return admin.Stats{Events: admin.EventStats{Unresolved: s.EventsByStatus["unresolved"], Ambiguous: s.EventsByStatus["ambiguous"]},
			Deliveries: admin.DeliveryStats{Exhausted: s.DeliveriesByStatus["exhausted"]}}, err
	}
}

func TestDigestOfHeldEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e.ingest(t, 1, "APPA_1", now) // matched: no alert
	orphan := e.ingest(t, 2, "ZZ_2", now)
	gone := e.ingest(t, 3, "ZZ_3", now)

	// Resolved before the digest is built: left out.
	rp := &reprocess.Reprocessor{Store: e.st, Logger: logger}
	if err := rp.Reroute(ctx, gone, "app_a", "test", "cli"); err != nil {
		t.Fatal(err)
	}

	c := &capture{}
	a := &Alerter{Store: e.st, Sinks: []Sink{c.sink()}, StatsFn: stats(e.st), Logger: logger}
	if err := a.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.digests) != 1 {
		t.Fatalf("digests: %+v", c.digests)
	}
	d := c.digests[0]
	if d.Type != admin.AlertDigestType || !strings.HasPrefix(d.ID, "alr_") || d.Reminder ||
		d.Counts[admin.AlertUnresolved] != 1 || len(d.Alerts) != 1 || d.Alerts[0].EventID != orphan ||
		d.Alerts[0].Reference != "ZZ_2" || d.Stats.Events.Unresolved != 1 {
		t.Fatalf("digest: %+v", d)
	}

	// Sent alerts are not sent again.
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 1 {
		t.Fatalf("resent: %v %d", err, len(c.digests))
	}
}

func TestFailedSinkRetriesSameAlerts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.ingest(t, 2, "ZZ_2", time.Now().UTC())
	c := &capture{fail: errors.New("down")}
	a := &Alerter{Store: e.st, Sinks: []Sink{c.sink()}, Logger: logger}
	if err := a.RunOnce(ctx); err == nil {
		t.Fatal("a failing sink must fail the run")
	}
	c.fail = nil
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 1 || c.digests[0].Counts[admin.AlertUnresolved] != 1 {
		t.Fatalf("retry: %v %+v", err, c.digests)
	}
}

func TestDigestCapsListedAlerts(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UTC()
	for i := range admin.MaxDigestAlerts + 5 {
		e.ingest(t, 100+i, "ZZ_"+itoa(i), now)
	}
	c := &capture{}
	a := &Alerter{Store: e.st, Sinks: []Sink{c.sink()}, Logger: logger}
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := c.digests[0]
	if len(d.Alerts) != admin.MaxDigestAlerts || d.Omitted != 5 || d.Counts[admin.AlertUnresolved] != admin.MaxDigestAlerts+5 {
		t.Fatalf("cap: %d listed, %d omitted, %v", len(d.Alerts), d.Omitted, d.Counts)
	}
}

func TestReminderAndExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	clk := &clock{t: time.Now().UTC()}
	id := e.ingest(t, 2, "ZZ_2", clk.Now().Add(-40*24*time.Hour))
	c := &capture{}
	a := &Alerter{Store: e.st, Sinks: []Sink{c.sink()}, Reminder: 24 * time.Hour, StatsFn: stats(e.st), Logger: logger, Now: clk.Now}
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 1 {
		t.Fatalf("first digest: %v %d", err, len(c.digests))
	}
	// Nothing new and the reminder is not due yet.
	clk.Add(time.Hour)
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 1 {
		t.Fatalf("early reminder: %v %d", err, len(c.digests))
	}
	clk.Add(24 * time.Hour)
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 2 || !c.digests[1].Reminder || c.digests[1].Stats.Events.Unresolved != 1 {
		t.Fatalf("reminder: %v %+v", err, c.digests)
	}

	// Expiry dismisses it with a final alert; it is then no longer held.
	n, err := e.st.ExpireHeld(ctx, clk.Now().Add(-30*24*time.Hour), clk.Now(), "held longer than 30 days")
	if err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 3 {
		t.Fatalf("expiry digest: %v %d", err, len(c.digests))
	}
	d := c.digests[2]
	if d.Counts[admin.AlertExpired] != 1 || d.Alerts[0].EventID != id || d.Alerts[0].Detail != "held longer than 30 days" ||
		d.Stats.Events.Unresolved != 0 {
		t.Fatalf("expiry digest: %+v", d)
	}
	// Nothing needs attention any more: no reminders.
	clk.Add(48 * time.Hour)
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 3 {
		t.Fatalf("reminder with nothing held: %v %d", err, len(c.digests))
	}
}

func TestExhaustedDeliveryAlert(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.ingest(t, 1, "APPA_1", time.Now().UTC())
	now := time.Now()
	claimed, err := e.st.ClaimDue(ctx, "w", now, now.Add(time.Minute), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(claimed, err)
	}
	if err := e.st.CompleteAttempt(ctx, store.AttemptResult{DeliveryID: claimed[0].ID, AttemptID: claimed[0].AttemptID,
		Owner: "w", FinishedAt: now, HTTPStatus: 500, ErrorCode: "http_status", Error: "HTTP 500",
		Status: store.DeliveryExhausted}); err != nil {
		t.Fatal(err)
	}
	c := &capture{}
	a := &Alerter{Store: e.st, Sinks: []Sink{c.sink()}, Logger: logger}
	if err := a.RunOnce(ctx); err != nil || len(c.digests) != 1 {
		t.Fatalf("%v %+v", err, c.digests)
	}
	al := c.digests[0].Alerts[0]
	if al.Kind != admin.AlertDeliveryExhausted || al.DeliveryID != claimed[0].ID || al.ApplicationID != "app_a" || al.Detail != "HTTP 500" {
		t.Fatalf("alert: %+v", al)
	}
}

func TestWebhookSinks(t *testing.T) {
	const secret = "alerts_secret_0123456789abcdef_0123"
	var (
		got     []byte
		headers http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		headers = r.Header.Clone()
	}))
	defer srv.Close()
	d := &admin.AlertDigest{Type: admin.AlertDigestType, ID: "alr_1", Counts: map[string]int{admin.AlertUnresolved: 1},
		Alerts: []admin.Alert{{Kind: admin.AlertUnresolved, EventID: "evt_1", Provider: "paystack", Reference: "<!here>"}}}

	w := &Webhook{URL: srv.URL, Format: config.AlertFormatJSON, Secret: secret, Client: delivery.NewHTTPClient(true), UserAgent: "Relay"}
	if err := w.Send(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Verify(headers, got, []string{secret}, receiver.Options{}); err != nil {
		t.Fatalf("json digest must verify with receiver.Verify: %v", err)
	}
	var back admin.AlertDigest
	if err := json.Unmarshal(got, &back); err != nil || back.Alerts[0].EventID != "evt_1" {
		t.Fatalf("json body: %s", got)
	}

	w = &Webhook{URL: srv.URL, Format: config.AlertFormatSlack, Client: delivery.NewHTTPClient(true), UserAgent: "Relay"}
	if err := w.Send(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var msg map[string]string
	if err := json.Unmarshal(got, &msg); err != nil || !strings.Contains(msg["text"], "1 new alert") ||
		!strings.Contains(msg["text"], "&lt;!here&gt;") || strings.Contains(msg["text"], "<!here>") {
		t.Fatalf("slack text must escape mentions: %s", got)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer failing.Close()
	w.URL = failing.URL
	if err := w.Send(context.Background(), d); err == nil {
		t.Fatal("a non-2xx answer must fail")
	}
}
