package flutterwave

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

func TestConfirm(t *testing.T) {
	var gotPath, gotAuth string
	status := http.StatusOK
	body := `{"status":"success","data":{"id":285959875,"tx_ref":"APPB_FLW_1","status":"successful","amount":100.00,"currency":"NGN"}}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)
	a := Adapter{SecretHash: secret, ConfirmWithAPI: true, SecretKey: "FLWSECK_TEST-x", APIBaseURL: api.URL}
	confirm := func(data string) error {
		e, err := Parse([]byte(`{"event":"charge.completed","data":` + data + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return a.Confirm(context.Background(), e, ExtractIdentifiers(e))
	}
	genuine := `{"id":285959875,"tx_ref":"APPB_FLW_1","status":"successful","amount":100,"currency":"NGN"}`

	if err := confirm(genuine); err != nil {
		t.Fatalf("genuine: %v", err)
	}
	if gotPath != "/v3/transactions/285959875/verify" || gotAuth != "Bearer FLWSECK_TEST-x" {
		t.Fatalf("request %s with %q", gotPath, gotAuth)
	}
	for name, data := range map[string]string{
		"other tx_ref":    `{"id":285959875,"tx_ref":"APPA_1","status":"successful","amount":100,"currency":"NGN"}`,
		"inflated amount": `{"id":285959875,"tx_ref":"APPB_FLW_1","status":"successful","amount":1000,"currency":"NGN"}`,
		"failed claimed":  `{"id":285959875,"tx_ref":"APPB_FLW_1","status":"failed","amount":100,"currency":"NGN"}`,
		"no id":           `{"tx_ref":"APPB_FLW_1","status":"successful","amount":100,"currency":"NGN"}`,
	} {
		if err := confirm(data); !errors.Is(err, provider.ErrNotConfirmed) {
			t.Errorf("%s: got %v, want ErrNotConfirmed", name, err)
		}
	}
	status, body = http.StatusBadRequest, `{"status":"error","message":"No transaction was found for this id"}`
	if err := confirm(genuine); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("unknown id: got %v, want ErrNotConfirmed", err)
	}
	status, body = http.StatusServiceUnavailable, ``
	if err := confirm(genuine); err == nil || errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("API outage: got %v, want a retryable error", err)
	}
	a.SecretKey = ""
	if err := confirm(genuine); !errors.Is(err, ErrMissingSecretKey) {
		t.Errorf("no secret key: got %v", err)
	}
}
