package pgstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"slotfare/internal/market"
	"testing"
)

// TestRecordListingFullWidthID runs pgSQLRecordListing - the exact
// statement the indexer and confirmList execute - with a PDA-derived id
// above int64max (12685038934131738550, the id that failed to encode into
// the signed column and wedged the indexer). The id travels as its wrapped
// int64, so the row round-trips through every read the market routes take.
func TestRecordListingFullWidthID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "funding.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open stand-in pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	fs := &FundingStore{db: db}

	const id = uint64(0xaf0a4fb3ceec07b6) // 12685038934131738550 > int64max
	got, err := fs.RecordListing(ctx, market.Listing{
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

	// A replay of the same on-chain listing must dedupe on the id instead
	// of racing a second row (and a second lock) for one escrow.
	again, err := fs.RecordListing(ctx, market.Listing{
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
	if err != nil || again != 0 {
		t.Errorf("RecordListing replay = id %d, err %v; want a no-op (0, nil)", again, err)
	}

	moved, err := fs.CancelListing(ctx, id, "0xcancel", 1, "2026-10-11T00:01:00Z")
	if err != nil || !moved {
		t.Fatalf("CancelListing = %v, %v; want the row to move", moved, err)
	}
}
