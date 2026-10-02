// Package session persists the console's sign-in nonces and bearer
// sessions. It owns the sessions and nonces tables, their statements and the
// single-use rule that makes a nonce one-shot; the HTTP side of sign-in
// (issuing, verifying, sweeping) lives in internal/console and reads this
// store through Store. The SQLite driver is SQLite; the Postgres driver
// lives in internal/pgstore.
package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// TTL is how long a console session stays valid: a generous window that
// slides forward on activity (see Touch), so an active user is never signed
// out mid-session and an abandoned one ages out.
const TTL = 14 * 24 * time.Hour

// Nonce is the persisted form of a nonceRecord: the base58 Solana public
// key it was issued to and its timing in unix seconds like every other
// table. The statement is a constant and is not stored. Nonces live in the
// session store rather than in memory so that the verify request may land
// on a different slotfared replica than the one that issued the nonce.
type Nonce struct {
	Address   string
	Nonce     string
	IssuedAt  int64
	ExpiresAt int64
}

// Session is one persisted console login: the keccak256 hash of its bearer
// token (never the plaintext), the account it authenticates and its timing,
// in unix seconds like every other table. The plaintext token is returned
// exactly once at sign-in.
type Session struct {
	Hash      [32]byte
	AccountID string
	IssuedAt  int64
	ExpiresAt int64
	LastSeen  int64
}

// Store persists console session tokens so a slotfared restart keeps a
// user signed in, exactly like the API keys. The SQLite driver is
// SQLite; the Postgres driver lives in internal/pgstore.
type Store interface {
	// Save inserts a freshly issued session row.
	Save(ctx context.Context, s Session) error
	// Lookup returns the stored session for a token hash, or false when the
	// hash is not a known session. A non-nil error means the query itself
	// failed and found is meaningless; it is wrapped so callers can tell a
	// canceled request context apart from a genuine store failure.
	Lookup(ctx context.Context, h [32]byte) (Session, bool, error)
	// Touch slides the session: last_seen is set to now and expires_at to
	// now + TTL, but only when the last write is at least a minute
	// old, so the request path does not write on every call.
	Touch(ctx context.Context, h [32]byte, now int64) error
	// Revoke deletes the session row (sign-out) and reports whether a row
	// was removed.
	Revoke(ctx context.Context, h [32]byte) (bool, error)
	// SweepExpired deletes every session and nonce row whose expiry has
	// passed and reports how many session rows it removed.
	SweepExpired(ctx context.Context, now int64) (int64, error)
	// SaveNonce stores an outstanding sign-in nonce for an address,
	// replacing any nonce that address already holds.
	SaveNonce(ctx context.Context, n Nonce) error
	// TakeNonce returns and removes the outstanding nonce for an address in
	// one atomic step, or false when the address holds none. Single use is
	// enforced by the store, so it holds across slotfared replicas.
	TakeNonce(ctx context.Context, address string) (Nonce, bool, error)
	Close() error
}

// SQLSessionSave inserts one issued session; both drivers execute it.
const SQLSessionSave = `INSERT INTO sessions (token_hash, account_id, issued_at, expires_at, last_seen) VALUES (?, ?, ?, ?, ?)`

// SQLSessionLookup reads one stored session by token hash; both drivers
// execute it.
const SQLSessionLookup = `SELECT account_id, issued_at, expires_at, last_seen FROM sessions WHERE token_hash = ?`

// SQLSessionTouch slides one session at most once a minute: the last_seen < ?
// guard makes requests inside the throttle window no-ops. Both drivers
// execute it.
const SQLSessionTouch = `UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ? AND last_seen < ?`

// SQLSessionRevoke deletes one session row on sign-out; both drivers execute
// it.
const SQLSessionRevoke = `DELETE FROM sessions WHERE token_hash = ?`

// SQLSessionSweep deletes every expired session row; both drivers execute
// it.
const SQLSessionSweep = `DELETE FROM sessions WHERE expires_at < ?`

// SQLNonceSave stores one outstanding nonce, replacing the address's
// previous one; both drivers execute it.
const SQLNonceSave = `INSERT INTO nonces (address, nonce, issued_at, expires_at) VALUES (?, ?, ?, ?)
	ON CONFLICT (address) DO UPDATE SET nonce = excluded.nonce, issued_at = excluded.issued_at, expires_at = excluded.expires_at`

// SQLNonceTake removes and returns the outstanding nonce for an address in
// one statement; both drivers execute it.
const SQLNonceTake = `DELETE FROM nonces WHERE address = ? RETURNING nonce, issued_at, expires_at`

// SQLNonceSweep deletes every expired nonce row; both drivers execute it.
const SQLNonceSweep = `DELETE FROM nonces WHERE expires_at < ?`

// SQLite is the Store driver backed by the shared slotfared
// SQLite file.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewStore opens (creating if needed) the shared database at path,
// enables WAL and ensures the sessions table exists.
func NewStore(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("session: open sessions sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("session: enable WAL on %s: %w", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
		token_hash BLOB PRIMARY KEY,
		account_id TEXT NOT NULL,
		issued_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("session: create sessions table in %s: %w", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS nonces (
		address TEXT PRIMARY KEY,
		nonce TEXT NOT NULL,
		issued_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("session: create nonces table in %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// Save persists one freshly issued session.
func (s *SQLite) Save(ctx context.Context, rec Session) error {
	if _, err := s.db.ExecContext(ctx, SQLSessionSave,
		rec.Hash[:], rec.AccountID, rec.IssuedAt, rec.ExpiresAt, rec.LastSeen); err != nil {
		return fmt.Errorf("session: save session for account %s: %w", rec.AccountID, err)
	}
	return nil
}

// Lookup returns the stored session for a hash, or false when the hash is
// not a known session. A query error - including a canceled request
// context - is returned wrapped, never reported as an unknown session:
// the caller would turn it into a 401 instead of a 500.
func (s *SQLite) Lookup(ctx context.Context, h [32]byte) (Session, bool, error) {
	var rec Session
	err := s.db.QueryRowContext(ctx, SQLSessionLookup, h[:]).
		Scan(&rec.AccountID, &rec.IssuedAt, &rec.ExpiresAt, &rec.LastSeen)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("session: session lookup failed: %w", err)
	}
	rec.Hash = h
	return rec, true, nil
}

// Touch slides the session's expiry forward to now + TTL, writing at
// most once a minute: the last_seen < now - 60s guard makes earlier requests
// no-ops.
func (s *SQLite) Touch(ctx context.Context, h [32]byte, now int64) error {
	if _, err := s.db.ExecContext(ctx, SQLSessionTouch,
		now, now+int64(TTL.Seconds()), h[:], now-60); err != nil {
		return fmt.Errorf("session: touch session: %w", err)
	}
	return nil
}

// Revoke deletes the session row and reports whether a row was removed.
func (s *SQLite) Revoke(ctx context.Context, h [32]byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, SQLSessionRevoke, h[:])
	if err != nil {
		return false, fmt.Errorf("session: revoke session: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SweepExpired deletes every session and nonce past its expiry and returns
// the number of session rows removed.
func (s *SQLite) SweepExpired(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, SQLSessionSweep, now)
	if err != nil {
		return 0, fmt.Errorf("session: sweep expired sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, SQLNonceSweep, now); err != nil {
		return 0, fmt.Errorf("session: sweep expired nonces: %w", err)
	}
	n, err := res.RowsAffected()
	return n, err
}

// SaveNonce stores the outstanding nonce for an address, replacing the
// previous one.
func (s *SQLite) SaveNonce(ctx context.Context, n Nonce) error {
	if _, err := s.db.ExecContext(ctx, SQLNonceSave, n.Address, n.Nonce, n.IssuedAt, n.ExpiresAt); err != nil {
		return fmt.Errorf("session: save nonce for %s: %w", n.Address, err)
	}
	return nil
}

// TakeNonce removes and returns the outstanding nonce for an address.
func (s *SQLite) TakeNonce(ctx context.Context, address string) (Nonce, bool, error) {
	n := Nonce{Address: address}
	err := s.db.QueryRowContext(ctx, SQLNonceTake, address).Scan(&n.Nonce, &n.IssuedAt, &n.ExpiresAt)
	if err == sql.ErrNoRows {
		return Nonce{}, false, nil
	}
	if err != nil {
		return Nonce{}, false, fmt.Errorf("session: take nonce for %s: %w", address, err)
	}
	return n, true, nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}
