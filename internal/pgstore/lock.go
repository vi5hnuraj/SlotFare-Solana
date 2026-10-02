package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"

	"slotfare/internal/meter"
)

// AdvisoryLocker takes Postgres session advisory locks, the single-flight
// primitive slotfared's background loops share across replicas: the lock lives
// on a dedicated connection from the pool, so it is released with that
// connection - when the holder process dies, Postgres closes its sessions
// and any other replica takes the lock on its next tick. No k8s API leader
// election; the database is the coordinator.
type AdvisoryLocker struct {
	db *sql.DB
}

var _ meter.Locker = (*AdvisoryLocker)(nil)

// NewAdvisoryLocker returns a Locker over db's pool.
func NewAdvisoryLocker(db *sql.DB) *AdvisoryLocker {
	return &AdvisoryLocker{db: db}
}

// LockKey maps a loop name to a stable 64-bit advisory-lock key: every
// replica computes the same key for the same name, so the lock is shared
// across the deployment and stable across builds.
func LockKey(name string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "slotfared/%s", name)
	return int64(h.Sum64())
}

// Try attempts to take the named lock on a dedicated connection. ok is
// false when another replica holds it; the returned release unlocks on the
// same connection and returns it to the pool.
func (l *AdvisoryLocker) Try(ctx context.Context, key string) (func(context.Context) error, bool, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("pgstore: advisory lock %s: %w", key, err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, LockKey(key)).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("pgstore: advisory lock %s: %w", key, err)
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}
	release := func(ctx context.Context) error {
		var unlocked bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, LockKey(key)).Scan(&unlocked); err != nil {
			_ = conn.Close()
			return fmt.Errorf("pgstore: advisory unlock %s: %w", key, err)
		}
		return conn.Close()
	}
	return release, true, nil
}
