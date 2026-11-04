// Package flutterwave verifies and interprets Flutterwave webhook requests.
package flutterwave

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

const (
	// VerifHashHeader carries the dashboard secret hash verbatim (v3).
	VerifHashHeader = "Verif-Hash"
	// SignatureHeader carries base64(HMAC-SHA256(secret hash, raw body)) (v4).
	SignatureHeader = "Flutterwave-Signature"
)

var (
	ErrMissingSignature = errors.New("flutterwave: missing signature")
	ErrInvalidSignature = errors.New("flutterwave: invalid signature")
	// ErrMissingSecret means Verify was given no secret hash. An HMAC under
	// an empty key is computable by anyone, so nothing is accepted.
	ErrMissingSecret = errors.New("flutterwave: secret hash is not configured")
)

// Sign returns the v4 signature Flutterwave would send for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify accepts the request if either header proves knowledge of the
// secret hash: a v4 signature over the exact raw body, or a v3 verif-hash
// equal to the secret. Both comparisons are constant time. Accepting either
// is no weaker than accepting verif-hash alone, which v3 accounts send. An
// empty secret rejects every request with ErrMissingSecret.
func Verify(secret string, body []byte, signature, verifHash string) error {
	if secret == "" {
		return ErrMissingSecret
	}
	signature, verifHash = strings.TrimSpace(signature), strings.TrimSpace(verifHash)
	if signature == "" && verifHash == "" {
		return ErrMissingSignature
	}
	if signature != "" {
		if got, err := base64.StdEncoding.DecodeString(signature); err == nil && len(got) == sha256.Size {
			m := hmac.New(sha256.New, []byte(secret))
			m.Write(body)
			if hmac.Equal(got, m.Sum(nil)) {
				return nil
			}
		}
	}
	if verifHash != "" {
		// Compare digests so the comparison does not leak the secret's length.
		got, want := sha256.Sum256([]byte(verifHash)), sha256.Sum256([]byte(secret))
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return nil
		}
	}
	return ErrInvalidSignature
}
