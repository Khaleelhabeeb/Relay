package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/config"
	"github.com/khaleelhabeeb/relay/internal/ingest"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/paystack"
)

const secret = "sk_test_relay_fixture_secret"

type recorder struct {
	got []*ingest.Accepted
	err error
}

func (r *recorder) Accept(_ context.Context, a *ingest.Accepted) error {
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, a)
	return nil
}

func testConfig() *config.Config {
	c := &config.Config{}
	c.Server.Listen = "127.0.0.1:0"
	c.Server.MaxBodyBytes = 64 << 10
	c.Providers.Paystack.Secret = secret
	return c
}

func newTestServer(t *testing.T, acc ingest.Acceptor) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(New(testConfig(), logger, Deps{Acceptor: acc}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "paystack", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(t *testing.T, url string, body []byte, sig string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/webhooks/paystack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("x-paystack-signature", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Error.Code
}

func TestWebhookAcceptsSignedFixtures(t *testing.T) {
	rec := &recorder{}
	ts := newTestServer(t, rec)
	names := []string{
		"charge_success_checkout.json", "charge_success_metadata_object.json", "charge_success_metadata_string.json",
		"charge_success_dedicated_nuban.json", "transfer_success.json", "subscription_create.json",
	}
	for _, n := range names {
		body := fixture(t, n)
		if status, code := post(t, ts.URL, body, paystack.Sign(secret, body)); status != http.StatusOK {
			t.Errorf("%s: status %d (%s)", n, status, code)
		}
	}
	if len(rec.got) != len(names) {
		t.Fatalf("acceptor got %d events, want %d", len(rec.got), len(names))
	}
	first := rec.got[0]
	if first.DedupKey != "charge.success:4099260516" || first.Identifiers.Reference != "APPA_01JBQ7Z3K9X2" ||
		!bytes.Equal(first.Event.Raw, fixture(t, names[0])) || first.ReceivedAt.IsZero() {
		t.Errorf("unexpected accepted event: %+v", first)
	}
}

func TestWebhookRejections(t *testing.T) {
	body := fixture(t, "charge_success_checkout.json")
	big := append(bytes.Repeat([]byte(" "), 65<<10), body...)
	garbage := []byte(`{"event":`)

	cases := []struct {
		name     string
		acceptor ingest.Acceptor
		body     []byte
		sig      string
		status   int
		code     string
	}{
		{"forged signature", &recorder{}, body, paystack.Sign("sk_test_attacker", body), 401, "invalid_signature"},
		{"missing signature", &recorder{}, body, "", 401, "invalid_signature"},
		{"oversized body", &recorder{}, big, paystack.Sign(secret, big), 413, "payload_too_large"},
		{"signed garbage", &recorder{}, garbage, paystack.Sign(secret, garbage), 400, "invalid_payload"},
		{"no acceptor", nil, body, paystack.Sign(secret, body), 503, "storage_unavailable"},
		{"acceptor fails", &recorder{err: errors.New("disk full")}, body, paystack.Sign(secret, body), 503, "storage_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, tc.acceptor)
			status, code := post(t, ts.URL, tc.body, tc.sig)
			if status != tc.status || code != tc.code {
				t.Fatalf("got %d %q, want %d %q", status, code, tc.status, tc.code)
			}
			if r, ok := tc.acceptor.(*recorder); ok && len(r.got) != 0 {
				t.Fatal("rejected request reached the acceptor")
			}
		})
	}
}

func TestRoutesAndMethods(t *testing.T) {
	ts := newTestServer(t, &recorder{})
	check := func(method, path string, want int) {
		req, _ := http.NewRequest(method, ts.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s %s = %d, want %d", method, path, resp.StatusCode, want)
		}
	}
	check(http.MethodGet, "/healthz", 200)
	check(http.MethodGet, "/webhooks/paystack", 405)
	check(http.MethodGet, "/nope", 404)
}

func TestGracefulShutdown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(testConfig(), logger, Deps{Acceptor: &recorder{}}).Serve(ctx, ln) }()

	url := "http://" + ln.Addr().String() + "/healthz"
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}
	if _, err := http.Get(url); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected connection refused after shutdown, got %v", err)
	}
}

func TestAPIMounting(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	get := func(h http.Handler, path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	shared := New(testConfig(), logger, Deps{Acceptor: &recorder{}, API: api})
	if get(shared.Handler(), "/v1/registrations") != http.StatusTeapot || shared.APIHandler() != nil {
		t.Fatal("without api_listen the API must be on the main listener")
	}

	cfg := testConfig()
	cfg.Server.APIListen = "127.0.0.1:0"
	split := New(cfg, logger, Deps{Acceptor: &recorder{}, API: api, Metrics: metrics.New(nil)})
	if get(split.Handler(), "/v1/registrations") != http.StatusNotFound {
		t.Fatal("with api_listen the API must not be on the public listener")
	}
	if split.APIHandler() == nil || get(split.APIHandler(), "/v1/registrations") != http.StatusTeapot {
		t.Fatal("API listener must serve the API")
	}
	if get(split.APIHandler(), "/webhooks/paystack") == http.StatusOK {
		t.Fatal("API listener must not serve webhooks")
	}
	if get(split.Handler(), "/metrics") != http.StatusNotFound || get(split.APIHandler(), "/metrics") != http.StatusOK {
		t.Fatal("metrics belong on the private listener when it exists")
	}
}

func TestReadiness(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	get := func(ready func(context.Context) error) int {
		rec := httptest.NewRecorder()
		New(testConfig(), logger, Deps{Acceptor: &recorder{}, Ready: ready}).Handler().
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec.Code
	}
	if get(func(context.Context) error { return nil }) != http.StatusOK {
		t.Error("healthy store must be ready")
	}
	if get(func(context.Context) error { return errors.New("disk full") }) != http.StatusServiceUnavailable {
		t.Error("failing store must not be ready")
	}
}
