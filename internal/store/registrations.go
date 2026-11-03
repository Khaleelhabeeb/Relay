package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/khaleelhabeeb/relay/internal/ids"
	"github.com/khaleelhabeeb/relay/internal/routing"
)

// ErrRegistrationConflict means another application owns (or, for
// transaction references, has ever owned) the identifier.
var ErrRegistrationConflict = errors.New("store: identifier is registered to another application")

type Registration struct {
	ID              string    `json:"id"`
	Provider        string    `json:"provider"`
	IdentifierType  string    `json:"identifier_type"`
	IdentifierValue string    `json:"identifier_value"`
	ApplicationID   string    `json:"application_id"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
	DeactivatedAt   time.Time `json:"deactivated_at"` // zero while active
}

const registrationColumns = `id, provider, identifier_type, identifier_value, application_id, active, created_at, deactivated_at`

func scanRegistration(row interface{ Scan(...any) error }) (*Registration, error) {
	var (
		r           Registration
		created     int64
		deactivated sql.NullInt64
	)
	if err := row.Scan(&r.ID, &r.Provider, &r.IdentifierType, &r.IdentifierValue, &r.ApplicationID,
		&r.Active, &created, &deactivated); err != nil {
		return nil, err
	}
	r.CreatedAt = fromMS(created)
	if deactivated.Valid {
		r.DeactivatedAt = fromMS(deactivated.Int64)
	}
	return &r, nil
}

// CreateRegistration claims an identifier for application. It is
// idempotent: if application already actively owns it, the existing
// registration is returned with created == false. Reuse rules:
//   - an identifier actively owned by another application → conflict;
//   - a transaction_reference ever owned by another application → conflict,
//     forever;
//   - a dedicated_account deactivated by its previous owner may be claimed;
//     routing then gives each event to whoever owned it when it arrived.
func (s *Store) CreateRegistration(ctx context.Context, provider, identifierType, value, application string, now time.Time) (*Registration, bool, error) {
	var (
		reg     *Registration
		created bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+registrationColumns+` FROM registrations
			WHERE provider = ? AND identifier_type = ? AND identifier_value = ? ORDER BY created_at`,
			provider, identifierType, value)
		if err != nil {
			return err
		}
		var history []*Registration
		for rows.Next() {
			r, err := scanRegistration(rows)
			if err != nil {
				rows.Close()
				return err
			}
			history = append(history, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, r := range history {
			if !r.Active {
				continue
			}
			if r.ApplicationID == application {
				reg = r
				return nil
			}
			return ErrRegistrationConflict
		}
		if identifierType == routing.IdentifierReference {
			for _, r := range history {
				if r.ApplicationID != application {
					return ErrRegistrationConflict
				}
			}
		}

		reg = &Registration{
			ID: ids.New(ids.Registration), Provider: provider, IdentifierType: identifierType,
			IdentifierValue: value, ApplicationID: application, Active: true, CreatedAt: now,
		}
		created = true
		_, err = tx.ExecContext(ctx, `INSERT INTO registrations
			(id, provider, identifier_type, identifier_value, application_id, active, created_at)
			VALUES (?, ?, ?, ?, ?, 1, ?)`, reg.ID, provider, identifierType, value, application, ms(now))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return reg, created, nil
}

// GetRegistration reads one registration.
func (s *Store) GetRegistration(ctx context.Context, id string) (*Registration, error) {
	r, err := scanRegistration(s.r.QueryRowContext(ctx, `SELECT `+registrationColumns+` FROM registrations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RegistrationFilter narrows ListRegistrations. Zero values mean "any".
type RegistrationFilter struct {
	Application    string
	IdentifierType string
	Active         *bool
	After          string // registration ID cursor (IDs sort by creation)
	Limit          int
}

// ListRegistrations returns registrations in ID (creation) order.
func (s *Store) ListRegistrations(ctx context.Context, f RegistrationFilter) ([]Registration, error) {
	var (
		where []string
		args  []any
	)
	if f.Application != "" {
		where, args = append(where, "application_id = ?"), append(args, f.Application)
	}
	if f.IdentifierType != "" {
		where, args = append(where, "identifier_type = ?"), append(args, f.IdentifierType)
	}
	if f.Active != nil {
		where, args = append(where, "active = ?"), append(args, *f.Active)
	}
	if f.After != "" {
		where, args = append(where, "id > ?"), append(args, f.After)
	}
	q := `SELECT ` + registrationColumns + ` FROM registrations`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += " ORDER BY id LIMIT ?"
	args = append(args, limit)

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		r, err := scanRegistration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// DeactivateRegistration deactivates application's registration id. It is
// idempotent; another application's registration reads as ErrNotFound.
func (s *Store) DeactivateRegistration(ctx context.Context, application, id string, now time.Time) (*Registration, error) {
	var reg *Registration
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := scanRegistration(tx.QueryRowContext(ctx, `SELECT `+registrationColumns+` FROM registrations
			WHERE id = ? AND application_id = ?`, id, application))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.Active {
			if _, err := tx.ExecContext(ctx, `UPDATE registrations SET active = 0, deactivated_at = ? WHERE id = ?`,
				ms(now), id); err != nil {
				return err
			}
			r.Active, r.DeactivatedAt = false, now
		}
		reg = r
		return nil
	})
	return reg, err
}

// UnresolvedEventIDs lists unresolved events carrying an identifier,
// received at or after since, oldest first.
func (s *Store) UnresolvedEventIDs(ctx context.Context, provider, identifierType, value string, since time.Time) ([]string, error) {
	column := "reference"
	if identifierType == routing.IdentifierDedicatedAccount {
		column = "dedicated_account"
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id FROM events WHERE provider = ? AND `+column+` = ?
		AND routing_status = 'unresolved' AND received_at >= ? ORDER BY received_at`, provider, value, ms(since))
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

// GetEvent reads an event inside the transaction.
func (t *Tx) GetEvent(ctx context.Context, id string) (*Event, error) {
	return scanEvent(t.tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
}

// SetEventRouting records an event's new routing outcome.
func (t *Tx) SetEventRouting(ctx context.Context, id string, outcome routing.Outcome, application string) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE events SET routing_status = ?, application_id = ? WHERE id = ?`,
		string(outcome), nullString(application), id)
	return err
}

// ApplyDecision records a routing decision for an event and, when it
// matched, creates the pending delivery (due at). It does not change the
// event row; callers inserting or updating the event set routing_status.
func (t *Tx) ApplyDecision(ctx context.Context, eventID string, d routing.Decision, decidedBy string, at time.Time) error {
	if err := t.InsertRoutingDecision(ctx, &RoutingDecision{
		ID:            ids.New(ids.RoutingDecision),
		EventID:       eventID,
		Outcome:       d.Outcome,
		ApplicationID: d.Application,
		Candidates:    d.Candidates,
		Reason:        d.Reason,
		DecidedBy:     decidedBy,
		DecidedAt:     at,
	}); err != nil {
		return err
	}
	if d.Outcome != routing.Matched {
		return nil
	}
	return t.InsertDelivery(ctx, &Delivery{
		ID:            ids.New(ids.Delivery),
		EventID:       eventID,
		ApplicationID: d.Application,
		Status:        DeliveryPending,
		NextAttemptAt: at,
		CreatedAt:     at,
	})
}
