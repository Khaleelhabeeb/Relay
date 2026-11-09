package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/routing"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// seed inserts an event with optional delivery directly.
func seed(t *testing.T, s *Store, id string, status routing.Outcome, received time.Time, delivery *Delivery) {
	t.Helper()
	err := s.WithTx(context.Background(), func(tx *Tx) error {
		if err := tx.InsertEvent(context.Background(), &Event{
			ID: id, Provider: "paystack", EventType: "charge.success", DedupKey: "k:" + id,
			RawPayload: []byte(`{"event":"charge.success","data":{}}`), PayloadSHA256: "x",
			ReceivedAt: received, RoutingStatus: status, ApplicationID: delivery.appOrEmpty(),
		}); err != nil {
			return err
		}
		if delivery != nil {
			delivery.EventID = id
			return tx.InsertDelivery(context.Background(), delivery)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (d *Delivery) appOrEmpty() string {
	if d == nil {
		return ""
	}
	return d.ApplicationID
}

func TestRetryDeliveryStartsNewCycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seed(t, s, "evt_1", routing.Matched, t0, &Delivery{ID: "del_1", ApplicationID: "a", Status: DeliveryPending, NextAttemptAt: t0, CreatedAt: t0})

	// Three failed attempts, then exhausted.
	for i := 1; i <= 3; i++ {
		c, err := s.ClaimDue(ctx, "w", t0, t0.Add(time.Minute), 1)
		if err != nil || len(c) != 1 || c[0].AttemptNo != i || c[0].CycleAttempt != i {
			t.Fatalf("claim %d: %+v %v", i, c, err)
		}
		status := DeliveryPending
		if i == 3 {
			status = DeliveryExhausted
		}
		if err := s.CompleteAttempt(ctx, AttemptResult{DeliveryID: "del_1", AttemptID: c[0].AttemptID, Owner: "w",
			FinishedAt: t0, ErrorCode: "http_status", Status: status, NextAttemptAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RetryDelivery(ctx, "nope", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.RetryDelivery(ctx, "del_1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryDelivery(ctx, "del_1", t0); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("retrying a pending delivery: %v", err)
	}
	c, err := s.ClaimDue(ctx, "w", t0.Add(time.Hour), t0.Add(2*time.Hour), 1)
	if err != nil || len(c) != 1 || c[0].AttemptNo != 4 || c[0].CycleAttempt != 1 {
		t.Fatalf("after retry: %+v %v (want attempt 4, cycle attempt 1)", c, err)
	}
	if a, _ := s.AttemptsForDelivery(ctx, "del_1"); len(a) != 4 {
		t.Fatalf("history must be kept: %d attempts", len(a))
	}
}

func TestPurge(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	old, recent := t0.Add(-100*24*time.Hour), t0.Add(-time.Hour)
	pending := func(id string) *Delivery {
		return &Delivery{ID: id, ApplicationID: "a", Status: DeliveryPending, NextAttemptAt: old, CreatedAt: old}
	}
	seed(t, s, "evt_old_delivered", routing.Matched, old, pending("del_a"))
	seed(t, s, "evt_recent_delivered", routing.Matched, old, pending("del_b"))
	seed(t, s, "evt_exhausted", routing.Matched, old, pending("del_c"))
	seed(t, s, "evt_unsupported_old", routing.Unsupported, old, nil)
	seed(t, s, "evt_unsupported_new", routing.Unsupported, recent, nil)
	seed(t, s, "evt_unresolved_old", routing.Unresolved, old, nil)
	seed(t, s, "evt_rejected_old", routing.Matched, old, nil)
	seed(t, s, "evt_pending_old", routing.Matched, old, nil)
	if _, err := s.w.Exec(`UPDATE events SET confirmation='rejected' WHERE id='evt_rejected_old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE events SET confirmation='pending', confirm_next_at=0 WHERE id='evt_pending_old'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.w.Exec(`UPDATE deliveries SET status='delivered', delivered_at=? WHERE id='del_a'`, ms(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE deliveries SET status='delivered', delivered_at=? WHERE id='del_b'`, ms(recent)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE deliveries SET status='exhausted' WHERE id='del_c'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`INSERT INTO delivery_attempts (id, delivery_id, attempt_no, started_at, finished_at)
		VALUES ('att_old','del_a',1,?,?), ('att_new','del_b',1,?,?), ('att_open','del_c',1,?,NULL)`,
		ms(old), ms(old), ms(recent), ms(recent), ms(old)); err != nil {
		t.Fatal(err)
	}

	res, err := s.Purge(ctx, t0.Add(-90*24*time.Hour), t0.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.PayloadsCleared != 3 || res.AttemptsDeleted != 1 {
		t.Fatalf("purge = %+v, want 3 payloads, 1 attempt", res)
	}
	for id, wantPayload := range map[string]bool{
		"evt_old_delivered": false, "evt_unsupported_old": false, "evt_rejected_old": false, "evt_pending_old": true,
		"evt_recent_delivered": true, "evt_exhausted": true, "evt_unsupported_new": true, "evt_unresolved_old": true,
	} {
		e, err := s.GetEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if (e.RawPayload != nil) != wantPayload {
			t.Errorf("%s: payload kept = %v, want %v", id, e.RawPayload != nil, wantPayload)
		}
	}
	var open int
	_ = s.r.QueryRow(`SELECT COUNT(*) FROM delivery_attempts WHERE id='att_open'`).Scan(&open)
	if open != 1 {
		t.Error("unfinished attempts must never be purged")
	}
}

func TestStatsAndListings(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	seed(t, s, "evt_1", routing.Matched, t0, &Delivery{ID: "del_1", ApplicationID: "a", Status: DeliveryPending, NextAttemptAt: t0, CreatedAt: t0.Add(-time.Hour)})
	seed(t, s, "evt_2", routing.Unresolved, t0, nil)
	seed(t, s, "evt_3", routing.Ambiguous, t0.Add(time.Minute), nil)

	st, err := s.Stats(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if st.DeliveriesByStatus["pending"] != 1 || st.EventsByStatus["unresolved"] != 1 || st.EventsByStatus["ambiguous"] != 1 ||
		st.OldestPendingAge != time.Hour {
		t.Fatalf("stats = %+v", st)
	}
	evs, _ := s.ListEvents(ctx, EventFilter{Status: "unresolved"})
	if len(evs) != 1 || evs[0].ID != "evt_2" {
		t.Fatalf("list unresolved = %+v", evs)
	}
	if evs, _ := s.ListEvents(ctx, EventFilter{}); len(evs) != 3 || evs[0].ID != "evt_3" {
		t.Fatalf("newest first: %+v", evs)
	}
	if dels, _ := s.ListDeliveries(ctx, DeliveryFilter{Application: "a"}); len(dels) != 1 {
		t.Fatalf("deliveries = %+v", dels)
	}
	if err := s.Heartbeat(ctx, t0); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTool(t *testing.T) {
	s, path := openTemp(t)
	// Works alongside the locked server connection, and can write.
	tool, err := OpenTool(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenTool while server is open: %v", err)
	}
	if err := tool.Heartbeat(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	tool.Close()

	if _, err := OpenTool(context.Background(), path+".missing"); err == nil {
		t.Fatal("missing database must fail")
	}

	// Schema mismatch: pretend a newer server migrated.
	if _, err := s.w.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (999, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTool(context.Background(), path); err == nil {
		t.Fatal("schema mismatch must fail")
	}
}
