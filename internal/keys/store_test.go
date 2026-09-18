package keys

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreAddLookupRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	k := Issue("slotfare-secret-001")
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok, err := s.Lookup(ctx, k.Hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup returned false for a stored key")
	}
	if got != k {
		t.Errorf("Lookup = %+v, want %+v", got, k)
	}
}

func TestStoreLookupAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var missing [32]byte
	if _, ok, err := s.Lookup(context.Background(), missing); err != nil || ok {
		t.Errorf("Lookup of an absent hash = (ok=%v, err=%v), want false, nil", ok, err)
	}
}

func TestStoreDuplicateAddErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	k := Issue("slotfare-secret-001")
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := s.Add(ctx, k); err == nil {
		t.Error("duplicate Add succeeded, want an error")
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	k := Issue("slotfare-secret-001")
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, ok, err := s2.Lookup(ctx, k.Hash)
	if err != nil {
		t.Fatalf("Lookup after restart: %v", err)
	}
	if !ok {
		t.Fatal("key lost across restart")
	}
	if got != k {
		t.Errorf("after restart Lookup = %+v, want %+v", got, k)
	}
}

func TestStoreRevokeByPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	k := Key{Hash: HashKey("secret-a"), AccountID: "0xaccount-a", Prefix: "attk_deadbeef"}
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("Add: %v", err)
	}

	revoked, err := s.RevokeByPrefix(ctx, "0xaccount-a", "attk_deadbeef")
	if err != nil {
		t.Fatalf("RevokeByPrefix: %v", err)
	}
	if !revoked {
		t.Error("RevokeByPrefix for the owning account returned false, want true")
	}
	got, ok, err := s.Lookup(ctx, k.Hash)
	if err != nil {
		t.Fatalf("Lookup after revoke: %v", err)
	}
	if !ok || !got.Revoked {
		t.Errorf("after revoke Lookup = %+v (ok=%v), want Revoked true", got, ok)
	}

	// The same prefix owned by another account is not the caller's key.
	revoked, err = s.RevokeByPrefix(ctx, "0xaccount-b", "attk_deadbeef")
	if err != nil {
		t.Fatalf("RevokeByPrefix foreign account: %v", err)
	}
	if revoked {
		t.Error("RevokeByPrefix for a different account returned true, want false")
	}
}

func TestStoreTouchIncrementsUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	k := Key{Hash: HashKey("secret-a"), AccountID: "0xaccount-a", Prefix: "attk_deadbeef"}
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.Touch(ctx, k.Hash); err != nil {
			t.Fatalf("Touch %d: %v", i+1, err)
		}
	}

	list, err := s.ListByAccount(ctx, "0xaccount-a")
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListByAccount returned %d keys, want 1", len(list))
	}
	if list[0].RequestCount != 3 {
		t.Errorf("request_count = %d, want 3", list[0].RequestCount)
	}
	if list[0].LastUsed == 0 {
		t.Error("last_used not set by Touch")
	}
}

// TestStoreLookupCanceledContext proves a canceled request context - a
// client hangup mid-flight - comes back as a wrapped error, never a panic.
func TestStoreLookupCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	k := Issue("slotfare-secret-canceled")
	if err := s.Add(ctx, k); err != nil {
		t.Fatalf("Add: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, ok, err := s.Lookup(canceled, k.Hash)
	if ok {
		t.Error("Lookup on a canceled context returned true")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want context.Canceled", err)
	}
}

// TestStoreLookupGenuineErrorSurfaces proves a real database failure is
// returned as an error the caller can distinguish from a canceled context.
func TestStoreLookupGenuineErrorSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var h [32]byte
	_, ok, err := s.Lookup(context.Background(), h)
	if ok {
		t.Error("Lookup after Close returned true")
	}
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want a genuine (non-canceled) error", err)
	}
}
