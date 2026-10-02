package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"slotfare/internal/keys"
)

// KeysStore implements keys.Store on Postgres. Every statement is the
// shared constant from internal/keys, rewritten for $n placeholders.
type KeysStore struct {
	db *sql.DB
}

// Add records an issued key; a duplicate hash is an error.
func (s *KeysStore) Add(ctx context.Context, k keys.Key) error {
	if _, err := s.db.ExecContext(ctx, PG(keys.SQLAdd),
		k.Hash[:], k.AccountID, k.Prefix, k.Label, k.CreatedAt, k.LastUsed, k.RequestCount, boolToInt(k.Revoked)); err != nil {
		return fmt.Errorf("pgstore: insert key for account %s: %w", k.AccountID, err)
	}
	return nil
}

// Lookup returns the stored key for a hash, or false when the hash is not a
// known key. A query error - including a canceled request context - is
// returned wrapped, never reported as an unknown key: the caller would turn
// it into a 401 instead of a 500.
func (s *KeysStore) Lookup(ctx context.Context, h [32]byte) (keys.Key, bool, error) {
	var k keys.Key
	var revoked int64
	err := s.db.QueryRowContext(ctx, PG(keys.SQLLookup), h[:]).
		Scan(&k.AccountID, &k.Prefix, &k.Label, &k.CreatedAt, &k.LastUsed, &k.RequestCount, &revoked)
	if err == sql.ErrNoRows {
		return keys.Key{}, false, nil
	}
	if err != nil {
		return keys.Key{}, false, fmt.Errorf("pgstore: key lookup failed: %w", err)
	}
	k.Hash = h
	k.Revoked = revoked == 1
	return k, true, nil
}

// ListByAccount returns every key stored for accountID, oldest first.
func (s *KeysStore) ListByAccount(ctx context.Context, accountID string) ([]keys.Key, error) {
	rows, err := s.db.QueryContext(ctx, PG(keys.SQLListByAccount), accountID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list keys for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []keys.Key{}
	for rows.Next() {
		var k keys.Key
		var hash []byte
		var revoked int64
		if err := rows.Scan(&hash, &k.AccountID, &k.Prefix, &k.Label, &k.CreatedAt, &k.LastUsed, &k.RequestCount, &revoked); err != nil {
			return nil, fmt.Errorf("pgstore: scan key for account %s: %w", accountID, err)
		}
		if len(hash) != 32 {
			return nil, fmt.Errorf("pgstore: scan key for account %s: key_hash is %d bytes, want 32", accountID, len(hash))
		}
		copy(k.Hash[:], hash)
		k.Revoked = revoked == 1
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list keys for account %s: %w", accountID, err)
	}
	return out, nil
}

// RevokeByPrefix marks the key with the given prefix revoked when it belongs
// to accountID and reports whether a row was changed. A prefix owned by
// another account affects no row, so the caller cannot learn it exists.
func (s *KeysStore) RevokeByPrefix(ctx context.Context, accountID, prefix string) (bool, error) {
	res, err := s.db.ExecContext(ctx, PG(keys.SQLRevokeByPrefix), accountID, prefix)
	if err != nil {
		return false, fmt.Errorf("pgstore: revoke prefix %s for account %s: %w", prefix, accountID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pgstore: revoke prefix %s for account %s: %w", prefix, accountID, err)
	}
	return n > 0, nil
}

// Touch records one authenticated use of a key: last_used is set to now and
// request_count is incremented. It returns an error instead of panicking,
// because it runs on the request path against a database shared with the
// meter checkpoint and a transient lock error must not kill slotfared.
func (s *KeysStore) Touch(ctx context.Context, h [32]byte) error {
	if _, err := s.db.ExecContext(ctx, PG(keys.SQLTouch), time.Now().Unix(), h[:]); err != nil {
		return fmt.Errorf("pgstore: touch key: %w", err)
	}
	return nil
}

// Close closes the underlying pool.
func (s *KeysStore) Close() error {
	return s.db.Close()
}

// boolToInt stores a bool as 1 or 0, matching the SQLite driver.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
