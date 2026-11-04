// Package monnify verifies and interprets Monnify webhook requests.
package monnify

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader carries hex(HMAC-SHA512(client secret key, raw body)).
// Monnify sends it on production webhooks only; sandbox webhooks are
// unsigned (see Adapter.AllowUnsignedSandbox).
const SignatureHeader = "Monnify-Signature"

var (
	ErrMissingSignature = errors.New("monnify: missing signature")
	ErrInvalidSignature = errors.New("monnify: invalid signature")
	// ErrMissingSecret means Verify was given no secret key. An HMAC under
	// an empty key is computable by anyone, so nothing is accepted.
	ErrMissingSecret = errors.New("monnify: secret key is not configured")
)

// Sign returns the signature Monnify would send for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks signature against the exact raw body in constant time. Hex
// of either case is accepted. An empty secret rejects every request with
// ErrMissingSecret.
func Verify(secret string, body []byte, signature string) error {
	if secret == "" {
		return ErrMissingSecret
	}
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return ErrMissingSignature
	}
	got, err := hex.DecodeString(signature)
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
