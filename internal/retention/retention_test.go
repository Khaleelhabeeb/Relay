package retention

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

// TestPrunesAlertsWithoutAlerter: the outbox is filled whether or not alerts
// are configured, so retention must keep it bounded on its own.
func TestPrunesAlertsWithoutAlerter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	old := now.Add(-(AlertDays + 1) * 24 * time.Hour)
	if err := st.WithTx(ctx, func(tx *store.Tx) error {
		if err := tx.InsertEvent(ctx, &store.Event{ID: "evt_old", Provider: "paystack", EventType: "charge.success",
			DedupKey: "k1", PayloadSHA256: "x", ReceivedAt: old, RoutingStatus: "unresolved"}); err != nil {
			return err
		}
		return tx.RecordDedupConflict(ctx, &store.DedupConflict{ID: "ddc_old", EventID: "evt_old", PayloadSHA256: "y", ReceivedAt: old})
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PendingAlerts(ctx, 10); len(got) != 1 {
		t.Fatalf("queued alerts: %d", len(got))
	}
	p := &Purger{Store: st, PayloadDays: 90, AttemptDays: 90, Now: func() time.Time { return now }}
	if _, err := p.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.PendingAlerts(ctx, 10); len(got) != 0 {
		t.Fatalf("old alerts not pruned: %d", len(got))
	}
}

func TestHeldDaysExpiresOldHeldEvents(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for id, age := range map[string]time.Duration{"evt_old": 40 * 24 * time.Hour, "evt_new": time.Hour} {
		if err := st.WithTx(ctx, func(tx *store.Tx) error {
			return tx.InsertEvent(ctx, &store.Event{ID: id, Provider: "paystack", EventType: "charge.success",
				DedupKey: id, PayloadSHA256: "x", RawPayload: []byte(`{}`), ReceivedAt: now.Add(-age), RoutingStatus: "unresolved"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := &Purger{Store: st, PayloadDays: 90, AttemptDays: 90, HeldDays: 30, Now: func() time.Time { return now }}
	if _, err := p.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	old, _ := st.GetEvent(ctx, "evt_old")
	recent, _ := st.GetEvent(ctx, "evt_new")
	if old.DismissedAt.IsZero() || old.DismissedBy != "retention" || !recent.DismissedAt.IsZero() {
		t.Fatalf("expiry: old %+v, new %+v", old, recent)
	}
	alerts, _ := st.PendingAlerts(ctx, 10)
	if len(alerts) != 1 || alerts[0].Kind != store.AlertExpired || alerts[0].EventID != "evt_old" {
		t.Fatalf("expiry alert: %+v", alerts)
	}
}
