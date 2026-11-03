package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/internal/ids"
)

// ClaimedDelivery is a leased delivery plus what is needed to send it.
type ClaimedDelivery struct {
	ID            string
	EventID       string
	ApplicationID string
	AttemptID     string
	AttemptNo     int // 1-based; already counted in deliveries.attempt_count
	CycleAttempt  int // attempt number since the last manual retry; compare with max_attempts
	Provider      string
	EventType     string
	RawPayload    []byte
	ReceivedAt    time.Time
}

// ClaimDue leases up to limit deliveries that are due (pending with
// next_attempt_at <= now) or whose lease expired, marks them in_flight,
// counts the attempt and opens an attempt row, all in one transaction.
// Counting at claim time means an attempt interrupted by a crash still
// counts toward max_attempts.
func (s *Store) ClaimDue(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]ClaimedDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []ClaimedDelivery
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.id, d.event_id, d.application_id, d.attempt_count, d.retry_base,
				e.provider, e.event_type, e.raw_payload, e.received_at
			FROM deliveries d JOIN events e ON e.id = d.event_id
			WHERE (d.status = 'pending' AND d.next_attempt_at <= ?)
			   OR (d.status = 'in_flight' AND d.lease_until < ?)
			ORDER BY d.next_attempt_at, d.id
			LIMIT ?`, ms(now), ms(now), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				c        ClaimedDelivery
				attempts int
				base     int
				received int64
			)
			if err := rows.Scan(&c.ID, &c.EventID, &c.ApplicationID, &attempts, &base, &c.Provider, &c.EventType, &c.RawPayload, &received); err != nil {
				rows.Close()
				return err
			}
			c.AttemptNo = attempts + 1
			c.CycleAttempt = c.AttemptNo - base
			c.AttemptID = ids.New(ids.Attempt)
			c.ReceivedAt = fromMS(received)
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = 'in_flight', attempt_count = ?,
				lease_owner = ?, lease_until = ?, updated_at = ? WHERE id = ?`,
				c.AttemptNo, owner, ms(leaseUntil), ms(now), c.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_attempts (id, delivery_id, attempt_no, started_at)
				VALUES (?, ?, ?, ?)`, c.AttemptID, c.ID, c.AttemptNo, ms(now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AttemptResult is the outcome of one delivery attempt.
type AttemptResult struct {
	DeliveryID string
	AttemptID  string
	Owner      string
	FinishedAt time.Time
	Duration   time.Duration
	HTTPStatus int    // 0 if no response
	ErrorCode  string // "" on success
	Error      string // short, non-sensitive description

	// Next state of the delivery.
	Status        DeliveryStatus // delivered, pending or exhausted
	NextAttemptAt time.Time      // when Status == pending
}

// ErrLeaseLost means the delivery is no longer leased by this owner, so the
// result was not recorded against it.
var ErrLeaseLost = errors.New("store: delivery lease lost")

// CompleteAttempt records an attempt's outcome and moves the delivery to its
// next state, but only if owner still holds the lease.
func (s *Store) CompleteAttempt(ctx context.Context, r AttemptResult) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var status sql.NullInt64
		if r.HTTPStatus != 0 {
			status = sql.NullInt64{Int64: int64(r.HTTPStatus), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_attempts SET finished_at = ?, http_status = ?,
			error_code = ?, duration_ms = ? WHERE id = ?`,
			ms(r.FinishedAt), status, nullString(r.ErrorCode), r.Duration.Milliseconds(), r.AttemptID); err != nil {
			return err
		}

		var delivered, next sql.NullInt64
		switch r.Status {
		case DeliveryDelivered:
			delivered = sql.NullInt64{Int64: ms(r.FinishedAt), Valid: true}
		case DeliveryPending:
			next = sql.NullInt64{Int64: ms(r.NextAttemptAt), Valid: true}
		}
		res, err := tx.ExecContext(ctx, `UPDATE deliveries SET status = ?, lease_owner = NULL, lease_until = NULL,
				last_error = ?, delivered_at = COALESCE(?, delivered_at),
				next_attempt_at = COALESCE(?, next_attempt_at), updated_at = ?
			WHERE id = ? AND status = 'in_flight' AND lease_owner = ?`,
			string(r.Status), nullString(truncate(r.Error, 500)), delivered, next, ms(r.FinishedAt), r.DeliveryID, r.Owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

// RecoverOrphanedLeases returns every in_flight delivery to pending. Call it
// at startup only: the process lock guarantees no other Relay is running, so
// any lease still held belongs to a process that died mid-attempt.
func (s *Store) RecoverOrphanedLeases(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE deliveries SET status = 'pending', lease_owner = NULL,
		lease_until = NULL, next_attempt_at = ?, updated_at = ? WHERE status = 'in_flight'`, ms(now), ms(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReleaseLeases returns owner's in_flight deliveries to pending, due now.
func (s *Store) ReleaseLeases(ctx context.Context, owner string, now time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, `UPDATE deliveries SET status = 'pending', lease_owner = NULL,
		lease_until = NULL, next_attempt_at = ?, updated_at = ? WHERE status = 'in_flight' AND lease_owner = ?`,
		ms(now), ms(now), owner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Attempt is one row of delivery_attempts.
type Attempt struct {
	ID         string    `json:"id"`
	AttemptNo  int       `json:"attempt_no"`
	StartedAt  time.Time `json:"started_at"`
	Finished   bool      `json:"finished"`
	HTTPStatus int       `json:"http_status"`
	ErrorCode  string    `json:"error_code"`
	DurationMS int64     `json:"duration_ms"`
}

// AttemptsForDelivery lists a delivery's attempts in order.
func (s *Store) AttemptsForDelivery(ctx context.Context, deliveryID string) ([]Attempt, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, attempt_no, started_at, finished_at, http_status, error_code,
		duration_ms FROM delivery_attempts WHERE delivery_id = ? ORDER BY attempt_no`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var (
			a                          Attempt
			started                    int64
			finished, status, duration sql.NullInt64
			code                       sql.NullString
		)
		if err := rows.Scan(&a.ID, &a.AttemptNo, &started, &finished, &status, &code, &duration); err != nil {
			return nil, err
		}
		a.StartedAt, a.Finished = fromMS(started), finished.Valid
		a.HTTPStatus, a.ErrorCode, a.DurationMS = int(status.Int64), code.String, duration.Int64
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetDelivery reads one delivery.
func (s *Store) GetDelivery(ctx context.Context, id string) (*Delivery, error) {
	var (
		d                      Delivery
		status                 string
		next, created, updated int64
	)
	err := s.r.QueryRowContext(ctx, `SELECT id, event_id, application_id, status, attempt_count,
		next_attempt_at, created_at, updated_at FROM deliveries WHERE id = ?`, id).
		Scan(&d.ID, &d.EventID, &d.ApplicationID, &status, &d.AttemptCount, &next, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Status = DeliveryStatus(status)
	d.NextAttemptAt, d.CreatedAt, d.UpdatedAt = fromMS(next), fromMS(created), fromMS(updated)
	return &d, nil
}

func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
