package paystack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ChannelDedicatedNUBAN marks a bank transfer into a dedicated virtual account.
const ChannelDedicatedNUBAN = "dedicated_nuban"

var ErrMalformedEvent = errors.New("paystack: malformed event")

// Event is a parsed webhook. Raw keeps the exact signed bytes, which are
// forwarded unchanged in "original" payload mode.
type Event struct {
	Type string
	Data json.RawMessage
	Raw  []byte
}

// Parse decodes a verified webhook body. It requires a non-empty "event"
// string and a JSON object "data".
func Parse(raw []byte) (*Event, error) {
	var env struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedEvent, err)
	}
	if strings.TrimSpace(env.Event) == "" {
		return nil, fmt.Errorf("%w: missing event type", ErrMalformedEvent)
	}
	if d := bytes.TrimSpace(env.Data); len(d) == 0 || d[0] != '{' {
		return nil, fmt.Errorf("%w: data must be an object", ErrMalformedEvent)
	}
	return &Event{Type: env.Event, Data: env.Data, Raw: raw}, nil
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

// Identifiers are the ownership-relevant fields of an event. Customer
// fields are deliberately absent: customers transact across applications.
type Identifiers struct {
	TransactionID    string            // data.id, decimal string
	Reference        string            // data.reference
	Channel          string            // data.channel, else data.authorization.channel
	Metadata         map[string]string // top-level scalar values of data.metadata
	DedicatedAccount *DedicatedAccount // set only for dedicated_nuban deposits
}

// ExtractIdentifiers reads ownership identifiers from e.Data. Missing or
// oddly typed fields are left empty rather than treated as errors; routing
// treats absent identifiers as "no match".
func ExtractIdentifiers(e *Event) Identifiers {
	var d struct {
		ID            json.Number     `json:"id"`
		Reference     string          `json:"reference"`
		Channel       string          `json:"channel"`
		Metadata      json.RawMessage `json:"metadata"`
		Authorization struct {
			Channel                   string `json:"channel"`
			ReceiverBank              string `json:"receiver_bank"`
			ReceiverBankAccountNumber string `json:"receiver_bank_account_number"`
		} `json:"authorization"`
	}
	dec := json.NewDecoder(bytes.NewReader(e.Data))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		// A field with an unexpected type (e.g. reference as a number) fails
		// the whole decode; fall back to extracting fields independently.
		d.ID, d.Reference, d.Channel, d.Metadata = "", "", "", nil
		var loose map[string]json.RawMessage
		if json.Unmarshal(e.Data, &loose) == nil {
			d.ID = json.Number(scalarString(loose["id"]))
			_ = json.Unmarshal(loose["reference"], &d.Reference)
			_ = json.Unmarshal(loose["channel"], &d.Channel)
			d.Metadata = loose["metadata"]
			_ = json.Unmarshal(loose["authorization"], &d.Authorization)
		}
	}

	ids := Identifiers{
		TransactionID: d.ID.String(),
		Reference:     strings.TrimSpace(d.Reference),
		Channel:       d.Channel,
		Metadata:      parseMetadata(d.Metadata),
	}
	if ids.Channel == "" {
		ids.Channel = d.Authorization.Channel
	}
	if ids.Channel == ChannelDedicatedNUBAN {
		if da, ok := NewDedicatedAccount(d.Authorization.ReceiverBank, d.Authorization.ReceiverBankAccountNumber); ok {
			ids.DedicatedAccount = &da
		}
	}
	return ids
}

// parseMetadata accepts an object, a JSON-encoded object string, or
// empty/null, and returns its top-level scalar values as strings.
func parseMetadata(raw json.RawMessage) map[string]string {
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
		if s := scalarString(v); s != "" {
			out[k] = s
		}
	}
	return out
}

// scalarString renders a JSON string, number or bool as a string; objects,
// arrays and null yield "".
func scalarString(v json.RawMessage) string {
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

// semanticDedupEvents are one-shot events whose data.id identifies the
// occurrence, so re-sends with different bytes still deduplicate.
var semanticDedupEvents = map[string]bool{
	"charge.success": true,
}

// DedupKey returns the event's deduplication identity: "<type>:<data.id>"
// for one-shot events, otherwise "sha256:<hex>" of the exact raw body.
func DedupKey(e *Event, ids Identifiers) string {
	if semanticDedupEvents[e.Type] && ids.TransactionID != "" {
		return e.Type + ":" + ids.TransactionID
	}
	sum := sha256.Sum256(e.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
