package flutterwave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

// DefaultAPIBaseURL is Flutterwave's API, used by Confirm.
const DefaultAPIBaseURL = "https://api.flutterwave.com"

const maxAPIResponseBytes = 1 << 20

// ErrMissingSecretKey means ConfirmWithAPI is set without SecretKey.
var ErrMissingSecretKey = errors.New("flutterwave: secret key for confirmation is not configured")

var defaultAPIClient = &http.Client{Timeout: 10 * time.Second}

// transaction holds the fields Confirm compares between a webhook's data and
// Flutterwave's record of the transaction.
type transaction struct {
	ID       json.Number `json:"id"`
	TxRef    string      `json:"tx_ref"`
	Status   string      `json:"status"`
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

// ConfirmEnabled implements provider.ConfirmSwitch.
func (a Adapter) ConfirmEnabled() bool { return a.ConfirmWithAPI }

// Confirm implements provider.Confirmer. When ConfirmWithAPI is set it looks
// the event's transaction up with GET /v3/transactions/:id/verify and
// requires the id, tx_ref, status, amount and currency to match.
func (a Adapter) Confirm(ctx context.Context, e *provider.Event, ids provider.Identifiers) error {
	if !a.ConfirmWithAPI {
		return nil
	}
	if a.SecretKey == "" {
		return ErrMissingSecretKey
	}
	var got transaction
	if err := json.Unmarshal(e.Data, &got); err != nil || ids.TransactionID == "" {
		return fmt.Errorf("%w: event has no usable transaction id", provider.ErrNotConfirmed)
	}
	base := a.APIBaseURL
	if base == "" {
		base = DefaultAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/v3/transactions/"+url.PathEscape(ids.TransactionID)+"/verify", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.SecretKey)
	req.Header.Set("Accept", "application/json")
	client := a.HTTPClient
	if client == nil {
		client = defaultAPIClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("flutterwave: verify transaction: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: flutterwave has no transaction %s (HTTP %d)", provider.ErrNotConfirmed, ids.TransactionID, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("flutterwave: verify transaction: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string      `json:"status"`
		Data   transaction `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&body); err != nil {
		return fmt.Errorf("flutterwave: verify transaction: decode response: %w", err)
	}
	if body.Status != "success" {
		return fmt.Errorf("%w: flutterwave did not verify transaction %s", provider.ErrNotConfirmed, ids.TransactionID)
	}
	want := body.Data
	for _, f := range []struct{ name, event, record string }{
		{"id", got.ID.String(), want.ID.String()},
		{"tx_ref", got.TxRef, want.TxRef},
		{"status", strings.ToLower(got.Status), strings.ToLower(want.Status)},
		{"currency", got.Currency, want.Currency},
	} {
		if f.event != f.record {
			return fmt.Errorf("%w: %s is %q in the event but %q at flutterwave", provider.ErrNotConfirmed, f.name, f.event, f.record)
		}
	}
	if !sameAmount(got.Amount, want.Amount) {
		return fmt.Errorf("%w: amount is %s in the event but %s at flutterwave", provider.ErrNotConfirmed, got.Amount, want.Amount)
	}
	return nil
}

// sameAmount compares decimal amounts numerically ("100" equals "100.00").
func sameAmount(a, b json.Number) bool {
	x, okx := new(big.Rat).SetString(a.String())
	y, oky := new(big.Rat).SetString(b.String())
	return okx && oky && x.Cmp(y) == 0
}
