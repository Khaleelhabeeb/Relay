package monnify

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

const (
	apiKey     = "MK_TEST_relay"
	genuineRef = "MNFY|04|20261102191704|000101"
)

type fakeAPI struct {
	*httptest.Server
	logins, queries atomic.Int32
	status          int    // query status
	body            string // query body
	gotQuery        string
	gotAuth         string
	rejectToken     string // a token the query endpoint answers 401 for
	nextToken       atomic.Int32
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{status: http.StatusOK,
		body: `{"requestSuccessful":true,"responseMessage":"success","responseCode":"0","responseBody":{"transactionReference":"` + genuineRef + `","paymentReference":"APPA_616626414629","amountPaid":"3000.00","paymentStatus":"PAID","currencyCode":"NGN"}}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			f.logins.Add(1)
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte(apiKey+":"+secret))
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != want {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			n := f.nextToken.Add(1)
			_, _ = w.Write([]byte(`{"requestSuccessful":true,"responseMessage":"success","responseCode":"0","responseBody":{"accessToken":"tok` +
				string(rune('0'+n)) + `","expiresIn":3600}}`))
		case "/api/v2/merchant/transactions/query":
			f.queries.Add(1)
			f.gotQuery, f.gotAuth = r.URL.Query().Get("transactionReference"), r.Header.Get("Authorization")
			if f.rejectToken != "" && f.gotAuth == "Bearer "+f.rejectToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func confirmData(t *testing.T, a Adapter, data string) error {
	t.Helper()
	e, err := Parse([]byte(`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":` + data + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return a.Confirm(context.Background(), e, ExtractIdentifiers(e))
}

const genuine = `{"transactionReference":"` + genuineRef + `","paymentReference":"APPA_616626414629","amountPaid":3000,"paymentStatus":"PAID","currency":"NGN"}`

func TestConfirm(t *testing.T) {
	api := newFakeAPI(t)
	a := Adapter{SecretKey: secret, ConfirmWithAPI: true, APIKey: apiKey, APIBaseURL: api.URL}

	if err := confirmData(t, a, genuine); err != nil {
		t.Fatalf("genuine: %v", err)
	}
	if api.gotQuery != genuineRef || !strings.HasPrefix(api.gotAuth, "Bearer tok") {
		t.Fatalf("query %q with %q", api.gotQuery, api.gotAuth)
	}
	if err := confirmData(t, a, genuine); err != nil {
		t.Fatalf("genuine again: %v", err)
	}
	if n := api.logins.Load(); n != 1 {
		t.Fatalf("logged in %d times, want 1 (token reused)", n)
	}

	for name, data := range map[string]string{
		"other paymentReference":  strings.Replace(genuine, "APPA_616626414629", "APPB_1", 1),
		"inflated amount":         strings.Replace(genuine, `"amountPaid":3000`, `"amountPaid":30000`, 1),
		"other currency":          strings.Replace(genuine, `"NGN"`, `"USD"`, 1),
		"other status":            strings.Replace(genuine, `"PAID"`, `"OVERPAID"`, 1),
		"no transactionReference": `{"paymentReference":"APPA_616626414629","amountPaid":3000,"paymentStatus":"PAID","currency":"NGN"}`,
	} {
		if err := confirmData(t, a, data); !errors.Is(err, provider.ErrNotConfirmed) {
			t.Errorf("%s: got %v, want ErrNotConfirmed", name, err)
		}
	}
	if err := confirmData(t, a, strings.Replace(genuine, `"PAID"`, `"paid"`, 1)); err != nil {
		t.Errorf("status case differs: %v", err)
	}

	api.status, api.body = http.StatusOK, `{"requestSuccessful":false,"responseMessage":"Could not find transaction","responseCode":"99"}`
	if err := confirmData(t, a, genuine); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("unknown transaction: got %v, want ErrNotConfirmed", err)
	}
	api.status, api.body = http.StatusNotFound, ``
	if err := confirmData(t, a, genuine); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("404: got %v, want ErrNotConfirmed", err)
	}
	api.status, api.body = http.StatusServiceUnavailable, ``
	if err := confirmData(t, a, genuine); err == nil || errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("API outage: got %v, want a retryable error", err)
	}
}

func TestConfirmRefreshesRejectedToken(t *testing.T) {
	api := newFakeAPI(t)
	a := Adapter{SecretKey: secret, ConfirmWithAPI: true, APIKey: apiKey, APIBaseURL: api.URL}
	if err := confirmData(t, a, genuine); err != nil {
		t.Fatal(err)
	}
	api.rejectToken = "tok1" // Monnify revoked the cached token
	if err := confirmData(t, a, genuine); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	if n := api.logins.Load(); n != 2 {
		t.Fatalf("logged in %d times, want 2", n)
	}
}

func TestConfirmLoginFailureIsRetryable(t *testing.T) {
	api := newFakeAPI(t)
	a := Adapter{SecretKey: "wrong", ConfirmWithAPI: true, APIKey: apiKey, APIBaseURL: api.URL}
	if err := confirmData(t, a, genuine); err == nil || errors.Is(err, provider.ErrNotConfirmed) {
		t.Fatalf("bad credentials: got %v, want a retryable error", err)
	}
}

func TestConfirmOffOrUnconfigured(t *testing.T) {
	if err := confirmData(t, Adapter{SecretKey: secret}, genuine); err != nil {
		t.Fatalf("confirmation off: %v", err)
	}
	a := Adapter{SecretKey: secret, ConfirmWithAPI: true}
	if err := confirmData(t, a, genuine); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("no api key: got %v", err)
	}
}
