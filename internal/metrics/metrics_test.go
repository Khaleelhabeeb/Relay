package metrics

import (
	"context"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/store"
)

func TestNilSafe(t *testing.T) {
	var m *Metrics
	m.Webhook("paystack", "accepted")
	m.AcceptLatency(time.Millisecond)
	m.Routed("matched")
	m.Attempt("a", "", time.Millisecond, false)
}

func TestExposition(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st)
	m.Webhook("paystack", "accepted")
	m.Webhook("flutterwave", "invalid_signature")
	m.Routed("unresolved")
	m.Attempt("app_a", "", 20*time.Millisecond, false)
	m.Attempt("app_a", "timeout", time.Second, true)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`relay_webhooks_received_total{provider="paystack",result="accepted"} 1`,
		`relay_webhooks_received_total{provider="flutterwave",result="invalid_signature"} 1`,
		`relay_events_routed_total{outcome="unresolved"} 1`,
		`relay_delivery_attempts_total{application="app_a",result="success"} 1`,
		`relay_delivery_attempts_total{application="app_a",result="timeout"} 1`,
		`relay_deliveries_exhausted_total{application="app_a"} 1`,
		`relay_deliveries{status="pending"} 0`,
		`relay_events_unrouted{status="unresolved"} 0`,
		`relay_store_up 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q", want)
		}
	}
}
