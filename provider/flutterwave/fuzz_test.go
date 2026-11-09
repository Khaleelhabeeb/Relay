package flutterwave

import (
	"encoding/json"
	"testing"
)

// FuzzVerify: arbitrary bodies and headers never panic, and a request is
// accepted only if one header is exactly correct.
func FuzzVerify(f *testing.F) {
	f.Add([]byte(`{"event":"charge.completed","data":{}}`), "", "")
	f.Add([]byte(`{}`), Sign(secret, []byte(`{}`)), "")
	f.Add([]byte{}, "", secret)
	f.Fuzz(func(t *testing.T, body []byte, sig, verifHash string) {
		err := Verify(secret, body, sig, verifHash)
		if err == nil && Verify(secret, body, sig, "") != nil && Verify(secret, body, "", verifHash) != nil {
			t.Fatalf("accepted with neither header valid alone: %q %q", sig, verifHash)
		}
		if Verify(secret, body, Sign(secret, body), "") != nil {
			t.Fatal("rejected the correct signature")
		}
	})
}

// FuzzParseAndExtract: any signed-but-hostile payload is either rejected
// or yields identifiers and a stable dedup key without panicking.
func FuzzParseAndExtract(f *testing.F) {
	for _, s := range []string{
		`{"event":"charge.completed","data":{"id":1,"tx_ref":"APPA_1","status":"successful","meta":{"a":"b"}}}`,
		`{"event":"charge.completed","data":{"id":"x","meta":"{\"application\":\"y\"}"},"meta_data":{"z":1}}`,
		`{"event":"x","data":{"meta":[1,2],"tx_ref":{"a":1},"id":1e400,"status":null}}`,
		`{"event":"transfer.completed","data":{"reference":"R","status":"SUCCESSFUL","id":3}}`,
		`{"event":"x","data":{},"meta_data":"\"nested\""}`,
		`{"id":1,"txRef":"APPA_1","status":"successful","amount":100,"currency":"NGN","event.type":"CARD_TRANSACTION"}`,
		`{"event.type":"Transfer","transfer":{"id":5,"reference":"R"}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		e, err := Parse(raw)
		if err != nil {
			return
		}
		if e.Type == "" || !json.Valid(e.Data) {
			t.Fatalf("Parse accepted invalid event: %q", raw)
		}
		ids := ExtractIdentifiers(e)
		k1, k2 := DedupKey(e, ids), DedupKey(e, ExtractIdentifiers(e))
		if k1 == "" || k1 != k2 {
			t.Fatalf("dedup key unstable: %q vs %q", k1, k2)
		}
	})
}
