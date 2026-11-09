package confirm_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/confirm"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

// api stands in for a provider's confirmation API: per reference, the
// answers it gives, one per call (the last repeats).
type api struct {
	mu      sync.Mutex
	answers map[string][]error
	calls   map[string]int
}

type confirming struct {
	paystack.Adapter
	api *api
}

func (c confirming) Confirm(_ context.Context, _ *provider.Event, ids provider.Identifiers) error {
	c.api.mu.Lock()
	defer c.api.mu.Unlock()
	c.api.calls[ids.Reference]++
	a := c.api.answers[ids.Reference]
	if len(a) == 0 {
		return nil
	}
	err := a[0]
	if len(a) > 1 {
		c.api.answers[ids.Reference] = a[1:]
	}
	return err
}

var (
	errOutage = errors.New("verify transaction: context deadline exceeded")
	errForged = fmt.Errorf("%w: amount differs", provider.ErrNotConfirmed)
)

type harness struct {
	t         *testing.T
	st        *store.Store
	acc       *ingest.StoreAcceptor
	w         *confirm.Worker
	api       *api
	now       time.Time
	delivered int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, st: st, api: &api{answers: map[string][]error{}, calls: map[string]int{}},
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rules := []routing.Rule{{Name: "routes[0]", Application: "app_a", ReferencePrefix: "APPA_"}}
	ad := confirming{Adapter: paystack.Adapter{Secret: "sk_test"}, api: h.api}
	h.acc = &ingest.StoreAcceptor{Store: st, Engine: routing.NewEngine(rules, ad), Logger: logger}
	h.w = confirm.New(st, []provider.Adapter{ad}, logger, nil)
	h.w.Now = func() time.Time { return h.now }
	h.w.MaxAttempts = 4
	h.w.OnDelivery = func() { h.delivered++ }
	return h
}

func body(id int, ref string, amount int) []byte {
	return fmt.Appendf(nil, `{"event":"charge.success","data":{"id":%d,"reference":%q,"status":"success","amount":%d,"currency":"NGN"}}`,
		id, ref, amount)
}

// accept stores raw as ingest does after a failed API check.
func (h *harness) accept(raw []byte, conf store.Confirmation) error {
	e, err := paystack.Parse(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	ids := paystack.ExtractIdentifiers(e)
	a := &ingest.Accepted{Event: e, Identifiers: ids, DedupKey: paystack.DedupKey(e, ids), ReceivedAt: h.now,
		Confirmation: conf}
	if conf == store.ConfirmPending {
		a.ConfirmError, a.ConfirmNextAt = errOutage.Error(), h.now
	}
	return h.acc.Accept(context.Background(), a)
}

func (h *harness) event(ref string) *store.Event {
	h.t.Helper()
	evs, err := h.st.ListEvents(context.Background(), store.EventFilter{})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, s := range evs {
		if s.Reference == ref {
			e, err := h.st.GetEvent(context.Background(), s.ID)
			if err != nil {
				h.t.Fatal(err)
			}
			return e
		}
	}
	h.t.Fatalf("no event with reference %s", ref)
	return nil
}

func (h *harness) deliveries(e *store.Event) int {
	h.t.Helper()
	ds, err := h.st.DeliveriesForEvent(context.Background(), e.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(ds)
}

func (h *harness) sweep() int {
	h.t.Helper()
	n, err := h.w.RunOnce(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestPendingEventIsDeliveredOnceConfirmed(t *testing.T) {
	h := newHarness(t)
	h.api.answers["APPA_1"] = []error{errOutage, nil}
	if err := h.accept(body(1, "APPA_1", 500), store.ConfirmPending); err != nil {
		t.Fatal(err)
	}
	e := h.event("APPA_1")
	if e.Confirmation != store.ConfirmPending || e.RoutingStatus != routing.Matched || h.deliveries(e) != 0 {
		t.Fatalf("stored %s/%s with %d deliveries, want pending, matched, none", e.Confirmation, e.RoutingStatus, h.deliveries(e))
	}

	// First background try fails: backed off, still undelivered.
	if n := h.sweep(); n != 1 {
		t.Fatalf("swept %d", n)
	}
	e = h.event("APPA_1")
	if e.Confirmation != store.ConfirmPending || e.ConfirmAttempts != 2 ||
		!e.ConfirmNextAt.Equal(h.now.Add(confirm.Backoff(2))) || e.ConfirmError == "" || h.deliveries(e) != 0 {
		t.Fatalf("after outage: %+v, %d deliveries", e, h.deliveries(e))
	}
	if n := h.sweep(); n != 0 {
		t.Fatalf("swept %d before the backoff elapsed", n)
	}

	h.now = h.now.Add(confirm.Backoff(2))
	h.sweep()
	e = h.event("APPA_1")
	if e.Confirmation != store.ConfirmConfirmed || h.deliveries(e) != 1 || h.delivered != 1 {
		t.Fatalf("after confirmation: %s, %d deliveries, %d nudges", e.Confirmation, h.deliveries(e), h.delivered)
	}
	if h.api.calls["APPA_1"] != 2 {
		t.Fatalf("API calls %d", h.api.calls["APPA_1"])
	}
}

func TestRejectedEventIsNeverDeliveredAndFreesItsKey(t *testing.T) {
	h := newHarness(t)
	// A forged copy (inflated amount, valid signature) arrives during an
	// outage, before the genuine event.
	h.api.answers["APPA_2"] = []error{errForged}
	forged := body(2, "APPA_2", 900000)
	if err := h.accept(forged, store.ConfirmPending); err != nil {
		t.Fatal(err)
	}
	// The genuine webhook arrives while the forgery is pending: it differs,
	// so it is refused for now rather than counted as the forgery's duplicate.
	genuine := body(2, "APPA_2", 500)
	if err := h.accept(genuine, store.ConfirmConfirmed); !errors.Is(err, ingest.ErrConfirmationPending) {
		t.Fatalf("different bytes during pending: %v", err)
	}
	// An identical resend of the pending one is an ordinary duplicate.
	if err := h.accept(forged, store.ConfirmPending); err != nil {
		t.Fatalf("identical resend: %v", err)
	}

	h.sweep()
	e := h.event("APPA_2")
	if e.Confirmation != store.ConfirmRejected || h.deliveries(e) != 0 || e.DedupKey[:9] != "rejected:" {
		t.Fatalf("forgery: %s, %d deliveries, key %s", e.Confirmation, h.deliveries(e), e.DedupKey)
	}
	// The provider's retry of the genuine event is now stored and delivered.
	if err := h.accept(genuine, store.ConfirmConfirmed); err != nil {
		t.Fatal(err)
	}
	evs, _ := h.st.ListEvents(context.Background(), store.EventFilter{Confirmation: string(store.ConfirmConfirmed)})
	if len(evs) != 1 || evs[0].ApplicationID != "app_a" {
		t.Fatalf("genuine event: %+v", evs)
	}
	g, _ := h.st.GetEvent(context.Background(), evs[0].ID)
	if h.deliveries(g) != 1 {
		t.Fatalf("genuine event has %d deliveries", h.deliveries(g))
	}
}

func TestFailedAfterRetryBudgetAndOperatorRetry(t *testing.T) {
	h := newHarness(t)
	h.api.answers["APPA_3"] = []error{errOutage, errOutage, errOutage, nil}
	if err := h.accept(body(3, "APPA_3", 500), store.ConfirmPending); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		h.sweep()
		h.now = h.now.Add(time.Hour)
	}
	e := h.event("APPA_3")
	if e.Confirmation != store.ConfirmFailed || e.ConfirmAttempts != 4 || h.deliveries(e) != 0 {
		t.Fatalf("after budget: %s after %d attempts, %d deliveries", e.Confirmation, e.ConfirmAttempts, h.deliveries(e))
	}
	if err := h.st.RetryConfirmation(context.Background(), e.ID, h.now); err != nil {
		t.Fatal(err)
	}
	h.sweep()
	if e = h.event("APPA_3"); e.Confirmation != store.ConfirmConfirmed || h.deliveries(e) != 1 {
		t.Fatalf("after operator retry: %s, %d deliveries", e.Confirmation, h.deliveries(e))
	}
	if err := h.st.RetryConfirmation(context.Background(), e.ID, h.now); !errors.Is(err, store.ErrNotPending) {
		t.Fatalf("retry of a confirmed event: %v", err)
	}
}

// A pending event routed later (registration, reprocess, operator) gets its
// delivery only once confirmed.
func TestLateRoutingWaitsForConfirmation(t *testing.T) {
	h := newHarness(t)
	h.api.answers["ORDER_4"] = []error{errOutage, nil}
	if err := h.accept(body(4, "ORDER_4", 500), store.ConfirmPending); err != nil {
		t.Fatal(err)
	}
	h.sweep() // fails once; stays pending
	e := h.event("ORDER_4")
	if e.RoutingStatus != routing.Unresolved {
		t.Fatalf("routing %s", e.RoutingStatus)
	}
	d := routing.Decision{Outcome: routing.Matched, Application: "app_a", Reason: "registered"}
	err := h.st.WithTx(context.Background(), func(tx *store.Tx) error {
		if err := tx.SetEventRouting(context.Background(), e.ID, d.Outcome, d.Application); err != nil {
			return err
		}
		return tx.ApplyDecision(context.Background(), e.ID, d, store.DecidedByRegistration, h.now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.deliveries(e) != 0 {
		t.Fatal("delivery created before confirmation")
	}
	h.now = h.now.Add(time.Hour)
	h.sweep()
	if e = h.event("ORDER_4"); e.Confirmation != store.ConfirmConfirmed || h.deliveries(e) != 1 {
		t.Fatalf("after confirmation: %s, %d deliveries", e.Confirmation, h.deliveries(e))
	}
}

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 6: 16 * time.Minute, 7: 30 * time.Minute, 100: 30 * time.Minute,
	} {
		if got := confirm.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
	// The default budget lasts about three days.
	var total time.Duration
	for a := 1; a < confirm.DefaultMaxAttempts; a++ {
		total += confirm.Backoff(a)
	}
	if total < 70*time.Hour || total > 76*time.Hour {
		t.Errorf("default retry window %v, want about 72h", total)
	}
}
