package market

import (
	"context"
	"path/filepath"
	"testing"
)

// A listing id is the listing PDA's last 8 bytes: a full uint64, and half
// of them exceed int64max - 12685038934131738550 is the one that failed to
// encode into the signed column and wedged the indexer. The id travels as
// its wrapped int64 at the SQL boundary; this walks every read and guarded
// move through it, which is the round trip the confirm route and the
// indexer's replay both take to one row.
func TestListingIDRoundTripsFullWidth(t *testing.T) {
	ctx := context.Background()
	fs, err := NewStore(filepath.Join(t.TempDir(), "market.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { fs.Close() })

	const id = uint64(0xaf0a4fb3ceec07b6) // 12685038934131738550 > int64max
	got, err := fs.RecordListing(ctx, Listing{
		ListingID: id,
		Account:   "0xbig",
		Seller:    "SellerKey1111111111111111111111111111111111",
		Credits:   500,
		Price:     1_000_000,
		Nonce:     7,
		State:     "open",
		ListedTx:  "0xlist",
		ListedAt:  "2026-10-11T00:00:00Z",
	})
	if err != nil || got != id {
		t.Fatalf("RecordListing = id %d, err %v; want the explicit id %d", got, err, id)
	}

	byID, err := fs.ListingByID(ctx, id)
	if err != nil {
		t.Fatalf("ListingByID: %v", err)
	}
	if byID.ListingID != id || byID.Credits != 500 {
		t.Errorf("ListingByID = %+v, want id %d with 500 credits", byID, id)
	}

	owner, err := fs.AccountForListing(ctx, id)
	if err != nil || owner != "0xbig" {
		t.Errorf("AccountForListing = %q, %v; want 0xbig", owner, err)
	}

	open, err := fs.OpenListings(ctx, 10, 0)
	if err != nil {
		t.Fatalf("OpenListings: %v", err)
	}
	if len(open) != 1 || open[0].ListingID != id {
		t.Errorf("OpenListings = %+v, want one row with id %d", open, id)
	}

	acct, err := fs.AccountListings(ctx, "0xbig", 10, 0)
	if err != nil {
		t.Fatalf("AccountListings: %v", err)
	}
	if len(acct) != 1 || acct[0].ListingID != id {
		t.Errorf("AccountListings = %+v, want one row with id %d", acct, id)
	}

	moved, err := fs.CancelListing(ctx, id, "0xcancel", 1, "2026-10-11T00:01:00Z")
	if err != nil || !moved {
		t.Fatalf("CancelListing = %v, %v; want the row to move", moved, err)
	}
	after, err := fs.OpenListings(ctx, 10, 0)
	if err != nil {
		t.Fatalf("OpenListings after cancel: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("OpenListings after cancel = %+v, want empty", after)
	}
}
