package keys

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists issued keys. It holds only the account id, the key hash
// and bookkeeping per key (prefix, label, usage counters, revoked flag) -
// no email, no address, no per-request log tied to an identity. The SQLite
// driver is SQLite; the Postgres driver lives in internal/pgstore.
type Store interface {
	Add(ctx context.Context, k Key) error
	// Lookup returns the stored key for a hash, or false when the hash is
	// not a known key. A non-nil error means the query itself failed and
	// found is meaningless; it is wrapped so callers can tell a canceled
	// request context apart from a genuine store failure.
	Lookup(ctx context.Context, h [32]byte) (Key, bool, error)
	ListByAccount(ctx context.Context, accountID string) ([]Key, error)
	RevokeByPrefix(ctx context.Context, accountID, prefix string) (bool, error)
	Touch(ctx context.Context, h [32]byte) error
	Close() error
}

// SQLite is the Store driver backed by a single SQLite file. The pure-Go
// driver keeps the binary cgo-free so it cross-compiles.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the SQLite database at path, enables WAL,
// and ensures the api_keys table exists. Databases created before the
// prefix/label/usage/revoked columns existed are migrated in place.
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("keys: open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("keys: enable WAL on %s: %w", path, err)
	}
	schema := `CREATE TABLE IF NOT EXISTS api_keys (
		key_hash BLOB PRIMARY KEY,
		account_id TEXT NOT NULL,
		prefix TEXT NOT NULL DEFAULT '',
		label TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		last_used INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		revoked INTEGER NOT NULL DEFAULT 0
	)`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("keys: create api_keys table in %s: %w", path, err)
	}
	// Older databases have the table without the later columns; the new
	// CREATE TABLE above is a no-op for them, so add the columns here.
	// A duplicate column means the database is already migrated.
	migrations := []string{
		`ALTER TABLE api_keys ADD COLUMN prefix TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN last_used INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE api_keys ADD COLUMN request_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE api_keys ADD COLUMN revoked INTEGER NOT NULL DEFAULT 0`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				db.Close()
				return nil, fmt.Errorf("keys: migrate api_keys in %s: %w", path, err)
			}
		}
	}
	return &SQLite{db: db}, nil
}

// SQLAdd inserts one issued key; both drivers execute it.
const SQLAdd = `INSERT INTO api_keys (key_hash, account_id, prefix, label, created_at, last_used, request_count, revoked) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// Add records an issued key, persisting exactly what Key carries; the
// caller sets CreatedAt. A duplicate hash is an error.
func (s *SQLite) Add(ctx context.Context, k Key) error {
	if _, err := s.db.ExecContext(ctx, SQLAdd,
		k.Hash[:], k.AccountID, k.Prefix, k.Label, k.CreatedAt, k.LastUsed, k.RequestCount, boolToInt(k.Revoked)); err != nil {
		return fmt.Errorf("keys: insert key for account %s: %w", k.AccountID, err)
	}
	return nil
}

// SQLLookup reads one stored key by hash; both drivers execute it.
const SQLLookup = `SELECT account_id, prefix, label, created_at, last_used, request_count, revoked FROM api_keys WHERE key_hash = ?`

// Lookup returns the stored key for a hash, or false when the hash is not a
// known key. A query error - including a canceled request context - is
// returned wrapped, never reported as an unknown key: the caller would turn
// it into a 401 instead of a 500.
func (s *SQLite) Lookup(ctx context.Context, h [32]byte) (Key, bool, error) {
	var k Key
	var revoked int
	err := s.db.QueryRowContext(ctx, SQLLookup, h[:]).
		Scan(&k.AccountID, &k.Prefix, &k.Label, &k.CreatedAt, &k.LastUsed, &k.RequestCount, &revoked)
	if err == sql.ErrNoRows {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("keys: lookup key failed: %w", err)
	}
	k.Hash = h
	k.Revoked = revoked == 1
	return k, true, nil
}

// SQLListByAccount lists every key of one account, oldest first; both
// drivers execute it.
const SQLListByAccount = `SELECT key_hash, account_id, prefix, label, created_at, last_used, request_count, revoked FROM api_keys WHERE account_id = ? ORDER BY created_at`

// ListByAccount returns every key stored for accountID, oldest first.
func (s *SQLite) ListByAccount(ctx context.Context, accountID string) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, SQLListByAccount, accountID)
	if err != nil {
		return nil, fmt.Errorf("keys: list keys for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		var hash []byte
		var revoked int
		if err := rows.Scan(&hash, &k.AccountID, &k.Prefix, &k.Label, &k.CreatedAt, &k.LastUsed, &k.RequestCount, &revoked); err != nil {
			return nil, fmt.Errorf("keys: scan key for account %s: %w", accountID, err)
		}
		if len(hash) != 32 {
			return nil, fmt.Errorf("keys: scan key for account %s: key_hash is %d bytes, want 32", accountID, len(hash))
		}
		copy(k.Hash[:], hash)
		k.Revoked = revoked == 1
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keys: list keys for account %s: %w", accountID, err)
	}
	return out, nil
}

// SQLRevokeByPrefix revokes one key by account and prefix; both drivers
// execute it.
const SQLRevokeByPrefix = `UPDATE api_keys SET revoked = 1 WHERE account_id = ? AND prefix = ?`

// RevokeByPrefix marks the key with the given prefix revoked when it belongs
// to accountID and reports whether a row was changed. A prefix owned by
// another account affects no row, so the caller cannot learn it exists.
func (s *SQLite) RevokeByPrefix(ctx context.Context, accountID, prefix string) (bool, error) {
	res, err := s.db.ExecContext(ctx, SQLRevokeByPrefix, accountID, prefix)
	if err != nil {
		return false, fmt.Errorf("keys: revoke prefix %s for account %s: %w", prefix, accountID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("keys: revoke prefix %s for account %s: %w", prefix, accountID, err)
	}
	return n > 0, nil
}

// SQLTouch records one authenticated use of a key; both drivers execute it.
const SQLTouch = `UPDATE api_keys SET last_used = ?, request_count = request_count + 1 WHERE key_hash = ?`

// Touch records one authenticated use of a key: last_used is set to now and
// request_count is incremented. It returns an error instead of panicking,
// because it runs on the request path against a database shared with the
// meter checkpoint and a transient lock error must not kill slotfared.
func (s *SQLite) Touch(ctx context.Context, h [32]byte) error {
	if _, err := s.db.ExecContext(ctx, SQLTouch, time.Now().Unix(), h[:]); err != nil {
		return fmt.Errorf("keys: touch key: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// boolToInt stores a bool as 1 or 0 for SQLite.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
