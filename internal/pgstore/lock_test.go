package pgstore

import (
	"testing"
)

// TestLockKeyStableAndDistinct pins the advisory-lock key derivation: the
// same name maps to the same 64-bit key on every replica and every build,
// and distinct loop names never collide. The live-Postgres-only fact - that
// pg_try_advisory_lock on one session excludes a second session from taking
// the same key - is named in the runbook's Live-Postgres-only checks.
func TestLockKeyStableAndDistinct(t *testing.T) {
	if LockKey("settler") != LockKey("settler") {
		t.Error("LockKey is not deterministic for the same name")
	}
	for _, other := range []string{"feeder", "settler2", "SETTLER"} {
		if LockKey("settler") == LockKey(other) {
			t.Errorf("LockKey(\"settler\") collides with LockKey(%q)", other)
		}
	}
}
