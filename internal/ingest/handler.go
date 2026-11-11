// Package ingest implements POST /webhooks/<provider>: verify the raw body,
// parse it, derive identifiers and the dedup key, hand it to an Acceptor,
// and acknowledge the provider only once the Acceptor reports success.
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
	"github.com/khaleelhabeeb/relay/internal/store"
	"github.com/khaleelhabeeb/relay/provider"
)

// Accepted is a verified, parsed inbound event.
type Accepted struct {
	Event       *provider.Event
	Identifiers provider.Identifiers
	DedupKey    string
	ReceivedAt  time.Time
	// Confirmation is store.ConfirmConfirmed after a successful API check,
	// store.ConfirmPending when the API could not be reached (the event is
	// stored but not delivered until a background check succeeds), and empty
	// when the adapter does not confirm events.
	Confirmation  store.Confirmation
	ConfirmError  string
	ConfirmNextAt time.Time
}

// Acceptor durably records an accepted event. A nil error means it is safe
// to acknowledge the provider; any error makes the handler answer non-2xx so
// the provider retries.
type Acceptor interface {
	Accept(ctx context.Context, a *Accepted) error
}

// confirmTimeout bounds a provider API confirmation, leaving room within the
// server's write timeout to store the event.
const confirmTimeout = 8 * time.Second

type Handler struct {
	Adapter      provider.Adapter
	MaxBodyBytes int64
	Acceptor     Acceptor // nil: every event is refused with 503
	Logger       *slog.Logger
	Metrics      *metrics.Metrics // optional
	Now          func() time.Time
	IPFilter     *IPFilter // optional source allowlist
	// MaxConcurrent caps webhooks being processed at once (0: unlimited).
	// Excess requests get 503 so the provider retries them later, instead of
	// piling up behind the single database writer. Sem, if set, is used
	// instead, so several handlers can share one budget.
	MaxConcurrent int
	Sem           chan struct{}

	semOnce sync.Once
	sem     chan struct{}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := h.Adapter.Name()
	log := h.Logger.With("component", "ingest", "provider", name, "remote_addr", r.RemoteAddr)

	if ip, ok := h.IPFilter.Allows(r); !ok {
		log.Warn("webhook rejected: source address not allowed", "client_ip", ip.String())
		h.Metrics.Webhook(name, "forbidden_source")
		apierr.Write(w, http.StatusForbidden, apierr.Forbidden, "source address not allowed")
		return
	}
	h.semOnce.Do(func() {
		h.sem = h.Sem
		if h.sem == nil && h.MaxConcurrent > 0 {
			h.sem = make(chan struct{}, h.MaxConcurrent)
		}
	})
	if h.sem != nil {
		select {
		case h.sem <- struct{}{}:
			defer func() { <-h.sem }()
		default:
			log.Warn("webhook rejected: too many concurrent requests", "limit", cap(h.sem))
			h.Metrics.Webhook(name, "server_busy")
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
			h.Metrics.Webhook(name, "payload_too_large")
			apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.PayloadTooLarge, "request body too large")
			return
		}
		log.Warn("webhook rejected: reading body failed", "error", err)
		h.Metrics.Webhook(name, "invalid_payload")
		apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "could not read request body")
		return
	}

	if err := h.Adapter.Verify(r.Header, body); err != nil {
		log.Warn("webhook rejected: signature", "error", err, "body_bytes", len(body))
		h.Metrics.Webhook(name, "invalid_signature")
		apierr.Write(w, http.StatusUnauthorized, apierr.InvalidSignature, "signature verification failed")
		return
	}

	evt, err := h.Adapter.Parse(body)
	if err != nil {
		log.Error("signed webhook could not be parsed", "error", err, "body_bytes", len(body))
		h.Metrics.Webhook(name, "invalid_payload")
		apierr.Write(w, http.StatusBadRequest, apierr.InvalidPayload, "malformed event")
		return
	}
	ids := h.Adapter.ExtractIdentifiers(evt)
	var (
		confirmation store.Confirmation
		confirmErr   string
		receivedAt   = h.now()
	)
	if c, ok := h.Adapter.(provider.Confirmer); ok && provider.ConfirmEnabled(h.Adapter) && h.Adapter.Routes(evt.Type) {
		cctx, cancel := context.WithTimeout(r.Context(), confirmTimeout)
		err := c.Confirm(cctx, evt, ids)
		cancel()
		switch {
		case errors.Is(err, provider.ErrNotConfirmed):
			// A valid signature the provider's API disowns: forged by a
			// holder of the account secret. Never stored or delivered.
			log.Error("webhook rejected: provider API does not confirm the event", "event_type", evt.Type,
				"reference", ids.Reference, "transaction_id", ids.TransactionID, "error", err)
			h.Metrics.Webhook(name, "not_confirmed")
			apierr.Write(w, http.StatusUnprocessableEntity, apierr.NotConfirmed, "event does not match the provider's record")
			return
		case err != nil:
			// The API is unreachable or slow: store the event now, deliver
			// nothing until the background confirmer gets an answer. Asking
			// the provider to retry instead would lose the event once its
			// (few) retries ran out.
			log.Warn("provider API confirmation failed; storing the event and confirming in the background",
				"event_type", evt.Type, "error", err)
			confirmation, confirmErr = store.ConfirmPending, err.Error()
		default:
			confirmation = store.ConfirmConfirmed
		}
	}
	a := &Accepted{Event: evt, Identifiers: ids, DedupKey: h.Adapter.DedupKey(evt, ids), ReceivedAt: receivedAt,
		Confirmation: confirmation, ConfirmError: confirmErr}
	if confirmation == store.ConfirmPending {
		// Due at once: the background check allows a slow API far longer
		// than the ingest check could.
		a.ConfirmNextAt = receivedAt
	}
	log = log.With("event_type", evt.Type, "dedup_key", a.DedupKey)

	defer func() { h.Metrics.AcceptLatency(time.Since(start)) }()
	if h.Acceptor == nil {
		log.Error("webhook refused: no durable storage configured")
		h.Metrics.Webhook(name, "storage_error")
		apierr.Write(w, http.StatusServiceUnavailable, apierr.StorageUnavailable, "event could not be stored")
		return
	}
	if err := h.Acceptor.Accept(r.Context(), a); errors.Is(err, ErrConfirmationPending) {
		log.Warn("webhook refused: an earlier copy with different bytes awaits provider API confirmation")
		h.Metrics.Webhook(name, "confirm_pending")
		w.Header().Set("Retry-After", "60")
		apierr.Write(w, http.StatusServiceUnavailable, apierr.ConfirmUnavailable, "an earlier copy of this event is awaiting confirmation, retry later")
		return
	} else if err != nil {
		log.Error("webhook refused: accept failed", "error", err)
		h.Metrics.Webhook(name, "storage_error")
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
