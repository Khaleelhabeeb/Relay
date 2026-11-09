package monnify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/khaleelhabeeb/relay/provider"
)

// Monnify API base URLs. Confirm uses DefaultAPIBaseURL unless
// Adapter.APIBaseURL is set.
const (
	DefaultAPIBaseURL = "https://api.monnify.com"
	SandboxAPIBaseURL = "https://sandbox.monnify.com"
)

const maxAPIResponseBytes = 1 << 20

// tokenSlack refreshes an access token this long before Monnify expires it.
const tokenSlack = time.Minute

// ErrMissingAPIKey means ConfirmWithAPI is set without APIKey or SecretKey.
var ErrMissingAPIKey = errors.New("monnify: api key and secret key for confirmation are not configured")

var defaultAPIClient = &http.Client{Timeout: 10 * time.Second}

// errUnauthorized marks a 401 from the query endpoint: the cached token was
// rejected and a fresh one should be tried once.
var errUnauthorized = errors.New("monnify: access token rejected")

// tokens caches access tokens per base URL and API key. It lives outside
// Adapter because Adapter is used by value.
var tokens = struct {
	sync.Mutex
	m map[string]cachedToken
}{m: map[string]cachedToken{}}

type cachedToken struct {
	value   string
	expires time.Time
}

// envelope is Monnify's API response wrapper.
type envelope[T any] struct {
	RequestSuccessful bool   `json:"requestSuccessful"`
	ResponseMessage   string `json:"responseMessage"`
	ResponseBody      T      `json:"responseBody"`
}

// transaction holds the fields Confirm compares between a webhook's
// eventData and Monnify's record of the transaction.
type transaction struct {
	TransactionReference string      `json:"transactionReference"`
	PaymentReference     string      `json:"paymentReference"`
	PaymentStatus        string      `json:"paymentStatus"`
	AmountPaid           json.Number `json:"amountPaid"`
	Currency             string      `json:"currency"`     // webhook
	CurrencyCode         string      `json:"currencyCode"` // API record
}

// ConfirmEnabled implements provider.ConfirmSwitch.
func (a Adapter) ConfirmEnabled() bool { return a.ConfirmWithAPI }

// Confirm implements provider.Confirmer. When ConfirmWithAPI is set it looks
// the event's transactionReference up with GET
// /api/v2/merchant/transactions/query and requires the transaction
// reference, payment reference, payment status, amount paid and currency to
// match.
func (a Adapter) Confirm(ctx context.Context, e *provider.Event, ids provider.Identifiers) error {
	if !a.ConfirmWithAPI {
		return nil
	}
	if a.APIKey == "" || a.SecretKey == "" {
		return ErrMissingAPIKey
	}
	var got transaction
	if err := json.Unmarshal(e.Data, &got); err != nil || ids.TransactionID == "" {
		return fmt.Errorf("%w: event has no usable transactionReference", provider.ErrNotConfirmed)
	}
	want, err := a.query(ctx, ids.TransactionID)
	if errors.Is(err, errUnauthorized) {
		a.forgetToken()
		want, err = a.query(ctx, ids.TransactionID)
	}
	if err != nil {
		return err
	}
	for _, f := range []struct{ name, event, record string }{
		{"transactionReference", got.TransactionReference, want.TransactionReference},
		{"paymentReference", got.PaymentReference, want.PaymentReference},
		{"paymentStatus", strings.ToUpper(got.PaymentStatus), strings.ToUpper(want.PaymentStatus)},
		{"currency", got.Currency, want.CurrencyCode},
	} {
		if f.event != f.record {
			return fmt.Errorf("%w: %s is %q in the event but %q at monnify", provider.ErrNotConfirmed, f.name, f.event, f.record)
		}
	}
	if !sameAmount(got.AmountPaid, want.AmountPaid) {
		return fmt.Errorf("%w: amountPaid is %s in the event but %s at monnify", provider.ErrNotConfirmed, got.AmountPaid, want.AmountPaid)
	}
	return nil
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

func (a Adapter) cacheKey() string { return a.base() + "\x00" + a.APIKey }

func (a Adapter) forgetToken() {
	tokens.Lock()
	delete(tokens.m, a.cacheKey())
	tokens.Unlock()
}

func (a Adapter) query(ctx context.Context, transactionRef string) (*transaction, error) {
	token, err := a.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.base()+"/api/v2/merchant/transactions/query?transactionReference="+url.QueryEscape(transactionRef), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("monnify: query transaction: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errUnauthorized
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: monnify has no transaction %s (HTTP %d)", provider.ErrNotConfirmed, transactionRef, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("monnify: query transaction: HTTP %d", resp.StatusCode)
	}
	var body envelope[transaction]
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("monnify: query transaction: decode response: %w", err)
	}
	if !body.RequestSuccessful {
		return nil, fmt.Errorf("%w: monnify did not find transaction %s: %s", provider.ErrNotConfirmed, transactionRef, body.ResponseMessage)
	}
	return &body.ResponseBody, nil
}

// token returns a cached access token, logging in when there is none or it
// is about to expire.
func (a Adapter) token(ctx context.Context) (string, error) {
	key := a.cacheKey()
	tokens.Lock()
	t, ok := tokens.m[key]
	tokens.Unlock()
	if ok && time.Now().Before(t.expires) {
		return t.value, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base()+"/api/v1/auth/login", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.APIKey+":"+a.SecretKey)))
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("monnify: login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("monnify: login: HTTP %d", resp.StatusCode)
	}
	var body envelope[struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int64  `json:"expiresIn"`
	}]
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAPIResponseBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("monnify: login: decode response: %w", err)
	}
	if !body.RequestSuccessful || body.ResponseBody.AccessToken == "" {
		return "", fmt.Errorf("monnify: login refused: %s", body.ResponseMessage)
	}
	ttl := time.Duration(body.ResponseBody.ExpiresIn)*time.Second - tokenSlack
	if ttl > 0 {
		tokens.Lock()
		tokens.m[key] = cachedToken{value: body.ResponseBody.AccessToken, expires: time.Now().Add(ttl)}
		tokens.Unlock()
	}
	return body.ResponseBody.AccessToken, nil
}

// sameAmount compares decimal amounts numerically ("100" equals "100.00").
func sameAmount(a, b json.Number) bool {
	x, okx := new(big.Rat).SetString(a.String())
	y, oky := new(big.Rat).SetString(b.String())
	return okx && oky && x.Cmp(y) == 0
}
