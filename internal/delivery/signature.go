package delivery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Outbound request headers.
const (
	HeaderEventID    = "X-Relay-Event-ID"
	HeaderDeliveryID = "X-Relay-Delivery-ID"
	HeaderTimestamp  = "X-Relay-Timestamp"
	HeaderSignature  = "X-Relay-Signature"
	HeaderAttempt    = "X-Relay-Attempt"
)

// DefaultTolerance is the clock skew recipients should accept.
const DefaultTolerance = 5 * time.Minute

var (
	ErrSignatureMissing  = errors.New("relay: missing signature")
	ErrSignatureMismatch = errors.New("relay: signature mismatch")
	ErrTimestampInvalid  = errors.New("relay: invalid timestamp")
	ErrTimestampStale    = errors.New("relay: timestamp outside tolerance")
)

// signingInput is "v1:" + timestamp + ":" + event_id + ":" + delivery_id + ":" + body.
func signingInput(timestamp, eventID, deliveryID string, body []byte) []byte {
	b := make([]byte, 0, len(timestamp)+len(eventID)+len(deliveryID)+len(body)+6)
	b = append(b, "v1:"...)
	b = append(b, timestamp...)
	b = append(b, ':')
	b = append(b, eventID...)
	b = append(b, ':')
	b = append(b, deliveryID...)
	b = append(b, ':')
	return append(b, body...)
}

func mac(secret string, input []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(input)
	return m.Sum(nil)
}

// Sign returns the X-Relay-Signature value: "v1=<hex>" per secret, comma
// separated. Pass the current secret first and, during rotation, the
// previous one second.
func Sign(secrets []string, timestamp, eventID, deliveryID string, body []byte) string {
	input := signingInput(timestamp, eventID, deliveryID, body)
	parts := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			parts = append(parts, "v1="+hex.EncodeToString(mac(s, input)))
		}
	}
	return strings.Join(parts, ",")
}

// Verify is the recipient-side check: the timestamp must be within
// tolerance of now and some v1 signature in header must match secret over
// the exact body bytes. Recipients should then deduplicate on eventID.
func Verify(secret, header, timestamp, eventID, deliveryID string, body []byte, now time.Time, tolerance time.Duration) error {
	if strings.TrimSpace(header) == "" {
		return ErrSignatureMissing
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrTimestampInvalid
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return ErrTimestampStale
	}
	want := mac(secret, signingInput(timestamp, eventID, deliveryID, body))
	for _, part := range strings.Split(header, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		if !ok {
			continue
		}
		if got, err := hex.DecodeString(v); err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
