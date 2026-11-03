package paystack

import (
	"net/http"

	"github.com/khaleelhabeeb/relay/internal/provider"
)

// Adapter is the provider.Adapter for Paystack.
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
