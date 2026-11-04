package monnify

import (
	"encoding/json"
	"net/http"
	"testing"
)

// FuzzVerify: arbitrary bodies and signatures never panic, only the exact
// signature is accepted, and the unsigned sandbox path admits only routed
// event types.
func FuzzVerify(f *testing.F) {
	f.Add([]byte(`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":{}}`), "")
	f.Add([]byte(`{}`), Sign(secret, []byte(`{}`)))
	f.Add([]byte{}, "00")
	sandbox := Adapter{SecretKey: secret, APIKey: apiKey, ConfirmWithAPI: true, AllowUnsignedSandbox: true}
	f.Fuzz(func(t *testing.T, body []byte, sig string) {
		if Verify(secret, body, sig) == nil && Verify(secret, body, Sign(secret, body)) != nil {
			t.Fatal("inconsistent verification")
		}
		if Verify(secret, body, Sign(secret, body)) != nil {
			t.Fatal("rejected the correct signature")
		}
		if sandbox.Verify(http.Header{}, body) == nil {
			e, err := Parse(body)
			if err != nil || !sandbox.Routes(e.Type) {
				t.Fatalf("unsigned body accepted without a routed type: %q", body)
			}
		}
	})
}

// FuzzParseAndExtract: any signed-but-hostile payload is either rejected
// or yields identifiers and a stable dedup key without panicking.
func FuzzParseAndExtract(f *testing.F) {
	for _, s := range []string{
		`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":{"transactionReference":"MNFY|1","paymentReference":"APPA_1","paymentStatus":"PAID","metaData":{"a":"b"}}}`,
		`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":{"product":{"type":"RESERVED_ACCOUNT","reference":"R"},"destinationAccountInformation":{"bankName":"B","accountNumber":"1"}}}`,
		`{"eventType":"x","eventData":{"metaData":"{\"application\":\"y\"}","product":"x","paymentStatus":null}}`,
		`{"eventType":"SUCCESSFUL_DISBURSEMENT","eventData":{"reference":"R","status":"SUCCESS"}}`,
		`{"eventType":"x","eventData":{"product":{"type":["RESERVED_ACCOUNT"]},"paymentReference":1e400}}`,
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
