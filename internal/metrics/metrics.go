// Package metrics defines Relay's Prometheus metrics. All methods are safe
// on a nil *Metrics, so components work unchanged when metrics are off.
package metrics

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/khaleelhabeeb/relay/internal/store"
)

type Metrics struct {
	registry        *prometheus.Registry
	webhooks        *prometheus.CounterVec
	acceptLatency   prometheus.Histogram
	routed          *prometheus.CounterVec
	attempts        *prometheus.CounterVec
	attemptDuration prometheus.Histogram
	exhausted       *prometheus.CounterVec
	confirmations   *prometheus.CounterVec
	alerts          *prometheus.CounterVec
}

// New registers Relay's metrics. st, if non-nil, backs scrape-time gauges
// (queue depth, unrouted events, oldest pending delivery).
func New(st *store.Store) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_webhooks_received_total",
			Help: "Inbound webhooks by provider and result (accepted, duplicate, dedup_conflict, invalid_signature, invalid_payload, payload_too_large, not_confirmed, confirm_pending, storage_error, server_busy, forbidden_source).",
		}, []string{"provider", "result"}),
		acceptLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "relay_webhook_accept_duration_seconds",
			Help:    "Time from request start to durable acceptance (or refusal) of a verified webhook.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		routed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_events_routed_total",
			Help: "Routing outcomes for newly stored events and reprocessing (matched, unresolved, ambiguous, unsupported).",
		}, []string{"outcome"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_delivery_attempts_total",
			Help: "Delivery attempts by application and result (success or the error code).",
		}, []string{"application", "result"}),
		attemptDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "relay_delivery_attempt_duration_seconds",
			Help:    "Duration of delivery attempts.",
			Buckets: prometheus.DefBuckets,
		}),
		exhausted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_deliveries_exhausted_total",
			Help: "Deliveries that used their whole retry budget and need operator replay.",
		}, []string{"application"}),
		confirmations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_confirmations_total",
			Help: "Provider API confirmations finished in the background, by provider and result (deferred: the API failed at ingest and the event waits; confirmed; rejected; retry; failed: retry budget spent).",
		}, []string{"provider", "result"}),
		alerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_alert_digests_total",
			Help: "Operator alert digests by sink and result (sent, failed).",
		}, []string{"sink", "result"}),
	}
	m.registry.MustRegister(m.webhooks, m.acceptLatency, m.routed, m.attempts, m.attemptDuration, m.exhausted, m.confirmations, m.alerts,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if st != nil {
		m.registry.MustRegister(&storeCollector{store: st})
	}
	return m
}

// Handler serves the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) Webhook(provider, result string) {
	if m != nil {
		m.webhooks.WithLabelValues(provider, result).Inc()
	}
}

func (m *Metrics) AcceptLatency(d time.Duration) {
	if m != nil {
		m.acceptLatency.Observe(d.Seconds())
	}
}

func (m *Metrics) Routed(outcome string) {
	if m != nil {
		m.routed.WithLabelValues(outcome).Inc()
	}
}

// AlertDigest counts an alert digest sent to sink, or failed.
func (m *Metrics) AlertDigest(sink string, ok bool) {
	if m == nil {
		return
	}
	result := "sent"
	if !ok {
		result = "failed"
	}
	m.alerts.WithLabelValues(sink, result).Inc()
}

// Confirmation counts a provider API confirmation result.
func (m *Metrics) Confirmation(provider, result string) {
	if m != nil {
		m.confirmations.WithLabelValues(provider, result).Inc()
	}
}

// Attempt records a delivery attempt; errorCode "" means success.
func (m *Metrics) Attempt(application, errorCode string, d time.Duration, exhausted bool) {
	if m == nil {
		return
	}
	result := errorCode
	if result == "" {
		result = "success"
	}
	m.attempts.WithLabelValues(application, result).Inc()
	m.attemptDuration.Observe(d.Seconds())
	if exhausted {
		m.exhausted.WithLabelValues(application).Inc()
	}
}

// storeCollector reads gauges from the database at scrape time.
type storeCollector struct {
	store *store.Store
}

var (
	descDeliveries = prometheus.NewDesc("relay_deliveries", "Deliveries by status.", []string{"status"}, nil)
	descUnrouted   = prometheus.NewDesc("relay_events_unrouted", "Events needing operator attention, by routing status.", []string{"status"}, nil)
	descOldest     = prometheus.NewDesc("relay_oldest_pending_delivery_age_seconds", "Age of the oldest pending or in-flight delivery (0 if none).", nil, nil)
	descStoreUp    = prometheus.NewDesc("relay_store_up", "1 if the database gauges could be read.", nil, nil)
	descConfirm    = prometheus.NewDesc("relay_events_awaiting_confirmation", "Events not delivered because provider API confirmation is pending (retrying) or failed (needs relay events confirm).", []string{"state"}, nil)
)

func (c *storeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descDeliveries
	ch <- descUnrouted
	ch <- descOldest
	ch <- descStoreUp
	ch <- descConfirm
}

func (c *storeCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := c.store.Stats(ctx, time.Now())
	if err != nil {
		ch <- prometheus.MustNewConstMetric(descStoreUp, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(descStoreUp, prometheus.GaugeValue, 1)
	for _, s := range []string{"pending", "in_flight", "delivered", "exhausted"} {
		ch <- prometheus.MustNewConstMetric(descDeliveries, prometheus.GaugeValue, float64(st.DeliveriesByStatus[s]), s)
	}
	for _, s := range []string{"unresolved", "ambiguous"} {
		ch <- prometheus.MustNewConstMetric(descUnrouted, prometheus.GaugeValue, float64(st.EventsByStatus[s]), s)
	}
	ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, st.OldestPendingAge.Seconds())
	for _, s := range []string{"pending", "failed"} {
		ch <- prometheus.MustNewConstMetric(descConfirm, prometheus.GaugeValue, float64(st.Confirmations[s]), s)
	}
}
