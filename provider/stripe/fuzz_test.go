package stripe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzVerify: arbitrary bodies and headers never panic, a body is accepted
// only with a v1 signature that is valid for it, and Sign always verifies.
func FuzzVerify(f *testing.F) {
	f.Add([]byte(`{"id":"evt_1","type":"x","livemode":false,"data":{"object":{}}}`), "")
	f.Add([]byte(`{}`), Sign(secret, []byte(`{}`), now))
	f.Add([]byte{}, "t=1,v1=00,v0=zz,,=")
	f.Fuzz(func(t *testing.T, body []byte, header string) {
		if Verify([]string{secret}, body, header, now, 0) == nil {
			if !strings.Contains(header, "v1=") {
				t.Fatalf("accepted header without v1: %q", header)
			}
		}
		if err := Verify([]string{secret}, body, Sign(secret, body, now), now, 0); err != nil {
			t.Fatalf("rejected the correct signature: %v", err)
		}
		if Verify([]string{"whsec_other"}, body, Sign(secret, body, now), now, time.Hour) == nil {
			t.Fatal("accepted a signature under another secret")
		}
	})
}

// FuzzParseAndExtract: any signed-but-hostile payload is either rejected
// or yields identifiers and a stable dedup key without panicking.
func FuzzParseAndExtract(f *testing.F) {
	for _, s := range []string{
		`{"id":"evt_1","type":"checkout.session.completed","livemode":false,"data":{"object":{"id":"cs_1","object":"checkout.session","client_reference_id":"APPA_1","payment_intent":"pi_1","metadata":{"a":"b"},"payment_method_types":["card"]}}}`,
		`{"id":"evt_2","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","object":"payment_intent","metadata":{"reference":"R"}}}}`,
		`{"id":"evt_3","type":"x","account":"acct_1","data":{"object":{"metadata":"x","payment_method_types":"card"}}}`,
		`{"id":1,"type":"x","data":{"object":{"id":["x"],"client_reference_id":1e400}}}`,
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
		if k1 == "evt:" || k1 != k2 {
			t.Fatalf("dedup key unstable or empty: %q vs %q", k1, k2)
		}
	})
}
