package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/khaleelhabeeb/relay/internal/ids"
	"github.com/khaleelhabeeb/relay/internal/routing"
)

// Confirmation is an event's provider API confirmation state.
type Confirmation string

const (
	ConfirmNone      Confirmation = "none"      // not required
	ConfirmConfirmed Confirmation = "confirmed" // the provider API vouched for it
	ConfirmPending   Confirmation = "pending"   // retrying in the background
	ConfirmRejected  Confirmation = "rejected"  // the provider API disowned it
	ConfirmFailed    Confirmation = "failed"    // retry window closed
)

// Deliverable reports whether an event in state c may be delivered.
func (c Confirmation) Deliverable() bool { return c == ConfirmNone || c == ConfirmConfirmed || c == "" }

// ErrNotPending means a confirmation result arrived for an event that is no
// longer pending (resolved elsewhere, or retried by an operator).
var ErrNotPending = errors.New("store: event is not awaiting confirmation")

// ErrPurged means retention has cleared the event's payload.
var ErrPurged = errors.New("store: event payload has been purged")

// DueConfirmation is a pending event whose next confirmation attempt is due.
type DueConfirmation struct {
	ID         string
	Provider   string
	RawPayload []byte
	Attempts   int
	ReceivedAt time.Time
}

// DueConfirmations lists pending events due at or before now, oldest first.
func (s *Store) DueConfirmations(ctx context.Context, now time.Time, limit int) ([]DueConfirmation, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, provider, raw_payload, confirm_attempts, received_at FROM events
		WHERE confirmation = 'pending' AND confirm_next_at <= ? ORDER BY confirm_next_at, id LIMIT ?`,
		ms(now), limitOr(limit, 20))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueConfirmation
	for rows.Next() {
		var (
			d        DueConfirmation
			received int64
		)
		if err := rows.Scan(&d.ID, &d.Provider, &d.RawPayload, &d.Attempts, &received); err != nil {
			return nil, err
		}
		d.ReceivedAt = fromMS(received)
		out = append(out, d)
	}
	return out, rows.Err()
}

// NextConfirmationDue returns when the earliest pending confirmation is due.
func (s *Store) NextConfirmationDue(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT MIN(confirm_next_at) FROM events WHERE confirmation = 'pending'`).Scan(&next)
	if err != nil || !next.Valid {
		return time.Time{}, false, err
	}
	return fromMS(next.Int64), true, nil
}

// ConfirmEvent marks a pending event confirmed and, if it is routed to an
// application, creates its delivery. It reports whether a delivery was made.
func (s *Store) ConfirmEvent(ctx context.Context, id string, now time.Time) (delivery bool, err error) {
	err = s.WithTx(ctx, func(tx *Tx) error {
		e, err := tx.GetEvent(ctx, id)
		if err != nil {
			return err
		}
		if e.Confirmation != ConfirmPending {
			return ErrNotPending
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE events SET confirmation = 'confirmed', confirm_next_at = NULL,
			confirm_error = NULL WHERE id = ?`, id); err != nil {
			return err
		}
		if e.RoutingStatus != routing.Matched || e.ApplicationID == "" {
			return nil
		}
		var n int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id = ?`, id).Scan(&n); err != nil || n > 0 {
			return err
		}
		delivery = true
		return tx.InsertDelivery(ctx, &Delivery{
			ID: ids.New(ids.Delivery), EventID: id, ApplicationID: e.ApplicationID,
			Status: DeliveryPending, NextAttemptAt: now, CreatedAt: now,
		})
	})
	return delivery, err
}

// RejectEvent marks a pending event rejected by the provider API. Its
// dedup key is rewritten to "rejected:<id>:<key>" so a genuine event with
// the same key is not mistaken for a duplicate of this one.
func (s *Store) RejectEvent(ctx context.Context, id, reason string) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		res, err := tx.tx.ExecContext(ctx, `UPDATE events SET confirmation = 'rejected', confirm_next_at = NULL,
			confirm_error = ?, dedup_key = 'rejected:' || id || ':' || dedup_key
			WHERE id = ? AND confirmation = 'pending'`, truncate(reason, 500), id)
		if err := pendingRow(res, err); err != nil {
			return err
		}
		return queueAlert(ctx, tx.tx, AlertConfirmationRejects, id, "", time.Now().UTC())
	})
}

// DeferConfirmation records a failed attempt. A zero next means the retry
// budget is spent: the event becomes failed.
func (s *Store) DeferConfirmation(ctx context.Context, id string, attempts int, next time.Time, reason string) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		if next.IsZero() {
			res, err := tx.tx.ExecContext(ctx, `UPDATE events SET confirmation = 'failed', confirm_attempts = ?,
				confirm_next_at = NULL, confirm_error = ? WHERE id = ? AND confirmation = 'pending'`,
				attempts, truncate(reason, 500), id)
			if err := pendingRow(res, err); err != nil {
				return err
			}
			return queueAlert(ctx, tx.tx, AlertConfirmationFailed, id, "", time.Now().UTC())
		}
		res, err := tx.tx.ExecContext(ctx, `UPDATE events SET confirm_attempts = ?, confirm_next_at = ?,
			confirm_error = ? WHERE id = ? AND confirmation = 'pending'`, attempts, ms(next), truncate(reason, 500), id)
		return pendingRow(res, err)
	})
}

// RetryConfirmation makes a pending or failed event due for confirmation
// now, with its attempt count (and so its retry window) reset. A running Relay picks it
// up within seconds.
func (s *Store) RetryConfirmation(ctx context.Context, id string, now time.Time) error {
	return s.WithTx(ctx, func(tx *Tx) error {
		e, err := tx.GetEvent(ctx, id)
		if err != nil {
			return err
		}
		if e.Confirmation != ConfirmPending && e.Confirmation != ConfirmFailed {
			return fmt.Errorf("%w (confirmation is %s)", ErrNotPending, e.Confirmation)
		}
		if len(e.RawPayload) == 0 {
			return fmt.Errorf("%w; it cannot be confirmed", ErrPurged)
		}
		_, err = tx.tx.ExecContext(ctx, `UPDATE events SET confirmation = 'pending', confirm_next_at = ?,
			confirm_attempts = 0 WHERE id = ?`, ms(now), id)
		return err
	})
}

func pendingRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPending
	}
	return nil
}
