// Package provider holds the provider-neutral event model shared by the
// payment-provider adapters (paystack, flutterwave) and the pipeline that
// verifies, routes and stores their webhooks.
package provider

import (
	"bytes"
	"encoding/json"
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

// Adapter verifies and interprets one provider's webhooks.
type Adapter interface {
	// Name is the provider name, e.g. "paystack".
	Name() string
	// Verify authenticates the exact raw body. Call it before interpreting
	// the body in any way.
	Verify(h http.Header, body []byte) error
	// Parse decodes a verified body into an Event with Provider set.
	Parse(raw []byte) (*Event, error)
	ExtractIdentifiers(e *Event) Identifiers
	// DedupKey is the event's deduplication identity within the provider.
	DedupKey(e *Event, ids Identifiers) string
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
