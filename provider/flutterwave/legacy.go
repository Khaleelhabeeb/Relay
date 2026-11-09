package flutterwave

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/khaleelhabeeb/relay/provider"
)

// Legacy webhooks are what accounts without the dashboard's v3 webhook
// setting still receive: a flat object with the type in a key literally
// named "event.type" and camel-case fields, for example
//
//	{"id":10598711,"txRef":"ORDER_1","flwRef":"...","status":"successful",
//	 "amount":100,"currency":"NGN",...,"event.type":"BANK_TRANSFER_TRANSACTION"}
//
// They carry no meta. A charge ("<CHANNEL>_TRANSACTION") is normalized to
// the v3 shape: Type "charge.completed" and Data
// {"id","tx_ref","flw_ref","status","amount","currency","payment_type"}, so
// routing, deduplication and API confirmation treat both formats alike and
// the same charge delivered once in each format is one event. Other legacy
// types keep their own name (and are stored as unsupported) with the whole
// body as Data. Raw stays the exact body, which is what applications get.
const legacyTypeKey = "event.type"

// parseLegacy returns nil if raw is not a legacy webhook.
func parseLegacy(raw []byte) *provider.Event {
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	var typ string
	if json.Unmarshal(b[legacyTypeKey], &typ) != nil || strings.TrimSpace(typ) == "" {
		return nil
	}
	typ = strings.TrimSpace(typ)
	channel, ok := strings.CutSuffix(strings.ToUpper(typ), "_TRANSACTION")
	if !ok || channel == "" {
		return &provider.Event{Provider: provider.Flutterwave, Type: typ, Data: bytes.Clone(raw), Raw: raw}
	}
	data := map[string]json.RawMessage{}
	for from, to := range map[string]string{
		"id": "id", "txRef": "tx_ref", "flwRef": "flw_ref",
		"status": "status", "amount": "amount", "currency": "currency",
	} {
		if v, ok := b[from]; ok {
			data[to] = v
		}
	}
	pt, _ := json.Marshal(strings.ToLower(channel))
	data["payment_type"] = pt
	norm, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return &provider.Event{Provider: provider.Flutterwave, Type: "charge.completed", Data: norm, Raw: raw}
}
