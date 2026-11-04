package paystack

import (
	"net/http"

	"github.com/khaleelhabeeb/relay/provider"
)

// Adapter is the provider.Adapter for Paystack. The zero value parses but
// rejects every signature.
type Adapter struct {
	Secret string // Paystack secret key
}

func (Adapter) Name() string { return provider.Paystack }

func (a Adapter) Verify(h http.Header, body []byte) error {
	return Verify(a.Secret, body, h.Get(SignatureHeader))
}

func (Adapter) Parse(raw []byte) (*Event, error) { return Parse(raw) }

func (Adapter) ExtractIdentifiers(e *Event) Identifiers { return ExtractIdentifiers(e) }

func (Adapter) DedupKey(e *Event, ids Identifiers) string { return DedupKey(e, ids) }

// routedEvents are routed; others are stored as unsupported until their
// ownership fields are verified against real payloads.
var routedEvents = map[string]bool{"charge.success": true}

func (Adapter) Routes(eventType string) bool { return routedEvents[eventType] }

func (Adapter) UnresolvedHint(ids Identifiers) string {
	if ids.Channel == ChannelDedicatedNUBAN && ids.DedicatedAccount == nil {
		return "dedicated_nuban event without receiver account fields"
	}
	return ""
}
