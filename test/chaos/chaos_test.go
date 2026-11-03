//go:build chaos

package chaos

import (
	"context"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

// TestChaosKill: SIGKILL relay repeatedly while Paystack-like senders push
// webhooks and a flaky receiver accepts deliveries. Invariants:
//  1. every webhook relay acknowledged (200) is durably stored;
//  2. every stored event is eventually delivered exactly once as a
//     delivery row, and reaches the receiver at least once;
//  3. every request the receiver saw carried a valid signature.
func TestChaosKill(t *testing.T) {
	const n = 600
	rc := &receiver{
		seen:     map[string]int{},
		failRate: func() bool { return rand.IntN(10) == 0 },
		delay:    func() time.Duration { return time.Duration(rand.IntN(15)) * time.Millisecond },
	}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	r := newRelay(t, srv.URL+"/hook", `, worker_count: 16, max_attempts: 50, retry_schedule: [0s, 100ms, 200ms]`)
	r.start()

	var (
		mu       sync.Mutex
		acked    = map[int]bool{}
		resent   = map[int]bool{}
		ackCount atomic.Int64
		sends    atomic.Int64
		refused  atomic.Int64
	)
	queue := make(chan int, n*3)
	for i := range n {
		queue <- i
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case i := <-queue:
					sends.Add(1)
					code, err := post(client, r.url, chargeBody(i))
					if err != nil || code != http.StatusOK {
						refused.Add(1)
						time.Sleep(20 * time.Millisecond) // Paystack retries later
						queue <- i
						continue
					}
					mu.Lock()
					first := !acked[i]
					acked[i] = true
					// Paystack occasionally resends an acknowledged event.
					again := first && !resent[i] && rand.IntN(20) == 0
					if again {
						resent[i] = true
					}
					mu.Unlock()
					if first {
						ackCount.Add(1)
					}
					if again {
						queue <- i
					}
				}
			}
		}()
	}

	// Kill relay three times during ingest.
	for _, at := range []int64{n / 4, n / 2, 3 * n / 4} {
		for ackCount.Load() < at {
			time.Sleep(time.Millisecond)
		}
		r.kill()
		time.Sleep(150 * time.Millisecond)
		r.start()
	}
	for ackCount.Load() < n {
		time.Sleep(10 * time.Millisecond)
	}
	// Let queued resends drain, then stop the senders.
	time.Sleep(300 * time.Millisecond)
	cancel()
	wg.Wait()

	// Kill relay three more times while deliveries are in flight.
	for range 3 {
		time.Sleep(300 * time.Millisecond)
		r.kill()
		time.Sleep(150 * time.Millisecond)
		r.start()
	}

	// Invariant 1: every acknowledged webhook is stored.
	st, err := store.OpenTool(context.Background(), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(context.Background(), store.EventFilter{Limit: 1_000_000})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]store.EventSummary{}
	dupWebhooks := 0
	for _, e := range events {
		stored[e.Reference] = e
		dupWebhooks += e.DuplicateCount
	}
	for i := range n {
		ref := "CH_" + pad(i)
		e, ok := stored[ref]
		if acked[i] && !ok {
			t.Errorf("event %s was acknowledged but is not stored", ref)
		} else if ok && e.RoutingStatus != "matched" {
			t.Errorf("event %s: routing %s", ref, e.RoutingStatus)
		}
	}
	if len(events) != n {
		t.Errorf("stored %d events, want %d (one per unique webhook)", len(events), n)
	}

	// Invariant 2: one delivery row per event, all delivered.
	waitAllDelivered(t, r.dsn, n, 90*time.Second)

	// Invariant 2b and 3: the receiver saw every event, signatures valid.
	rc.mu.Lock()
	defer rc.mu.Unlock()
	redeliveries := 0
	for _, e := range events {
		c := rc.seen[e.ID]
		if c == 0 {
			t.Errorf("receiver never got %s", e.ID)
		}
		redeliveries += c - 1
	}
	if len(rc.seen) != n {
		t.Errorf("receiver saw %d distinct events, want %d", len(rc.seen), n)
	}
	if rc.badSig != 0 {
		t.Errorf("%d requests had invalid signatures", rc.badSig)
	}
	t.Logf("kills=%d webhook_sends=%d refused_or_failed=%d duplicate_webhooks_deduped=%d receiver_redeliveries=%d",
		r.kills, sends.Load(), refused.Load(), dupWebhooks, redeliveries)
}

func pad(i int) string {
	s := itoa(i)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

// TestLoad measures single-instance ingest throughput and acknowledgement
// latency, then end-to-end delivery throughput, on this machine.
func TestLoad(t *testing.T) {
	for _, tc := range []struct{ events, concurrency int }{{3000, 16}, {3000, 64}} {
		t.Run(itoa(tc.concurrency)+"_concurrent", func(t *testing.T) {
			rc := &receiver{seen: map[string]int{}}
			srv := httptest.NewServer(rc)
			defer srv.Close()
			r := newRelay(t, srv.URL+"/hook", `, worker_count: 32`)
			r.start()

			// Warm up connections and the database.
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: tc.concurrency}}
			for i := range 20 {
				post(client, r.url, chargeBody(1_000_000+i))
			}

			latencies := make([]time.Duration, tc.events)
			var next atomic.Int64
			var failed atomic.Int64
			start := time.Now()
			var wg sync.WaitGroup
			for range tc.concurrency {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						i := int(next.Add(1)) - 1
						if i >= tc.events {
							return
						}
						t0 := time.Now()
						code, err := post(client, r.url, chargeBody(i))
						latencies[i] = time.Since(t0)
						if err != nil || code != http.StatusOK {
							failed.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			ingest := time.Since(start)
			waitAllDelivered(t, r.dsn, tc.events+20, 120*time.Second)
			total := time.Since(start)

			sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
			pct := func(p float64) time.Duration { return latencies[int(p*float64(len(latencies)-1))] }
			t.Logf("events=%d concurrency=%d failed=%d ingest=%.0f/s ack_p50=%v ack_p95=%v ack_p99=%v ack_max=%v end_to_end_delivered=%.0f/s",
				tc.events, tc.concurrency, failed.Load(), float64(tc.events)/ingest.Seconds(),
				pct(0.50).Round(10*time.Microsecond), pct(0.95).Round(10*time.Microsecond), pct(0.99).Round(10*time.Microsecond),
				latencies[len(latencies)-1].Round(10*time.Microsecond), float64(tc.events)/total.Seconds())
			if failed.Load() != 0 {
				t.Errorf("%d webhooks were not acknowledged", failed.Load())
			}
		})
	}
}
