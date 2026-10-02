package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// These tests run the Postgres Lookup drivers against a SQLite stand-in pool,
// exactly like atomic_test.go: live Postgres is unavailable in the offline
// suite, and the shared ?-placeholder statements - the only text these
// drivers execute, after the PG placeholder rewrite - are the same ones the
// SQLite drivers prove. They pin the new error contract: a canceled request
// context and a genuine database failure both come back as wrapped errors,
// never a panic, and never as a false with no error.

// lookupPool opens one *sql.DB pool on the SQLite file at path with the
// sessions and api_keys tables created and one seeded row in each.
func lookupPool(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open stand-in pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE sessions (
		token_hash BLOB PRIMARY KEY,
		account_id TEXT NOT NULL,
		issued_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE api_keys (
		key_hash BLOB PRIMARY KEY,
		account_id TEXT NOT NULL,
		prefix TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_used INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		revoked INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create api_keys: %v", err)
	}
	ctx := context.Background()
	var sessionHash, keyHash [32]byte
	sessionHash[0] = 0xcd
	keyHash[0] = 0xab
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (token_hash, account_id, issued_at, expires_at, last_seen)
		VALUES (?, 'acct-1', 1700000000, 1700121600, 1700000000)`, sessionHash[:]); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO api_keys (key_hash, account_id, prefix, label, created_at, last_used, request_count, revoked)
		VALUES (?, 'acct-1', 'attk_0123456789', 'test', 1700000000, 0, 0, 0)`, keyHash[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return db
}

// TestPGSessionLookupReturnsErrors pins the SessionsStore.Lookup contract on
// the SQLite stand-in: the happy path returns the row, a canceled context
// returns the cancellation wrapped, and a closed pool returns a genuine
// error. None of the three may panic.
func TestPGSessionLookupReturnsErrors(t *testing.T) {
	db := lookupPool(t, filepath.Join(t.TempDir(), "lookup.db"))
	s := &SessionsStore{db: db}
	ctx := context.Background()

	// Happy path: the stored row comes back with its hash restored.
	rec, found, err := s.Lookup(ctx, [32]byte{0xcd})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("Lookup returned false for a stored session")
	}
	if rec.AccountID != "acct-1" || rec.IssuedAt != 1700000000 || rec.ExpiresAt != 1700121600 {
		t.Errorf("Lookup = %+v, want the seeded session", rec)
	}

	// Absent hash: a clean miss, no error.
	if _, found, err := s.Lookup(ctx, [32]byte{0xee}); err != nil || found {
		t.Errorf("absent Lookup = (found=%v, err=%v), want false, nil", found, err)
	}

	// Canceled context: the browser-hangup case. No panic, found false,
	// and the cancellation survives the wrap so the caller can tell it
	// apart from a real failure.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, found, err = s.Lookup(canceled, [32]byte{0xcd})
	if found {
		t.Error("Lookup on a canceled context returned true")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Lookup error = %v, want context.Canceled", err)
	}
}

// TestPGSessionLookupClosedPool is the genuine-failure half: after the pool
// is closed the lookup must return an error that is NOT a cancellation.
func TestPGSessionLookupClosedPool(t *testing.T) {
	db := lookupPool(t, filepath.Join(t.TempDir(), "lookup.db"))
	s := &SessionsStore{db: db}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, found, err := s.Lookup(context.Background(), [32]byte{0xcd})
	if found {
		t.Error("Lookup after Close returned true")
	}
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want a genuine (non-canceled) error", err)
	}
}

// TestPGKeyLookupReturnsErrors is the KeysStore counterpart of
// TestPGSessionLookupReturnsErrors.
func TestPGKeyLookupReturnsErrors(t *testing.T) {
	db := lookupPool(t, filepath.Join(t.TempDir(), "lookup.db"))
	s := &KeysStore{db: db}
	ctx := context.Background()

	k, found, err := s.Lookup(ctx, [32]byte{0xab})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("Lookup returned false for a stored key")
	}
	if k.AccountID != "acct-1" || k.Prefix != "attk_0123456789" || k.Label != "test" || k.CreatedAt != 1700000000 {
		t.Errorf("Lookup = %+v, want the seeded key", k)
	}

	if _, found, err := s.Lookup(ctx, [32]byte{0xee}); err != nil || found {
		t.Errorf("absent Lookup = (found=%v, err=%v), want false, nil", found, err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, found, err = s.Lookup(canceled, [32]byte{0xab})
	if found {
		t.Error("Lookup on a canceled context returned true")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Lookup error = %v, want context.Canceled", err)
	}
}

// TestPGKeyLookupClosedPool is the genuine-failure half for KeysStore.
func TestPGKeyLookupClosedPool(t *testing.T) {
	db := lookupPool(t, filepath.Join(t.TempDir(), "lookup.db"))
	s := &KeysStore{db: db}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, found, err := s.Lookup(context.Background(), [32]byte{0xab})
	if found {
		t.Error("Lookup after Close returned true")
	}
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want a genuine (non-canceled) error", err)
	}
}
