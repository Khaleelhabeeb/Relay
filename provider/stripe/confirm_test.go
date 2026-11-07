package stripe

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

const apiKey = "rk_test_relay_fixture"

// stripeAPI serves body for GET /v1/events/{id} with status, recording the
// requested path and Authorization header.
func stripeAPI(t *testing.T, status int, body []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func confirmer(srv *httptest.Server) Adapter {
	return Adapter{WebhookSecrets: []string{secret}, ConfirmWithAPI: true, APIKey: apiKey,
		APIBaseURL: srv.URL, HTTPClient: srv.Client()}
}

func confirm(t *testing.T, a Adapter, raw []byte) error {
	t.Helper()
	e, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return a.Confirm(context.Background(), e, ExtractIdentifiers(e))
}

func TestConfirmMatches(t *testing.T) {
	for _, f := range []string{"checkout_session_completed.json", "payment_intent_succeeded.json"} {
		t.Run(f, func(t *testing.T) {
			raw := fixture(t, f)
			// The API renders the same event with different formatting and
			// extra fields; only the compared fields matter.
			api := bytes.Replace(raw, []byte(`"pending_webhooks":1`), []byte(`"pending_webhooks":0,"extra":{"a":1}`), 1)
			srv, seen := stripeAPI(t, http.StatusOK, api)
			if err := confirm(t, confirmer(srv), raw); err != nil {
				t.Fatalf("Confirm: %v", err)
			}
			if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "GET /v1/events/evt_") || !strings.HasSuffix((*seen)[0], " Bearer "+apiKey) {
				t.Fatalf("requests = %q", *seen)
			}
		})
	}
}

func TestConfirmRejectsForgery(t *testing.T) {
	record := fixture(t, "checkout_session_completed.json")
	cases := map[string][]byte{
		"reference redirected": bytes.Replace(record, []byte(`"APPA_order_1001"`), []byte(`"APPB_order_1001"`), 1),
		"metadata added":       bytes.Replace(record, []byte(`"metadata":{}`), []byte(`"metadata":{"application":"app_b"}`), 1),
		"amount changed":       bytes.Replace(record, []byte(`"amount_total":500000`), []byte(`"amount_total":1`), 1),
		"status changed":       bytes.Replace(record, []byte(`"payment_status":"paid"`), []byte(`"payment_status":"unpaid"`), 1),
		"type changed":         bytes.Replace(record, []byte(`"type":"checkout.session.completed"`), []byte(`"type":"checkout.session.async_payment_succeeded"`), 1),
		"livemode flipped":     bytes.Replace(record, []byte(`"livemode":false,"pending`), []byte(`"livemode":true,"pending`), 1),
	}
	srv, _ := stripeAPI(t, http.StatusOK, record)
	for name, forged := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(forged, record) {
				t.Fatal("replacement did not apply")
			}
			if err := confirm(t, confirmer(srv), forged); !errors.Is(err, provider.ErrNotConfirmed) {
				t.Fatalf("got %v, want ErrNotConfirmed", err)
			}
		})
	}
}

func TestConfirmHTTPOutcomes(t *testing.T) {
	raw := fixture(t, "payment_intent_succeeded.json")
	cases := []struct {
		status       int
		notConfirmed bool
	}{
		{http.StatusNotFound, true},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		srv, _ := stripeAPI(t, tc.status, []byte(`{"error":{}}`))
		err := confirm(t, confirmer(srv), raw)
		if err == nil || errors.Is(err, provider.ErrNotConfirmed) != tc.notConfirmed {
			t.Errorf("HTTP %d: got %v", tc.status, err)
		}
	}
	// A different event id in the response is not this event.
	srv, _ := stripeAPI(t, http.StatusOK, bytes.Replace(raw, []byte("evt_3RelayFixturePI0003"), []byte("evt_other"), 1))
	if err := confirm(t, confirmer(srv), raw); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("mismatched id: got %v", err)
	}
}

func TestConfirmOffAndMisconfigured(t *testing.T) {
	raw := fixture(t, "payment_intent_succeeded.json")
	if err := confirm(t, Adapter{}, raw); err != nil {
		t.Fatalf("confirm off: %v", err)
	}
	if err := confirm(t, Adapter{ConfirmWithAPI: true}, raw); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("no key: got %v", err)
	}
	// An id that is not an event id is never put in the request path.
	srv, seen := stripeAPI(t, http.StatusOK, raw)
	odd := bytes.Replace(raw, []byte(`"evt_3RelayFixturePI0003"`), []byte(`"../v1/charges"`), 1)
	if err := confirm(t, confirmer(srv), odd); !errors.Is(err, provider.ErrNotConfirmed) || len(*seen) != 0 {
		t.Fatalf("odd id: got %v after %d requests", err, len(*seen))
	}
}
