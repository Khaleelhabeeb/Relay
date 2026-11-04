package paystack

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

func TestConfirm(t *testing.T) {
	const record = `{"status":true,"data":{"id":77,"reference":"APPA_1","status":"success","amount":500000,"currency":"NGN"}}`
	var gotPath, gotAuth string
	status, body := http.StatusOK, record
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(api.Close)
	a := Adapter{Secret: secret, ConfirmWithAPI: true, APIBaseURL: api.URL}
	confirm := func(data string) error {
		e, err := Parse([]byte(`{"event":"charge.success","data":` + data + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return a.Confirm(context.Background(), e, ExtractIdentifiers(e))
	}
	genuine := `{"id":77,"reference":"APPA_1","status":"success","amount":500000,"currency":"NGN"}`

	if err := confirm(genuine); err != nil {
		t.Fatalf("genuine: %v", err)
	}
	if gotPath != "/transaction/verify/APPA_1" || gotAuth != "Bearer "+secret {
		t.Fatalf("request %s with %q", gotPath, gotAuth)
	}
	for name, data := range map[string]string{
		"other id (pre-empting)": `{"id":78,"reference":"APPA_1","status":"success","amount":500000,"currency":"NGN"}`,
		"inflated amount":        `{"id":77,"reference":"APPA_1","status":"success","amount":900000,"currency":"NGN"}`,
		"other currency":         `{"id":77,"reference":"APPA_1","status":"success","amount":500000,"currency":"USD"}`,
		"no reference":           `{"id":77,"status":"success","amount":500000,"currency":"NGN"}`,
	} {
		if err := confirm(data); !errors.Is(err, provider.ErrNotConfirmed) {
			t.Errorf("%s: got %v, want ErrNotConfirmed", name, err)
		}
	}
	body = `{"status":true,"data":{"id":77,"reference":"APPA_1","status":"abandoned","amount":500000,"currency":"NGN"}}`
	if err := confirm(genuine); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("abandoned at paystack: got %v, want ErrNotConfirmed", err)
	}
	status, body = http.StatusNotFound, `{"status":false,"message":"Transaction reference not found"}`
	if err := confirm(genuine); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("unknown reference: got %v, want ErrNotConfirmed", err)
	}
	status, body = http.StatusBadGateway, ``
	if err := confirm(genuine); err == nil || errors.Is(err, provider.ErrNotConfirmed) {
		t.Errorf("API outage: got %v, want a retryable error", err)
	}

	gotPath = ""
	a.ConfirmWithAPI = false
	if err := confirm(genuine); err != nil || gotPath != "" {
		t.Errorf("disabled: err %v, API called %q", err, gotPath)
	}
}
