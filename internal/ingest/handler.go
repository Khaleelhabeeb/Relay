// Package ingest implements POST /webhooks/paystack: verify the raw body,
// parse it, derive identifiers and the dedup key, hand it to an Acceptor,
// and acknowledge Paystack only once the Acceptor reports success.
package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/khaleelhabeeb/relay/internal/apierr"
	"github.com/khaleelhabeeb/relay/internal/metrics"
	"github.com/khaleelhabeeb/relay/internal/paystack"
)

// Accepted is a verified, parsed inbound event.
type Accepted struct {
	Event       *paystack.Event
	Identifiers paystack.Identifiers
	DedupKey    string
	ReceivedAt  time.Time
}

// Acceptor durably records an accepted event. A nil error means it is safe
// to acknowledge Paystack; any error makes the handler answer non-2xx so
// Paystack retries.
type Acceptor interface {
	Accept(ctx context.Context, a *Accepted) error
}

type Handler struct {
	Secret       string
	MaxBodyBytes int64
	Acceptor     Acceptor // nil: every event is refused with 503
	Logger       *slog.Logger
	Metrics      *metrics.Metrics // optional
	Now          func() time.Time
	IPFilter     *IPFilter // optional source allowlist
	// MaxConcurrent caps webhooks being processed at once (0: unlimited).
	// Excess requests get 503 so Paystack retries them later, instead of
	// piling up behind the single database writer.
	MaxConcurrent int

	semOnce sync.Once
	sem     chan struct{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log := h.Logger.With("component", "ingest", "remote_addr", r.RemoteAddr)

	if ip, ok := h.IPFilter.Allows(r); !ok {
		log.Warn("webhook rejected: source address not allowed", "client_ip", ip.String())
		h.Metrics.Webhook("forbidden_source")
		apierr.Write(w, http.StatusForbidden, apierr.Forbidden, "source address not allowed")
		return
	}
	if h.MaxConcurrent > 0 {
		h.semOnce.Do(func() { h.sem = make(chan struct{}, h.MaxConcurrent) })
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		default:
			log.Warn("webhook rejected: too many concurrent requests", "limit", h.MaxConcurrent)
			h.Metrics.Webhook("server_busy")
			w.Header().Set("Retry-After", "5")
			apierr.Write(w, http.StatusServiceUnavailable, apierr.ServerBusy, "server busy, retry later")
			return
		}
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.MaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			log.Warn("webhook rejected: body too large", "limit_bytes", h.MaxBodyBytes)
			h.Metrics.Webhook("payload_too_large")
			apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.PayloadTooLarge, "request body too large")
			return
		}
		log.Warn("webhook rejected: reading body failed", "error", err)
		h.Metrics.Webhook("invalid_payload")
		apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "could not read request body")
		return
	}

	if err := paystack.Verify(h.Secret, body, r.Header.Get(paystack.SignatureHeader)); err != nil {
		log.Warn("webhook rejected: signature", "error", err, "body_bytes", len(body))
		h.Metrics.Webhook("invalid_signature")
		apierr.Write(w, http.StatusUnauthorized, apierr.InvalidSignature, "signature verification failed")
		return
	}

	evt, err := paystack.Parse(body)
	if err != nil {
		log.Error("signed webhook could not be parsed", "error", err, "body_bytes", len(body))
		h.Metrics.Webhook("invalid_payload")
		apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "malformed event")
		return
	}
	ids := paystack.ExtractIdentifiers(evt)
	a := &Accepted{Event: evt, Identifiers: ids, DedupKey: paystack.DedupKey(evt, ids), ReceivedAt: h.now()}
	log = log.With("event_type", evt.Type, "dedup_key", a.DedupKey)

	defer func() { h.Metrics.AcceptLatency(time.Since(start)) }()
	if h.Acceptor == nil {
		log.Error("webhook refused: no durable storage configured")
		h.Metrics.Webhook("storage_error")
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "event could not be stored")
		return
	}
	if err := h.Acceptor.Accept(r.Context(), a); err != nil {
		log.Error("webhook refused: accept failed", "error", err)
		h.Metrics.Webhook("storage_error")
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "event could not be stored")
		return
	}
	log.Info("webhook accepted")
	apierr.WriteJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}
