// Package provider defines the provider-neutral event model and the Adapter
// interface that teaches Relay a payment provider's webhooks. The built-in
// adapters live in provider/paystack and provider/flutterwave; implement
// Adapter to add another provider and pass it to relay.WithProvider.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Provider names, as stored in events.provider and registrations.provider
// and used in the webhook path /webhooks/<name>.
const (
	Paystack    = "paystack"
	Flutterwave = "flutterwave"
)

// Event is a parsed webhook. Raw keeps the exact signed bytes, which are
// forwarded unchanged in "original" payload mode.
type Event struct {
	Provider string
	Type     string
	Data     json.RawMessage
	Raw      []byte
}

// Identifiers are the ownership-relevant fields of an event. Customer
// fields are deliberately absent: customers transact across applications.
type Identifiers struct {
	TransactionID    string            // provider transaction id, decimal string
	Reference        string            // merchant-set transaction reference
	Channel          string            // payment channel
	Metadata         map[string]string // top-level scalar values of the event's metadata
	DedicatedAccount *DedicatedAccount // set only for dedicated virtual account deposits
}

// Adapter verifies and interprets one provider's webhooks. Implementations
// must be safe for concurrent use; Relay calls them from many requests at
// once. Parse, ExtractIdentifiers, DedupKey and Routes must not depend on
// secrets: Relay also uses them to re-read stored events.
type Adapter interface {
	// Name is the provider name: lower case letters, digits, '-' or '_'.
	// It is stored with every event and registration, and the webhook
	// endpoint is /webhooks/<Name>, so it must never change.
	Name() string
	// Verify authenticates the exact raw body. Call it before interpreting
	// the body in any way.
	Verify(h http.Header, body []byte) error
	// Parse decodes a verified body into an Event with Provider set.
	Parse(raw []byte) (*Event, error)
	// ExtractIdentifiers reads the ownership fields. Missing or oddly typed
	// fields are left empty; routing treats them as "no match".
	ExtractIdentifiers(e *Event) Identifiers
	// DedupKey is the event's deduplication identity within the provider.
	// Deliveries of the same occurrence must yield the same key.
	DedupKey(e *Event, ids Identifiers) string
	// Routes reports whether events of this type are routed to an
	// application. Return true only for types whose ownership fields you
	// have checked against real payloads; others are stored as unsupported.
	Routes(eventType string) bool
}

// Confirmer is optionally implemented by an Adapter to check a verified
// event against the provider's own API before Relay accepts it. A webhook
// signature proves only that the sender knows the account's secret, which
// every application sharing the account may hold; the provider's record of
// the transaction cannot be forged that way. Relay calls Confirm only for
// event types the adapter Routes.
type Confirmer interface {
	// Confirm returns nil when the provider's record matches e (or
	// confirmation is turned off), an error wrapping ErrNotConfirmed when
	// it does not, and any other error when the provider could not be
	// asked; Relay then answers 503 so the provider retries the webhook.
	Confirm(ctx context.Context, e *Event, ids Identifiers) error
}

// ErrNotConfirmed means the provider's API has no matching record of an
// event: it was forged, or describes a transaction differently.
var ErrNotConfirmed = errors.New("provider: event does not match the provider's record")

// Hinter is optionally implemented by an Adapter to explain, for operators,
// why an event that could not be routed carried no usable identifiers. An
// empty string falls back to Relay's generic reason.
type Hinter interface {
	UnresolvedHint(ids Identifiers) string
}

// DedicatedAccount identifies the receiving dedicated virtual account of a
// deposit. Paystack sends no stable DVA id in charge events, so the bank name
// and account number together are the identifier.
type DedicatedAccount struct {
	Bank          string // normalized: lower case, single spaces
	AccountNumber string // digits only
}

// NewDedicatedAccount normalizes a bank name and account number exactly as
// identifiers extracted from webhooks are, so registrations and events
// compare equal. ok is false if either part is empty after normalizing.
func NewDedicatedAccount(bank, accountNumber string) (DedicatedAccount, bool) {
	da := DedicatedAccount{Bank: normalizeBank(bank), AccountNumber: digitsOnly(accountNumber)}
	return da, da.Bank != "" && da.AccountNumber != ""
}

// Key is the canonical registration value, e.g. "test bank:9930000902".
func (d DedicatedAccount) Key() string { return d.Bank + ":" + d.AccountNumber }

// ParseMetadata accepts an object, a JSON-encoded object string, or
// empty/null, and returns its top-level scalar values as strings.
func ParseMetadata(raw json.RawMessage) map[string]string {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		raw = bytes.TrimSpace([]byte(s))
	}
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s := ScalarString(v); s != "" {
			out[k] = s
		}
	}
	return out
}

// ScalarString renders a JSON string, number or bool as a string; objects,
// arrays and null yield "".
func ScalarString(v json.RawMessage) string {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return ""
	}
	switch v[0] {
	case '"':
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	case '{', '[', 'n':
		return ""
	default:
		return string(v)
	}
	return ""
}

func normalizeBank(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
