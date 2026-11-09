package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/khaleelhabeeb/relay/internal/routing"
)

// ErrNotFound is returned by single-row reads that match nothing.
var ErrNotFound = errors.New("store: not found")

type Event struct {
	ID               string          `json:"id"`
	Provider         string          `json:"provider"`
	EventType        string          `json:"event_type"`
	DedupKey         string          `json:"dedup_key"`
	RawPayload       []byte          `json:"-"`
	PayloadSHA256    string          `json:"payload_sha256"`
	Reference        string          `json:"reference"`
	DedicatedAccount string          `json:"dedicated_account"`
	ReceivedAt       time.Time       `json:"received_at"`
	LastSeenAt       time.Time       `json:"last_seen_at"`
	DuplicateCount   int             `json:"duplicate_count"`
	RoutingStatus    routing.Outcome `json:"routing_status"`
	ApplicationID    string          `json:"application_id"`
	Confirmation     Confirmation    `json:"confirmation"`
	ConfirmAttempts  int             `json:"confirm_attempts"`
	ConfirmNextAt    time.Time       `json:"confirm_next_at,omitzero"`
	ConfirmError     string          `json:"confirm_error,omitempty"`
}

type RoutingDecision struct {
	ID            string              `json:"id"`
	EventID       string              `json:"event_id"`
	Outcome       routing.Outcome     `json:"outcome"`
	ApplicationID string              `json:"application_id"`
	Candidates    []routing.Candidate `json:"candidates"`
	Reason        string              `json:"reason"`
	DecidedBy     string              `json:"decided_by"`
	DecidedAt     time.Time           `json:"decided_at"`
}

// Who made a routing decision.
const (
	DecidedByIngest       = "ingest"
	DecidedByReprocess    = "reprocess"
	DecidedByRegistration = "registration"
	DecidedByOperator     = "operator"
)

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryInFlight  DeliveryStatus = "in_flight"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryExhausted DeliveryStatus = "exhausted"
)

type Delivery struct {
	ID            string         `json:"id"`
	EventID       string         `json:"event_id"`
	ApplicationID string         `json:"application_id"`
	Status        DeliveryStatus `json:"status"`
	AttemptCount  int            `json:"attempt_count"`
	NextAttemptAt time.Time      `json:"next_attempt_at"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// Tx is a write transaction.
type Tx struct {
	tx *sql.Tx
}

// EventIDByDedupKey returns the ID of an already-stored event.
func (t *Tx) EventIDByDedupKey(ctx context.Context, provider, key string) (string, bool, error) {
	var id string
	err := t.tx.QueryRowContext(ctx, `SELECT id FROM events WHERE provider = ? AND dedup_key = ?`, provider, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// StoredIdentity is the stored event a duplicate webhook is compared with.
type StoredIdentity struct {
	ID               string
	Reference        string
	DedicatedAccount string
	PayloadSHA256    string
	Confirmation     Confirmation
}

// IdentityByDedupKey returns the already-stored event with key, if any.
func (t *Tx) IdentityByDedupKey(ctx context.Context, provider, key string) (StoredIdentity, bool, error) {
	var (
		s        StoredIdentity
		ref, dva sql.NullString
	)
	err := t.tx.QueryRowContext(ctx, `SELECT id, reference, dedicated_account, payload_sha256, confirmation
		FROM events WHERE provider = ? AND dedup_key = ?`,
		provider, key).Scan(&s.ID, &ref, &dva, &s.PayloadSHA256, &s.Confirmation)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredIdentity{}, false, nil
	}
	s.Reference, s.DedicatedAccount = ref.String, dva.String
	return s, err == nil, err
}

// DedupConflict is a webhook that shares an event's dedup key but not its
// ownership identifiers.
type DedupConflict struct {
	ID               string    `json:"id"`
	EventID          string    `json:"event_id"`
	Reference        string    `json:"reference,omitempty"`
	DedicatedAccount string    `json:"dedicated_account,omitempty"`
	RawPayload       []byte    `json:"-"`
	PayloadSHA256    string    `json:"payload_sha256"`
	ReceivedAt       time.Time `json:"received_at"`
}

// RecordDedupConflict keeps a conflicting duplicate for operator review.
func (t *Tx) RecordDedupConflict(ctx context.Context, c *DedupConflict) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO dedup_conflicts
		(id, event_id, reference, dedicated_account, raw_payload, payload_sha256, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.EventID, nullString(c.Reference), nullString(c.DedicatedAccount), c.RawPayload, c.PayloadSHA256, ms(c.ReceivedAt))
	return err
}

// DedupConflictsForEvent lists an event's conflicting duplicates, oldest first.
func (s *Store) DedupConflictsForEvent(ctx context.Context, eventID string) ([]DedupConflict, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, event_id, reference, dedicated_account, raw_payload, payload_sha256, received_at
		FROM dedup_conflicts WHERE event_id = ? ORDER BY received_at, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DedupConflict
	for rows.Next() {
		var (
			c        DedupConflict
			ref, dva sql.NullString
			received int64
		)
		if err := rows.Scan(&c.ID, &c.EventID, &ref, &dva, &c.RawPayload, &c.PayloadSHA256, &received); err != nil {
			return nil, err
		}
		c.Reference, c.DedicatedAccount, c.ReceivedAt = ref.String, dva.String, fromMS(received)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordDuplicate counts a repeated webhook for an existing event.
func (t *Tx) RecordDuplicate(ctx context.Context, eventID string, seenAt time.Time) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE events SET duplicate_count = duplicate_count + 1, last_seen_at = ? WHERE id = ?`, ms(seenAt), eventID)
	return err
}

func (t *Tx) InsertEvent(ctx context.Context, e *Event) error {
	conf := e.Confirmation
	if conf == "" {
		conf = ConfirmNone
	}
	var next sql.NullInt64
	if !e.ConfirmNextAt.IsZero() {
		next = sql.NullInt64{Int64: ms(e.ConfirmNextAt), Valid: true}
	}
	_, err := t.tx.ExecContext(ctx, `INSERT INTO events
		(id, provider, event_type, dedup_key, raw_payload, payload_sha256, reference, dedicated_account,
		 received_at, last_seen_at, routing_status, application_id,
		 confirmation, confirm_attempts, confirm_next_at, confirm_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Provider, e.EventType, e.DedupKey, e.RawPayload, e.PayloadSHA256,
		nullString(e.Reference), nullString(e.DedicatedAccount),
		ms(e.ReceivedAt), ms(e.ReceivedAt), string(e.RoutingStatus), nullString(e.ApplicationID),
		string(conf), e.ConfirmAttempts, next, nullString(truncate(e.ConfirmError, 500)))
	return err
}

func (t *Tx) InsertRoutingDecision(ctx context.Context, d *RoutingDecision) error {
	cands := d.Candidates
	if cands == nil {
		cands = []routing.Candidate{}
	}
	cj, err := json.Marshal(cands)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx, `INSERT INTO routing_decisions
		(id, event_id, outcome, application_id, candidates, reason, decided_by, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EventID, string(d.Outcome), nullString(d.ApplicationID), string(cj), d.Reason, d.DecidedBy, ms(d.DecidedAt))
	return err
}

func (t *Tx) InsertDelivery(ctx context.Context, d *Delivery) error {
	_, err := t.tx.ExecContext(ctx, `INSERT INTO deliveries
		(id, event_id, application_id, status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EventID, d.ApplicationID, string(d.Status), ms(d.NextAttemptAt), ms(d.CreatedAt), ms(d.CreatedAt))
	return err
}

// RegistrationLookup returns a lookup over provider's registrations that
// reads inside this transaction, so routing sees a consistent snapshot.
func (t *Tx) RegistrationLookup(provider string) routing.RegistrationLookup {
	return txLookup{tx: t.tx, provider: provider}
}

type txLookup struct {
	tx       *sql.Tx
	provider string
}

func (l txLookup) Registrations(ctx context.Context, identifierType, value string) ([]routing.Registration, error) {
	rows, err := l.tx.QueryContext(ctx, `SELECT id, application_id, active, created_at, deactivated_at
		FROM registrations WHERE provider = ? AND identifier_type = ? AND identifier_value = ?
		ORDER BY created_at`, l.provider, identifierType, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routing.Registration
	for rows.Next() {
		var (
			r           routing.Registration
			created     int64
			deactivated sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.Application, &r.Active, &created, &deactivated); err != nil {
			return nil, err
		}
		r.CreatedAt = fromMS(created)
		if deactivated.Valid {
			r.DeactivatedAt = fromMS(deactivated.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const eventColumns = `id, provider, event_type, dedup_key, raw_payload, payload_sha256,
	reference, dedicated_account, received_at, last_seen_at, duplicate_count, routing_status, application_id,
	confirmation, confirm_attempts, confirm_next_at, confirm_error`

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var (
		e                   Event
		ref, dva, app, cerr sql.NullString
		received, lastSeen  int64
		next                sql.NullInt64
		status              string
	)
	err := row.Scan(&e.ID, &e.Provider, &e.EventType, &e.DedupKey, &e.RawPayload, &e.PayloadSHA256,
		&ref, &dva, &received, &lastSeen, &e.DuplicateCount, &status, &app,
		&e.Confirmation, &e.ConfirmAttempts, &next, &cerr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.Reference, e.DedicatedAccount, e.ApplicationID = ref.String, dva.String, app.String
	e.ReceivedAt, e.LastSeenAt = fromMS(received), fromMS(lastSeen)
	e.RoutingStatus = routing.Outcome(status)
	e.ConfirmError = cerr.String
	if next.Valid {
		e.ConfirmNextAt = fromMS(next.Int64)
	}
	return &e, nil
}

// GetEvent reads one event by ID.
func (s *Store) GetEvent(ctx context.Context, id string) (*Event, error) {
	return scanEvent(s.r.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
}

// DeliveriesForEvent lists an event's deliveries.
func (s *Store) DeliveriesForEvent(ctx context.Context, eventID string) ([]Delivery, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, event_id, application_id, status, attempt_count,
		next_attempt_at, created_at, updated_at FROM deliveries WHERE event_id = ? ORDER BY created_at`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var (
			d                      Delivery
			status                 string
			next, created, updated int64
		)
		if err := rows.Scan(&d.ID, &d.EventID, &d.ApplicationID, &status, &d.AttemptCount, &next, &created, &updated); err != nil {
			return nil, err
		}
		d.Status = DeliveryStatus(status)
		d.NextAttemptAt, d.CreatedAt, d.UpdatedAt = fromMS(next), fromMS(created), fromMS(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RoutingDecisionsForEvent lists an event's routing history, oldest first.
func (s *Store) RoutingDecisionsForEvent(ctx context.Context, eventID string) ([]RoutingDecision, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, event_id, outcome, application_id, candidates, reason,
		decided_by, decided_at FROM routing_decisions WHERE event_id = ? ORDER BY decided_at, id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutingDecision
	for rows.Next() {
		var (
			d       RoutingDecision
			outcome string
			app     sql.NullString
			cj      string
			decided int64
		)
		if err := rows.Scan(&d.ID, &d.EventID, &outcome, &app, &cj, &d.Reason, &d.DecidedBy, &decided); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cj), &d.Candidates); err != nil {
			return nil, err
		}
		d.Outcome, d.ApplicationID, d.DecidedAt = routing.Outcome(outcome), app.String, fromMS(decided)
		out = append(out, d)
	}
	return out, rows.Err()
}
