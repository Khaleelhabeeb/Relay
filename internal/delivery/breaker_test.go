package delivery

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

func TestHealthOf(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   health
	}{
		{200, "", healthy},
		{404, "http_status", healthy},
		{302, "redirect_not_followed", healthy},
		{500, "http_status", unhealthy},
		{503, "http_status", unhealthy},
		{429, "http_status", unhealthy},
		{0, "timeout", unhealthy},
		{0, "connection_error", unhealthy},
		{0, "aborted", neutral},
		{0, "payload_error", neutral},
		{0, "blocked_destination", neutral},
		{0, "unknown_application", neutral},
	} {
		if got := healthOf(tc.status, tc.code); got != tc.want {
			t.Errorf("healthOf(%d, %q) = %v, want %v", tc.status, tc.code, got, tc.want)
		}
	}
}

func TestBreakerStateMachine(t *testing.T) {
	d := &Dispatcher{Workers: 8, MaxPerApp: 4, Breaker: BreakerSettings{Threshold: 3, Cooldown: time.Minute, MaxCooldown: 3 * time.Minute}}
	d.init()
	now := time.Unix(1_700_000_000, 0)
	capA := func() int { return d.capacity(now)("app_a") }

	// Failures below the threshold, and a success that resets the count.
	d.recordHealth("app_a", "d1", unhealthy, now)
	d.recordHealth("app_a", "d2", unhealthy, now)
	d.recordHealth("app_a", "d3", healthy, now)
	d.recordHealth("app_a", "d4", unhealthy, now)
	d.recordHealth("app_a", "d5", unhealthy, now)
	if got := capA(); got != 4 {
		t.Fatalf("closed circuit: capacity %d, want MaxPerApp", got)
	}
	if ev, cd := d.recordHealth("app_a", "d6", unhealthy, now); ev != breakerOpened || cd != time.Minute {
		t.Fatalf("third consecutive failure: %v %v, want opened for 1m", ev, cd)
	}
	if got := capA(); got != 0 {
		t.Fatalf("open circuit: capacity %d, want 0", got)
	}
	if got := d.capacity(now)("app_b"); got != 4 {
		t.Fatalf("other application: capacity %d, want 4", got)
	}
	// A straggler that started before the circuit opened changes nothing.
	if ev, _ := d.recordHealth("app_a", "d7", unhealthy, now); ev != breakerNone {
		t.Fatalf("straggler failure: %v, want none", ev)
	}

	// After the cooldown, exactly one probe.
	now = now.Add(time.Minute)
	if got := capA(); got != 1 {
		t.Fatalf("cooldown over: capacity %d, want 1 probe", got)
	}
	d.markProbe("app_a", "p1", now)
	if got := capA(); got != 0 {
		t.Fatalf("probe in flight: capacity %d, want 0", got)
	}
	// A failed probe doubles the cooldown, capped at MaxCooldown.
	if ev, cd := d.recordHealth("app_a", "p1", unhealthy, now); ev != breakerReopened || cd != 2*time.Minute {
		t.Fatalf("failed probe: %v %v, want reopened for 2m", ev, cd)
	}
	now = now.Add(2 * time.Minute)
	d.markProbe("app_a", "p2", now)
	if _, cd := d.recordHealth("app_a", "p2", unhealthy, now); cd != 3*time.Minute {
		t.Fatalf("second failed probe: cooldown %v, want capped at 3m", cd)
	}
	// A probe that proves nothing frees the slot for another probe.
	now = now.Add(3 * time.Minute)
	d.markProbe("app_a", "p3", now)
	d.recordHealth("app_a", "p3", neutral, now)
	if got := capA(); got != 1 {
		t.Fatalf("after neutral probe: capacity %d, want 1", got)
	}
	d.markProbe("app_a", "p4", now)
	if ev, _ := d.recordHealth("app_a", "p4", healthy, now); ev != breakerClosed {
		t.Fatalf("successful probe: %v, want closed", ev)
	}
	if got := capA(); got != 4 {
		t.Fatalf("closed again: capacity %d, want 4", got)
	}
}

func TestBreakerDisabled(t *testing.T) {
	d := &Dispatcher{Workers: 8}
	d.init()
	for i := range 20 {
		d.recordHealth("app_a", fmt.Sprint(i), unhealthy, time.Now())
	}
	if d.capacity(time.Now()) != nil {
		t.Fatal("no cap and no breaker: capacity should be unlimited (nil)")
	}
}

// A down application stops getting attempts once its circuit opens, its
// deliveries keep their retry budget, healthy applications are unaffected,
// and after the cooldown a probe resumes it.
func TestDownApplicationIsPausedThenResumed(t *testing.T) {
	var (
		requests atomic.Int64
		up       atomic.Bool
	)
	appA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(appA.Close)
	appB := okServer(t, nil)

	h := newHarness(t, appA.URL, appB.URL)
	var offset atomic.Int64 // test clock, ahead of real time
	h.disp.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	h.disp.Breaker = BreakerSettings{Threshold: 2, Cooldown: time.Hour}
	// One attempt at a time to app_a, so no attempt is already in flight
	// when the circuit opens and the request count is exact.
	h.disp.MaxPerApp = 1

	var as []store.Delivery
	for i := range 6 {
		as = append(as, h.ingestCharge(100+i, fmt.Sprintf("APPA_%d", i), ""))
	}
	b := h.ingestCharge(200, "T200", "app_b")
	h.start()

	h.waitStatus(b.ID, store.DeliveryDelivered)
	deadline := time.Now().Add(5 * time.Second)
	for requests.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // many poll intervals: nothing more may be sent
	if n := requests.Load(); n != 2 {
		t.Fatalf("app_a got %d requests with its circuit open, want the 2 that opened it", n)
	}
	var attempted int
	for _, d := range as {
		attempted += len(h.attempts(d.ID))
	}
	if attempted != 2 {
		t.Fatalf("app_a deliveries used %d attempts, want 2", attempted)
	}

	// The cooldown passes and the endpoint is back: a probe succeeds and
	// the backlog drains.
	up.Store(true)
	offset.Store(int64(2 * time.Hour))
	for _, d := range as {
		h.waitStatus(d.ID, store.DeliveryDelivered)
	}
}
