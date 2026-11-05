package monnify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/khaleelhabeeb/relay/provider"
)

// ProductReservedAccount is product.type for a deposit into a reserved
// (dedicated virtual) account.
const ProductReservedAccount = "RESERVED_ACCOUNT"

var ErrMalformedEvent = errors.New("monnify: malformed event")

// Parse decodes a verified webhook body. It requires a non-empty
// "eventType" string and a JSON object "eventData".
func Parse(raw []byte) (*provider.Event, error) {
	var env struct {
		EventType string          `json:"eventType"`
		EventData json.RawMessage `json:"eventData"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedEvent, err)
	}
	if strings.TrimSpace(env.EventType) == "" {
		return nil, fmt.Errorf("%w: missing event type", ErrMalformedEvent)
	}
	if d := bytes.TrimSpace(env.EventData); len(d) == 0 || d[0] != '{' {
		return nil, fmt.Errorf("%w: eventData must be an object", ErrMalformedEvent)
	}
	return &provider.Event{Provider: provider.Monnify, Type: env.EventType, Data: env.EventData, Raw: raw}, nil
}

type eventData struct {
	TransactionReference json.RawMessage `json:"transactionReference"`
	PaymentReference     json.RawMessage `json:"paymentReference"`
	Reference            json.RawMessage `json:"reference"`
	PaymentMethod        json.RawMessage `json:"paymentMethod"`
	PaymentStatus        json.RawMessage `json:"paymentStatus"`
	MetaData             json.RawMessage `json:"metaData"`
	Product              json.RawMessage `json:"product"`
	Destination          json.RawMessage `json:"destinationAccountInformation"`
}

// object decodes raw into a map when it is a JSON object, else nil.
func object(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if d := bytes.TrimSpace(raw); len(d) == 0 || d[0] != '{' || json.Unmarshal(d, &m) != nil {
		return nil
	}
	return m
}

func str(raw json.RawMessage) string { return strings.TrimSpace(provider.ScalarString(raw)) }

// ExtractIdentifiers reads ownership identifiers. The reference is the
// merchant's paymentReference, except for reserved-account deposits, whose
// paymentReference is Monnify-generated: for those it is product.reference,
// the accountReference the merchant chose when reserving the account, and
// the receiving account is the dedicated account. Disbursement events fall
// back to their merchant "reference". Missing or oddly typed fields are left
// empty; routing treats them as "no match".
func ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	var d eventData
	_ = json.Unmarshal(e.Data, &d)
	product := object(d.Product)
	ids := provider.Identifiers{
		TransactionID: str(d.TransactionReference),
		Reference:     str(d.PaymentReference),
		Channel:       str(d.PaymentMethod),
		Metadata:      provider.ParseMetadata(d.MetaData),
	}
	if strings.EqualFold(str(product["type"]), ProductReservedAccount) {
		ids.Reference = str(product["reference"])
		if dest := object(d.Destination); dest != nil {
			if da, ok := provider.NewDedicatedAccount(str(dest["bankName"]), str(dest["accountNumber"])); ok {
				ids.DedicatedAccount = &da
			}
		}
	}
	if ids.Reference == "" {
		ids.Reference = str(d.Reference)
	}
	return ids
}

// DedupKey returns "<eventType>:<transactionReference>:<paymentStatus>"
// when both are present (the body has no event id, and a status change is a
// new occurrence), otherwise "sha256:<hex>" of the exact raw body.
func DedupKey(e *provider.Event, ids provider.Identifiers) string {
	var d eventData
	_ = json.Unmarshal(e.Data, &d)
	status := strings.ToLower(str(d.PaymentStatus))
	if ids.TransactionID != "" && status != "" {
		return e.Type + ":" + ids.TransactionID + ":" + status
	}
	sum := sha256.Sum256(e.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Adapter is the provider.Adapter for Monnify. The zero value parses but
// rejects every signature.
type Adapter struct {
	// SecretKey is the Monnify client secret key. It keys webhook
	// signatures and, with APIKey, authenticates Confirm.
	SecretKey string
	// ConfirmWithAPI makes Confirm check each routed event with Monnify's
	// transaction query API before Relay accepts it (see
	// provider.Confirmer). It needs APIKey.
	ConfirmWithAPI bool
	APIKey         string // Monnify API key (MK_...), used only by Confirm
	// AllowUnsignedSandbox accepts webhooks without a monnify-signature
	// header, which Monnify's sandbox never sends. It has effect only with
	// ConfirmWithAPI and only for routed event types, so every such event is
	// still checked against Monnify's API. Never enable it in production.
	AllowUnsignedSandbox bool
	APIBaseURL           string       // default DefaultAPIBaseURL
	HTTPClient           *http.Client // default: a client with a 10s timeout
}

func (Adapter) Name() string { return provider.Monnify }

func (a Adapter) Verify(h http.Header, body []byte) error {
	sig := h.Get(SignatureHeader)
	if strings.TrimSpace(sig) == "" && a.AllowUnsignedSandbox && a.ConfirmWithAPI && a.SecretKey != "" {
		// Only routed types reach Confirm, so only those may skip the
		// signature; anything else unsigned is refused.
		if e, err := Parse(body); err == nil && a.Routes(e.Type) {
			return nil
		}
	}
	return Verify(a.SecretKey, body, sig)
}

func (Adapter) Parse(raw []byte) (*provider.Event, error) { return Parse(raw) }

func (Adapter) ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	return ExtractIdentifiers(e)
}

func (Adapter) DedupKey(e *provider.Event, ids provider.Identifiers) string { return DedupKey(e, ids) }

// routedEvents are routed; others are stored as unsupported until their
// ownership fields are verified against real payloads.
var routedEvents = map[string]bool{"SUCCESSFUL_TRANSACTION": true}

func (Adapter) Routes(eventType string) bool { return routedEvents[eventType] }

func (Adapter) UnresolvedHint(ids provider.Identifiers) string {
	if ids.DedicatedAccount == nil && ids.Reference == "" && ids.TransactionID != "" {
		return "monnify event without paymentReference or reserved account fields"
	}
	return ""
}
