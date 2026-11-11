// Package admin defines the records and requests of Relay's operator
// interface. The same types are returned by the operator methods on
// relay.Relay, encoded by the HTTP admin API (/v1/admin) and printed by the
// relay CLI's --json output, so a record looks the same however it is read.
//
// Raw payment payloads are never included unless explicitly asked for.
//
// The API is pre-1.0: it may change between minor versions, and the
// CHANGELOG records every change.
package admin

import (
	"encoding/json"
	"time"
)

// Routing statuses of an event. Unresolved and ambiguous events are held:
// nothing is delivered until a registration, a new route or an operator
// decides their owner, or an operator dismisses them (Event.DismissedAt).
const (
	StatusMatched     = "matched"
	StatusUnresolved  = "unresolved"
	StatusAmbiguous   = "ambiguous"
	StatusUnsupported = "unsupported"
)

// Provider API confirmation states of an event.
const (
	ConfirmationNone      = "none"      // not required
	ConfirmationConfirmed = "confirmed" // the provider API vouched for it
	ConfirmationPending   = "pending"   // being retried in the background
	ConfirmationRejected  = "rejected"  // the provider API disowned it; never delivered
	ConfirmationFailed    = "failed"    // retry window closed; retry it to deliver
)

// Delivery statuses.
const (
	DeliveryPending   = "pending"
	DeliveryInFlight  = "in_flight"
	DeliveryDelivered = "delivered"
	DeliveryExhausted = "exhausted" // retries used up; retry it to deliver
)

// Event is a received webhook without its payload.
type Event struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	EventType        string    `json:"event_type"`
	RoutingStatus    string    `json:"routing_status"`
	ApplicationID    string    `json:"application_id,omitempty"`
	Reference        string    `json:"reference,omitempty"`
	DedicatedAccount string    `json:"dedicated_account,omitempty"`
	ReceivedAt       time.Time `json:"received_at"`
	DuplicateCount   int       `json:"duplicate_count"`
	// PayloadBytes is 0 once retention has cleared the payload.
	PayloadBytes int    `json:"payload_bytes"`
	Confirmation string `json:"confirmation"`
	// DismissedAt is set when the event was closed without an owner, by an
	// operator or by retention.held_days. It keeps its routing status but
	// is no longer held; rerouting it clears the dismissal.
	DismissedAt time.Time `json:"dismissed_at,omitzero"`
}

// EventDetail is an event with its full history.
type EventDetail struct {
	Event
	DedupKey        string    `json:"dedup_key"`
	PayloadSHA256   string    `json:"payload_sha256"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	ConfirmAttempts int       `json:"confirm_attempts"`
	ConfirmNextAt   time.Time `json:"confirm_next_at,omitzero"`
	ConfirmError    string    `json:"confirm_error,omitempty"`
	// DismissedBy is "operator via <channel>" or "retention".
	DismissedBy   string `json:"dismissed_by,omitempty"`
	DismissReason string `json:"dismiss_reason,omitempty"`

	RoutingDecisions []RoutingDecision `json:"routing_decisions"`
	Deliveries       []Delivery        `json:"deliveries"`
	// DedupConflicts are webhooks that shared this event's dedup key but
	// carried different ownership identifiers: possible forgeries.
	DedupConflicts []DedupConflict `json:"dedup_conflicts"`

	// Payload is the raw webhook body, present only when asked for and not
	// yet cleared by retention. It is sensitive payment data.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// RoutingDecision is one entry in an event's routing history.
type RoutingDecision struct {
	Outcome       string      `json:"outcome"`
	ApplicationID string      `json:"application_id,omitempty"`
	Candidates    []Candidate `json:"candidates"`
	Reason        string      `json:"reason"`
	// DecidedBy is ingest, reprocess, registration or operator.
	DecidedBy string    `json:"decided_by"`
	DecidedAt time.Time `json:"decided_at"`
}

// Candidate is evidence that an application owns an event.
type Candidate struct {
	Application string `json:"application"`
	Source      string `json:"source"`
	Detail      string `json:"detail"`
}

// Delivery is an event's delivery to its owner.
type Delivery struct {
	ID            string    `json:"id"`
	EventID       string    `json:"event_id"`
	ApplicationID string    `json:"application_id"`
	Status        string    `json:"status"`
	AttemptCount  int       `json:"attempt_count"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastError     string    `json:"last_error,omitempty"`
	// Attempts is filled in EventDetail only.
	Attempts []Attempt `json:"attempts,omitempty"`
}

// Attempt is one try at a delivery.
type Attempt struct {
	AttemptNo  int       `json:"attempt_no"`
	StartedAt  time.Time `json:"started_at"`
	Finished   bool      `json:"finished"`
	HTTPStatus int       `json:"http_status,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

// DedupConflict is a webhook that shared an event's dedup key but not its
// ownership identifiers.
type DedupConflict struct {
	ID               string          `json:"id"`
	Reference        string          `json:"reference,omitempty"`
	DedicatedAccount string          `json:"dedicated_account,omitempty"`
	PayloadSHA256    string          `json:"payload_sha256"`
	ReceivedAt       time.Time       `json:"received_at"`
	Payload          json.RawMessage `json:"payload,omitempty"`
}

// MaxLimit is the most rows one list call returns.
const MaxLimit = 500

// EventFilter narrows a list of events. Zero values mean "any".
type EventFilter struct {
	Status        string // a Status constant
	Confirmation  string // a Confirmation constant
	EventType     string // e.g. charge.success
	Provider      string // e.g. paystack
	ApplicationID string // owning application
	// Held selects only events that need an owner decided: unresolved or
	// ambiguous, not dismissed and not rejected by the provider API.
	Held  bool
	Since time.Time // received at or after
	Limit int       // default 50, at most MaxLimit
	// After continues a list: pass the previous page's Next.
	After string
}

// EventPage is one page of events, newest first.
type EventPage struct {
	Events []Event `json:"data"`
	// Next is set when more events match; pass it as EventFilter.After.
	Next string `json:"next,omitempty"`
}

// DeliveryFilter narrows a list of deliveries. Zero values mean "any".
type DeliveryFilter struct {
	Status        string // a Delivery constant
	ApplicationID string
	EventID       string
	Limit         int // default 50, at most MaxLimit
}

// RerouteResult is an event after an operator assigned its owner.
type RerouteResult struct {
	Event Event `json:"event"`
	// AwaitingConfirmation means delivery waits for the provider API to
	// confirm the event; otherwise it is queued now.
	AwaitingConfirmation bool `json:"awaiting_confirmation"`
}

// ReprocessRequest selects unresolved events to route again with the
// current rules and registrations. With EventIDs, only those; otherwise
// every unresolved event received at or after Since (zero: all).
type ReprocessRequest struct {
	EventIDs []string  `json:"event_ids,omitempty"`
	Since    time.Time `json:"since,omitzero"`
}

// ReprocessResult reports a reprocess. Events that were already routed are
// never changed; they count as considered with their current status.
type ReprocessResult struct {
	Considered int                `json:"considered"`
	Matched    int                `json:"matched"`
	Ambiguous  int                `json:"ambiguous"`
	Unresolved int                `json:"unresolved"`
	Failed     int                `json:"failed"`
	Events     []ReprocessedEvent `json:"events"`
}

// ReprocessedEvent is one event's outcome in a reprocess.
type ReprocessedEvent struct {
	ID     string `json:"id"`
	Status string `json:"routing_status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Alert kinds. Each is raised once, when the change happens.
const (
	AlertUnresolved           = "event.unresolved"      // no owner found
	AlertAmbiguous            = "event.ambiguous"       // conflicting owners
	AlertExpired              = "event.expired"         // dismissed by retention.held_days
	AlertDeliveryExhausted    = "delivery.exhausted"    // retries used up
	AlertConfirmationFailed   = "confirmation.failed"   // the provider API never answered
	AlertConfirmationRejected = "confirmation.rejected" // the provider API disowned a signed webhook: possible forgery
	AlertDedupConflict        = "dedup.conflict"        // a duplicate carried different identifiers: possible forgery
)

// AlertDigestType is AlertDigest.Type.
const AlertDigestType = "relay.alerts"

// MaxDigestAlerts is the most alerts one digest lists; Counts still covers
// every one.
const MaxDigestAlerts = 50

// Alert is one change that needs an operator. It never carries a payload.
type Alert struct {
	Kind          string    `json:"kind"`
	EventID       string    `json:"event_id"`
	DeliveryID    string    `json:"delivery_id,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	EventType     string    `json:"event_type,omitempty"`
	Reference     string    `json:"reference,omitempty"`
	ApplicationID string    `json:"application_id,omitempty"`
	Detail        string    `json:"detail,omitempty"`
	At            time.Time `json:"at"`
}

// AlertDigest is what Relay sends to alerts.url and to an alert handler:
// the alerts raised since the last digest, and the current totals. Alerts
// that no longer hold when the digest is built (e.g. an event rerouted in
// the meantime) are left out.
type AlertDigest struct {
	Type   string    `json:"type"` // AlertDigestType
	ID     string    `json:"id"`
	SentAt time.Time `json:"sent_at"`
	// Reminder marks a periodic summary sent while something still needs
	// attention; it has no new alerts.
	Reminder bool           `json:"reminder"`
	Counts   map[string]int `json:"counts"` // new alerts by kind
	Alerts   []Alert        `json:"alerts"` // the first MaxDigestAlerts
	Omitted  int            `json:"omitted"`
	Stats    Stats          `json:"stats"`
}

// Stats are the counts an operator watches.
type Stats struct {
	Events     EventStats    `json:"events"`
	Deliveries DeliveryStats `json:"deliveries"`
}

// EventStats counts events needing attention.
type EventStats struct {
	Unresolved int64 `json:"unresolved"`
	Ambiguous  int64 `json:"ambiguous"`
	// OldestHeldAt is when the oldest unresolved or ambiguous event
	// arrived; zero if none is held.
	OldestHeldAt        time.Time `json:"oldest_held_at,omitzero"`
	ConfirmationPending int64     `json:"confirmation_pending"`
	ConfirmationFailed  int64     `json:"confirmation_failed"`
}

// DeliveryStats counts deliveries by status.
type DeliveryStats struct {
	Pending   int64 `json:"pending"`
	InFlight  int64 `json:"in_flight"`
	Delivered int64 `json:"delivered"`
	Exhausted int64 `json:"exhausted"`
	// OldestPendingAt is when the oldest undelivered (pending or in-flight)
	// delivery was created; zero if none.
	OldestPendingAt time.Time `json:"oldest_pending_at,omitzero"`
}
