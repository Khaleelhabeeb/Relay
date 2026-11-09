package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Alert kinds, queued in the same transaction as the change they report.
const (
	AlertUnresolved          = "event.unresolved"
	AlertAmbiguous           = "event.ambiguous"
	AlertExpired             = "event.expired"
	AlertDeliveryExhausted   = "delivery.exhausted"
	AlertConfirmationFailed  = "confirmation.failed"
	AlertConfirmationRejects = "confirmation.rejected"
	AlertDedupConflict       = "dedup.conflict"
)

func queueAlert(ctx context.Context, tx *sql.Tx, kind, eventID, deliveryID string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO alert_outbox (kind, event_id, delivery_id, created_at) VALUES (?, ?, ?, ?)`,
		kind, eventID, nullString(deliveryID), ms(at))
	return err
}

// PendingAlert is an unsent alert with the current state of what it
// reports, so the alerter can drop alerts that no longer hold.
type PendingAlert struct {
	ID         int64
	Kind       string
	EventID    string
	DeliveryID string
	CreatedAt  time.Time

	Provider       string
	EventType      string
	Reference      string
	RoutingStatus  string
	ApplicationID  string
	Confirmation   Confirmation
	ConfirmError   string
	Dismissed      bool
	DismissReason  string
	DeliveryApp    string
	DeliveryStatus DeliveryStatus
	LastError      string
}

// PendingAlerts returns up to limit unsent alerts, oldest first.
func (s *Store) PendingAlerts(ctx context.Context, limit int) ([]PendingAlert, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT a.id, a.kind, a.event_id, a.delivery_id, a.created_at,
			COALESCE(e.provider, ''), COALESCE(e.event_type, ''), e.reference, COALESCE(e.routing_status, ''),
			e.application_id, COALESCE(e.confirmation, ''), e.confirm_error, e.dismissed_at, e.dismiss_reason,
			d.application_id, d.status, d.last_error
		FROM alert_outbox a
		LEFT JOIN events e ON e.id = a.event_id
		LEFT JOIN deliveries d ON d.id = a.delivery_id
		WHERE a.sent_at IS NULL ORDER BY a.id LIMIT ?`, limitOr(limit, 1000))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAlert
	for rows.Next() {
		var (
			p                          PendingAlert
			delID, ref, app, cerr, why sql.NullString
			dapp, dstatus, lastErr     sql.NullString
			created                    int64
			dismissed                  sql.NullInt64
		)
		if err := rows.Scan(&p.ID, &p.Kind, &p.EventID, &delID, &created, &p.Provider, &p.EventType, &ref,
			&p.RoutingStatus, &app, &p.Confirmation, &cerr, &dismissed, &why, &dapp, &dstatus, &lastErr); err != nil {
			return nil, err
		}
		p.DeliveryID, p.CreatedAt, p.Reference, p.ApplicationID = delID.String, fromMS(created), ref.String, app.String
		p.ConfirmError, p.Dismissed, p.DismissReason = cerr.String, dismissed.Valid, why.String
		p.DeliveryApp, p.DeliveryStatus, p.LastError = dapp.String, DeliveryStatus(dstatus.String), lastErr.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkAlertsSent marks every unsent alert with an ID up to maxID sent.
func (s *Store) MarkAlertsSent(ctx context.Context, maxID int64, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `UPDATE alert_outbox SET sent_at = ? WHERE id <= ? AND sent_at IS NULL`, ms(now), maxID)
	return err
}

// PruneAlerts deletes alerts created before cutoff, sent or not.
func (s *Store) PruneAlerts(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM alert_outbox WHERE created_at < ?`, ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AlertState reads a named timestamp.
func (s *Store) AlertState(ctx context.Context, name string) (time.Time, bool, error) {
	var at int64
	err := s.r.QueryRowContext(ctx, `SELECT at FROM alert_state WHERE name = ?`, name).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return fromMS(at), true, nil
}

// SetAlertState writes a named timestamp.
func (s *Store) SetAlertState(ctx context.Context, name string, at time.Time) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO alert_state (name, at) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET at = excluded.at`, name, ms(at))
	return err
}

// ErrNotHeld means the event is not unresolved or ambiguous, or is already
// dismissed.
var ErrNotHeld = errors.New("store: only held (unresolved or ambiguous, not dismissed) events can be dismissed")

// heldCondition selects events that need an owner decided.
const heldCondition = `routing_status IN ('unresolved', 'ambiguous') AND dismissed_at IS NULL AND confirmation != 'rejected'`

// DismissEvent closes a held event without an owner. by records who
// dismissed it (e.g. "operator via cli").
func (s *Store) DismissEvent(ctx context.Context, id, by, reason string, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE events SET dismissed_at = ?, dismissed_by = ?, dismiss_reason = ?
			WHERE id = ? AND `+heldCondition, ms(now), by, truncate(reason, 500), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var status string
		var dismissed sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT routing_status, dismissed_at FROM events WHERE id = ?`, id).Scan(&status, &dismissed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case dismissed.Valid:
			return fmt.Errorf("%w (already dismissed)", ErrNotHeld)
		default:
			return fmt.Errorf("%w (status is %s)", ErrNotHeld, status)
		}
	})
}

// ExpireHeld dismisses every held event received before cutoff, with an
// event.expired alert for each. It returns how many it expired.
func (s *Store) ExpireHeld(ctx context.Context, cutoff, now time.Time, reason string) (int64, error) {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM events WHERE `+heldCondition+` AND received_at < ?`, ms(cutoff))
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE events SET dismissed_at = ?, dismissed_by = 'retention',
				dismiss_reason = ? WHERE id = ?`, ms(now), reason, id); err != nil {
				return err
			}
			if err := queueAlert(ctx, tx, AlertExpired, id, "", now); err != nil {
				return err
			}
		}
		n = int64(len(ids))
		return nil
	})
	return n, err
}
