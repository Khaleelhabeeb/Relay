package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/flutterwave"
	"github.com/khaleelhabeeb/relay/internal/paystack"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
)

var testRules = []routing.Rule{
	{Name: "routes[0]", Application: "app_a", ReferencePrefix: "APPA_"},
	{Name: "routes[1]", Application: "app_b", Metadata: map[string]string{"application": "app_b"}},
}

func newAcceptor(t *testing.T) (*StoreAcceptor, *store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &StoreAcceptor{
		Store:  st,
		Engine: routing.NewEngine(testRules),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st, path
}

func accepted(t *testing.T, raw []byte) *Accepted {
	t.Helper()
	e, err := paystack.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	ids := paystack.ExtractIdentifiers(e)
	return &Accepted{Event: e, Identifiers: ids, DedupKey: paystack.DedupKey(e, ids), ReceivedAt: time.Now().UTC()}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "paystack", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// eventByKey finds the stored event for a dedup key via the routing decision trail.
func eventByKey(t *testing.T, st *store.Store, key string) *store.Event {
	t.Helper()
	var id string
	err := st.WithTx(context.Background(), func(tx *store.Tx) error {
		var err error
		id, _, err = tx.EventIDByDedupKey(context.Background(), "paystack", key)
		return err
	})
	if err != nil || id == "" {
		t.Fatalf("no event for %s (%v)", key, err)
	}
	e, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAcceptRoutesFixtures(t *testing.T) {
	acc, st, _ := newAcceptor(t)
	ctx := context.Background()
	cases := []struct {
		fixture string
		status  routing.Outcome
		app     string
	}{
		{"charge_success_checkout.json", routing.Matched, "app_a"},
		{"charge_success_metadata_object.json", routing.Matched, "app_b"},
		{"charge_success_metadata_string.json", routing.Matched, "app_b"},
		{"charge_success_dedicated_nuban.json", routing.Unresolved, ""}, // no registration yet
		{"transfer_success.json", routing.Unsupported, ""},
		{"subscription_create.json", routing.Unsupported, ""},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			raw := fixture(t, tc.fixture)
			a := accepted(t, raw)
			if err := acc.Accept(ctx, a); err != nil {
				t.Fatal(err)
			}
			e := eventByKey(t, st, a.DedupKey)
			if e.RoutingStatus != tc.status || e.ApplicationID != tc.app || !bytes.Equal(e.RawPayload, raw) {
				t.Fatalf("event = %+v", e)
			}
			dels, err := st.DeliveriesForEvent(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == routing.Matched {
				if len(dels) != 1 || dels[0].ApplicationID != tc.app || dels[0].Status != store.DeliveryPending {
					t.Fatalf("deliveries = %+v", dels)
				}
			} else if len(dels) != 0 {
				t.Fatalf("unmatched event must not have deliveries: %+v", dels)
			}
			decs, err := st.RoutingDecisionsForEvent(ctx, e.ID)
			if err != nil || len(decs) != 1 || decs[0].Outcome != tc.status || decs[0].DecidedBy != store.DecidedByIngest {
				t.Fatalf("decisions = %+v (%v)", decs, err)
			}
			if tc.status == routing.Matched && len(decs[0].Candidates) == 0 {
				t.Fatal("matched decision must record evidence")
			}
		})
	}
}

func TestAcceptStoresDedicatedAccountIdentifier(t *testing.T) {
	acc, st, _ := newAcceptor(t)
	a := accepted(t, fixture(t, "charge_success_dedicated_nuban.json"))
	if err := acc.Accept(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if e := eventByKey(t, st, a.DedupKey); e.DedicatedAccount != "test bank:9930000902" {
		t.Fatalf("dedicated_account = %q", e.DedicatedAccount)
	}
}

func TestAcceptDuplicates(t *testing.T) {
	acc, st, _ := newAcceptor(t)
	ctx := context.Background()
	raw := fixture(t, "charge_success_checkout.json")
	var compact bytes.Buffer
	_ = json.Compact(&compact, raw)

	for _, body := range [][]byte{raw, raw, compact.Bytes()} {
		if err := acc.Accept(ctx, accepted(t, body)); err != nil {
			t.Fatal(err)
		}
	}
	e := eventByKey(t, st, "charge.success:4099260516")
	if e.DuplicateCount != 2 || !bytes.Equal(e.RawPayload, raw) {
		t.Fatalf("duplicate_count = %d; raw kept from first delivery = %v", e.DuplicateCount, bytes.Equal(e.RawPayload, raw))
	}
	if dels, _ := st.DeliveriesForEvent(ctx, e.ID); len(dels) != 1 {
		t.Fatalf("duplicates created %d deliveries, want 1", len(dels))
	}
	if decs, _ := st.RoutingDecisionsForEvent(ctx, e.ID); len(decs) != 1 {
		t.Fatalf("duplicates created %d routing decisions, want 1", len(decs))
	}
}

func TestAcceptConcurrentDuplicates(t *testing.T) {
	acc, st, _ := newAcceptor(t)
	raw := fixture(t, "charge_success_checkout.json")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- acc.Accept(context.Background(), accepted(t, raw))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	e := eventByKey(t, st, "charge.success:4099260516")
	if dels, _ := st.DeliveriesForEvent(context.Background(), e.ID); len(dels) != 1 || e.DuplicateCount != 19 {
		t.Fatalf("deliveries = %d, duplicate_count = %d", len(dels), e.DuplicateCount)
	}
}

// rawExec runs SQL on the database file through a separate connection,
// with the store closed, then reopens the store for the acceptor.
func rawExec(t *testing.T, acc *StoreAcceptor, path string, stmts ...string) {
	t.Helper()
	if err := acc.Store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	acc.Store = st
}

func TestAcceptIsAtomic(t *testing.T) {
	acc, _, path := newAcceptor(t)
	ctx := context.Background()

	// Make the last write of the transaction (the delivery insert) fail. The
	// event and routing decision written before it must roll back too.
	rawExec(t, acc, path, `CREATE TRIGGER fail_delivery BEFORE INSERT ON deliveries
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)

	a := accepted(t, fixture(t, "charge_success_checkout.json"))
	if err := acc.Accept(ctx, a); err == nil {
		t.Fatal("Accept must fail when the transaction cannot commit")
	}
	var found bool
	_ = acc.Store.WithTx(ctx, func(tx *store.Tx) error {
		var err error
		_, found, err = tx.EventIDByDedupKey(ctx, "paystack", a.DedupKey)
		return err
	})
	if found {
		t.Fatal("event persisted despite failed transaction")
	}

	// Unmatched events write no delivery, so they still succeed.
	if err := acc.Accept(ctx, accepted(t, fixture(t, "transfer_success.json"))); err != nil {
		t.Fatalf("unsupported event: %v", err)
	}
}

func TestAcceptUsesRegistrations(t *testing.T) {
	acc, _, path := newAcceptor(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour).UnixMilli()
	rawExec(t, acc, path, fmt.Sprintf(`INSERT INTO registrations
		(id, provider, identifier_type, identifier_value, application_id, active, created_at) VALUES
		('reg_dva', 'paystack', 'dedicated_account', 'test bank:9930000902', 'app_b', 1, %[1]d),
		('reg_ref', 'paystack', 'transaction_reference', 'APPA_01JBQ7Z3K9X2', 'app_b', 1, %[1]d)`, past))

	dva := accepted(t, fixture(t, "charge_success_dedicated_nuban.json"))
	if err := acc.Accept(ctx, dva); err != nil {
		t.Fatal(err)
	}
	if e := eventByKey(t, acc.Store, dva.DedupKey); e.RoutingStatus != routing.Matched || e.ApplicationID != "app_b" {
		t.Fatalf("dva event = %s/%s", e.RoutingStatus, e.ApplicationID)
	}

	// The APPA_ prefix rule says app_a, the registration says app_b:
	// fail closed as ambiguous, with no delivery.
	chk := accepted(t, fixture(t, "charge_success_checkout.json"))
	if err := acc.Accept(ctx, chk); err != nil {
		t.Fatal(err)
	}
	e := eventByKey(t, acc.Store, chk.DedupKey)
	if e.RoutingStatus != routing.Ambiguous {
		t.Fatalf("conflicting rule/registration = %s, want ambiguous", e.RoutingStatus)
	}
	if dels, _ := acc.Store.DeliveriesForEvent(ctx, e.ID); len(dels) != 0 {
		t.Fatal("ambiguous event must not be delivered")
	}
}

func TestAcceptFlutterwave(t *testing.T) {
	acc, st, _ := newAcceptor(t)
	ctx := context.Background()
	accept := func(raw []byte) *store.Event {
		t.Helper()
		ad := flutterwave.Adapter{}
		e, err := ad.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		ids := ad.ExtractIdentifiers(e)
		a := &Accepted{Event: e, Identifiers: ids, DedupKey: ad.DedupKey(e, ids), ReceivedAt: time.Now().UTC()}
		if err := acc.Accept(ctx, a); err != nil {
			t.Fatal(err)
		}
		var id string
		err = st.WithTx(ctx, func(tx *store.Tx) error {
			var err error
			id, _, err = tx.EventIDByDedupKey(ctx, "flutterwave", a.DedupKey)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		ev, err := st.GetEvent(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}

	for _, tc := range []struct {
		fixture string
		status  routing.Outcome
		app     string
	}{
		{"charge_completed_card.json", routing.Matched, "app_a"},
		{"charge_completed_meta.json", routing.Matched, "app_b"},
		{"charge_completed_meta_data_toplevel.json", routing.Matched, "app_b"},
		{"transfer_completed.json", routing.Unsupported, ""},
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "flutterwave", tc.fixture))
		if err != nil {
			t.Fatal(err)
		}
		if ev := accept(raw); ev.Provider != "flutterwave" || ev.RoutingStatus != tc.status || ev.ApplicationID != tc.app {
			t.Errorf("%s: event = %+v", tc.fixture, ev)
		}
	}

	// Registrations are per provider: a Paystack registration of a tx_ref
	// does not claim a Flutterwave event with that reference.
	past := time.Now().Add(-time.Hour)
	if _, _, err := st.CreateRegistration(ctx, "paystack", routing.IdentifierReference, "FLW_REF_1", "app_a", past); err != nil {
		t.Fatal(err)
	}
	if ev := accept([]byte(`{"event":"charge.completed","data":{"id":1,"tx_ref":"FLW_REF_1","status":"successful"}}`)); ev.RoutingStatus != routing.Unresolved {
		t.Fatalf("paystack registration claimed a flutterwave event: %+v", ev)
	}
	if _, _, err := st.CreateRegistration(ctx, "flutterwave", routing.IdentifierReference, "FLW_REF_2", "app_b", past); err != nil {
		t.Fatal(err)
	}
	if ev := accept([]byte(`{"event":"charge.completed","data":{"id":2,"tx_ref":"FLW_REF_2","status":"successful"}}`)); ev.RoutingStatus != routing.Matched || ev.ApplicationID != "app_b" {
		t.Fatalf("flutterwave registration did not route: %+v", ev)
	}
}
