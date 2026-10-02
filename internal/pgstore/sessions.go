package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"slotfare/internal/session"
	"time"
)

// SessionsStore implements session.Store on Postgres. Every statement
// is the shared constant from internal/console, rewritten for $n
// placeholders.
type SessionsStore struct {
	db *sql.DB
}

// NewSessionsStore returns the Postgres session.Store for dsn.
func NewSessionsStore(ctx context.Context, dsn string) (session.Store, error) {
	db, err := openPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &SessionsStore{db: db}, nil
}

// Save persists one freshly issued session.
func (s *SessionsStore) Save(ctx context.Context, rec session.Session) error {
	if _, err := s.db.ExecContext(ctx, PG(session.SQLSessionSave),
		rec.Hash[:], rec.AccountID, rec.IssuedAt, rec.ExpiresAt, rec.LastSeen); err != nil {
		return fmt.Errorf("pgstore: save session for account %s: %w", rec.AccountID, err)
	}
	return nil
}

// Lookup returns the stored session for a hash, or false when the hash is
// not a known session. A query error - including a canceled request
// context - is returned wrapped, never reported as an unknown session:
// the caller would turn it into a 401 instead of a 500.
func (s *SessionsStore) Lookup(ctx context.Context, h [32]byte) (session.Session, bool, error) {
	var rec session.Session
	err := s.db.QueryRowContext(ctx, PG(session.SQLSessionLookup), h[:]).
		Scan(&rec.AccountID, &rec.IssuedAt, &rec.ExpiresAt, &rec.LastSeen)
	if err == sql.ErrNoRows {
		return session.Session{}, false, nil
	}
	if err != nil {
		return session.Session{}, false, fmt.Errorf("pgstore: session lookup failed: %w", err)
	}
	rec.Hash = h
	return rec, true, nil
}

// Touch slides the session's expiry forward to now + 14 days, writing at
// most once a minute: the last_seen guard in the shared statement makes
// earlier requests no-ops.
func (s *SessionsStore) Touch(ctx context.Context, h [32]byte, now int64) error {
	if _, err := s.db.ExecContext(ctx, PG(session.SQLSessionTouch),
		now, now+int64((14*24*time.Hour).Seconds()), h[:], now-60); err != nil {
		return fmt.Errorf("pgstore: touch session: %w", err)
	}
	return nil
}

// Revoke deletes the session row and reports whether a row was removed.
func (s *SessionsStore) Revoke(ctx context.Context, h [32]byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, PG(session.SQLSessionRevoke), h[:])
	if err != nil {
		return false, fmt.Errorf("pgstore: revoke session: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SweepExpired deletes every session and nonce past its expiry and returns
// the number of session rows removed.
func (s *SessionsStore) SweepExpired(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, PG(session.SQLSessionSweep), now)
	if err != nil {
		return 0, fmt.Errorf("pgstore: sweep expired sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, PG(session.SQLNonceSweep), now); err != nil {
		return 0, fmt.Errorf("pgstore: sweep expired nonces: %w", err)
	}
	n, err := res.RowsAffected()
	return n, err
}

// SaveNonce stores the outstanding nonce for an address, replacing the
// previous one.
func (s *SessionsStore) SaveNonce(ctx context.Context, n session.Nonce) error {
	if _, err := s.db.ExecContext(ctx, PG(session.SQLNonceSave), n.Address, n.Nonce, n.IssuedAt, n.ExpiresAt); err != nil {
		return fmt.Errorf("pgstore: save nonce for %s: %w", n.Address, err)
	}
	return nil
}

// TakeNonce removes and returns the outstanding nonce for an address in one
// statement, so only one replica can ever accept it.
func (s *SessionsStore) TakeNonce(ctx context.Context, address string) (session.Nonce, bool, error) {
	n := session.Nonce{Address: address}
	err := s.db.QueryRowContext(ctx, PG(session.SQLNonceTake), address).Scan(&n.Nonce, &n.IssuedAt, &n.ExpiresAt)
	if err == sql.ErrNoRows {
		return session.Nonce{}, false, nil
	}
	if err != nil {
		return session.Nonce{}, false, fmt.Errorf("pgstore: take nonce for %s: %w", address, err)
	}
	return n, true, nil
}

// Close closes the underlying pool.
func (s *SessionsStore) Close() error {
	return s.db.Close()
}
