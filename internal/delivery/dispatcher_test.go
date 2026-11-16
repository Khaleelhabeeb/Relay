package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider/paystack"
)

const (
	appASecret = "whsec_app_a_0123456789abcdef0123456789ab"
	appBSecret = "whsec_app_b_0123456789abcdef0123456789abcdef"
)

type harness struct {
	t     *testing.T
	store *store.Store
	acc   *ingest.StoreAcceptor
	disp  *Dispatcher
}

func newHarness(t *testing.T, appAURL, appBURL string) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disp := &Dispatcher{
		Store: st,
		Apps: map[string]App{
			"app_a": {ID: "app_a", Endpoint: appAURL, PayloadFormat: "original", Secrets: []string{appASecret}},
			"app_b": {ID: "app_b", Endpoint: appBURL, PayloadFormat: "envelope", Secrets: []string{appBSecret}},
		},
		Client:        NewHTTPClient(true),
		Logger:        logger,
		MaxAttempts:   5,
		Timeout:       2 * time.Second,
		Workers:       4,
		RetryDelay:    func(int) time.Duration { return 0 },
		PollInterval:  10 * time.Millisecond,
		ShutdownGrace: time.Second,
	}
	acc := &ingest.StoreAcceptor{
		Store: st,
		Engine: routing.NewEngine([]routing.Rule{
			{Name: "routes[0]", Application: "app_a", ReferencePrefix: "APPA_"},
			{Name: "routes[1]", Application: "app_b", Metadata: map[string]string{"application": "app_b"}},
		}),
		Logger:   logger,
		OnRouted: disp.Nudge,
	}
	return &harness{t: t, store: st, acc: acc, disp: disp}
}

// ingestCharge stores a charge.success and returns its delivery.
func (h *harness) ingestCharge(id int, reference, metadataApp string) store.Delivery {
	h.t.Helper()
	meta := "{}"
	if metadataApp != "" {
		meta = fmt.Sprintf(`{"application":%q}`, metadataApp)
	}
	raw := []byte(fmt.Sprintf(`{"event":"charge.success","data":{"id":%d,"reference":%q,"metadata":%s}}`, id, reference, meta))
	e, err := paystack.Parse(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	ids := paystack.ExtractIdentifiers(e)
	a := &ingest.Accepted{Event: e, Identifiers: ids, DedupKey: paystack.DedupKey(e, ids), ReceivedAt: time.Now().UTC()}
	if err := h.acc.Accept(context.Background(), a); err != nil {
		h.t.Fatal(err)
	}
	var eventID string
	_ = h.store.WithTx(context.Background(), func(tx *store.Tx) error {
		eventID, _, err = tx.EventIDByDedupKey(context.Background(), "paystack", a.DedupKey)
		return err
	})
	dels, err := h.store.DeliveriesForEvent(context.Background(), eventID)
	if err != nil || len(dels) != 1 {
		h.t.Fatalf("expected one delivery, got %v (%v)", dels, err)
	}
	return dels[0]
}

// start runs the dispatcher; the returned func stops it and waits.
func (h *harness) start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.disp.Run(ctx) }()
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Run: %v", err)
		}
	}
	h.t.Cleanup(func() {
		select {
		case <-ctx.Done():
		default:
			stop()
		}
	})
	return stop
}

func (h *harness) waitStatus(id string, want store.DeliveryStatus) *store.Delivery {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := h.store.GetDelivery(context.Background(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		if d.Status == want {
			return d
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("delivery %s: status %s after 5s, want %s", id, d.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) attempts(id string) []store.Attempt {
	h.t.Helper()
	a, err := h.store.AttemptsForDelivery(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func okServer(t *testing.T, fn func(r *http.Request, body []byte)) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if fn != nil {
			fn(r, body)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestDeliversBothFormatsWithValidSignatures(t *testing.T) {
	type got struct {
		format string
		err    error
		header http.Header
		body   []byte
	}
	results := make(chan got, 2)
	check := func(format, secret string) func(*http.Request, []byte) {
		return func(r *http.Request, body []byte) {
			err := Verify(secret, r.Header.Get(HeaderSignature), r.Header.Get(HeaderTimestamp),
				r.Header.Get(HeaderEventID), r.Header.Get(HeaderDeliveryID), body, time.Now(), DefaultTolerance)
			results <- got{format, err, r.Header.Clone(), body}
		}
	}
	appA := okServer(t, check("original", appASecret))
	appB := okServer(t, check("envelope", appBSecret))
	h := newHarness(t, appA.URL, appB.URL)

	d1 := h.ingestCharge(1, "APPA_1", "")
	d2 := h.ingestCharge(2, "T2", "app_b")
	h.start()
	h.waitStatus(d1.ID, store.DeliveryDelivered)
	h.waitStatus(d2.ID, store.DeliveryDelivered)

	for range 2 {
		r := <-results
		if r.err != nil {
			t.Errorf("%s: signature: %v", r.format, r.err)
		}
		if r.header.Get("Content-Type") != "application/json" || r.header.Get(HeaderAttempt) != "1" ||
			r.header.Get(HeaderProvider) != "paystack" {
			t.Errorf("%s: headers %v", r.format, r.header)
		}
		switch r.format {
		case "original":
			if string(r.body) != `{"event":"charge.success","data":{"id":1,"reference":"APPA_1","metadata":{}}}` {
				t.Errorf("original body = %s", r.body)
			}
		case "envelope":
			if r.header.Get(HeaderEventID) != d2.EventID {
				t.Errorf("event id header = %s", r.header.Get(HeaderEventID))
			}
		}
	}
	if a := h.attempts(d1.ID); len(a) != 1 || a[0].HTTPStatus != 200 || !a[0].Finished || a[0].ErrorCode != "" {
		t.Errorf("attempts = %+v", a)
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	h := newHarness(t, ts.URL, ts.URL)
	d := h.ingestCharge(1, "APPA_1", "")
	h.start()
	h.waitStatus(d.ID, store.DeliveryDelivered)

	a := h.attempts(d.ID)
	if len(a) != 3 || a[0].HTTPStatus != 500 || a[0].ErrorCode != "http_status" || a[2].HTTPStatus != 200 {
		t.Fatalf("attempts = %+v", a)
	}
}

func TestExhaustsAfterMaxAttempts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()
	h := newHarness(t, ts.URL, ts.URL)
	h.disp.MaxAttempts = 3
	d := h.ingestCharge(1, "APPA_1", "")
	h.start()
	got := h.waitStatus(d.ID, store.DeliveryExhausted)
	if got.AttemptCount != 3 || len(h.attempts(d.ID)) != 3 {
		t.Fatalf("attempt_count = %d, rows = %d", got.AttemptCount, len(h.attempts(d.ID)))
	}
}

func TestTimeoutAndRedirectAreFailures(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()

	h := newHarness(t, slow.URL, redirect.URL)
	h.disp.MaxAttempts, h.disp.Timeout = 1, 100*time.Millisecond
	d1 := h.ingestCharge(1, "APPA_1", "")
	d2 := h.ingestCharge(2, "T2", "app_b")
	h.start()
	h.waitStatus(d1.ID, store.DeliveryExhausted)
	h.waitStatus(d2.ID, store.DeliveryExhausted)
	if a := h.attempts(d1.ID); a[0].ErrorCode != "timeout" || a[0].HTTPStatus != 0 {
		t.Errorf("slow attempt = %+v", a[0])
	}
	if a := h.attempts(d2.ID); a[0].ErrorCode != "redirect_not_followed" || a[0].HTTPStatus != 302 {
		t.Errorf("redirect attempt = %+v", a[0])
	}
}

func TestSchedulesNextAttemptWithBackoff(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	h := newHarness(t, ts.URL, ts.URL)
	var asked atomic.Int32
	h.disp.RetryDelay = func(attempt int) time.Duration {
		asked.Store(int32(attempt))
		return time.Hour
	}
	d := h.ingestCharge(1, "APPA_1", "")
	before := time.Now()
	stop := h.start()

	deadline := time.Now().Add(5 * time.Second)
	for len(h.attempts(d.ID)) == 0 || !h.attempts(d.ID)[0].Finished {
		if time.Now().After(deadline) {
			t.Fatal("first attempt never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // several poll cycles: must not retry early
	stop()

	got, _ := h.store.GetDelivery(context.Background(), d.ID)
	if got.Status != store.DeliveryPending || len(h.attempts(d.ID)) != 1 || asked.Load() != 2 {
		t.Fatalf("status %s, attempts %d, asked delay for attempt %d", got.Status, len(h.attempts(d.ID)), asked.Load())
	}
	wait := got.NextAttemptAt.Sub(before)
	if wait < 47*time.Minute || wait > 73*time.Minute {
		t.Fatalf("next attempt in %v, want 1h ±20%%", wait)
	}
}

func TestConcurrentDeliveriesExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	ts := okServer(t, func(r *http.Request, _ []byte) {
		mu.Lock()
		seen[r.Header.Get(HeaderDeliveryID)]++
		mu.Unlock()
	})
	h := newHarness(t, ts.URL, ts.URL)
	h.disp.Workers = 8
	var dels []store.Delivery
	for i := range 60 {
		dels = append(dels, h.ingestCharge(1000+i, fmt.Sprintf("APPA_%d", i), ""))
	}
	h.start()
	for _, d := range dels {
		h.waitStatus(d.ID, store.DeliveryDelivered)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, d := range dels {
		if seen[d.ID] != 1 {
			t.Errorf("delivery %s sent %d times", d.ID, seen[d.ID])
		}
	}
}

func TestRecoversLeaseFromCrashedProcess(t *testing.T) {
	ts := okServer(t, nil)
	h := newHarness(t, ts.URL, ts.URL)
	d := h.ingestCharge(1, "APPA_1", "")

	// A previous process claimed the delivery and died before finishing.
	now := time.Now()
	if c, err := h.store.ClaimDue(context.Background(), "wrk_dead", now, now.Add(time.Hour), 10); err != nil || len(c) != 1 {
		t.Fatalf("claim: %v %v", c, err)
	}
	h.start()
	h.waitStatus(d.ID, store.DeliveryDelivered)

	a := h.attempts(d.ID)
	if len(a) != 2 || a[0].Finished || !a[1].Finished || a[1].HTTPStatus != 200 {
		t.Fatalf("attempts = %+v (want an unfinished crashed attempt, then a success)", a)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	h := newHarness(t, "http://unused.invalid", "http://unused.invalid")
	d := h.ingestCharge(1, "APPA_1", "")
	now := time.Now()
	if _, err := h.store.ClaimDue(context.Background(), "wrk_a", now, now.Add(time.Second), 10); err != nil {
		t.Fatal(err)
	}
	if c, _ := h.store.ClaimDue(context.Background(), "wrk_b", now, now.Add(time.Minute), 10); len(c) != 0 {
		t.Fatal("a live lease must not be claimed")
	}
	c, err := h.store.ClaimDue(context.Background(), "wrk_b", now.Add(2*time.Second), now.Add(time.Minute), 10)
	if err != nil || len(c) != 1 || c[0].ID != d.ID || c[0].AttemptNo != 2 {
		t.Fatalf("expired lease reclaim = %+v, %v", c, err)
	}
	// The old owner can no longer record a result.
	err = h.store.CompleteAttempt(context.Background(), store.AttemptResult{
		DeliveryID: d.ID, AttemptID: "att_x", Owner: "wrk_a", FinishedAt: now, Status: store.DeliveryDelivered})
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale owner complete = %v, want ErrLeaseLost", err)
	}
}

func TestShutdownAbortsAndReleases(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)

	h := newHarness(t, ts.URL, ts.URL)
	h.disp.ShutdownGrace = 50 * time.Millisecond
	d := h.ingestCharge(1, "APPA_1", "")
	stop := h.start()
	<-arrived
	stop()

	got, _ := h.store.GetDelivery(context.Background(), d.ID)
	a := h.attempts(d.ID)
	if got.Status != store.DeliveryPending || len(a) != 1 || a[0].ErrorCode != "aborted" {
		t.Fatalf("status %s, attempts %+v", got.Status, a)
	}
}

func TestUnknownApplicationFails(t *testing.T) {
	h := newHarness(t, "http://unused.invalid", "http://unused.invalid")
	delete(h.disp.Apps, "app_a") // removed from config after the event was routed
	h.disp.MaxAttempts = 2
	d := h.ingestCharge(1, "APPA_1", "")
	h.start()
	h.waitStatus(d.ID, store.DeliveryExhausted)
	if a := h.attempts(d.ID); a[0].ErrorCode != "unknown_application" {
		t.Fatalf("attempts = %+v", a)
	}
}

func TestSlowApplicationDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	var hung atomic.Int32
	appA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hung.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(appA.Close)
	t.Cleanup(func() { close(release) })
	appB := okServer(t, nil)

	h := newHarness(t, appA.URL, appB.URL)
	h.disp.Workers, h.disp.MaxPerApp = 4, 2
	h.disp.Timeout = 30 * time.Second // app A holds its workers for the whole test

	for i := range 6 {
		h.ingestCharge(100+i, fmt.Sprintf("APPA_%d", i), "")
	}
	b := h.ingestCharge(200, "T200", "app_b")
	h.start()

	h.waitStatus(b.ID, store.DeliveryDelivered)
	if n := hung.Load(); n > 2 {
		t.Fatalf("app_a has %d concurrent attempts, want at most MaxPerApp (2)", n)
	}
}
