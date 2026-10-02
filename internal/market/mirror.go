package market

import (
	"context"
	"encoding/binary"
	"fmt"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/solana"
)

// Indexer is the indexer-facing shape the console's mirror satisfies.
// Kept here rather than in internal/solana so the package that owns the
// ledger constants never imports the console back; the indexer names the
// raw plumbing MarketIndexerStore, and this is the console's view of it.
type Indexer interface {
	RecordListing(ctx context.Context, l solana.ConsoleListingForIndex) (uint64, error)
	CancelListingBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64, sig string, slot uint64, at string) (bool, error)
	SoldListingBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64, buyer string, sig string, slot uint64, at string) (bool, error)
	ListingExistsBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64) (bool, error)
}

// Mirror adapts the console's FundingStore to the indexer's contract,
// without widening the store interface: it derives the same listing_id the
// indexer uses for INSERTs from the listing PDA's last 8 little-endian
// bytes, then falls back to the store's guarded, id-based moves
// (SQLCancelListing / SQLSoldListing). Both sides agreeing on the one
// derivation is what makes a replay an exact no-op: re-recording IGNOREs a
// duplicate id, and a second cancel or sale of the same id loses the
// state='open' guard and reports "already moved".
type Mirror struct {
	Store Store
	// Program is the ledger program whose listing PDAs the rows mirror.
	Program gsolana.PublicKey
}

var _ Indexer = Mirror{}

// ListingIDFor re-derives the store's listing_id from the listing PDA: the
// last 8 bytes little-endian. A rescan of the same on-chain listing always
// lands on the same id, which is the dedupe key the listings INSERT OR
// IGNORE guards on (console and pgstore drivers alike).
func (m Mirror) ListingIDFor(seller gsolana.PublicKey, nonce uint64) (uint64, error) {
	addr, err := solana.LedgerListingAddress(m.Program, seller, nonce)
	if err != nil {
		return 0, fmt.Errorf("market: listing address for seller %s nonce %d: %w", seller, nonce, err)
	}
	return binary.LittleEndian.Uint64(addr[24:32]), nil
}

// RecordListing mirrors one List into the listings table, leaving the
// token-symbol columns at the store's defaults; the market surfaces resolve
// unknown symbols out of the pay mint when they read the row.
func (m Mirror) RecordListing(ctx context.Context, l solana.ConsoleListingForIndex) (uint64, error) {
	row := Listing{
		ListingID:   l.ListingID,
		Nonce:       l.Nonce,
		Account:     l.Account,
		Seller:      l.Seller,
		Credits:     l.Credits,
		PayToken:    l.PayToken,
		Price:       l.Price,
		ListedTx:    l.ListedTx,
		ListedBlock: l.ListedBlock,
		ListedAt:    l.ListedAt,
	}
	id, err := m.Store.RecordListing(ctx, row)
	return id, err
}

// CancelListingBySeller resolves the (seller, nonce) pair to its listing
// row and applies the same guarded move a frontend-driven cancel uses, so
// an indexer replay can never overwrite a state a real move already set.
func (m Mirror) CancelListingBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64, sig string, slot uint64, at string) (bool, error) {
	id, ok, err := m.find(ctx, seller, nonce)
	if err != nil || !ok {
		return false, err
	}
	return m.Store.CancelListing(ctx, id, sig, slot, at)
}

// SoldListingBySeller resolves the (seller, nonce) pair and applies the
// guarded sale. The buyer is recorded as the console account the buyer's
// wallet maps to: the same 0x-hex AccountIDFor form.
func (m Mirror) SoldListingBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64, buyer string, sig string, slot uint64, at string) (bool, error) {
	id, ok, err := m.find(ctx, seller, nonce)
	if err != nil || !ok {
		return false, err
	}
	return m.Store.SoldListing(ctx, id, buyer, sig, slot, at)
}

// ListingExistsBySeller reads the row by the PDA-derived id and reports
// whether it exists at all. The guarded moves handle every other case.
func (m Mirror) ListingExistsBySeller(ctx context.Context, seller gsolana.PublicKey, nonce uint64) (bool, error) {
	_, ok, err := m.find(ctx, seller, nonce)
	return ok, err
}

func (m Mirror) find(ctx context.Context, seller gsolana.PublicKey, nonce uint64) (uint64, bool, error) {
	id, err := m.ListingIDFor(seller, nonce)
	if err != nil {
		return 0, false, err
	}
	row, err := m.Store.ListingByID(ctx, id)
	if err != nil {
		return 0, false, err
	}
	if row.ListingID != id {
		return 0, false, nil
	}
	return id, true, nil
}
