package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a store on a throwaway SQLite file.
func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A session round-trips exactly as stored and Lookup reports unknown hashes
// as "not found", not as an error.
func TestSessionRoundtripAndUnknownHash(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	want := Session{
		Hash:      [32]byte{1, 2, 3},
		AccountID: "0xabc",
		IssuedAt:  100,
		ExpiresAt: 200,
		LastSeen:  150,
	}
	if err := s.Save(ctx, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Lookup(ctx, want.Hash)
	if err != nil || !ok {
		t.Fatalf("Lookup = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if got.AccountID != want.AccountID || got.IssuedAt != want.IssuedAt ||
		got.ExpiresAt != want.ExpiresAt || got.LastSeen != want.LastSeen {
		t.Fatalf("Lookup = %+v, want %+v", got, want)
	}
	if _, ok, err := s.Lookup(ctx, [32]byte{9}); err != nil || ok {
		t.Fatalf("unknown hash Lookup = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	if n, err := s.Revoke(ctx, want.Hash); err != nil || !n {
		t.Fatalf("Revoke = (%v, %v), want (true, nil)", n, err)
	}
	if _, ok, err := s.Lookup(ctx, want.Hash); err != nil || ok {
		t.Fatalf("after Revoke Lookup = (_, %v, %v), want (_, false, nil)", ok, err)
	}
}

// A nonce is single use: the first Take removes it, the second finds
// nothing. A new SaveNonce replaces whatever the address held.
func TestNonceIsSingleUseAndReplacedOnSave(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	addr := "So1anaAddress"
	first := Nonce{Address: addr, Nonce: "n1", IssuedAt: 1, ExpiresAt: 100}
	if err := s.SaveNonce(ctx, first); err != nil {
		t.Fatalf("SaveNonce: %v", err)
	}
	got, ok, err := s.TakeNonce(ctx, addr)
	if err != nil || !ok {
		t.Fatalf("TakeNonce = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if got.Nonce != "n1" || got.IssuedAt != 1 || got.ExpiresAt != 100 {
		t.Fatalf("TakeNonce = %+v, want %+v", got, first)
	}
	if _, ok, err := s.TakeNonce(ctx, addr); err != nil || ok {
		t.Fatalf("second TakeNonce = (_, %v, %v), want (_, false, nil)", ok, err)
	}
	// Replacement: the newest nonce wins and the old one is gone.
	second := Nonce{Address: addr, Nonce: "n2", IssuedAt: 2, ExpiresAt: 200}
	if err := s.SaveNonce(ctx, first); err != nil {
		t.Fatalf("re-SaveNonce: %v", err)
	}
	if err := s.SaveNonce(ctx, second); err != nil {
		t.Fatalf("SaveNonce replace: %v", err)
	}
	got, ok, err = s.TakeNonce(ctx, addr)
	if err != nil || !ok || got.Nonce != "n2" {
		t.Fatalf("TakeNonce after replace = (%+v, %v, %v), want n2", got, ok, err)
	}
}

// Touch slides expiry to now + TTL but writes at most once a minute, so the
// request path does not hit the store on every call.
func TestTouchSlidesExpiryAtMostOnceAMinute(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	h := [32]byte{7}
	if err := s.Save(ctx, Session{Hash: h, AccountID: "0xa", IssuedAt: 0, ExpiresAt: 500}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if err := s.Touch(ctx, h, now); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, ok, err := s.Lookup(ctx, h)
	if err != nil || !ok {
		t.Fatalf("Lookup = (_, %v, %v)", ok, err)
	}
	if got.ExpiresAt != now+int64(TTL.Seconds()) || got.LastSeen != now {
		t.Fatalf("after Touch ExpiresAt=%d LastSeen=%d, want %d and %d",
			got.ExpiresAt, got.LastSeen, now+int64(TTL.Seconds()), now)
	}
	// Inside the throttle window the write is a no-op.
	if err := s.Touch(ctx, h, now+30); err != nil {
		t.Fatalf("Touch in window: %v", err)
	}
	again, _, _ := s.Lookup(ctx, h)
	if again.ExpiresAt != got.ExpiresAt || again.LastSeen != got.LastSeen {
		t.Fatalf("Touch inside a minute changed the row: %+v -> %+v", got, again)
	}
	// Past the window it slides again.
	if err := s.Touch(ctx, h, now+120); err != nil {
		t.Fatalf("Touch after window: %v", err)
	}
	third, _, _ := s.Lookup(ctx, h)
	if third.LastSeen != now+120 || third.ExpiresAt != now+120+int64(TTL.Seconds()) {
		t.Fatalf("Touch after a minute did not slide: %+v", third)
	}
}

// SweepExpired removes expired sessions and nonces in one pass and reports
// the session count; live rows stay.
func TestSweepExpiredRemovesSessionsAndNonces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := int64(1_000_000)
	dead := Session{Hash: [32]byte{1}, AccountID: "dead", ExpiresAt: now - 1}
	live := Session{Hash: [32]byte{2}, AccountID: "live", ExpiresAt: now + 1}
	for _, rec := range []Session{dead, live} {
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	if err := s.SaveNonce(ctx, Nonce{Address: "a", Nonce: "old", ExpiresAt: now - 1}); err != nil {
		t.Fatalf("SaveNonce: %v", err)
	}
	n, err := s.SweepExpired(ctx, now)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepExpired = %d, want 1", n)
	}
	if _, ok, err := s.Lookup(ctx, dead.Hash); err != nil || ok {
		t.Fatalf("expired session still present (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := s.Lookup(ctx, live.Hash); err != nil || !ok {
		t.Fatalf("live session swept (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := s.TakeNonce(ctx, "a"); err != nil || ok {
		t.Fatalf("expired nonce still present (ok=%v, err=%v)", ok, err)
	}
}
