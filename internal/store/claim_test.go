package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/routing"
)

func seedPending(t *testing.T, s *Store, id, app string, due time.Time) {
	t.Helper()
	seed(t, s, "evt_"+id, routing.Matched, due, &Delivery{ID: id, ApplicationID: app, Status: DeliveryPending,
		NextAttemptAt: due, CreatedAt: due})
}

func claimedIDs(c []ClaimedDelivery) []string {
	out := make([]string, len(c))
	for i, d := range c {
		out[i] = d.ID
	}
	return out
}

func TestClaimDueOrdersAcrossApplications(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seedPending(t, s, "del_a2", "a", t0.Add(2*time.Second))
	seedPending(t, s, "del_b1", "b", t0.Add(1*time.Second))
	seedPending(t, s, "del_a3", "a", t0.Add(3*time.Second))
	seedPending(t, s, "del_c0", "c", t0)
	seedPending(t, s, "del_future", "b", t0.Add(time.Hour))

	c, err := s.ClaimDue(ctx, "w", t0.Add(time.Minute), t0.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(claimedIDs(c)), "[del_c0 del_b1 del_a2 del_a3]"; got != want {
		t.Fatalf("claimed %s, want %s (oldest due first, not-yet-due skipped)", got, want)
	}
	if c[0].Provider != "paystack" || c[0].EventID != "evt_del_c0" || len(c[0].RawPayload) == 0 || !c[0].ReceivedAt.Equal(t0) {
		t.Fatalf("claim is missing event fields: %+v", c[0])
	}
}

func TestClaimDueFairRespectsPerApplicationCapacity(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	// Application a has a large, older backlog; b has one newer delivery.
	for i := range 20 {
		seedPending(t, s, fmt.Sprintf("del_a%02d", i), "a", t0.Add(time.Duration(i)*time.Millisecond))
	}
	seedPending(t, s, "del_b", "b", t0.Add(time.Second))

	capacity := func(app string) int {
		if app == "a" {
			return 2
		}
		return 8
	}
	c, err := s.ClaimDueFair(ctx, "w", t0.Add(time.Minute), t0.Add(2*time.Minute), 8, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(claimedIDs(c)), "[del_a00 del_a01 del_b]"; got != want {
		t.Fatalf("claimed %s, want %s: a's backlog must not crowd out b", got, want)
	}

	// An application with no room is skipped entirely.
	c, err = s.ClaimDueFair(ctx, "w", t0.Add(time.Minute), t0.Add(2*time.Minute), 8, func(string) int { return 0 })
	if err != nil || len(c) != 0 {
		t.Fatalf("no capacity: claimed %v (%v)", claimedIDs(c), err)
	}
}

func TestClaimDueTakesExpiredLeasesFirstWithinCapacity(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seedPending(t, s, "del_lease", "a", t0)
	if c, err := s.ClaimDue(ctx, "dead", t0, t0.Add(time.Second), 1); err != nil || len(c) != 1 {
		t.Fatalf("first claim: %v %v", c, err)
	}
	seedPending(t, s, "del_a", "a", t0.Add(-time.Hour)) // due earlier than the lease expiry
	seedPending(t, s, "del_b", "b", t0.Add(-time.Hour))

	capacity := func(string) int { return 1 }
	c, err := s.ClaimDueFair(ctx, "w", t0.Add(time.Minute), t0.Add(2*time.Minute), 8, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(claimedIDs(c)), "[del_lease del_b]"; got != want {
		t.Fatalf("claimed %s, want %s", got, want)
	}
	if c[0].AttemptNo != 2 {
		t.Fatalf("reclaimed lease attempt = %d, want 2", c[0].AttemptNo)
	}
}

func TestPurgeSpansBatches(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	old := t0.Add(-100 * 24 * time.Hour)
	const n = 2*purgeBatch + 37
	err := s.WithTx(ctx, func(tx *Tx) error {
		for i := range n {
			id := fmt.Sprintf("evt_%04d", i)
			status := routing.Unsupported
			if i%3 == 0 {
				status = routing.Unresolved // held: keeps its payload
			}
			if err := tx.InsertEvent(ctx, &Event{ID: id, Provider: "paystack", EventType: "x", DedupKey: id,
				RawPayload: []byte("{}"), PayloadSHA256: "x", ReceivedAt: old.Add(time.Duration(i%7) * time.Millisecond),
				RoutingStatus: status}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Purge(ctx, t0.Add(-90*24*time.Hour), t0.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	held := (n + 2) / 3
	if res.PayloadsCleared != int64(n-held) {
		t.Fatalf("cleared %d payloads, want %d", res.PayloadsCleared, n-held)
	}
	var kept int
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM events WHERE raw_payload IS NOT NULL`).Scan(&kept); err != nil || kept != held {
		t.Fatalf("payloads kept = %d (%v), want %d held events", kept, err, held)
	}
	if res, err := s.Purge(ctx, t0.Add(-90*24*time.Hour), t0.Add(-90*24*time.Hour)); err != nil || res.PayloadsCleared != 0 {
		t.Fatalf("second purge = %+v (%v), want nothing left to clear", res, err)
	}
}
