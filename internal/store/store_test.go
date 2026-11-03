package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "relay.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenPragmasAndPermissions(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()

	var mode string
	var sync, fk int
	if err := s.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", mode, err)
	}
	if err := s.w.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
		t.Errorf("synchronous = %d (%v), want 2 (FULL)", sync, err)
	}
	if err := s.w.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d (%v), want 1", fk, err)
	}
	if _, err := s.r.ExecContext(ctx, `CREATE TABLE nope (x)`); err == nil {
		t.Error("read pool must be query-only")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db file mode = %v, want 0600", fi.Mode().Perm())
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() || v < 2 {
		t.Errorf("schema version = %d (%v), want %d", v, err, LatestSchemaVersion())
	}
}

func TestSingleInstanceLock(t *testing.T) {
	s, path := openTemp(t)
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
	s.Close()
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	s2.Close()
}

func TestMigrationsIdempotent(t *testing.T) {
	s, path := openTemp(t)
	s.Close()
	for range 2 {
		s2, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s2.r.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != LatestSchemaVersion() {
			t.Fatalf("schema_migrations rows = %d (%v)", n, err)
		}
		s2.Close()
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", "relay.db?mode=memory", "a#b"} {
		if _, err := Open(context.Background(), p); err == nil {
			t.Errorf("Open(%q) should fail", p)
		}
	}
}
