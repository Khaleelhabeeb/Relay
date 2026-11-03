package paystack

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "sk_test_relay_fixture_secret"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "paystack", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerify(t *testing.T) {
	body := fixture(t, "charge_success_checkout.json")
	good := Sign(secret, body)

	flipped := bytes.Clone(body)
	flipped[len(flipped)/2] ^= 0x01

	var reserialized bytes.Buffer
	if err := json.Compact(&reserialized, body); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		secret string
		body   []byte
		header string
		want   error
	}{
		{"valid", secret, body, good, nil},
		{"valid uppercase hex", secret, body, strings.ToUpper(good), nil},
		{"valid with surrounding space", secret, body, " " + good + " ", nil},
		{"one byte flipped", secret, flipped, good, ErrInvalidSignature},
		{"re-serialized json", secret, reserialized.Bytes(), good, ErrInvalidSignature},
		{"wrong secret", "sk_test_other", body, good, ErrInvalidSignature},
		{"missing header", secret, body, "", ErrMissingSignature},
		{"not hex", secret, body, strings.Repeat("z", 128), ErrInvalidSignature},
		{"truncated", secret, body, good[:64], ErrInvalidSignature},
		{"sha256 length", secret, body, strings.Repeat("ab", 32), ErrInvalidSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.secret, tc.body, tc.header); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParse(t *testing.T) {
	e, err := Parse(fixture(t, "charge_success_checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != "charge.success" || len(e.Data) == 0 || len(e.Raw) == 0 {
		t.Fatalf("unexpected event %+v", e)
	}
	for name, body := range map[string]string{
		"not json":     `{"event":`,
		"no event":     `{"data":{}}`,
		"blank event":  `{"event":"  ","data":{}}`,
		"no data":      `{"event":"charge.success"}`,
		"data array":   `{"event":"charge.success","data":[]}`,
		"data null":    `{"event":"charge.success","data":null}`,
		"top is array": `[]`,
	} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("%s: got %v, want ErrMalformedEvent", name, err)
		}
	}
}

func TestExtractIdentifiers(t *testing.T) {
	cases := []struct {
		fixture   string
		txID      string
		ref       string
		channel   string
		metaKey   string
		metaValue string
		dva       *DedicatedAccount
	}{
		{"charge_success_checkout.json", "4099260516", "APPA_01JBQ7Z3K9X2", "card", "student_id", "STU-0042", nil},
		{"charge_success_metadata_object.json", "4099260777", "T893401827364551", "card", "application", "app_b", nil},
		{"charge_success_metadata_string.json", "4099261003", "T112233445566778", "bank", "application", "app_b", nil},
		{"charge_success_dedicated_nuban.json", "4099262210", "1730542800000abcdefgh", "dedicated_nuban", "receiver_bank", "Test Bank",
			&DedicatedAccount{Bank: "test bank", AccountNumber: "9930000902"}},
		{"transfer_success.json", "81020301", "APPA_PAYOUT_0001", "", "", "", nil},
		{"subscription_create.json", "501122", "", "", "", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			e, err := Parse(fixture(t, tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			ids := ExtractIdentifiers(e)
			if ids.TransactionID != tc.txID || ids.Reference != tc.ref || ids.Channel != tc.channel {
				t.Errorf("got id=%q ref=%q channel=%q", ids.TransactionID, ids.Reference, ids.Channel)
			}
			if tc.metaKey != "" && ids.Metadata[tc.metaKey] != tc.metaValue {
				t.Errorf("metadata[%s] = %q, want %q", tc.metaKey, ids.Metadata[tc.metaKey], tc.metaValue)
			}
			if _, ok := ids.Metadata["custom_fields"]; ok {
				t.Error("non-scalar metadata values must be dropped")
			}
			switch {
			case tc.dva == nil && ids.DedicatedAccount != nil:
				t.Errorf("unexpected dedicated account %+v", ids.DedicatedAccount)
			case tc.dva != nil && (ids.DedicatedAccount == nil || *ids.DedicatedAccount != *tc.dva):
				t.Errorf("dedicated account = %+v, want %+v", ids.DedicatedAccount, tc.dva)
			}
		})
	}
}

func TestExtractIdentifiersOddTypes(t *testing.T) {
	cases := map[string]Identifiers{
		`{"id":"abc","reference":123,"metadata":""}`:                      {TransactionID: "abc"},
		`{"id":42,"metadata":null,"reference":"R1"}`:                      {TransactionID: "42", Reference: "R1"},
		`{"metadata":{"n":7,"b":true,"x":null,"o":{}},"reference":" R "}`: {Reference: "R", Metadata: map[string]string{"n": "7", "b": "true"}},
		`{"channel":"dedicated_nuban","authorization":{"receiver_bank":"  Wema   BANK ","receiver_bank_account_number":"012-345 6789"}}`: {
			Channel: "dedicated_nuban", DedicatedAccount: &DedicatedAccount{Bank: "wema bank", AccountNumber: "0123456789"}},
		`{"channel":"dedicated_nuban","authorization":{"receiver_bank":"Wema"}}`: {Channel: "dedicated_nuban"},
	}
	for data, want := range cases {
		ids := ExtractIdentifiers(&Event{Type: "charge.success", Data: json.RawMessage(data)})
		if ids.TransactionID != want.TransactionID || ids.Reference != want.Reference || ids.Channel != want.Channel {
			t.Errorf("%s: got %+v", data, ids)
		}
		for k, v := range want.Metadata {
			if ids.Metadata[k] != v {
				t.Errorf("%s: metadata[%s]=%q want %q", data, k, ids.Metadata[k], v)
			}
		}
		if len(ids.Metadata) != len(want.Metadata) {
			t.Errorf("%s: metadata %v, want %v", data, ids.Metadata, want.Metadata)
		}
		if (want.DedicatedAccount == nil) != (ids.DedicatedAccount == nil) ||
			(want.DedicatedAccount != nil && *want.DedicatedAccount != *ids.DedicatedAccount) {
			t.Errorf("%s: dva %+v, want %+v", data, ids.DedicatedAccount, want.DedicatedAccount)
		}
	}
}

func TestDedupKey(t *testing.T) {
	key := func(raw []byte) string {
		e, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return DedupKey(e, ExtractIdentifiers(e))
	}

	charge := fixture(t, "charge_success_checkout.json")
	var compact bytes.Buffer
	_ = json.Compact(&compact, charge)
	if got := key(charge); got != "charge.success:4099260516" {
		t.Fatalf("charge key = %q", got)
	}
	if key(charge) != key(compact.Bytes()) {
		t.Error("re-serialized charge.success must keep the same key")
	}

	transfer := fixture(t, "transfer_success.json")
	var tc bytes.Buffer
	_ = json.Compact(&tc, transfer)
	k := key(transfer)
	if !strings.HasPrefix(k, "sha256:") || len(k) != len("sha256:")+64 {
		t.Fatalf("transfer key = %q", k)
	}
	if k != key(transfer) {
		t.Error("identical bytes must give identical keys")
	}
	if k == key(tc.Bytes()) {
		t.Error("non-semantic events dedupe on exact bytes only")
	}

	noID := []byte(`{"event":"charge.success","data":{"reference":"X"}}`)
	if !strings.HasPrefix(key(noID), "sha256:") {
		t.Error("charge.success without data.id falls back to body hash")
	}
}
