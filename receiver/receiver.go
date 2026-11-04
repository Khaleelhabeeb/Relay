// Package receiver verifies Relay's deliveries in a Go application.
//
// Relay POSTs each event to its owning application, signed with that
// application's signing secret. Verify checks a request, and Handler wraps
// a function so it only ever sees verified events:
//
//	http.Handle("/webhooks/relay", receiver.Handler(secret, func(ctx context.Context, d *receiver.Delivery) error {
//		if seen(d.EventID) { // deduplicate: delivery is at least once
//			return nil
//		}
//		// d.Body is the provider's original payload (or an envelope).
//		return fulfil(ctx, d)
//	}))
//
// This package depends only on the standard library and Relay's own
// signing code; it does not open a database or start workers.
package receiver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/khaleelhabeeb/relay/internal/delivery"
)

// Request headers Relay sets on every delivery.
const (
	HeaderEventID    = delivery.HeaderEventID
	HeaderDeliveryID = delivery.HeaderDeliveryID
	HeaderTimestamp  = delivery.HeaderTimestamp
	HeaderSignature  = delivery.HeaderSignature
	HeaderAttempt    = delivery.HeaderAttempt
	HeaderProvider   = delivery.HeaderProvider
)

// DefaultTolerance is the clock skew Verify accepts by default.
const DefaultTolerance = delivery.DefaultTolerance

// DefaultMaxBodyBytes caps the body Handler reads.
const DefaultMaxBodyBytes = 1 << 20

// Verification errors. Compare with errors.Is.
var (
	ErrSignatureMissing  = delivery.ErrSignatureMissing
	ErrSignatureMismatch = delivery.ErrSignatureMismatch
	ErrTimestampInvalid  = delivery.ErrTimestampInvalid
	ErrTimestampStale    = delivery.ErrTimestampStale
)

// Delivery is a verified request from Relay.
type Delivery struct {
	EventID    string    // stable across retries and replays: deduplicate on it
	DeliveryID string    // diagnostics only
	Attempt    int       // 1-based; diagnostics only
	Provider   string    // e.g. "paystack"; informational, not signed
	Timestamp  time.Time // when Relay signed the request
	Body       []byte    // exact bytes Relay sent
}

// Envelope is the body of a delivery to an application configured with
// payload_format: envelope. Payload is the provider's event, byte for byte.
type Envelope struct {
	RelayVersion int             `json:"relay_version"`
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Provider     string          `json:"provider"`
	Application  string          `json:"application"`
	ReceivedAt   time.Time       `json:"received_at"`
	Payload      json.RawMessage `json:"payload"`
}

// Envelope decodes d.Body as an Envelope.
func (d *Delivery) Envelope() (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(d.Body, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Options tune verification. The zero value uses the defaults.
type Options struct {
	// Tolerance is the accepted clock skew (default DefaultTolerance).
	Tolerance time.Duration
	// MaxBodyBytes caps the body Handler reads (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// Now returns the current time (default time.Now); for tests.
	Now func() time.Time
}

func (o Options) tolerance() time.Duration {
	if o.Tolerance > 0 {
		return o.Tolerance
	}
	return DefaultTolerance
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Verify checks the signature in h over the exact body bytes with any of
// secrets (pass the current secret and, while rotating, the previous one)
// and that the timestamp is within tolerance. Read the body without
// re-encoding it first.
func Verify(h http.Header, body []byte, secrets []string, opt Options) (*Delivery, error) {
	ts := h.Get(HeaderTimestamp)
	eventID, deliveryID := h.Get(HeaderEventID), h.Get(HeaderDeliveryID)
	err := ErrSignatureMismatch
	for _, s := range secrets {
		if s == "" {
			continue
		}
		err = delivery.Verify(s, h.Get(HeaderSignature), ts, eventID, deliveryID, body, opt.now(), opt.tolerance())
		if err == nil || !errors.Is(err, ErrSignatureMismatch) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	unix, _ := strconv.ParseInt(ts, 10, 64) // validated by delivery.Verify
	attempt, _ := strconv.Atoi(h.Get(HeaderAttempt))
	return &Delivery{
		EventID:    eventID,
		DeliveryID: deliveryID,
		Attempt:    attempt,
		Provider:   h.Get(HeaderProvider),
		Timestamp:  time.Unix(unix, 0).UTC(),
		Body:       body,
	}, nil
}

// VerifyRequest reads r's body (up to opt.MaxBodyBytes) and verifies it.
func VerifyRequest(r *http.Request, secrets []string, opt Options) (*Delivery, error) {
	limit := opt.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrBodyTooLarge
	}
	return Verify(r.Header, body, secrets, opt)
}

// ErrBodyTooLarge means the request body exceeded MaxBodyBytes.
var ErrBodyTooLarge = errors.New("receiver: body too large")

// HandlerFunc handles one verified delivery. Return nil once the event is
// durably processed (or was already processed); any error makes Relay retry.
type HandlerFunc func(ctx context.Context, d *Delivery) error

// Handler returns an http.Handler that verifies each request with secret
// and passes it to fn. It answers 401 to unverifiable requests, 413 to
// oversized ones, 500 when fn fails (so Relay retries) and 200 otherwise.
func Handler(secret string, fn HandlerFunc) http.Handler {
	return HandlerWith([]string{secret}, Options{}, fn)
}

// HandlerWith is Handler with several secrets (for rotation) and options.
func HandlerWith(secrets []string, opt Options, fn HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		d, err := VerifyRequest(r, secrets, opt)
		switch {
		case errors.Is(err, ErrBodyTooLarge):
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if err := fn(r.Context(), d); err != nil {
			http.Error(w, "processing failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
