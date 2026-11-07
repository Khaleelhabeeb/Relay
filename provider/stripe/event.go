package stripe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

// Object types Relay reads ownership fields from.
const (
	ObjectCheckoutSession = "checkout.session"
	ObjectPaymentIntent   = "payment_intent"
)

// ConnectTypePrefix is prepended to the type of an event that happened on a
// connected account (the envelope has a top-level "account"). Relay does not
// route Connect events yet, so such types are never in Routes and are stored
// as unsupported.
const ConnectTypePrefix = "connect."

// MetadataReferenceKey is the metadata key Relay reads as the merchant
// reference when an object has no client_reference_id.
const MetadataReferenceKey = "reference"

var (
	ErrMalformedEvent = errors.New("stripe: malformed event")
	// ErrLivemodeMismatch means a correctly signed event came from the other
	// mode (test vs live) than this Relay is configured for.
	ErrLivemodeMismatch = errors.New("stripe: event livemode does not match configuration")
)

type envelope struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Livemode *bool  `json:"livemode"`
	Account  string `json:"account"`
	Data     struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

func decodeEnvelope(raw []byte) (*envelope, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedEvent, err)
	}
	if strings.TrimSpace(env.ID) == "" {
		return nil, fmt.Errorf("%w: missing event id", ErrMalformedEvent)
	}
	if strings.TrimSpace(env.Type) == "" {
		return nil, fmt.Errorf("%w: missing event type", ErrMalformedEvent)
	}
	if d := bytes.TrimSpace(env.Data.Object); len(d) == 0 || d[0] != '{' {
		return nil, fmt.Errorf("%w: data.object must be an object", ErrMalformedEvent)
	}
	return &env, nil
}

// Parse decodes a verified webhook body (a snapshot event). It requires a
// non-empty "id" and "type" and a JSON object "data.object", which becomes
// Event.Data. Events from a connected account get ConnectTypePrefix.
func Parse(raw []byte) (*provider.Event, error) {
	env, err := decodeEnvelope(raw)
	if err != nil {
		return nil, err
	}
	typ := env.Type
	if env.Account != "" {
		typ = ConnectTypePrefix + typ
	}
	return &provider.Event{Provider: provider.Stripe, Type: typ, Data: env.Data.Object, Raw: raw}, nil
}

type object struct {
	ID                 json.RawMessage   `json:"id"`
	Object             json.RawMessage   `json:"object"`
	PaymentIntent      json.RawMessage   `json:"payment_intent"`
	ClientReferenceID  json.RawMessage   `json:"client_reference_id"`
	Metadata           json.RawMessage   `json:"metadata"`
	PaymentMethodTypes []json.RawMessage `json:"payment_method_types"`
}

func str(raw json.RawMessage) string { return strings.TrimSpace(provider.ScalarString(raw)) }

// ExtractIdentifiers reads ownership identifiers from data.object.
//
// For a Checkout Session the reference is client_reference_id and the
// transaction is its PaymentIntent (or the session id when there is none).
// For other objects, such as a PaymentIntent, the transaction is the
// object's id and the reference is metadata.reference; set it through
// payment_intent_data.metadata on Checkout so both events route alike.
// Missing or oddly typed fields are left empty.
func ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	if strings.HasPrefix(e.Type, ConnectTypePrefix) {
		return provider.Identifiers{}
	}
	var o object
	_ = json.Unmarshal(e.Data, &o)
	ids := provider.Identifiers{
		TransactionID: str(o.ID),
		Metadata:      provider.ParseMetadata(o.Metadata),
	}
	if len(o.PaymentMethodTypes) > 0 {
		ids.Channel = str(o.PaymentMethodTypes[0])
	}
	if str(o.Object) == ObjectCheckoutSession {
		if pi := str(o.PaymentIntent); pi != "" {
			ids.TransactionID = pi
		}
		ids.Reference = str(o.ClientReferenceID)
	}
	if ids.Reference == "" {
		ids.Reference = ids.Metadata[MetadataReferenceKey]
	}
	return ids
}

// DedupKey returns "evt:<event id>". Stripe keeps the id across retries and
// gives every occurrence a new one.
func DedupKey(e *provider.Event, _ provider.Identifiers) string {
	var env struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(e.Raw, &env)
	return "evt:" + strings.TrimSpace(env.ID)
}

// Adapter is the provider.Adapter for Stripe. The zero value parses but
// rejects every signature.
type Adapter struct {
	// WebhookSecrets are the endpoint signing secrets (whsec_...). A
	// webhook is accepted if it is signed with any of them.
	WebhookSecrets []string
	// Livemode is the mode this Relay serves: events whose livemode differs
	// are rejected even when correctly signed.
	Livemode bool
	// Tolerance bounds the signature timestamp's age; default
	// DefaultTolerance.
	Tolerance time.Duration
	// Now returns the current time; default time.Now. For tests.
	Now func() time.Time
	// ConfirmWithAPI makes Confirm fetch each routed event from Stripe's
	// events API before Relay accepts it (see provider.Confirmer). It needs
	// APIKey.
	ConfirmWithAPI bool
	// APIKey is a secret key (sk_...) or restricted key (rk_...) with read
	// access to Events, used only by Confirm.
	APIKey     string
	APIBaseURL string       // default DefaultAPIBaseURL
	HTTPClient *http.Client // default: a client with a 10s timeout
}

func (Adapter) Name() string { return provider.Stripe }

func (a Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Verify checks the Stripe-Signature header and then that the event's
// livemode matches a.Livemode. A body that is not an event passes the
// livemode check and is rejected by Parse.
func (a Adapter) Verify(h http.Header, body []byte) error {
	if err := Verify(a.WebhookSecrets, body, h.Get(SignatureHeader), a.now(), a.Tolerance); err != nil {
		return err
	}
	var env struct {
		Livemode *bool `json:"livemode"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nil
	}
	if env.Livemode == nil || *env.Livemode != a.Livemode {
		return ErrLivemodeMismatch
	}
	return nil
}

func (Adapter) Parse(raw []byte) (*provider.Event, error) { return Parse(raw) }

func (Adapter) ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	return ExtractIdentifiers(e)
}

func (Adapter) DedupKey(e *provider.Event, ids provider.Identifiers) string { return DedupKey(e, ids) }

// routedEvents are routed; others are stored as unsupported until their
// ownership fields are verified against real payloads.
var routedEvents = map[string]bool{
	"checkout.session.completed":               true,
	"checkout.session.async_payment_succeeded": true,
	"checkout.session.async_payment_failed":    true,
	"checkout.session.expired":                 true,
	"payment_intent.succeeded":                 true,
	"payment_intent.payment_failed":            true,
	"payment_intent.canceled":                  true,
	"payment_intent.processing":                true,
}

func (Adapter) Routes(eventType string) bool { return routedEvents[eventType] }

func (Adapter) UnresolvedHint(ids provider.Identifiers) string {
	if ids.Reference == "" && len(ids.Metadata) == 0 && ids.TransactionID != "" {
		return "stripe event without client_reference_id or metadata: set client_reference_id on Checkout Sessions, or metadata.reference on PaymentIntents"
	}
	return ""
}
