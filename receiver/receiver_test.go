package receiver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/khaleelhabeeb/relay/internal/delivery"
)

const secret = "receiver_test_secret_0123456789abcdef"

var now = time.Unix(1_800_000_000, 0)

func signed(t *testing.T, body []byte, secrets ...string) http.Header {
	t.Helper()
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{}
	h.Set(HeaderEventID, "evt_1")
	h.Set(HeaderDeliveryID, "del_1")
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderAttempt, "2")
	h.Set(HeaderProvider, "flutterwave")
	h.Set(HeaderSignature, delivery.Sign(secrets, ts, "evt_1", "del_1", body))
	return h
}

func TestVerify(t *testing.T) {
	body := []byte(`{"event":"charge.completed","data":{}}`)
	opt := Options{Now: func() time.Time { return now }}

	d, err := Verify(signed(t, body, secret), body, []string{secret}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if d.EventID != "evt_1" || d.DeliveryID != "del_1" || d.Attempt != 2 || d.Provider != "flutterwave" ||
		!d.Timestamp.Equal(now) || !bytes.Equal(d.Body, body) {
		t.Fatalf("got %+v", d)
	}

	// Rotation: either side may hold the old or new secret.
	if _, err := Verify(signed(t, body, "new_secret_xxxxxxxxxxxxxxxxxxxxxxxx", secret), body, []string{secret}, opt); err != nil {
		t.Fatalf("old secret against rotating sender: %v", err)
	}
	if _, err := Verify(signed(t, body, secret), body, []string{"new_secret", secret}, opt); err != nil {
		t.Fatalf("rotating receiver: %v", err)
	}

	tampered := append(bytes.Clone(body), ' ')
	stale := Options{Now: func() time.Time { return now.Add(10 * time.Minute) }}
	noSig := signed(t, body, secret)
	noSig.Del(HeaderSignature)
	for name, tc := range map[string]struct {
		h    http.Header
		body []byte
		opt  Options
		want error
	}{
		"tampered body": {signed(t, body, secret), tampered, opt, ErrSignatureMismatch},
		"wrong secret":  {signed(t, body, "other"), body, opt, ErrSignatureMismatch},
		"stale":         {signed(t, body, secret), body, stale, ErrTimestampStale},
		"missing":       {noSig, body, opt, ErrSignatureMissing},
	} {
		if _, err := Verify(tc.h, tc.body, []string{secret}, tc.opt); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestHandler(t *testing.T) {
	body := []byte(`{"relay_version":1,"id":"evt_1","type":"charge.success","provider":"paystack","application":"app_b","received_at":"2026-11-02T10:15:42.123Z","payload":{"event":"charge.success"}}`)
	opt := Options{Now: func() time.Time { return now }, MaxBodyBytes: 512}
	var got *Delivery
	fail := false
	h := HandlerWith([]string{secret}, opt, func(_ context.Context, d *Delivery) error {
		if fail {
			return errors.New("db down")
		}
		got = d
		return nil
	})
	send := func(method string, hdr http.Header, b []byte) int {
		req := httptest.NewRequest(method, "/hook", bytes.NewReader(b))
		req.Header = hdr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if c := send(http.MethodPost, signed(t, body, secret), body); c != http.StatusOK {
		t.Fatalf("valid: %d", c)
	}
	env, err := got.Envelope()
	if err != nil || env.Provider != "paystack" || env.Application != "app_b" || string(env.Payload) != `{"event":"charge.success"}` {
		t.Fatalf("envelope %+v %v", env, err)
	}
	if c := send(http.MethodPost, signed(t, body, "other"), body); c != http.StatusUnauthorized {
		t.Errorf("forged: %d", c)
	}
	big := bytes.Repeat([]byte("x"), 513)
	if c := send(http.MethodPost, signed(t, big, secret), big); c != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", c)
	}
	if c := send(http.MethodGet, http.Header{}, nil); c != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", c)
	}
	fail = true
	if c := send(http.MethodPost, signed(t, body, secret), body); c != http.StatusInternalServerError {
		t.Errorf("handler error: %d", c)
	}
}

// The published test vector (README, docs/receiving.md) verifies.
func TestPublishedVector(t *testing.T) {
	h := http.Header{}
	h.Set(HeaderTimestamp, "1793570000")
	h.Set(HeaderEventID, "evt_01JABC")
	h.Set(HeaderDeliveryID, "del_01JXYZ")
	h.Set(HeaderSignature, "v1=fe8e97a7670f414a34ec2115f47bcda1ff597e880bfe6c5edf3b823ff2b12b7f")
	opt := Options{Now: func() time.Time { return time.Unix(1793570000, 0) }}
	if _, err := Verify(h, []byte(`{"event":"charge.success"}`), []string{"whsec_test_0123456789abcdef0123456789abcdef"}, opt); err != nil {
		t.Fatal(err)
	}
}
