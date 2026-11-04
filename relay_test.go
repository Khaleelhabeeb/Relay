package relay_test

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay"
	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/provider"
	"github.com/khaleelhabeeb/relay/provider/flutterwave"
	"github.com/khaleelhabeeb/relay/provider/monnify"
	"github.com/khaleelhabeeb/relay/provider/paystack"
	"github.com/khaleelhabeeb/relay/receiver"
)

// acme is a minimal custom provider: a shared token header and a flat body
// {"type","id","ref","meta"}.
type acme struct{ token string }

var errBadToken = errors.New("acme: bad token")

func (acme) Name() string { return "acme" }

func (a acme) Verify(h http.Header, _ []byte) error {
	if subtle.ConstantTimeCompare([]byte(h.Get("X-Acme-Token")), []byte(a.token)) != 1 || a.token == "" {
		return errBadToken
	}
	return nil
}

func (acme) Parse(raw []byte) (*provider.Event, error) {
	var b struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &b); err != nil || b.Type == "" {
		return nil, fmt.Errorf("acme: malformed event")
	}
	return &provider.Event{Provider: "acme", Type: b.Type, Data: raw, Raw: raw}, nil
}

func (acme) ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	var b struct {
		ID   string          `json:"id"`
		Ref  string          `json:"ref"`
		Meta json.RawMessage `json:"meta"`
	}
	_ = json.Unmarshal(e.Data, &b)
	return provider.Identifiers{TransactionID: b.ID, Reference: b.Ref, Metadata: provider.ParseMetadata(b.Meta)}
}

func (acme) DedupKey(e *provider.Event, ids provider.Identifiers) string {
	return e.Type + ":" + ids.TransactionID
}

func (acme) Routes(eventType string) bool { return eventType == "payment.succeeded" }

const (
	acmeToken = "acme_test_token"
	secretA   = "app_a_signing_secret_0123456789abcdef"
	secretB   = "app_b_signing_secret_0123456789abcdef"
)

type inbox struct {
	mu  sync.Mutex
	got []*receiver.Delivery
	ch  chan struct{}
}

func newInbox() *inbox { return &inbox{ch: make(chan struct{}, 16)} }

func (in *inbox) handle(_ context.Context, d *receiver.Delivery) error {
	in.mu.Lock()
	in.got = append(in.got, d)
	in.mu.Unlock()
	in.ch <- struct{}{}
	return nil
}

func (in *inbox) wait(t *testing.T) *receiver.Delivery {
	t.Helper()
	select {
	case <-in.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery")
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.got[len(in.got)-1]
}

func testConfig(t *testing.T, endpointA, endpointB string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Storage.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.Delivery.AllowInsecureLocal = true // test receivers are on 127.0.0.1
	cfg.Applications = map[string]*config.ApplicationConfig{
		"app_a": {Endpoint: endpointA, SigningSecret: secretA},
		"app_b": {Endpoint: endpointB, SigningSecret: secretB, PayloadFormat: config.PayloadEnvelope},
	}
	cfg.Routes = []config.RouteConfig{{Provider: "acme", Application: "app_a", Match: config.MatchConfig{ReferencePrefix: "APPA_"}}}
	return cfg
}

func post(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-Acme-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestEmbeddedCustomProvider(t *testing.T) {
	a, b := newInbox(), newInbox()
	appA := httptest.NewServer(receiver.Handler(secretA, a.handle))
	defer appA.Close()
	appB := httptest.NewServer(receiver.Handler(secretB, b.handle))
	defer appB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := testConfig(t, appA.URL, appB.URL)
	r, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: acmeToken}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := r.Providers(); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("Providers() = %v", got)
	}

	// Mounted in "our own" server.
	mux := http.NewServeMux()
	mux.Handle("/webhooks/", r.WebhookHandler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	body := `{"type":"payment.succeeded","id":"1","ref":"APPA_1"}`
	if c := post(t, srv.URL+"/webhooks/acme", acmeToken, body); c != http.StatusOK {
		t.Fatalf("webhook: %d", c)
	}
	d := a.wait(t)
	if d.Provider != "acme" || !bytes.Equal(d.Body, []byte(body)) || d.EventID == "" {
		t.Fatalf("delivery %+v", d)
	}
	if c := post(t, srv.URL+"/webhooks/acme", "wrong", body); c != http.StatusUnauthorized {
		t.Fatalf("forged webhook: %d", c)
	}

	// No owner yet: held, then routed by an in-process registration.
	if c := post(t, srv.URL+"/webhooks/acme", acmeToken, `{"type":"payment.succeeded","id":"2","ref":"ORDER_9"}`); c != http.StatusOK {
		t.Fatalf("webhook: %d", c)
	}
	reg, routed, err := r.RegisterReference(ctx, "app_b", "acme", "ORDER_9")
	if err != nil || routed != 1 || reg.Application != "app_b" || reg.Provider != "acme" || !reg.Active {
		t.Fatalf("RegisterReference = %+v, %d, %v", reg, routed, err)
	}
	env, err := b.wait(t).Envelope()
	if err != nil || env.Provider != "acme" || env.Application != "app_b" || env.Type != "payment.succeeded" {
		t.Fatalf("envelope %+v %v", env, err)
	}

	if _, _, err := r.RegisterReference(ctx, "app_a", "acme", "ORDER_9"); !errors.Is(err, relay.ErrConflict) {
		t.Errorf("conflict: %v", err)
	}
	if _, _, err := r.RegisterReference(ctx, "nope", "acme", "X"); !errors.Is(err, relay.ErrInvalid) {
		t.Errorf("unknown app: %v", err)
	}
	if _, _, err := r.RegisterDedicatedAccount(ctx, "app_b", "Test Bank", "123"); !errors.Is(err, relay.ErrInvalid) {
		t.Errorf("dva without paystack: %v", err)
	}
	if _, err := r.Deactivate(ctx, "app_a", reg.ID); !errors.Is(err, relay.ErrNotFound) {
		t.Errorf("deactivate other app's registration: %v", err)
	}
	if got, err := r.Deactivate(ctx, "app_b", reg.ID); err != nil || got.Active {
		t.Errorf("deactivate: %+v %v", got, err)
	}

	if err := r.Run(ctx); !errors.Is(err, relay.ErrRunning) {
		t.Errorf("second Run: %v", err)
	}
	if _, err := relay.New(ctx, cfg, relay.WithProvider(acme{token: acmeToken})); !errors.Is(err, relay.ErrLocked) {
		t.Errorf("second New on one database: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	if _, err := relay.New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "routes[0].provider") {
		t.Errorf("route to unknown provider: %v", err)
	}
	cfg = testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	cfg.Applications["app_a"].SigningSecret = ""
	if _, err := relay.New(ctx, cfg, relay.WithProvider(acme{})); err == nil || !strings.Contains(err.Error(), "SigningSecret") {
		t.Errorf("missing secret: %v", err)
	}
	cfg = testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	if _, err := relay.New(ctx, cfg, relay.WithProvider(badName{})); err == nil || !strings.Contains(err.Error(), "invalid provider name") {
		t.Errorf("bad provider name: %v", err)
	}
	for _, a := range []provider.Adapter{paystack.Adapter{}, &paystack.Adapter{}, flutterwave.Adapter{}, &flutterwave.Adapter{},
		flutterwave.Adapter{SecretHash: "h", ConfirmWithAPI: true}, monnify.Adapter{}, &monnify.Adapter{},
		monnify.Adapter{SecretKey: "s", ConfirmWithAPI: true}} {
		cfg = testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
		if _, err := relay.New(ctx, cfg, relay.WithProvider(a)); err == nil || !strings.Contains(err.Error(), "empty secret") {
			t.Errorf("%T without secret: %v", a, err)
		}
	}
}

type badName struct{ acme }

func (badName) Name() string { return "Bad Name" }

func TestHandlerServesBuiltInsAndHealth(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t, "http://127.0.0.1:1", "http://127.0.0.1:2")
	cfg.Routes = nil
	cfg.Providers.Paystack = &config.PaystackConfig{Secret: "sk_test_embedded"}
	r, err := relay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	srv := httptest.NewServer(r)
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200} {
		resp, err := http.Get(srv.URL + path)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s: %v %v", path, resp, err)
		}
		resp.Body.Close()
	}
	// Unsigned Paystack webhook is refused, so the endpoint exists and verifies.
	resp, err := http.Post(srv.URL+"/webhooks/paystack", "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("paystack: %v %v", resp, err)
	}
	resp.Body.Close()
}
