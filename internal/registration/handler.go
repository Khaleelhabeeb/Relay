// Package registration serves the ownership registration API
// (/v1/registrations). Each application authenticates with its own bearer
// token; the token alone decides which application a request acts for.
package registration

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/khaleelhabeeb/relay/config"
	"github.com/khaleelhabeeb/relay/internal/apierr"
	"github.com/khaleelhabeeb/relay/internal/reprocess"
	"github.com/khaleelhabeeb/relay/internal/routing"
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider"
)

const (
	maxBodyBytes      = 4 << 10
	maxReferenceLen   = 100
	defaultListLimit  = 100
	maxListLimit      = 500
	defaultRatePerSec = 10
	defaultBurst      = 20
)

type tokenEntry struct {
	digest [sha256.Size]byte
	app    string
}

type Handler struct {
	store       *store.Store
	reprocessor *reprocess.Reprocessor
	logger      *slog.Logger
	now         func() time.Time
	tokens      []tokenEntry
	providers   []string // enabled providers
	mux         *http.ServeMux

	ratePerSec rate.Limit
	burst      int
	mu         sync.Mutex
	limiters   map[string]*rate.Limiter
}

// NewHandler builds the API handler. Applications without a configured
// registration token cannot use it.
func NewHandler(cfg *config.Config, st *store.Store, rp *reprocess.Reprocessor, logger *slog.Logger) *Handler {
	h := &Handler{
		store:       st,
		reprocessor: rp,
		logger:      logger.With("component", "registration"),
		now:         func() time.Time { return time.Now().UTC() },
		ratePerSec:  defaultRatePerSec,
		burst:       defaultBurst,
		limiters:    map[string]*rate.Limiter{},
		providers:   rp.Engine.Providers(),
		mux:         http.NewServeMux(),
	}
	for id, app := range cfg.Applications {
		if app.RegistrationToken != "" {
			// Only digests stay in memory long-term.
			h.tokens = append(h.tokens, tokenEntry{sha256.Sum256([]byte(app.RegistrationToken)), id})
		}
	}
	h.mux.HandleFunc("POST /v1/registrations", h.authed(h.create))
	h.mux.HandleFunc("GET /v1/registrations", h.authed(h.list))
	h.mux.HandleFunc("DELETE /v1/registrations/{id}", h.authed(h.deactivate))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// authenticate maps the bearer token to an application. Every configured
// digest is compared, without early exit, in constant time.
func (h *Handler) authenticate(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	app := ""
	for _, t := range h.tokens {
		if subtle.ConstantTimeCompare(digest[:], t.digest[:]) == 1 {
			app = t.app
		}
	}
	return app, app != ""
}

func (h *Handler) limiter(app string) *rate.Limiter {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.limiters[app]
	if !ok {
		l = rate.NewLimiter(h.ratePerSec, h.burst)
		h.limiters[app] = l
	}
	return l
}

func (h *Handler) authed(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, ok := h.authenticate(r)
		if !ok {
			h.logger.Warn("registration API request rejected: bad or missing token", "remote_addr", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="relay"`)
			apierr.Write(w, http.StatusUnauthorized, apierr.Unauthorized, "missing or invalid bearer token")
			return
		}
		if !h.limiter(app).Allow() {
			w.Header().Set("Retry-After", "1")
			apierr.Write(w, http.StatusTooManyRequests, apierr.RateLimited, "rate limit exceeded")
			return
		}
		fn(w, r, app)
	}
}

// CreateRequest claims one identifier for an application.
type CreateRequest struct {
	Provider      string `json:"provider"`
	Type          string `json:"type"`
	Value         string `json:"value"`          // transaction_reference
	Bank          string `json:"bank"`           // dedicated_account
	AccountNumber string `json:"account_number"` // dedicated_account
}

type registrationJSON struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	Type          string     `json:"type"`
	Value         string     `json:"value"`
	Application   string     `json:"application"`
	Active        bool       `json:"active"`
	CreatedAt     time.Time  `json:"created_at"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

func toJSON(r *store.Registration) registrationJSON {
	out := registrationJSON{
		ID: r.ID, Provider: r.Provider, Type: r.IdentifierType, Value: r.IdentifierValue,
		Application: r.ApplicationID, Active: r.Active, CreatedAt: r.CreatedAt,
	}
	if !r.DeactivatedAt.IsZero() {
		t := r.DeactivatedAt
		out.DeactivatedAt = &t
	}
	return out
}

// normalize validates a create request against the enabled providers and
// returns the canonical identifier value.
func normalize(req CreateRequest, providers []string) (string, error) {
	if !slices.Contains(providers, req.Provider) {
		return "", fmt.Errorf("provider must be one of: %s", strings.Join(providers, ", "))
	}
	switch req.Type {
	case routing.IdentifierReference:
		v := strings.TrimSpace(req.Value)
		if v == "" || len(v) > maxReferenceLen {
			return "", errors.New("value must be 1-100 characters")
		}
		if strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
			return "", errors.New("value must not contain whitespace or control characters")
		}
		if req.Bank != "" || req.AccountNumber != "" {
			return "", errors.New("bank and account_number apply only to dedicated_account")
		}
		return v, nil
	case routing.IdentifierDedicatedAccount:
		if req.Provider != provider.Paystack {
			return "", errors.New("dedicated_account applies only to paystack")
		}
		if req.Value != "" {
			return "", errors.New("use bank and account_number for dedicated_account, not value")
		}
		da, ok := provider.NewDedicatedAccount(req.Bank, req.AccountNumber)
		if !ok {
			return "", errors.New("bank and account_number (digits) are required")
		}
		return da.Key(), nil
	default:
		return "", errors.New(`type must be "transaction_reference" or "dedicated_account"`)
	}
}

// ErrInvalid wraps a validation failure in a CreateRequest.
var ErrInvalid = errors.New("registration: invalid request")

// Register claims req's identifier for app, the same way POST
// /v1/registrations does. created is false when app already owned it. routed
// counts earlier unresolved events the new registration routed. Errors are
// ErrInvalid (wrapped), store.ErrRegistrationConflict, or a storage error.
func (h *Handler) Register(ctx context.Context, app string, req CreateRequest) (reg *store.Registration, created bool, routed int, err error) {
	value, err := normalize(req, h.providers)
	if err != nil {
		return nil, false, 0, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	log := h.logger.With("application", app, "provider", req.Provider, "type", req.Type)
	reg, created, err = h.store.CreateRegistration(ctx, req.Provider, req.Type, value, app, h.now())
	switch {
	case errors.Is(err, store.ErrRegistrationConflict):
		log.Warn("registration conflict")
		return nil, false, 0, err
	case err != nil:
		log.Error("registration failed", "error", err)
		return nil, false, 0, err
	}
	if created {
		log.Info("registration created", "registration_id", reg.ID)
		// Route webhooks that arrived before this registration. Failure here
		// leaves those events unresolved for operator reprocessing; the
		// registration itself is already durable.
		res, err := h.reprocessor.Identifier(context.WithoutCancel(ctx), req.Provider, req.Type, value, store.DecidedByRegistration)
		if err != nil {
			log.Error("reprocessing after registration failed", "registration_id", reg.ID, "error", err)
		}
		routed = res.Matched
	}
	return reg, created, routed, nil
}

// Deactivate ends app's registration id. It returns store.ErrNotFound for an
// unknown id or another application's registration.
func (h *Handler) Deactivate(ctx context.Context, app, id string) (*store.Registration, error) {
	reg, err := h.store.DeactivateRegistration(ctx, app, id, h.now())
	if err == nil {
		h.logger.Info("registration deactivated", "application", app, "registration_id", reg.ID)
	}
	return reg, err
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, app string) {
	var req CreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reg, created, routed, err := h.Register(r.Context(), app, req)
	switch {
	case errors.Is(err, ErrInvalid):
		apierr.Write(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
		return
	case errors.Is(err, store.ErrRegistrationConflict):
		apierr.Write(w, http.StatusConflict, apierr.RegistrationConflict, "identifier is registered to another application")
		return
	case err != nil:
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "registration could not be stored")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	apierr.WriteJSON(w, status, struct {
		registrationJSON
		RoutedEvents int `json:"routed_events"`
	}{toJSON(reg), routed})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, app string) {
	q := r.URL.Query()
	f := store.RegistrationFilter{Application: app, IdentifierType: q.Get("type"), After: q.Get("after"), Limit: defaultListLimit}
	if f.IdentifierType != "" && f.IdentifierType != routing.IdentifierReference && f.IdentifierType != routing.IdentifierDedicatedAccount {
		apierr.Write(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, "unknown type")
		return
	}
	if v := q.Get("active"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			apierr.Write(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, "active must be true or false")
			return
		}
		f.Active = &b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			apierr.Write(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, "limit must be 1-500")
			return
		}
		f.Limit = n
	}
	// Fetch one extra to know whether another page exists.
	f.Limit++
	regs, err := h.store.ListRegistrations(r.Context(), f)
	if err != nil {
		h.logger.Error("listing registrations failed", "application", app, "error", err)
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "registrations could not be read")
		return
	}
	resp := struct {
		Data []registrationJSON `json:"data"`
		Next string             `json:"next,omitempty"`
	}{Data: []registrationJSON{}}
	if len(regs) == f.Limit {
		regs = regs[:f.Limit-1]
		resp.Next = regs[len(regs)-1].ID
	}
	for i := range regs {
		resp.Data = append(resp.Data, toJSON(&regs[i]))
	}
	apierr.WriteJSON(w, http.StatusOK, resp)
}

func (h *Handler) deactivate(w http.ResponseWriter, r *http.Request, app string) {
	id := r.PathValue("id")
	reg, err := h.Deactivate(r.Context(), app, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Another application's registration is indistinguishable from none.
		apierr.Write(w, http.StatusNotFound, apierr.NotFound, "registration not found")
		return
	case err != nil:
		h.logger.Error("deactivating registration failed", "application", app, "error", err)
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "registration could not be updated")
		return
	}
	apierr.WriteJSON(w, http.StatusOK, toJSON(reg))
}

// decodeJSON reads one JSON object with unknown fields rejected.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.PayloadTooLarge, "request body too large")
		} else {
			apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "body must be a JSON object with known fields")
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "body must contain a single JSON object")
		return false
	}
	return true
}
