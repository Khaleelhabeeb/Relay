// Package store is Relay's SQLite persistence layer.
//
// One writer connection serializes all writes (SQLite allows a single
// writer anyway) and every write transaction starts with BEGIN IMMEDIATE,
// so read-then-write sequences never hit a lock upgrade failure. A separate
// query-only pool serves reads concurrently under WAL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// ErrLocked means another process holds the database lock.
var ErrLocked = errors.New("store: database is in use by another relay process")

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	w    *sql.DB // single writer connection
	r    *sql.DB // query-only read pool
	lock *os.File
}

// Open locks, opens and migrates the database at path. Only the server
// uses it: the lock guarantees a single serving process per database.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, true)
}

// OpenTool opens an existing, fully migrated database for operator commands
// that run alongside a live server. It takes no lock and never migrates;
// concurrent writes are safe under WAL, busy_timeout and BEGIN IMMEDIATE.
func OpenTool(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("store: %w (has relay serve run with this storage.dsn?)", err)
	}
	return open(ctx, path, false)
}

func open(ctx context.Context, path string, server bool) (*Store, error) {
	if path == "" || strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("store: invalid database path %q", path)
	}
	s := &Store{}
	if server {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		lock, err := acquireLock(path + ".lock")
		if err != nil {
			return nil, err
		}
		s.lock = lock
		// Create the file owner-only before SQLite does, so it is never
		// briefly world-readable. SQLite gives -wal/-shm the same mode.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- operator DSN
		if err != nil {
			s.Close()
			return nil, err
		}
		f.Close()
	}
	fail := func(err error) (*Store, error) {
		s.Close()
		return nil, err
	}

	var err error
	pragmas := []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)", "busy_timeout(5000)"}
	if s.w, err = sql.Open("sqlite", dsn(path, pragmas, "immediate")); err != nil {
		return fail(err)
	}
	s.w.SetMaxOpenConns(1)
	s.w.SetConnMaxLifetime(0)
	s.w.SetConnMaxIdleTime(0)
	if err := s.w.PingContext(ctx); err != nil {
		return fail(fmt.Errorf("store: open %s: %w", path, err))
	}
	if server {
		// The database file holds raw payment events.
		if err := os.Chmod(path, 0o600); err != nil {
			return fail(err)
		}
		if err := s.migrate(ctx); err != nil {
			return fail(err)
		}
	}

	if s.r, err = sql.Open("sqlite", dsn(path, append(pragmas[2:], "query_only(1)"), "")); err != nil {
		return fail(err)
	}
	s.r.SetMaxOpenConns(4)

	if !server {
		have, err := s.SchemaVersion(ctx)
		if err != nil {
			return fail(fmt.Errorf("store: read schema version: %w", err))
		}
		if want := LatestSchemaVersion(); have != want {
			return fail(fmt.Errorf("store: database schema is version %d, this relay expects %d; start relay serve (same version as this CLI) to migrate", have, want))
		}
	}
	return s, nil
}

func dsn(path string, pragmas []string, txlock string) string {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if txlock != "" {
		q.Set("_txlock", txlock)
	}
	return "file:" + path + "?" + q.Encode()
}

// Close closes the database and releases the process lock.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	return errors.Join(errs...)
}

// Ping checks that both pools are usable.
func (s *Store) Ping(ctx context.Context) error {
	return errors.Join(s.w.PingContext(ctx), s.r.PingContext(ctx))
}

type migration struct {
	version int
	name    string
}

func migrations() ([]migration, error) {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, n := range names {
		base := filepath.Base(n)
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("store: migration %s: bad version prefix", base)
		}
		ms = append(ms, migration{v, n})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// LatestSchemaVersion is the newest migration this binary carries.
func LatestSchemaVersion() int {
	ms, err := migrations()
	if err != nil || len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].version
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: migrations table: %w", err)
	}
	ms, err := migrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		err := s.withTx(ctx, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			body, err := migrationFS.ReadFile(m.name)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				m.version, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", filepath.Base(m.name), err)
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WithTx runs fn in a write transaction, committing if fn returns nil.
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	return s.withTx(ctx, func(tx *sql.Tx) error { return fn(&Tx{tx: tx}) })
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
