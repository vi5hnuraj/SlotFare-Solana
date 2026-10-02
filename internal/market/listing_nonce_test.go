package market

import (
	"context"
	"path/filepath"
	"testing"
)

// A listing nonce is a full 64-bit draw - half of them exceed int64max, and
// the row's own PDA is derived from it, so every read must return the exact
// value it was written with. The column stores the nonce as a decimal
// string; this walks all three row scans through it.
func TestListingNonceRoundTripsFullWidth(t *testing.T) {
	ctx := context.Background()
	fs, err := NewStore(filepath.Join(t.TempDir(), "market.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { fs.Close() })

	const nonce = uint64(0xa8cbfed5a12be772) // 12163095412129654642 > int64max
	id, err := fs.RecordListing(ctx, Listing{
		Account:  "0xnonce",
		Seller:   "SellerKey1111111111111111111111111111111111",
		Credits:  10,
		Price:    7,
		Nonce:    nonce,
		State:    "open",
		ListedAt: "2026-10-11T00:00:00Z",
	})
	if err != nil || id == 0 {
		t.Fatalf("RecordListing = id %d, err %v; want a fresh id", id, err)
	}

	byID, err := fs.ListingByID(ctx, id)
	if err != nil {
		t.Fatalf("ListingByID: %v", err)
	}
	if byID.Nonce != nonce {
		t.Errorf("ListingByID nonce = %d, want %d", byID.Nonce, nonce)
	}

	open, err := fs.OpenListings(ctx, 10, 0)
	if err != nil {
		t.Fatalf("OpenListings: %v", err)
	}
	if len(open) != 1 || open[0].Nonce != nonce {
		t.Errorf("OpenListings = %+v, want one row with nonce %d", open, nonce)
	}

	acct, err := fs.AccountListings(ctx, "0xnonce", 10, 0)
	if err != nil {
		t.Fatalf("AccountListings: %v", err)
	}
	if len(acct) != 1 || acct[0].Nonce != nonce {
		t.Errorf("AccountListings = %+v, want one row with nonce %d", acct, nonce)
	}
}
