package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Reference vector, independently computed with:
//
//	printf 'v1:1793570000:evt_01JABC:del_01JXYZ:{"event":"charge.success"}' |
//	  openssl dgst -sha256 -hmac '<secret>' -hex
const (
	vecSecret   = "whsec_test_0123456789abcdef0123456789abcdef"
	vecPrevious = "whsec_prev_fedcba9876543210fedcba9876543210"
	vecTS       = "1793570000"
	vecEvent    = "evt_01JABC"
	vecDelivery = "del_01JXYZ"
	vecBody     = `{"event":"charge.success"}`
	vecSig      = "fe8e97a7670f414a34ec2115f47bcda1ff597e880bfe6c5edf3b823ff2b12b7f"
	vecPrevSig  = "c63baf955a5593889e030b22690f1e8f4ff50c6d155c7c8c277f82452ca8694b"
)

func TestSignVector(t *testing.T) {
	if got := Sign([]string{vecSecret}, vecTS, vecEvent, vecDelivery, []byte(vecBody)); got != "v1="+vecSig {
		t.Fatalf("Sign = %s", got)
	}
	got := Sign([]string{vecSecret, vecPrevious}, vecTS, vecEvent, vecDelivery, []byte(vecBody))
	if got != "v1="+vecSig+",v1="+vecPrevSig {
		t.Fatalf("rotation Sign = %s", got)
	}
	if got := Sign([]string{vecSecret, ""}, vecTS, vecEvent, vecDelivery, []byte(vecBody)); got != "v1="+vecSig {
		t.Fatalf("empty previous secret must be skipped: %s", got)
	}
}

func TestVerify(t *testing.T) {
	now := time.Unix(1793570000, 0)
	both := "v1=" + vecSig + ",v1=" + vecPrevSig
	cases := []struct {
		name                  string
		secret, header, ts    string
		event, delivery, body string
		now                   time.Time
		want                  error
	}{
		{"valid", vecSecret, "v1=" + vecSig, vecTS, vecEvent, vecDelivery, vecBody, now, nil},
		{"rotation: new secret", vecSecret, both, vecTS, vecEvent, vecDelivery, vecBody, now, nil},
		{"rotation: old secret", vecPrevious, both, vecTS, vecEvent, vecDelivery, vecBody, now, nil},
		{"tampered body", vecSecret, "v1=" + vecSig, vecTS, vecEvent, vecDelivery, `{"event":"charge.failed"}`, now, ErrSignatureMismatch},
		{"swapped event id", vecSecret, "v1=" + vecSig, vecTS, "evt_OTHER", vecDelivery, vecBody, now, ErrSignatureMismatch},
		{"swapped delivery id", vecSecret, "v1=" + vecSig, vecTS, vecEvent, "del_OTHER", vecBody, now, ErrSignatureMismatch},
		{"wrong secret", "whsec_wrong", "v1=" + vecSig, vecTS, vecEvent, vecDelivery, vecBody, now, ErrSignatureMismatch},
		{"stale", vecSecret, "v1=" + vecSig, vecTS, vecEvent, vecDelivery, vecBody, now.Add(6 * time.Minute), ErrTimestampStale},
		{"future", vecSecret, "v1=" + vecSig, vecTS, vecEvent, vecDelivery, vecBody, now.Add(-6 * time.Minute), ErrTimestampStale},
		{"bad timestamp", vecSecret, "v1=" + vecSig, "soon", vecEvent, vecDelivery, vecBody, now, ErrTimestampInvalid},
		{"missing", vecSecret, "", vecTS, vecEvent, vecDelivery, vecBody, now, ErrSignatureMissing},
		{"unknown scheme", vecSecret, "v2=" + vecSig, vecTS, vecEvent, vecDelivery, vecBody, now, ErrSignatureMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.secret, tc.header, tc.ts, tc.event, tc.delivery, []byte(tc.body), tc.now, DefaultTolerance)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBuildBody(t *testing.T) {
	raw := []byte("{\n  \"event\": \"charge.success\",\n  \"data\": {\"id\": 1, \"amount\": 500000}\n}")
	received := time.Date(2026, 11, 2, 10, 0, 0, 123000000, time.UTC)

	orig, err := BuildBody("original", raw, "evt_1", "paystack", "charge.success", "app_a", received)
	if err != nil || !bytes.Equal(orig, raw) {
		t.Fatalf("original must be the exact bytes: %q %v", orig, err)
	}

	env, err := BuildBody("envelope", raw, "evt_1", "paystack", "charge.success", "app_b", received)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(env, raw) {
		t.Fatal("envelope must embed the original bytes verbatim")
	}
	var got struct {
		RelayVersion int             `json:"relay_version"`
		ID           string          `json:"id"`
		Type         string          `json:"type"`
		Provider     string          `json:"provider"`
		Application  string          `json:"application"`
		ReceivedAt   string          `json:"received_at"`
		Payload      json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(env, &got); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\n%s", err, env)
	}
	if got.RelayVersion != 1 || got.ID != "evt_1" || got.Type != "charge.success" || got.Provider != "paystack" ||
		got.Application != "app_b" || got.ReceivedAt != "2026-11-02T10:00:00.123Z" || !bytes.Equal(got.Payload, raw) {
		t.Fatalf("envelope = %+v", got)
	}

	if _, err := BuildBody("envelope", []byte("{bad"), "e", "p", "t", "a", received); err == nil {
		t.Error("invalid stored JSON must fail")
	}
	if _, err := BuildBody("xml", raw, "e", "p", "t", "a", received); err == nil {
		t.Error("unknown format must fail")
	}
}

func TestJitter(t *testing.T) {
	if jitter(0, nil) != 0 {
		t.Error("zero delay stays zero")
	}
	base := 10 * time.Minute
	if got := jitter(base, func() float64 { return 0 }); got != 8*time.Minute {
		t.Errorf("low bound = %v", got)
	}
	if got := jitter(base, func() float64 { return 0.999999 }); got < 11*time.Minute || got >= 12*time.Minute {
		t.Errorf("high bound = %v", got)
	}
	for range 1000 {
		if got := jitter(base, nil); got < 8*time.Minute || got >= 12*time.Minute {
			t.Fatalf("jitter out of range: %v", got)
		}
	}
}

func TestClientBlocksPrivateDestinations(t *testing.T) {
	hit := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer ts.Close()
	port := ts.URL[strings.LastIndex(ts.URL, ":"):]

	client := NewHTTPClient(false)
	// An IP literal and a hostname that resolves to loopback are both
	// refused at connect time, after resolution.
	for _, target := range []string{ts.URL, "http://localhost" + port} {
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: got %v, want ErrBlockedDestination", target, err)
		}
		if code := classify(context.Background(), err); code != "blocked_destination" {
			t.Errorf("classify = %s", code)
		}
	}
	if hit {
		t.Fatal("blocked request reached the server")
	}

	// Development opt-in allows it.
	resp, err := NewHTTPClient(true).Post(ts.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !hit {
		t.Fatal("insecure client should reach local server")
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHit = true }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	resp, err := NewHTTPClient(true).Post(redirector.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || targetHit {
		t.Fatalf("status %d, target hit %v", resp.StatusCode, targetHit)
	}
}
