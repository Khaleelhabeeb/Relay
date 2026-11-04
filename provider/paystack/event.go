package paystack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/khaleelhabeeb/relay/provider"
)

// ChannelDedicatedNUBAN marks a bank transfer into a dedicated virtual account.
const ChannelDedicatedNUBAN = "dedicated_nuban"

var ErrMalformedEvent = errors.New("paystack: malformed event")

// Event, Identifiers and DedicatedAccount are the provider-neutral types.
type (
	Event            = provider.Event
	Identifiers      = provider.Identifiers
	DedicatedAccount = provider.DedicatedAccount
)

// Parse decodes a verified webhook body. It requires a non-empty "event"
// string and a JSON object "data".
func Parse(raw []byte) (*Event, error) {
	var env struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedEvent, err)
	}
	if strings.TrimSpace(env.Event) == "" {
		return nil, fmt.Errorf("%w: missing event type", ErrMalformedEvent)
	}
	if d := bytes.TrimSpace(env.Data); len(d) == 0 || d[0] != '{' {
		return nil, fmt.Errorf("%w: data must be an object", ErrMalformedEvent)
	}
	return &Event{Provider: provider.Paystack, Type: env.Event, Data: env.Data, Raw: raw}, nil
}

// NewDedicatedAccount normalizes a bank name and account number; see
// provider.NewDedicatedAccount.
func NewDedicatedAccount(bank, accountNumber string) (DedicatedAccount, bool) {
	return provider.NewDedicatedAccount(bank, accountNumber)
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
			d.ID = json.Number(provider.ScalarString(loose["id"]))
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
		Metadata:      provider.ParseMetadata(d.Metadata),
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
