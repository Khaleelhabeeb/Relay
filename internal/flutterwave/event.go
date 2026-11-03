package flutterwave

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/khaleelhabeeb/relay/internal/provider"
)

var ErrMalformedEvent = errors.New("flutterwave: malformed event")

// Parse decodes a verified webhook body. It requires a non-empty "event"
// string and a JSON object "data". Payloads without that envelope (virtual
// card debits, some refunds) are rejected as malformed.
func Parse(raw []byte) (*provider.Event, error) {
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
	return &provider.Event{Provider: provider.Flutterwave, Type: env.Event, Data: env.Data, Raw: raw}, nil
}

// ExtractIdentifiers reads ownership identifiers. The reference is
// data.tx_ref (charges), else data.reference (transfers). Metadata is the
// top-level meta_data object overlaid with data.meta. Missing or oddly typed
// fields are left empty; routing treats them as "no match".
func ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	var d map[string]json.RawMessage
	_ = json.Unmarshal(e.Data, &d)
	var top struct {
		MetaData json.RawMessage `json:"meta_data"`
	}
	_ = json.Unmarshal(e.Raw, &top)

	ref := strings.TrimSpace(provider.ScalarString(d["tx_ref"]))
	if ref == "" {
		ref = strings.TrimSpace(provider.ScalarString(d["reference"]))
	}
	meta := provider.ParseMetadata(top.MetaData)
	for k, v := range provider.ParseMetadata(d["meta"]) {
		if meta == nil {
			meta = map[string]string{}
		}
		meta[k] = v
	}
	return provider.Identifiers{
		TransactionID: provider.ScalarString(d["id"]),
		Reference:     ref,
		Channel:       provider.ScalarString(d["payment_type"]),
		Metadata:      meta,
	}
}

// DedupKey returns "<event>:<data.id>:<status>" when both are present, as
// Flutterwave recommends (the body has no event id, and a status change is a
// new occurrence), otherwise "sha256:<hex>" of the exact raw body.
func DedupKey(e *provider.Event, ids provider.Identifiers) string {
	var d struct {
		Status json.RawMessage `json:"status"`
	}
	_ = json.Unmarshal(e.Data, &d)
	status := strings.ToLower(strings.TrimSpace(provider.ScalarString(d.Status)))
	if ids.TransactionID != "" && status != "" {
		return e.Type + ":" + ids.TransactionID + ":" + status
	}
	sum := sha256.Sum256(e.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Adapter is the provider.Adapter for Flutterwave.
type Adapter struct {
	SecretHash string // the secret hash set in the Flutterwave dashboard
}

func (Adapter) Name() string { return provider.Flutterwave }

func (a Adapter) Verify(h http.Header, body []byte) error {
	return Verify(a.SecretHash, body, h.Get(SignatureHeader), h.Get(VerifHashHeader))
}

func (Adapter) Parse(raw []byte) (*provider.Event, error) { return Parse(raw) }

func (Adapter) ExtractIdentifiers(e *provider.Event) provider.Identifiers {
	return ExtractIdentifiers(e)
}

func (Adapter) DedupKey(e *provider.Event, ids provider.Identifiers) string { return DedupKey(e, ids) }
