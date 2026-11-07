// Package stripe verifies and interprets Stripe webhook requests.
package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries "t=<unix seconds>,v1=<hex>[,v1=<hex>…]", where each
// v1 is hex(HMAC-SHA256(endpoint signing secret, "<t>.<raw body>")). Stripe
// sends one v1 per active signing secret while a secret is being rolled.
const SignatureHeader = "Stripe-Signature"

// DefaultTolerance is how far the signed timestamp may be from now. Stripe
// signs every retry afresh, so a genuine delivery is always recent.
const DefaultTolerance = 5 * time.Minute

var (
	ErrMissingSignature    = errors.New("stripe: missing signature")
	ErrMalformedHeader     = errors.New("stripe: malformed signature header")
	ErrInvalidSignature    = errors.New("stripe: invalid signature")
	ErrTimestampOutOfRange = errors.New("stripe: signature timestamp outside tolerance")
	// ErrMissingSecret means Verify was given no signing secret. An HMAC
	// under an empty key is computable by anyone, so nothing is accepted.
	ErrMissingSecret = errors.New("stripe: webhook signing secret is not configured")
)

// Sign returns the Stripe-Signature header value Stripe would send for body
// at time t.
func Sign(secret string, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

func mac(secret, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return m.Sum(nil)
}

// Verify checks header against the exact raw body. It accepts the body if
// any v1 signature matches any non-empty secret, and the timestamp is within
// tolerance of now (tolerance <= 0 means DefaultTolerance). v0 and unknown
// schemes are ignored. With no non-empty secret it returns ErrMissingSecret.
func Verify(secrets []string, body []byte, header string, now time.Time, tolerance time.Duration) error {
	keys := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			keys = append(keys, s)
		}
	}
	if len(keys) == 0 {
		return ErrMissingSecret
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrMissingSignature
	}
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrMalformedHeader
		}
		switch k {
		case "t":
			if ts != "" {
				return ErrMalformedHeader
			}
			ts = v
		case "v1":
			if b, err := hex.DecodeString(v); err == nil && len(b) == sha256.Size {
				sigs = append(sigs, b)
			}
		}
	}
	if ts == "" {
		return ErrMalformedHeader
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || unix <= 0 {
		return ErrMalformedHeader
	}
	if len(sigs) == 0 {
		return ErrInvalidSignature
	}
	// Authenticate before judging the timestamp, so a stale but genuine
	// header and a forged one are told apart only for genuine senders.
	matched := false
	for _, key := range keys {
		want := mac(key, ts, body)
		for _, s := range sigs {
			if hmac.Equal(s, want) {
				matched = true
			}
		}
	}
	if !matched {
		return ErrInvalidSignature
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	if d := now.Sub(time.Unix(unix, 0)); d > tolerance || d < -tolerance {
		return ErrTimestampOutOfRange
	}
	return nil
}
