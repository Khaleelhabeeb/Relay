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
)

// Sign returns the signature Paystack would send for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha512.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks header against the exact raw body bytes in constant time.
// Call it before interpreting the body in any way.
func Verify(secret string, body []byte, header string) error {
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
