package flutterwave

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khaleelhabeeb/relay/provider"
)

const secret = "flw_relay_fixture_secret_hash"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "flutterwave", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerify(t *testing.T) {
	body := fixture(t, "charge_completed_card.json")
	good := Sign(secret, body)
	flipped := bytes.Clone(body)
	flipped[len(flipped)/2] ^= 0x01

	cases := []struct {
		name           string
		body           []byte
		sig, verifHash string
		want           error
	}{
		{"v4 signature", body, good, "", nil},
		{"v4 signature with space", body, " " + good + " ", "", nil},
		{"v3 verif-hash", body, "", secret, nil},
		{"v3 verif-hash ignores body", flipped, "", secret, nil},
		{"bad v4, good v3", body, Sign("other", body), secret, nil},
		{"good v4, bad v3", body, good, "wrong", nil},
		{"v4 over tampered body", flipped, good, "", ErrInvalidSignature},
		{"v4 wrong secret", body, Sign("other", body), "", ErrInvalidSignature},
		{"v4 not base64", body, "!!!", "", ErrInvalidSignature},
		{"v4 truncated", body, good[:20], "", ErrInvalidSignature},
		{"wrong verif-hash", body, "", "wrong", ErrInvalidSignature},
		{"verif-hash prefix", body, "", secret[:len(secret)-1], ErrInvalidSignature},
		{"both missing", body, "", "", ErrMissingSignature},
		{"both blank", body, "  ", " ", ErrMissingSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(secret, tc.body, tc.sig, tc.verifHash); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAdapterVerifyReadsHeaders(t *testing.T) {
	body := fixture(t, "charge_completed_card.json")
	a := Adapter{SecretHash: secret}
	h := http.Header{}
	h.Set("verif-hash", secret)
	if err := a.Verify(h, body); err != nil {
		t.Fatalf("verif-hash: %v", err)
	}
	h = http.Header{}
	h.Set("flutterwave-signature", Sign(secret, body))
	if err := a.Verify(h, body); err != nil {
		t.Fatalf("flutterwave-signature: %v", err)
	}
	if err := a.Verify(http.Header{}, body); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("no headers: %v", err)
	}
}

// An HMAC under an empty key is computable by anyone, so the zero value
// must not accept it.
func TestVerifyEmptySecret(t *testing.T) {
	body := fixture(t, "charge_completed_card.json")
	for name, h := range map[string][2]string{
		"empty-key v4":     {Sign("", body), ""},
		"blank verif-hash": {"", " "},
		"no headers":       {"", ""},
	} {
		if err := Verify("", body, h[0], h[1]); !errors.Is(err, ErrMissingSecret) {
			t.Errorf("%s: got %v, want ErrMissingSecret", name, err)
		}
	}
	hdr := http.Header{}
	hdr.Set(SignatureHeader, Sign("", body))
	if err := (Adapter{}).Verify(hdr, body); !errors.Is(err, ErrMissingSecret) {
		t.Errorf("zero Adapter: got %v, want ErrMissingSecret", err)
	}
}

func TestParse(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{"data":{}}`,
		`{"event":"  ","data":{}}`,
		`{"event":"charge.completed"}`,
		`{"event":"charge.completed","data":[1]}`,
		`{"id":1,"FlwRef":"x","TransactionId":2}`, // refund-style payload without the envelope
	} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("Parse(%s) = %v, want ErrMalformedEvent", bad, err)
		}
	}
	e, err := Parse(fixture(t, "charge_completed_card.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider != provider.Flutterwave || e.Type != "charge.completed" {
		t.Fatalf("got %+v", e)
	}
}

func TestExtractIdentifiers(t *testing.T) {
	cases := []struct {
		file     string
		id, ref  string
		channel  string
		metadata map[string]string
		dedup    string
	}{
		{"charge_completed_card.json", "285959875", "APPA_616626414629", "card", nil,
			"charge.completed:285959875:successful"},
		{"charge_completed_meta.json", "285959876", "Links-616626414630", "bank_transfer",
			map[string]string{"application": "app_b", "order_id": "4411"}, "charge.completed:285959876:successful"},
		{"charge_completed_meta_data_toplevel.json", "285959877", "Links-616626414631", "card",
			map[string]string{"application": "app_b", "__CheckoutInitAddress": "https://checkout.example.com"},
			"charge.completed:285959877:successful"},
		{"charge_failed.json", "408136545", "APPA_1612345678", "card", nil, "charge.completed:408136545:failed"},
		{"transfer_completed.json", "33286", "APPA_TRF_5", "", nil, "transfer.completed:33286:successful"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			e, err := Parse(fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			ids := ExtractIdentifiers(e)
			if ids.TransactionID != tc.id || ids.Reference != tc.ref || ids.Channel != tc.channel {
				t.Fatalf("got %+v", ids)
			}
			if len(ids.Metadata) != len(tc.metadata) {
				t.Fatalf("metadata = %v, want %v", ids.Metadata, tc.metadata)
			}
			for k, v := range tc.metadata {
				if ids.Metadata[k] != v {
					t.Fatalf("metadata[%s] = %q, want %q", k, ids.Metadata[k], v)
				}
			}
			if ids.DedicatedAccount != nil {
				t.Fatalf("unexpected dedicated account %+v", ids.DedicatedAccount)
			}
			if got := DedupKey(e, ids); got != tc.dedup {
				t.Fatalf("dedup = %q, want %q", got, tc.dedup)
			}
		})
	}
}

func TestExtractIdentifiersEdgeCases(t *testing.T) {
	cases := []struct {
		name, raw string
		want      provider.Identifiers
	}{
		{"numeric tx_ref", `{"event":"charge.completed","data":{"id":"7","tx_ref":12345}}`,
			provider.Identifiers{TransactionID: "7", Reference: "12345"}},
		{"tx_ref wins over reference", `{"event":"charge.completed","data":{"tx_ref":" A ","reference":"B"}}`,
			provider.Identifiers{Reference: "A"}},
		{"object reference ignored", `{"event":"x","data":{"reference":{"a":1}}}`, provider.Identifiers{}},
		{"string-encoded meta", `{"event":"x","data":{"meta":"{\"application\":\"app_c\"}"}}`,
			provider.Identifiers{Metadata: map[string]string{"application": "app_c"}}},
		{"data.meta overrides meta_data", `{"event":"x","data":{"meta":{"application":"a"}},"meta_data":{"application":"b","k":"v"}}`,
			provider.Identifiers{Metadata: map[string]string{"application": "a", "k": "v"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractIdentifiers(e)
			if got.TransactionID != tc.want.TransactionID || got.Reference != tc.want.Reference || len(got.Metadata) != len(tc.want.Metadata) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for k, v := range tc.want.Metadata {
				if got.Metadata[k] != v {
					t.Fatalf("metadata[%s] = %q, want %q", k, got.Metadata[k], v)
				}
			}
		})
	}
}

func TestDedupKeyFallsBackToBodyHash(t *testing.T) {
	e, _ := Parse([]byte(`{"event":"subscription.cancelled","data":{"status":"cancelled"}}`))
	if k := DedupKey(e, ExtractIdentifiers(e)); !strings.HasPrefix(k, "sha256:") {
		t.Fatalf("got %q", k)
	}
}
