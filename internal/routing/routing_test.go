package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/provider/paystack"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type fakeLookup map[string][]Registration // key: type + "|" + value

func (f fakeLookup) Registrations(_ context.Context, typ, value string) ([]Registration, error) {
	return f[typ+"|"+value], nil
}

type errLookup struct{}

func (errLookup) Registrations(context.Context, string, string) ([]Registration, error) {
	return nil, errors.New("boom")
}

var rules = []Rule{
	{Name: "routes[0]", Application: "app_a", ReferencePrefix: "APPA_"},
	{Name: "routes[1]", Application: "app_b", Metadata: map[string]string{"application": "app_b"}},
	{Name: "routes[2]", Application: "shop", ReferencePrefix: "SHP_", Metadata: map[string]string{"channel": "web"}},
}

func active(id, app string, created time.Time) Registration {
	return Registration{ID: id, Application: app, Active: true, CreatedAt: created}
}

func TestResolve(t *testing.T) {
	dva := &paystack.DedicatedAccount{Bank: "test bank", AccountNumber: "9930000902"}
	lookup := fakeLookup{
		"transaction_reference|REG_1":            {active("reg_a", "app_a", t0.Add(-time.Hour))},
		"transaction_reference|APPA_CONFLICT":    {active("reg_b", "app_b", t0.Add(-time.Hour))},
		"dedicated_account|test bank:9930000902": {active("reg_c", "app_b", t0.Add(-time.Hour))},
	}
	cases := []struct {
		name    string
		event   string
		ids     paystack.Identifiers
		outcome Outcome
		app     string
		reason  string
	}{
		{"prefix", "charge.success", paystack.Identifiers{Reference: "APPA_123"}, Matched, "app_a", ""},
		{"metadata", "charge.success", paystack.Identifiers{Reference: "T1", Metadata: map[string]string{"application": "app_b"}}, Matched, "app_b", ""},
		{"prefix and metadata agree", "charge.success", paystack.Identifiers{Reference: "APPA_1", Metadata: map[string]string{"application": "app_a"}}, Matched, "app_a", ""},
		{"compound rule needs both", "charge.success", paystack.Identifiers{Reference: "SHP_1"}, Unresolved, "", "no rule or registration matched"},
		{"compound rule matches", "charge.success", paystack.Identifiers{Reference: "SHP_1", Metadata: map[string]string{"channel": "web"}}, Matched, "shop", ""},
		{"prefix vs metadata conflict", "charge.success", paystack.Identifiers{Reference: "APPA_1", Metadata: map[string]string{"application": "app_b"}}, Ambiguous, "", "app_a, app_b"},
		{"registered reference", "charge.success", paystack.Identifiers{Reference: "REG_1"}, Matched, "app_a", ""},
		{"registration conflicts with rule", "charge.success", paystack.Identifiers{Reference: "APPA_CONFLICT"}, Ambiguous, "", "conflicting owners"},
		{"registered dva", "charge.success", paystack.Identifiers{Reference: "1730abc", Channel: "dedicated_nuban", DedicatedAccount: dva}, Matched, "app_b", ""},
		{"dva fields missing", "charge.success", paystack.Identifiers{Reference: "1730abc", Channel: "dedicated_nuban"}, Unresolved, "", "without receiver account fields"},
		{"no identifiers", "charge.success", paystack.Identifiers{}, Unresolved, "", "no ownership identifiers"},
		{"prefix is case sensitive", "charge.success", paystack.Identifiers{Reference: "appa_1"}, Unresolved, "", ""},
		{"unsupported type", "transfer.success", paystack.Identifiers{Reference: "APPA_1"}, Unsupported, "", "not routed"},
	}
	e := NewEngine(rules)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := e.Resolve(context.Background(), "paystack", tc.event, tc.ids, t0, lookup)
			if err != nil {
				t.Fatal(err)
			}
			if d.Outcome != tc.outcome || d.Application != tc.app || !strings.Contains(d.Reason, tc.reason) {
				t.Fatalf("got %+v", d)
			}
			if d.Outcome == Matched && len(d.Candidates) == 0 {
				t.Fatal("matched decision must record its evidence")
			}
		})
	}
}

func TestResolveCustomerNeverRoutes(t *testing.T) {
	// Customer fields aren't even part of Identifiers; metadata keys that
	// look like customer data only route if an operator wrote a rule on them.
	d, _ := NewEngine(rules).Resolve(context.Background(), "paystack", "charge.success",
		paystack.Identifiers{Reference: "T1", Metadata: map[string]string{"customer_code": "CUS_1"}}, t0, nil)
	if d.Outcome != Unresolved {
		t.Fatalf("got %v", d.Outcome)
	}
}

func TestResolveProviders(t *testing.T) {
	scoped := []Rule{
		{Name: "routes[0]", Provider: "paystack", Application: "app_a", ReferencePrefix: "A_"},
		{Name: "routes[1]", Provider: "flutterwave", Application: "app_b", ReferencePrefix: "A_"},
		{Name: "routes[2]", Application: "shop", ReferencePrefix: "SHP_"},
	}
	cases := []struct {
		prov, event, ref string
		outcome          Outcome
		app              string
	}{
		{"paystack", "charge.success", "A_1", Matched, "app_a"},
		{"flutterwave", "charge.completed", "A_1", Matched, "app_b"},
		{"flutterwave", "charge.completed", "SHP_1", Matched, "shop"},
		{"paystack", "charge.success", "SHP_1", Matched, "shop"},
		{"flutterwave", "charge.success", "A_1", Unsupported, ""},
		{"paystack", "charge.completed", "A_1", Unsupported, ""},
		{"flutterwave", "transfer.completed", "A_1", Unsupported, ""},
		{"stripe", "charge.success", "A_1", Unsupported, ""},
	}
	for _, tc := range cases {
		d, err := NewEngine(scoped).Resolve(context.Background(), tc.prov, tc.event, paystack.Identifiers{Reference: tc.ref}, t0, nil)
		if err != nil || d.Outcome != tc.outcome || d.Application != tc.app {
			t.Errorf("%s %s %s: got %v %q %v, want %v %q", tc.prov, tc.event, tc.ref, d.Outcome, d.Application, err, tc.outcome, tc.app)
		}
	}
	// The dedicated_nuban hint is Paystack-specific.
	d, _ := NewEngine(nil).Resolve(context.Background(), "flutterwave", "charge.completed", paystack.Identifiers{Reference: "x", Channel: "dedicated_nuban"}, t0, nil)
	if strings.Contains(d.Reason, "dedicated_nuban") {
		t.Errorf("flutterwave got paystack hint: %q", d.Reason)
	}
}

func TestResolveLookupError(t *testing.T) {
	_, err := NewEngine(nil).Resolve(context.Background(), "paystack", "charge.success", paystack.Identifiers{Reference: "X"}, t0, errLookup{})
	if err == nil {
		t.Fatal("lookup errors must propagate so the webhook is not acknowledged")
	}
}

func TestOwnersAt(t *testing.T) {
	before, after := t0.Add(-time.Hour), t0.Add(time.Hour)
	inactive := func(id, app string, created, deactivated time.Time) Registration {
		return Registration{ID: id, Application: app, CreatedAt: created, DeactivatedAt: deactivated}
	}
	cases := []struct {
		name string
		regs []Registration
		want string // comma-joined IDs
	}{
		{"active before event", []Registration{active("a", "x", before)}, "a"},
		{"created exactly at event", []Registration{active("a", "x", t0)}, "a"},
		{"late first registration", []Registration{active("a", "x", after)}, "a"},
		{"deactivated before event", []Registration{inactive("a", "x", before.Add(-time.Hour), before)}, ""},
		{"deactivated after event still owned it", []Registration{inactive("a", "x", before, after)}, "a"},
		{"reassigned after event goes to old owner", []Registration{
			inactive("a", "x", before, t0.Add(30*time.Minute)), active("b", "y", after)}, "a"},
		{"reassigned before event goes to new owner", []Registration{
			inactive("a", "x", before.Add(-time.Hour), before), active("b", "y", before)}, "b"},
		{"gap after reassignment is nobody's", []Registration{
			inactive("a", "x", before.Add(-time.Hour), before), active("b", "y", after)}, ""},
		{"only registration, inactive", []Registration{inactive("a", "x", after, after.Add(time.Hour))}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range OwnersAt(tc.regs, t0) {
				got = append(got, r.ID)
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("got %v, want %q", got, tc.want)
			}
		})
	}
}
