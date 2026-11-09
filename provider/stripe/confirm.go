package stripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

// DefaultAPIBaseURL is the Stripe API Confirm uses unless
// Adapter.APIBaseURL is set. Test and live mode share it; the key decides.
const DefaultAPIBaseURL = "https://api.stripe.com"

const maxAPIResponseBytes = 1 << 20

// ErrMissingAPIKey means ConfirmWithAPI is set without APIKey.
var ErrMissingAPIKey = errors.New("stripe: api key for confirmation is not configured")

var defaultAPIClient = &http.Client{Timeout: 10 * time.Second}

var eventIDPattern = regexp.MustCompile(`^evt_[A-Za-z0-9_]{1,255}$`)

// comparedFields are the data.object fields that decide ownership or
// describe the payment. They are stable across Stripe API versions, so an
// endpoint pinned to a different version than the account still compares
// equal. A field absent from both sides is equal.
var comparedFields = []string{
	"id", "object", "client_reference_id", "metadata", "payment_intent",
	"status", "payment_status", "amount", "amount_total", "amount_received", "currency",
}

// ConfirmEnabled implements provider.ConfirmSwitch.
func (a Adapter) ConfirmEnabled() bool { return a.ConfirmWithAPI }

// Confirm implements provider.Confirmer. When ConfirmWithAPI is set it
// fetches the event with GET /v1/events/{id} and requires its type,
// livemode and the comparedFields of data.object to match the webhook. An
// event Stripe does not know (404) is ErrNotConfirmed.
func (a Adapter) Confirm(ctx context.Context, e *provider.Event, _ provider.Identifiers) error {
	if !a.ConfirmWithAPI {
		return nil
	}
	if a.APIKey == "" {
		return ErrMissingAPIKey
	}
	got, err := decodeEnvelope(e.Raw)
	if err != nil || !eventIDPattern.MatchString(got.ID) {
		return fmt.Errorf("%w: event has no usable id", provider.ErrNotConfirmed)
	}
	want, err := a.fetchEvent(ctx, got.ID)
	if err != nil {
		return err
	}
	if got.Type != want.Type {
		return fmt.Errorf("%w: type is %q in the event but %q at stripe", provider.ErrNotConfirmed, got.Type, want.Type)
	}
	if got.Livemode == nil || want.Livemode == nil || *got.Livemode != *want.Livemode {
		return fmt.Errorf("%w: livemode differs from stripe's record", provider.ErrNotConfirmed)
	}
	if got.Account != want.Account {
		return fmt.Errorf("%w: account differs from stripe's record", provider.ErrNotConfirmed)
	}
	g, w := fields(got.Data.Object), fields(want.Data.Object)
	if g == nil || w == nil {
		return fmt.Errorf("%w: data.object is not an object", provider.ErrNotConfirmed)
	}
	for _, f := range comparedFields {
		if !sameJSON(g[f], w[f]) {
			return fmt.Errorf("%w: data.object.%s is %s in the event but %s at stripe", provider.ErrNotConfirmed, f, show(g[f]), show(w[f]))
		}
	}
	return nil
}

func fields(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// sameJSON compares two JSON values semantically; nil (absent) equals null.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if len(bytes.TrimSpace(a)) > 0 && json.Unmarshal(a, &x) != nil {
		return false
	}
	if len(bytes.TrimSpace(b)) > 0 && json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func show(v json.RawMessage) string {
	if len(v) == 0 {
		return "absent"
	}
	if len(v) > 64 {
		return string(v[:64]) + "…"
	}
	return string(v)
}

func (a Adapter) base() string {
	if a.APIBaseURL == "" {
		return DefaultAPIBaseURL
	}
	return strings.TrimSuffix(a.APIBaseURL, "/")
}

func (a Adapter) client() *http.Client {
	if a.HTTPClient == nil {
		return defaultAPIClient
	}
	return a.HTTPClient
}

func (a Adapter) fetchEvent(ctx context.Context, id string) (*envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base()+"/v1/events/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe: retrieve event: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: stripe has no event %s", provider.ErrNotConfirmed, id)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("stripe: retrieve event: HTTP %d: check api_key_env and that the key can read Events", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("stripe: retrieve event: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("stripe: retrieve event: read response: %w", err)
	}
	env, err := decodeEnvelope(body)
	if err != nil {
		return nil, fmt.Errorf("stripe: retrieve event: %w", err)
	}
	if env.ID != id {
		return nil, fmt.Errorf("%w: stripe returned event %s for %s", provider.ErrNotConfirmed, env.ID, id)
	}
	return env, nil
}
