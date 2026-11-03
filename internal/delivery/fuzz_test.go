package delivery

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// FuzzEnvelope: for any valid JSON payload and arbitrary metadata strings,
// the envelope is valid JSON whose payload is the original bytes verbatim.
func FuzzEnvelope(f *testing.F) {
	f.Add([]byte(`{"event":"charge.success","data":{"id":1}}`), "evt_1", "charge.success", "app")
	f.Add([]byte(` [1, 2 ,3] `), "evt_\"quote", "type\n", "<script>")
	f.Add([]byte(`"str"`), "", "", "")
	f.Fuzz(func(t *testing.T, raw []byte, id, typ, app string) {
		body, err := BuildBody("envelope", raw, id, "paystack", typ, app, time.Unix(0, 0))
		if !json.Valid(raw) {
			if err == nil {
				t.Fatal("invalid stored JSON must be rejected")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(body) {
			t.Fatalf("envelope is not valid JSON: %q", body)
		}
		var env struct {
			ID      string          `json:"id"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(env.Payload), bytes.TrimSpace(raw)) || !bytes.Contains(body, raw) {
			t.Fatalf("payload not preserved: %q vs %q", env.Payload, raw)
		}
	})
}

// FuzzVerifySignature: the recipient check never panics and never accepts
// a header that wasn't produced for these exact inputs.
func FuzzVerifySignature(f *testing.F) {
	f.Add("v1=abc", "1793570000", "evt", "del", []byte("{}"))
	f.Add("v1=,v1=,,", "-1", "", "", []byte{})
	f.Fuzz(func(t *testing.T, header, ts, ev, del string, body []byte) {
		now := time.Unix(1793570000, 0)
		if Verify("secret", header, ts, ev, del, body, now, DefaultTolerance) == nil {
			good := Sign([]string{"secret"}, ts, ev, del, body)
			if !bytes.Contains([]byte(header), []byte(good[3:])) {
				t.Fatalf("accepted forged header %q", header)
			}
		}
	})
}
