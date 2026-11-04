package delivery

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/khaleelhabeeb/relay/config"
)

// EnvelopeVersion is the "relay_version" of envelope payloads.
const EnvelopeVersion = 1

// BuildBody returns the request body for an application's payload format.
// "original" is the exact signed provider bytes. "envelope" wraps them; the
// original bytes are spliced in verbatim rather than re-encoded, so
// recipients that verify or hash the provider payload see what the provider
// sent.
func BuildBody(format string, raw []byte, eventID, provider, eventType, application string, receivedAt time.Time) ([]byte, error) {
	switch format {
	case config.PayloadOriginal, "":
		return raw, nil
	case config.PayloadEnvelope:
		if !json.Valid(raw) {
			return nil, fmt.Errorf("delivery: stored payload is not valid JSON")
		}
		head, err := json.Marshal(struct {
			RelayVersion int    `json:"relay_version"`
			ID           string `json:"id"`
			Type         string `json:"type"`
			Provider     string `json:"provider"`
			Application  string `json:"application"`
			ReceivedAt   string `json:"received_at"`
		}{EnvelopeVersion, eventID, eventType, provider, application, receivedAt.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return nil, err
		}
		b := make([]byte, 0, len(head)+len(raw)+12)
		b = append(b, head[:len(head)-1]...) // drop closing brace
		b = append(b, `,"payload":`...)
		b = append(b, raw...)
		return append(b, '}'), nil
	default:
		return nil, fmt.Errorf("delivery: unknown payload format %q", format)
	}
}
