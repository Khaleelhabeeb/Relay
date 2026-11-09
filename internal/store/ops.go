package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventFilter narrows ListEvents. Zero values mean "any".
type EventFilter struct {
	Status       string
	Confirmation string
	EventType    string
	Since        time.Time
	Limit        int
}

// EventSummary is an event without its payload.
type EventSummary struct {
	ID             string    `json:"id"`
	EventType      string    `json:"event_type"`
	RoutingStatus  string    `json:"routing_status"`
	ApplicationID  string    `json:"application_id"`
	Reference      string    `json:"reference"`
	ReceivedAt     time.Time `json:"received_at"`
	DuplicateCount int       `json:"duplicate_count"`
	PayloadBytes   int       `json:"payload_bytes"`
	Confirmation   string    `json:"confirmation"`
}

// ListEvents returns events, newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]EventSummary, error) {
	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where, args = append(where, "routing_status = ?"), append(args, f.Status)
	}
	if f.Confirmation != "" {
		where, args = append(where, "confirmation = ?"), append(args, f.Confirmation)
	}
	if f.EventType != "" {
		where, args = append(where, "event_type = ?"), append(args, f.EventType)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "received_at >= ?"), append(args, ms(f.Since))
	}
	q := `SELECT id, event_type, routing_status, application_id, reference, received_at, duplicate_count,
		COALESCE(LENGTH(raw_payload), 0), confirmation FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // where holds only constant "col = ?" fragments
	}
	q += " ORDER BY received_at DESC, id DESC LIMIT ?"
	args = append(args, limitOr(f.Limit, 50))

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventSummary
	for rows.Next() {
		var (
			e        EventSummary
			app, ref sql.NullString
			received int64
		)
		if err := rows.Scan(&e.ID, &e.EventType, &e.RoutingStatus, &app, &ref, &received, &e.DuplicateCount, &e.PayloadBytes, &e.Confirmation); err != nil {
			return nil, err
		}
		e.ApplicationID, e.Reference, e.ReceivedAt = app.String, ref.String, fromMS(received)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeliveryFilter narrows ListDeliveries.
type DeliveryFilter struct {
	Status      string
	Application string
	Limit       int
}

// DeliverySummary is a delivery with its last error.
type DeliverySummary struct {
	Delivery
	LastError string `json:"last_error"`
}

// ListDeliveries returns deliveries, most recently updated first.
func (s *Store) ListDeliveries(ctx context.Context, f DeliveryFilter) ([]DeliverySummary, error) {
	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.Application != "" {
		where, args = append(where, "application_id = ?"), append(args, f.Application)
	}
	q := `SELECT id, event_id, application_id, status, attempt_count, next_attempt_at, created_at, updated_at,
		last_error FROM deliveries`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ") //nolint:gosec // where holds only constant "col = ?" fragments
	}
	q += " ORDER BY updated_at DESC, id DESC LIMIT ?"
	args = append(args, limitOr(f.Limit, 50))

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliverySummary
	for rows.Next() {
		var (
			d                      DeliverySummary
			status                 string
			next, created, updated int64
			lastErr                sql.NullString
		)
		if err := rows.Scan(&d.ID, &d.EventID, &d.ApplicationID, &status, &d.AttemptCount, &next, &created, &updated, &lastErr); err != nil {
			return nil, err
		}
		d.Status, d.LastError = DeliveryStatus(status), lastErr.String
		d.NextAttemptAt, d.CreatedAt, d.UpdatedAt = fromMS(next), fromMS(created), fromMS(updated)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrNotRetryable means the delivery is not exhausted.
var ErrNotRetryable = errors.New("store: only exhausted deliveries can be retried")

// RetryDelivery gives an exhausted delivery a fresh attempt budget, due
// now, keeping its owner and attempt history.
func (s *Store) RetryDelivery(ctx context.Context, id string, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM deliveries WHERE id = ?`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if DeliveryStatus(status) != DeliveryExhausted {
			return fmt.Errorf("%w (status is %s)", ErrNotRetryable, status)
		}
		_, err = tx.ExecContext(ctx, `UPDATE deliveries SET status = 'pending', retry_base = attempt_count,
			next_attempt_at = ?, last_error = NULL, updated_at = ? WHERE id = ?`, ms(now), ms(now), id)
		return err
	})
}

// Heartbeat performs a small durable write; readiness uses it to detect a
// full disk or read-only database.
func (s *Store) Heartbeat(ctx context.Context, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO heartbeat (id, at) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET at = excluded.at`, ms(now))
	return err
}

// PurgeResult reports what a retention run removed.
type PurgeResult struct {
	PayloadsCleared int64
	AttemptsDeleted int64
}

// Purge applies the retention policy. Raw payloads are cleared
// for unsupported events and events the provider API rejected received
// before payloadCutoff, and for matched events whose deliveries were all
// delivered before it. Unresolved,
// ambiguous, pending and exhausted events keep their payloads. Finished
// attempt rows older than attemptCutoff are deleted.
func (s *Store) Purge(ctx context.Context, payloadCutoff, attemptCutoff time.Time) (PurgeResult, error) {
	var r PurgeResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE events SET raw_payload = NULL
			WHERE raw_payload IS NOT NULL AND (
				(routing_status = 'unsupported' AND received_at < ?)
				OR (confirmation = 'rejected' AND received_at < ?)
				OR (routing_status = 'matched'
					AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = events.id
						AND (d.status != 'delivered' OR d.delivered_at >= ?))
					AND EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = events.id)))`,
			ms(payloadCutoff), ms(payloadCutoff), ms(payloadCutoff))
		if err != nil {
			return err
		}
		r.PayloadsCleared, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE dedup_conflicts SET raw_payload = NULL
			WHERE raw_payload IS NOT NULL AND received_at < ?`, ms(payloadCutoff)); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM delivery_attempts WHERE finished_at IS NOT NULL AND finished_at < ?`,
			ms(attemptCutoff))
		if err != nil {
			return err
		}
		r.AttemptsDeleted, _ = res.RowsAffected()
		return nil
	})
	return r, err
}

// Stats are point-in-time gauges for metrics.
type Stats struct {
	DeliveriesByStatus map[string]int64
	EventsByStatus     map[string]int64
	OldestPendingAge   time.Duration    // 0 if none pending
	Confirmations      map[string]int64 // events pending or failed API confirmation
}

// Stats reads current queue and routing gauges.
func (s *Store) Stats(ctx context.Context, now time.Time) (Stats, error) {
	st := Stats{DeliveriesByStatus: map[string]int64{}, EventsByStatus: map[string]int64{}, Confirmations: map[string]int64{}}
	count := func(q string, into map[string]int64) error {
		rows, err := s.r.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int64
			if err := rows.Scan(&k, &n); err != nil {
				return err
			}
			into[k] = n
		}
		return rows.Err()
	}
	if err := count(`SELECT status, COUNT(*) FROM deliveries GROUP BY status`, st.DeliveriesByStatus); err != nil {
		return st, err
	}
	if err := count(`SELECT routing_status, COUNT(*) FROM events WHERE routing_status IN ('unresolved', 'ambiguous') GROUP BY routing_status`, st.EventsByStatus); err != nil {
		return st, err
	}
	if err := count(`SELECT confirmation, COUNT(*) FROM events WHERE confirmation IN ('pending', 'failed') GROUP BY confirmation`, st.Confirmations); err != nil {
		return st, err
	}
	var oldest sql.NullInt64
	if err := s.r.QueryRowContext(ctx, `SELECT MIN(created_at) FROM deliveries WHERE status IN ('pending', 'in_flight')`).Scan(&oldest); err != nil {
		return st, err
	}
	if oldest.Valid {
		st.OldestPendingAge = now.Sub(fromMS(oldest.Int64))
	}
	return st, nil
}

func limitOr(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// EventIDsByStatus lists IDs of events with a routing status received at or
// after since (zero: any time), oldest first.
func (s *Store) EventIDsByStatus(ctx context.Context, status string, since time.Time) ([]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id FROM events WHERE routing_status = ? AND received_at >= ?
		AND confirmation != 'rejected' ORDER BY received_at`, status, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
