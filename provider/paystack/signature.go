// Package paystack verifies and interprets Paystack webhook requests.
package paystack

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader carries hex(HMAC-SHA512(secret key, raw body)).
const SignatureHeader = "X-Paystack-Signature"

var (
	ErrMissingSignature = errors.New("paystack: missing signature")
	ErrInvalidSignature = errors.New("paystack: invalid signature")
	// ErrMissingSecret means Verify was given no secret key. An HMAC under
	// an empty key is computable by anyone, so nothing is accepted.
	ErrMissingSecret = errors.New("paystack: secret key is not configured")
)

// Sign returns the signature Paystack would send for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks header against the exact raw body bytes in constant time.
// Call it before interpreting the body in any way. An empty secret rejects
// every request with ErrMissingSecret.
func Verify(secret string, body []byte, header string) error {
	if secret == "" {
		return ErrMissingSecret
	}
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrMissingSignature
	}
	got, err := hex.DecodeString(header)
	if err != nil || len(got) != sha512.Size {
		return ErrInvalidSignature
	}
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}
