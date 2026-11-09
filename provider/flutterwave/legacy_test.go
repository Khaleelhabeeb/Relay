package flutterwave

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

func TestParseLegacy(t *testing.T) {
	raw := fixture(t, "legacy_bank_transfer.json")
	e, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != "charge.completed" || !bytes.Equal(e.Raw, raw) || !(Adapter{}).Routes(e.Type) {
		t.Fatalf("got %+v", e)
	}

	// The same charge in v3 form is the same event, so a switch of format
	// between Flutterwave's retries cannot deliver it twice.
	v3, err := Parse([]byte(`{"event":"charge.completed","data":{"id":1000001,"tx_ref":"APPA_LEGACY_1001","status":"successful","amount":100,"currency":"NGN"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := DedupKey(e, ExtractIdentifiers(e)), DedupKey(v3, ExtractIdentifiers(v3)); a != b {
		t.Fatalf("legacy dedup %q != v3 dedup %q", a, b)
	}

	for _, tc := range []struct{ raw, typ, channel, ref string }{
		{`{"id":7,"txRef":"R1","status":"failed","event.type":"CARD_TRANSACTION"}`, "charge.completed", "card", "R1"},
		{`{"id":8,"txRef":"R2","status":"successful","event.type":"ussd_transaction"}`, "charge.completed", "ussd", "R2"},
		{`{"event.type":"Transfer","transfer":{"id":5,"reference":"R"}}`, "Transfer", "", ""},
		{`{"event.type":"_TRANSACTION","id":9,"txRef":"R3"}`, "_TRANSACTION", "", ""},
	} {
		e, err := Parse([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.raw, err)
		}
		ids := ExtractIdentifiers(e)
		if e.Type != tc.typ || ids.Channel != tc.channel || ids.Reference != tc.ref || ids.Metadata != nil {
			t.Errorf("%s: type %q ids %+v", tc.raw, e.Type, ids)
		}
	}
	if (Adapter{}).Routes("Transfer") {
		t.Error("legacy non-charge events must not be routed")
	}
	for _, bad := range []string{`{"event.type":"  "}`, `{"event.type":7}`, `{"event":"","event.type":"CARD_TRANSACTION"}`} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("Parse(%s) = %v, want ErrMalformedEvent", bad, err)
		}
	}
}

func TestConfirmLegacy(t *testing.T) {
	var gotPath string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"status":"success","data":{"id":1000001,"tx_ref":"APPA_LEGACY_1001","status":"successful","amount":100,"currency":"NGN"}}`))
	}))
	t.Cleanup(api.Close)
	a := Adapter{SecretHash: secret, ConfirmWithAPI: true, SecretKey: "FLWSECK_TEST-x", APIBaseURL: api.URL}

	e, err := Parse(fixture(t, "legacy_bank_transfer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Confirm(context.Background(), e, ExtractIdentifiers(e)); err != nil || gotPath != "/v3/transactions/1000001/verify" {
		t.Fatalf("genuine legacy charge: %v (path %s)", err, gotPath)
	}
	forged, _ := Parse([]byte(`{"id":1000001,"txRef":"APPB_STOLEN","status":"successful","amount":100,"currency":"NGN","event.type":"CARD_TRANSACTION"}`))
	if err := a.Confirm(context.Background(), forged, ExtractIdentifiers(forged)); !errors.Is(err, provider.ErrNotConfirmed) {
		t.Fatalf("forged legacy tx_ref: %v", err)
	}
}
