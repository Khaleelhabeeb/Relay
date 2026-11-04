package paystack

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzVerify: arbitrary bodies and headers never panic, and a header is
// accepted only if it is exactly the correct signature (any hex case).
func FuzzVerify(f *testing.F) {
	f.Add([]byte(`{"event":"charge.success","data":{}}`), "")
	f.Add([]byte(`{}`), strings.Repeat("ab", 64))
	f.Add([]byte{}, Sign(secret, []byte{}))
	f.Fuzz(func(t *testing.T, body []byte, header string) {
		err := Verify(secret, body, header)
		want := Sign(secret, body)
		if err == nil && !strings.EqualFold(strings.TrimSpace(header), want) {
			t.Fatalf("accepted wrong signature %q for body %q", header, body)
		}
		if Verify(secret, body, want) != nil {
			t.Fatal("rejected the correct signature")
		}
	})
}

// FuzzParseAndExtract: any signed-but-hostile payload is either rejected
// or yields identifiers and a stable dedup key without panicking.
func FuzzParseAndExtract(f *testing.F) {
	for _, s := range []string{
		`{"event":"charge.success","data":{"id":1,"reference":"APPA_1","metadata":{"a":"b"}}}`,
		`{"event":"charge.success","data":{"id":"x","metadata":"{\"application\":\"y\"}"}}`,
		`{"event":"charge.success","data":{"channel":"dedicated_nuban","authorization":{"receiver_bank":"B","receiver_bank_account_number":"1"}}}`,
		`{"event":"x","data":{"metadata":[1,2],"reference":{"a":1},"id":1e400}}`,
		`{"event":"charge.success","data":{"metadata":"\"nested\""}}`,
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
		if ids.DedicatedAccount != nil && (ids.DedicatedAccount.Bank == "" || ids.DedicatedAccount.AccountNumber == "") {
			t.Fatalf("partial dedicated account: %+v", ids.DedicatedAccount)
		}
		k1, k2 := DedupKey(e, ids), DedupKey(e, ExtractIdentifiers(e))
		if k1 == "" || k1 != k2 {
			t.Fatalf("dedup key unstable: %q vs %q", k1, k2)
		}
	})
}
