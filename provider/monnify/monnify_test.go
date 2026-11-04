package monnify

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

const secret = "SK_TEST_relay_fixture_secret"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "monnify", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A well-known HMAC-SHA512 vector, also what
// printf '%s' 'The quick brown fox jumps over the lazy dog' | openssl dgst -sha512 -hmac key
// prints. It pins the algorithm and the lower-case hex encoding.
func TestSignKnownAnswer(t *testing.T) {
	const want = "b42af09057bac1e2d41708e48a902e09b5ff7f12ab428a4fe86653c73dd248fb82f948a549f7b791a5b41915ee4d1ec3935357e4e2317250d0372afa2ebeeb3a"
	if got := Sign("key", []byte("The quick brown fox jumps over the lazy dog")); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}

func TestVerify(t *testing.T) {
	body := fixture(t, "successful_transaction_card.json")
	good := Sign(secret, body)
	flipped := bytes.Clone(body)
	flipped[len(flipped)/2] ^= 0x01

	cases := []struct {
		name string
		body []byte
		sig  string
		want error
	}{
		{"valid", body, good, nil},
		{"upper-case hex", body, strings.ToUpper(good), nil},
		{"surrounding space", body, " " + good + " ", nil},
		{"tampered body", flipped, good, ErrInvalidSignature},
		{"wrong secret", body, Sign("other", body), ErrInvalidSignature},
		{"not hex", body, "zz" + good[2:], ErrInvalidSignature},
		{"truncated to sha256 length", body, good[:64], ErrInvalidSignature},
		{"odd length", body, good[:127], ErrInvalidSignature},
		{"missing", body, "", ErrMissingSignature},
		{"blank", body, "   ", ErrMissingSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(secret, tc.body, tc.sig); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// An HMAC under an empty key is computable by anyone, so the zero value
// must not accept it.
func TestVerifyEmptySecret(t *testing.T) {
	body := fixture(t, "successful_transaction_card.json")
	if err := Verify("", body, Sign("", body)); !errors.Is(err, ErrMissingSecret) {
		t.Errorf("Verify: got %v, want ErrMissingSecret", err)
	}
	h := http.Header{}
	h.Set(SignatureHeader, Sign("", body))
	if err := (Adapter{}).Verify(h, body); !errors.Is(err, ErrMissingSecret) {
		t.Errorf("zero Adapter: got %v, want ErrMissingSecret", err)
	}
	// The unsigned sandbox path needs a secret key too: Confirm logs in with it.
	unsigned := Adapter{AllowUnsignedSandbox: true, ConfirmWithAPI: true, APIKey: "MK_TEST_x"}
	if err := unsigned.Verify(http.Header{}, body); !errors.Is(err, ErrMissingSecret) {
		t.Errorf("unsigned without secret: got %v, want ErrMissingSecret", err)
	}
}

func TestAdapterVerifyReadsHeader(t *testing.T) {
	body := fixture(t, "successful_transaction_card.json")
	a := Adapter{SecretKey: secret}
	h := http.Header{}
	h.Set("monnify-signature", Sign(secret, body))
	if err := a.Verify(h, body); err != nil {
		t.Fatalf("monnify-signature: %v", err)
	}
	if err := a.Verify(http.Header{}, body); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("no header: %v", err)
	}
}

func TestAdapterVerifyUnsignedSandbox(t *testing.T) {
	routed := fixture(t, "successful_transaction_card.json")
	unrouted := fixture(t, "rejected_payment.json")
	sandbox := Adapter{SecretKey: secret, APIKey: "MK_TEST_x", ConfirmWithAPI: true, AllowUnsignedSandbox: true}

	if err := sandbox.Verify(http.Header{}, routed); err != nil {
		t.Fatalf("unsigned routed event: %v", err)
	}
	if err := sandbox.Verify(http.Header{}, unrouted); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("unsigned unrouted event: got %v, want ErrMissingSignature", err)
	}
	if err := sandbox.Verify(http.Header{}, []byte(`not json`)); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("unsigned garbage: got %v, want ErrMissingSignature", err)
	}
	bad := http.Header{}
	bad.Set(SignatureHeader, Sign("other", routed))
	if err := sandbox.Verify(bad, routed); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong signature is never excused: got %v", err)
	}
	noConfirm := sandbox
	noConfirm.ConfirmWithAPI = false
	if err := noConfirm.Verify(http.Header{}, routed); !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("unsigned without confirmation: got %v, want ErrMissingSignature", err)
	}
}

func TestParse(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{"eventData":{}}`,
		`{"eventType":"  ","eventData":{}}`,
		`{"eventType":"SUCCESSFUL_TRANSACTION"}`,
		`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":[1]}`,
		`{"eventType":"SUCCESSFUL_TRANSACTION","eventData":"x"}`,
	} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("Parse(%s) = %v, want ErrMalformedEvent", bad, err)
		}
	}
	e, err := Parse(fixture(t, "successful_transaction_card.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider != provider.Monnify || e.Type != "SUCCESSFUL_TRANSACTION" {
		t.Fatalf("got %+v", e)
	}
}

func TestExtractIdentifiers(t *testing.T) {
	cases := []struct {
		file     string
		id, ref  string
		channel  string
		metadata map[string]string
		dva      string
		dedup    string
	}{
		{"successful_transaction_card.json", "MNFY|04|20261102191704|000101", "APPA_616626414629", "CARD", nil, "",
			"SUCCESSFUL_TRANSACTION:MNFY|04|20261102191704|000101:paid"},
		{"successful_transaction_metadata.json", "MNFY|04|20261102192210|000102", "Links-616626414630", "ACCOUNT_TRANSFER",
			map[string]string{"application": "app_b", "order_id": "4411"}, "",
			"SUCCESSFUL_TRANSACTION:MNFY|04|20261102192210|000102:paid"},
		{"successful_transaction_metadata_string.json", "MNFY|04|20261102193001|000103", "Links-616626414631", "USSD",
			map[string]string{"application": "app_b", "order_id": "4412"}, "",
			"SUCCESSFUL_TRANSACTION:MNFY|04|20261102193001|000103:paid"},
		{"successful_transaction_reserved_account.json", "MNFY|04|20261102112842|000170", "APPB_CUST_1636106097661", "ACCOUNT_TRANSFER",
			nil, "sterling bank:6000140770", "SUCCESSFUL_TRANSACTION:MNFY|04|20261102112842|000170:paid"},
		{"rejected_payment.json", "MNFY|85|20261102175354|041855", "APPA_616626414640", "",
			map[string]string{"application": "app_a"}, "", ""},
		{"successful_disbursement.json", "MFDS|20261102032332|002431", "APPA_TRF_5", "", nil, "", ""},
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
			var dva string
			if ids.DedicatedAccount != nil {
				dva = ids.DedicatedAccount.Key()
			}
			if dva != tc.dva {
				t.Fatalf("dedicated account = %q, want %q", dva, tc.dva)
			}
			got := DedupKey(e, ids)
			if tc.dedup == "" {
				if !strings.HasPrefix(got, "sha256:") {
					t.Fatalf("dedup = %q, want body hash", got)
				}
			} else if got != tc.dedup {
				t.Fatalf("dedup = %q, want %q", got, tc.dedup)
			}
		})
	}
}

func TestExtractIdentifiersEdgeCases(t *testing.T) {
	cases := []struct {
		name, raw string
		want      provider.Identifiers
		dva       string
	}{
		{"numeric paymentReference", `{"eventType":"x","eventData":{"transactionReference":"T","paymentReference":12345}}`,
			provider.Identifiers{TransactionID: "T", Reference: "12345"}, ""},
		{"reserved account type is case-insensitive", `{"eventType":"x","eventData":{"paymentReference":"MNFY","product":{"type":"reserved_account","reference":"ACC"}}}`,
			provider.Identifiers{Reference: "ACC"}, ""},
		{"reserved account without destination", `{"eventType":"x","eventData":{"product":{"type":"RESERVED_ACCOUNT","reference":"ACC"},"destinationAccountInformation":{}}}`,
			provider.Identifiers{Reference: "ACC"}, ""},
		{"destination ignored outside reserved accounts", `{"eventType":"x","eventData":{"paymentReference":"P","destinationAccountInformation":{"bankName":"B","accountNumber":"1"}}}`,
			provider.Identifiers{Reference: "P"}, ""},
		{"paymentReference wins over reference", `{"eventType":"x","eventData":{"paymentReference":" A ","reference":"B"}}`,
			provider.Identifiers{Reference: "A"}, ""},
		{"object references ignored", `{"eventType":"x","eventData":{"paymentReference":{"a":1},"product":[1],"destinationAccountInformation":"x"}}`,
			provider.Identifiers{}, ""},
		{"account number normalized", `{"eventType":"x","eventData":{"product":{"type":"RESERVED_ACCOUNT","reference":"A"},"destinationAccountInformation":{"bankName":" Wema  Bank ","accountNumber":"60-0014"}}}`,
			provider.Identifiers{Reference: "A"}, "wema bank:600014"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			got := ExtractIdentifiers(e)
			if got.TransactionID != tc.want.TransactionID || got.Reference != tc.want.Reference {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			var dva string
			if got.DedicatedAccount != nil {
				dva = got.DedicatedAccount.Key()
			}
			if dva != tc.dva {
				t.Fatalf("dedicated account = %q, want %q", dva, tc.dva)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	a := Adapter{}
	if !a.Routes("SUCCESSFUL_TRANSACTION") {
		t.Fatal("SUCCESSFUL_TRANSACTION must be routed")
	}
	for _, typ := range []string{"REJECTED_PAYMENT", "SUCCESSFUL_DISBURSEMENT", "SETTLEMENT", "successful_transaction"} {
		if a.Routes(typ) {
			t.Errorf("%s must not be routed", typ)
		}
	}
}
