package relay_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay"
	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/receiver"
)

const adminToken = "relay_admin_token_0123456789abcdef_xyz"

// TestEmbeddedOperatorMethods resolves a held event of a custom provider
// from Go, which the CLI cannot reprocess: it only knows built-in providers.
func TestEmbeddedOperatorMethods(t *testing.T) {
	b := newInbox()
	appB := httptest.NewServer(receiver.Handler(secretB, b.handle))
	defer appB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig(t, "http://127.0.0.1:1", appB.URL)
	cfg.Server.AdminToken = adminToken
	r, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: acmeToken}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	srv := httptest.NewServer(r.WebhookHandler())
	defer srv.Close()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	if c := post(t, srv.URL+"/webhooks/acme", acmeToken, `{"type":"payment.succeeded","id":"7","ref":"LEGACY_7"}`); c != http.StatusOK {
		t.Fatalf("webhook: %d", c)
	}
	st, err := r.Stats(ctx)
	if err != nil || st.Events.Unresolved != 1 || st.Events.OldestHeldAt.IsZero() {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	page, err := r.Events(ctx, admin.EventFilter{Status: admin.StatusUnresolved})
	if err != nil || len(page.Events) != 1 || page.Events[0].Provider != "acme" || page.Events[0].Reference != "LEGACY_7" {
		t.Fatalf("Events = %+v, %v", page, err)
	}
	id := page.Events[0].ID

	res, err := r.Reprocess(ctx, admin.ReprocessRequest{EventIDs: []string{id}})
	if err != nil || res.Unresolved != 1 || res.Failed != 0 {
		t.Fatalf("Reprocess of a custom provider's event = %+v, %v", res, err)
	}

	if _, err := r.Reroute(ctx, id, "app_b", ""); !errors.Is(err, relay.ErrInvalid) {
		t.Errorf("reroute without reason: %v", err)
	}
	if _, err := r.Reroute(ctx, id, "nope", "x"); !errors.Is(err, relay.ErrInvalid) {
		t.Errorf("reroute to unknown application: %v", err)
	}
	if _, err := r.Reroute(ctx, "evt_missing", "app_b", "x"); !errors.Is(err, relay.ErrNotFound) {
		t.Errorf("reroute missing event: %v", err)
	}
	rr, err := r.Reroute(ctx, id, "app_b", "legacy order, confirmed by support")
	if err != nil || rr.Event.ApplicationID != "app_b" || rr.AwaitingConfirmation {
		t.Fatalf("Reroute = %+v, %v", rr, err)
	}
	if d := b.wait(t); d.EventID != id {
		t.Fatalf("delivered %+v", d)
	}
	if _, err := r.Reroute(ctx, id, "app_a", "again"); !errors.Is(err, relay.ErrWrongState) {
		t.Errorf("second reroute: %v", err)
	}

	detail, err := r.Event(ctx, id, false)
	if err != nil || detail.Payload != nil || len(detail.RoutingDecisions) != 2 ||
		!strings.Contains(detail.RoutingDecisions[1].Reason, "via go api: legacy order") {
		t.Fatalf("Event = %+v, %v", detail, err)
	}
	if detail, _ := r.Event(ctx, id, true); !strings.Contains(string(detail.Payload), "LEGACY_7") {
		t.Errorf("payload not included: %s", detail.Payload)
	}
	dels, err := r.Deliveries(ctx, admin.DeliveryFilter{EventID: id})
	if err != nil || len(dels) != 1 {
		t.Fatalf("Deliveries = %+v, %v", dels, err)
	}
	if err := r.RetryDelivery(ctx, dels[0].ID); !errors.Is(err, relay.ErrWrongState) {
		t.Errorf("retry of a non-exhausted delivery: %v", err)
	}
	if err := r.RetryConfirmation(ctx, id); !errors.Is(err, relay.ErrWrongState) {
		t.Errorf("confirm an event that needs none: %v", err)
	}

	// The main handler never serves the admin API; AdminHandler does.
	main := httptest.NewServer(r.Handler())
	defer main.Close()
	adminSrv := httptest.NewServer(r.AdminHandler())
	defer adminSrv.Close()
	get := func(base, tok string) int {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/admin/stats", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(main.URL, adminToken); c == http.StatusOK {
		t.Error("admin API served on the main handler")
	}
	if c := get(adminSrv.URL, adminToken); c != http.StatusOK {
		t.Errorf("AdminHandler: %d", c)
	}
	if c := get(adminSrv.URL, "wrong"); c != http.StatusUnauthorized {
		t.Errorf("AdminHandler with a wrong token: %d", c)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestAdminTokenValidation(t *testing.T) {
	ctx := context.Background()
	for token, want := range map[string]string{
		"short": "at least 32 bytes",
		secretA: "signing_secret_env",
	} {
		cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
		cfg.Server.AdminToken = token
		if _, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: acmeToken})); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("admin token %q: %v", token, err)
		}
	}
}

// TestEmbeddedAlertHandler: a held event reaches a Go alert handler, and a
// dismissed one stops counting as held.
func TestEmbeddedAlertHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	got := make(chan admin.AlertDigest, 4)
	r, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: acmeToken}),
		relay.WithAlertHandler(func(_ context.Context, d admin.AlertDigest) error { got <- d; return nil }))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	srv := httptest.NewServer(r.WebhookHandler())
	defer srv.Close()
	if c := post(t, srv.URL+"/webhooks/acme", acmeToken, `{"type":"payment.succeeded","id":"8","ref":"STRAY_8"}`); c != http.StatusOK {
		t.Fatalf("webhook: %d", c)
	}

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }() // the first digest goes out at once
	var d admin.AlertDigest
	select {
	case d = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no alert digest")
	}
	if d.Counts[admin.AlertUnresolved] != 1 || d.Alerts[0].Reference != "STRAY_8" || d.Alerts[0].Provider != "acme" ||
		d.Stats.Events.Unresolved != 1 {
		t.Fatalf("digest: %+v", d)
	}

	e, err := r.Dismiss(ctx, d.Alerts[0].EventID, "not one of our applications")
	if err != nil || e.DismissedAt.IsZero() {
		t.Fatalf("Dismiss = %+v, %v", e, err)
	}
	if _, err := r.Dismiss(ctx, e.ID, "again"); !errors.Is(err, relay.ErrWrongState) {
		t.Errorf("second dismiss: %v", err)
	}
	if st, _ := r.Stats(ctx); st.Events.Unresolved != 0 {
		t.Errorf("a dismissed event still counts as held: %+v", st)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
