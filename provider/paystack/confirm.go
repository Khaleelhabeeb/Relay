package paystack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

// DefaultAPIBaseURL is Paystack's API, used by Confirm.
const DefaultAPIBaseURL = "https://api.paystack.co"

const maxAPIResponseBytes = 1 << 20

var defaultAPIClient = &http.Client{Timeout: 10 * time.Second}

// transaction holds the fields Confirm compares between a webhook's data and
// Paystack's record of the transaction.
type transaction struct {
	ID        json.Number `json:"id"`
	Reference string      `json:"reference"`
	Status    string      `json:"status"`
	Amount    json.Number `json:"amount"`
	Currency  string      `json:"currency"`
}

// ConfirmEnabled implements provider.ConfirmSwitch.
func (a Adapter) ConfirmEnabled() bool { return a.ConfirmWithAPI }

// Confirm implements provider.Confirmer. When ConfirmWithAPI is set it looks
// the event's reference up with GET /transaction/verify/:reference and
// requires the id, reference, status, amount and currency to match.
func (a Adapter) Confirm(ctx context.Context, e *Event, ids Identifiers) error {
	if !a.ConfirmWithAPI {
		return nil
	}
	if a.Secret == "" {
		return ErrMissingSecret
	}
	var got transaction
	if err := json.Unmarshal(e.Data, &got); err != nil || ids.Reference == "" {
		return fmt.Errorf("%w: event has no usable reference", provider.ErrNotConfirmed)
	}
	base := a.APIBaseURL
	if base == "" {
		base = DefaultAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/transaction/verify/"+url.PathEscape(ids.Reference), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Secret)
	req.Header.Set("Accept", "application/json")
	client := a.HTTPClient
	if client == nil {
		client = defaultAPIClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("paystack: verify transaction: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: paystack has no transaction %q (HTTP %d)", provider.ErrNotConfirmed, ids.Reference, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("paystack: verify transaction: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status bool        `json:"status"`
		Data   transaction `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&body); err != nil {
		return fmt.Errorf("paystack: verify transaction: decode response: %w", err)
	}
	if !body.Status {
		return fmt.Errorf("%w: paystack did not verify transaction %q", provider.ErrNotConfirmed, ids.Reference)
	}
	want := body.Data
	for _, f := range []struct{ name, event, record string }{
		{"id", got.ID.String(), want.ID.String()},
		{"reference", got.Reference, want.Reference},
		{"status", got.Status, want.Status},
		{"currency", got.Currency, want.Currency},
	} {
		if f.event != f.record {
			return fmt.Errorf("%w: %s is %q in the event but %q at paystack", provider.ErrNotConfirmed, f.name, f.event, f.record)
		}
	}
	if !sameAmount(got.Amount, want.Amount) {
		return fmt.Errorf("%w: amount is %s in the event but %s at paystack", provider.ErrNotConfirmed, got.Amount, want.Amount)
	}
	return nil
}

// sameAmount compares decimal amounts numerically ("100" equals "100.0").
func sameAmount(a, b json.Number) bool {
	x, okx := new(big.Rat).SetString(a.String())
	y, oky := new(big.Rat).SetString(b.String())
	return okx && oky && x.Cmp(y) == 0
}
