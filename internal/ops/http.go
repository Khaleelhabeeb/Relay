package ops

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/khaleelhabeeb/relay/admin"
	"github.com/khaleelhabeeb/relay/internal/apierr"
	"github.com/khaleelhabeeb/relay/internal/store"
)

const (
	maxRequestBytes = 64 << 10
	maxReprocessIDs = 1000
)

// Handler serves the admin API under /v1/admin/. One operator token,
// separate from every application's registration token, authorizes it.
type Handler struct {
	svc     *Service
	digest  [sha256.Size]byte
	enabled bool
	logger  *slog.Logger
	limiter *rate.Limiter
	mux     *http.ServeMux
}

// NewHandler builds the admin API. With an empty token every request is
// refused: the admin API is off until server.admin_token_env is set.
func NewHandler(svc *Service, token string, logger *slog.Logger) *Handler {
	h := &Handler{
		svc:     svc,
		enabled: token != "",
		digest:  sha256.Sum256([]byte(token)),
		logger:  logger.With("component", "admin"),
		limiter: rate.NewLimiter(20, 40),
		mux:     http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /v1/admin/events", h.listEvents)
	h.mux.HandleFunc("POST /v1/admin/events/reprocess", h.reprocess)
	h.mux.HandleFunc("GET /v1/admin/events/{id}", h.getEvent)
	h.mux.HandleFunc("POST /v1/admin/events/{id}/reroute", h.reroute)
	h.mux.HandleFunc("POST /v1/admin/events/{id}/confirm", h.confirm)
	h.mux.HandleFunc("POST /v1/admin/events/{id}/dismiss", h.dismiss)
	h.mux.HandleFunc("GET /v1/admin/deliveries", h.listDeliveries)
	h.mux.HandleFunc("POST /v1/admin/deliveries/{id}/retry", h.retryDelivery)
	h.mux.HandleFunc("GET /v1/admin/stats", h.stats)
	h.mux.HandleFunc("/v1/admin/", func(w http.ResponseWriter, _ *http.Request) {
		apierr.Write(w, http.StatusNotFound, apierr.NotFound, "no such admin endpoint")
	})
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.enabled {
		apierr.Write(w, http.StatusNotFound, apierr.AdminDisabled, "the admin API is disabled; set server.admin_token_env to enable it")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	if !ok || token == "" || subtle.ConstantTimeCompare(digest[:], h.digest[:]) != 1 {
		h.logger.Warn("admin API request rejected: bad or missing token", "remote_addr", r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Bearer realm="relay-admin"`)
		apierr.Write(w, http.StatusUnauthorized, apierr.Unauthorized, "missing or invalid bearer token")
		return
	}
	if !h.limiter.Allow() {
		w.Header().Set("Retry-After", "1")
		apierr.Write(w, http.StatusTooManyRequests, apierr.RateLimited, "rate limit exceeded")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

// fail writes err as the matching API error.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		apierr.Write(w, http.StatusUnprocessableEntity, apierr.ValidationFailed, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
	case errors.Is(err, store.ErrNotFound):
		apierr.Write(w, http.StatusNotFound, apierr.NotFound, err.Error())
	case errors.Is(err, ErrWrongState):
		apierr.Write(w, http.StatusConflict, apierr.InvalidState, strings.TrimPrefix(err.Error(), ErrWrongState.Error()+": "))
	default:
		h.logger.Error("admin API request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "the request could not be completed")
	}
}

func parseLimit(q string) (int, error) {
	if q == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 || n > admin.MaxLimit {
		return 0, invalid("limit must be 1-%d", admin.MaxLimit)
	}
	return n, nil
}

func parseTime(field, q string) (time.Time, error) {
	if q == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, q)
	if err != nil {
		return time.Time{}, invalid("%s must be an RFC 3339 time, e.g. 2026-01-02T15:04:05Z", field)
	}
	return t, nil
}

func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := admin.EventFilter{Status: q.Get("status"), Confirmation: q.Get("confirmation"), EventType: q.Get("type"),
		Provider: q.Get("provider"), ApplicationID: q.Get("application"), After: q.Get("after")}
	var err error
	if v := q.Get("held"); v != "" {
		if f.Held, err = strconv.ParseBool(v); err != nil {
			h.fail(w, r, invalid("held must be true or false"))
			return
		}
	}
	if f.Limit, err = parseLimit(q.Get("limit")); err != nil {
		h.fail(w, r, err)
		return
	}
	if f.Since, err = parseTime("since", q.Get("since")); err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.svc.ListEvents(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, page)
}

func (h *Handler) getEvent(w http.ResponseWriter, r *http.Request) {
	include := false
	if v := r.URL.Query().Get("include_payload"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			h.fail(w, r, invalid("include_payload must be true or false"))
			return
		}
		include = b
	}
	e, err := h.svc.GetEvent(r.Context(), r.PathValue("id"), include)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if include {
		h.logger.Info("event payload read through the admin API", "event_id", e.ID, "remote_addr", r.RemoteAddr)
	}
	apierr.WriteJSON(w, http.StatusOK, e)
}

type rerouteRequest struct {
	Application string `json:"application"`
	Reason      string `json:"reason"`
}

func (h *Handler) reroute(w http.ResponseWriter, r *http.Request) {
	var req rerouteRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.Reroute(r.Context(), r.PathValue("id"), req.Application, req.Reason, ViaAPI)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, res)
}

type dismissRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) dismiss(w http.ResponseWriter, r *http.Request) {
	var req dismissRequest
	if !decode(w, r, &req) {
		return
	}
	e, err := h.svc.Dismiss(r.Context(), r.PathValue("id"), req.Reason, ViaAPI)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, e)
}

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.RetryConfirmation(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]string{"id": id, "confirmation": admin.ConfirmationPending})
}

func (h *Handler) reprocess(w http.ResponseWriter, r *http.Request) {
	var req admin.ReprocessRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.EventIDs) > maxReprocessIDs {
		h.fail(w, r, invalid("at most %d event_ids per request", maxReprocessIDs))
		return
	}
	res, err := h.svc.Reprocess(r.Context(), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, res)
}

func (h *Handler) listDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := admin.DeliveryFilter{Status: q.Get("status"), ApplicationID: q.Get("application"), EventID: q.Get("event")}
	var err error
	if f.Limit, err = parseLimit(q.Get("limit")); err != nil {
		h.fail(w, r, err)
		return
	}
	dels, err := h.svc.ListDeliveries(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, struct {
		Data []admin.Delivery `json:"data"`
	}{dels})
}

func (h *Handler) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.RetryDelivery(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": admin.DeliveryPending})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Stats(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	apierr.WriteJSON(w, http.StatusOK, st)
}

// decode reads one JSON object with unknown fields rejected. An empty body
// decodes as {}.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) {
		return true
	}
	if err != nil {
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
