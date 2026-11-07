package stripe

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

const secret = "whsec_relay_fixture_secret"

var now = time.Unix(1762300000, 0)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "stripe", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// Pins the signed payload ("<t>.<body>"), HMAC-SHA256 and lower-case hex:
// printf '%s' '1.{}' | openssl dgst -sha256 -hmac key
func TestSignKnownAnswer(t *testing.T) {
	const want = "t=1,v1=1ba6b8171186efc613e8bcc0cbdab2748f24984d7c5a84faa2637afa0e40d224"
	if got := Sign("key", []byte("{}"), time.Unix(1, 0)); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
}

func TestVerify(t *testing.T) {
	body := fixture(t, "checkout_session_completed.json")
	good := Sign(secret, body, now)
	ts := strconv.FormatInt(now.Unix(), 10)
	v1 := strings.TrimPrefix(good, "t="+ts+",v1=")
	other := strings.TrimPrefix(Sign("whsec_other", body, now), "t="+ts+",v1=")
	flipped := bytes.Clone(body)
	flipped[len(flipped)/2] ^= 0x01

	cases := []struct {
		name    string
		secrets []string
		body    []byte
		header  string
		at      time.Time
		want    error
	}{
		{"valid", []string{secret}, body, good, now, nil},
		{"rolled secret: second v1 matches", []string{secret}, body, "t=" + ts + ",v1=" + other + ",v1=" + v1, now, nil},
		{"second configured secret matches", []string{"whsec_old", secret}, body, good, now, nil},
		{"v0 and unknown schemes ignored", []string{secret}, body, "t=" + ts + ",v0=abc,v1=" + v1 + ",x=1", now, nil},
		{"upper-case hex", []string{secret}, body, "t=" + ts + ",v1=" + strings.ToUpper(v1), now, nil},
		{"spaces around parts", []string{secret}, body, " t=" + ts + " , v1=" + v1 + " ", now, nil},
		{"tampered body", []string{secret}, flipped, good, now, ErrInvalidSignature},
		{"wrong secret", []string{"whsec_other"}, body, good, now, ErrInvalidSignature},
		{"v0 only", []string{secret}, body, "t=" + ts + ",v0=" + v1, now, ErrInvalidSignature},
		{"truncated v1", []string{secret}, body, "t=" + ts + ",v1=" + v1[:40], now, ErrInvalidSignature},
		{"timestamp changed", []string{secret}, body, "t=" + strconv.FormatInt(now.Unix()+1, 10) + ",v1=" + v1, now, ErrInvalidSignature},
		{"just inside tolerance", []string{secret}, body, good, now.Add(DefaultTolerance), nil},
		{"too old", []string{secret}, body, good, now.Add(DefaultTolerance + time.Second), ErrTimestampOutOfRange},
		{"from the future", []string{secret}, body, good, now.Add(-DefaultTolerance - time.Second), ErrTimestampOutOfRange},
		{"no timestamp", []string{secret}, body, "v1=" + v1, now, ErrMalformedHeader},
		{"two timestamps", []string{secret}, body, "t=" + ts + ",t=" + ts + ",v1=" + v1, now, ErrMalformedHeader},
		{"bad timestamp", []string{secret}, body, "t=abc,v1=" + v1, now, ErrMalformedHeader},
		{"part without =", []string{secret}, body, "t=" + ts + ",garbage", now, ErrMalformedHeader},
		{"missing", []string{secret}, body, "", now, ErrMissingSignature},
		{"no secret", nil, body, good, now, ErrMissingSecret},
		{"only empty secrets", []string{"", ""}, body, Sign("", body, now), now, ErrMissingSecret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.secrets, tc.body, tc.header, tc.at, 0); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyCustomTolerance(t *testing.T) {
	body := []byte(`{}`)
	h := Sign(secret, body, now)
	if err := Verify([]string{secret}, body, h, now.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrTimestampOutOfRange) {
		t.Fatalf("got %v, want ErrTimestampOutOfRange", err)
	}
}

func signed(body []byte) http.Header {
	h := http.Header{}
	h.Set(SignatureHeader, Sign(secret, body, now))
	return h
}

func TestAdapterVerifyLivemode(t *testing.T) {
	test := fixture(t, "checkout_session_completed.json") // livemode false
	a := Adapter{WebhookSecrets: []string{secret}, Now: func() time.Time { return now }}
	if err := a.Verify(signed(test), test); err != nil {
		t.Fatalf("test-mode adapter rejected test event: %v", err)
	}
	a.Livemode = true
	if err := a.Verify(signed(test), test); !errors.Is(err, ErrLivemodeMismatch) {
		t.Fatalf("live adapter: got %v, want ErrLivemodeMismatch", err)
	}
	noMode := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{}}}`)
	a.Livemode = false
	if err := a.Verify(signed(noMode), noMode); !errors.Is(err, ErrLivemodeMismatch) {
		t.Fatalf("missing livemode: got %v, want ErrLivemodeMismatch", err)
	}
	// The zero value accepts nothing.
	if err := (Adapter{}).Verify(signed(test), test); !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("zero Adapter: got %v, want ErrMissingSecret", err)
	}
}

func TestParse(t *testing.T) {
	for _, bad := range []string{
		``, `[]`, `{}`,
		`{"id":"evt_1","data":{"object":{}}}`,
		`{"type":"x","data":{"object":{}}}`,
		`{"id":"evt_1","type":"x"}`,
		`{"id":"evt_1","type":"x","data":{"object":"pi_1"}}`,
	} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("Parse(%q): got %v, want ErrMalformedEvent", bad, err)
		}
	}
	raw := fixture(t, "payment_intent_succeeded.json")
	e, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.Provider != provider.Stripe || e.Type != "payment_intent.succeeded" || !bytes.Equal(e.Raw, raw) {
		t.Fatalf("unexpected event %+v", e)
	}
	if !bytes.HasPrefix(e.Data, []byte(`{"id":"pi_3RelayFixturePI0003"`)) {
		t.Fatalf("Data is not data.object: %s", e.Data)
	}
}

func TestFixtures(t *testing.T) {
	cases := []struct {
		file, typ, txn, ref, channel string
		meta                         map[string]string
		routes                       bool
	}{
		{"checkout_session_completed.json", "checkout.session.completed", "pi_3RelayFixturePI0001", "APPA_order_1001", "card", map[string]string{}, true},
		{"checkout_session_completed_metadata.json", "checkout.session.completed", "pi_3RelayFixturePI0002", "", "card", map[string]string{"application": "app_b"}, true},
		{"payment_intent_succeeded.json", "payment_intent.succeeded", "pi_3RelayFixturePI0003", "APPA_order_1003", "card", map[string]string{"reference": "APPA_order_1003"}, true},
		{"payment_intent_payment_failed.json", "payment_intent.payment_failed", "pi_3RelayFixturePI0004", "", "card", map[string]string{"application": "app_b"}, true},
		{"charge_refunded.json", "charge.refunded", "ch_3RelayFixtureCH0005", "APPA_order_1003", "", map[string]string{"reference": "APPA_order_1003"}, false},
		{"connect_payment_intent_succeeded.json", "connect.payment_intent.succeeded", "", "", "", nil, false},
	}
	var a Adapter
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			raw := fixture(t, tc.file)
			e, err := a.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if e.Type != tc.typ {
				t.Errorf("Type = %q, want %q", e.Type, tc.typ)
			}
			ids := a.ExtractIdentifiers(e)
			if ids.TransactionID != tc.txn || ids.Reference != tc.ref || ids.Channel != tc.channel {
				t.Errorf("ids = %+v", ids)
			}
			if len(ids.Metadata) != len(tc.meta) {
				t.Errorf("Metadata = %v, want %v", ids.Metadata, tc.meta)
			}
			for k, v := range tc.meta {
				if ids.Metadata[k] != v {
					t.Errorf("Metadata[%s] = %q, want %q", k, ids.Metadata[k], v)
				}
			}
			if ids.DedicatedAccount != nil {
				t.Error("DedicatedAccount set")
			}
			if a.Routes(e.Type) != tc.routes {
				t.Errorf("Routes(%q) = %v", e.Type, !tc.routes)
			}
			if k := a.DedupKey(e, ids); !strings.HasPrefix(k, "evt:evt_") {
				t.Errorf("DedupKey = %q", k)
			}
		})
	}
}

// A retry is re-signed with a new timestamp but carries the same event id,
// so it deduplicates; a new occurrence has a new id.
func TestDedupKeyStableAcrossRetries(t *testing.T) {
	raw := fixture(t, "payment_intent_succeeded.json")
	e1, _ := Parse(raw)
	e2, _ := Parse(bytes.Replace(raw, []byte(`"pending_webhooks":1`), []byte(`"pending_webhooks":2`), 1))
	if DedupKey(e1, provider.Identifiers{}) != DedupKey(e2, provider.Identifiers{}) {
		t.Fatal("retry produced a different dedup key")
	}
	e3, _ := Parse(bytes.Replace(raw, []byte("evt_3RelayFixturePI0003"), []byte("evt_3RelayFixturePI9999"), 1))
	if DedupKey(e1, provider.Identifiers{}) == DedupKey(e3, provider.Identifiers{}) {
		t.Fatal("distinct events share a dedup key")
	}
}

func TestUnresolvedHint(t *testing.T) {
	var a Adapter
	if a.UnresolvedHint(provider.Identifiers{TransactionID: "pi_1"}) == "" {
		t.Error("no hint for an event without reference or metadata")
	}
	if a.UnresolvedHint(provider.Identifiers{TransactionID: "pi_1", Reference: "R"}) != "" {
		t.Error("hint for an event with a reference")
	}
}
