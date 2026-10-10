// The market event indexer: Slotfare's market event feed
// (docs/decisions.md D17). Solana's programs emit no events, so the
// indexer replays the ledger program's transaction history instead, parses
// each program instruction from the confirmed transaction envelope, and
// reports market activity - listings, purchases, cancellations - to the
// store, which the console then serves. Deposits to the treasury never come
// through here: the deposit watcher owns that surface
// (internal/solana/watcher.go), and a purchase that is also a deposit would
// be double-accounted.
//
// Idempotency. The unit of exactly-once is the transaction signature: a
// store that has already accepted signature S rejects it outright (the
// market table INSERT OR IGNOREs on the listing's listed/sold/cancelled
// signature), so replaying the same transaction is a no-op, and a stop and
// re-scan from slot zero converges. An unknown signature is only applied
// when its slot is at or beyond the cursor, so time can never flow
// backwards through the store even if signature history pages arrive out of
// order.
package solana

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// MarketEvent is one market instruction observed on chain, resolved to the
// console's listing rows' vocabulary. Kind is one of "list", "sold",
// "cancelled".
type MarketEvent struct {
	Kind string
	// Signature is the Solana transaction that carried the instruction, and
	// the event's identity: the same signature is never reported twice.
	Signature string
	// Slot the transaction landed in.
	Slot uint64
	// At is the block time in UTC (or scan time when the cluster strips it).
	At time.Time
	// Seller is the base58 wallet that listed; Buyer base58 on a sale.
	Seller solana.PublicKey
	Buyer  solana.PublicKey
	// Nonce identifies the listing PDA [b"listing", seller, nonce].
	Nonce uint64
	// Credits listed, and Price in pay-token base units.
	Credits uint64
	Price   uint64
	// PayMint the listing is priced in.
	PayMint solana.PublicKey
}

// MarketIndexerStore is the write side the indexer feeds: create the listing
// row on a fresh list, then move the open row with the guarded SQL moves the
// console's own write routes use (SQLCancelListing / SQLSoldListing), so the
// indexer and the console can never disagree about a row's state.
type MarketIndexerStore interface {
	// RecordListing mirrors one List into a row keyed by the on-chain
	// ListingID, ignoring a duplicate (the idempotency for re-scan).
	RecordListing(ctx context.Context, l ConsoleListingForIndex) (uint64, error)
	// CancelListing / SoldListing move an open row; false = already moved
	// (a repeated replay of the same sale or cancel, or a stale one).
	CancelListingBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64, sig string, slot uint64, at string) (bool, error)
	SoldListingBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64, buyer string, sig string, slot uint64, at string) (bool, error)
	// ListingExists reports whether this program listing already has a row
	// (idempotency shortcut for buys of already-recorded lists).
	ListingExistsBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64) (bool, error)
}

// ConsoleListingForIndex is the field set the indexer fills for a new
// listing row; the console's adapter (internal/console MarketMirror) copies
// it into console.Listing. Defined here because the indexer fills it, and
// naming it avoids importing the console package back.
type ConsoleListingForIndex struct {
	ListingID   uint64 // the program listing PDA's last 8 bytes, LE
	Account     string // seller's account id (0x + hex pubkey)
	Seller      string // base58
	Credits     uint64
	PayToken    string
	Price       uint64
	ListedTx    string
	ListedBlock uint64
	ListedAt    string
	Nonce       uint64
}

// Indexer replays the ledger program's transaction history into the store.
// It owns only reads - signatures for the program address, then the
// transaction envelopes - so it runs against any confirmed RPC.
type Indexer struct {
	client  *rpc.Client
	program solana.PublicKey
	store   MarketIndexerStore

	// resolve turns a listing PDA into its seller and nonce. It defaults to
	// resolveListing's chain read and is injectable so tests can run the
	// parser with no RPC at all.
	resolve func(addr solana.PublicKey) (solana.PublicKey, uint64, bool)

	// cursor is the newest slot already applied; signatures at or below it
	// are skipped. Zero means "first pass, take everything".
	cursor uint64
	// seen is the in-process signature memo: the same signature is never
	// parsed twice within a pass. Across passes, the store's signature
	// uniqueness carries the guarantee.
	seen map[string]bool
}

// NewIndexer returns an indexer over the ledger program. store may be nil
// for a scan-only deployment (health card only, no market mirror).
func NewIndexer(c *Client, store MarketIndexerStore) (*Indexer, error) {
	if !c.LedgerEnabled() {
		return nil, fmt.Errorf("solana: indexer requires SLOTFARE_LEDGER_PROGRAM")
	}
	ix := &Indexer{
		client:  c.RPC(),
		program: c.LedgerProgram(),
		store:   store,
		seen:    map[string]bool{},
	}
	ix.resolve = ix.resolveListing
	return ix, nil
}

// RunOnce performs one full-history pass and returns the applied event
// count. Errors from the RPC abort the pass; the caller decides whether to
// keep going (half-applied state is impossible - each event commits
// independently, and re-running converges).
func (ix *Indexer) RunOnce(ctx context.Context) (int, error) {
	ix.seen = map[string]bool{}
	applied := 0
	var before solana.Signature
	limit := 1000
	for {
		page, err := ix.client.GetSignaturesForAddressWithOpts(ctx, ix.program, &rpc.GetSignaturesForAddressOpts{
			Limit:  &limit,
			Before: before,
		})
		if err != nil {
			return applied, fmt.Errorf("solana: indexer signatures: %w", err)
		}
		for _, info := range page {
			sig := info.Signature.String()
			if info.Err != nil || ix.seen[sig] {
				continue // failed transactions moved nothing
			}
			ix.seen[sig] = true
			at := time.Now().UTC()
			if info.BlockTime != nil {
				at = time.Unix(int64(*info.BlockTime), 0).UTC()
			}
			tx, err := ix.transactionOf(ctx, info.Signature)
			if err != nil {
				return applied, fmt.Errorf("solana: indexer tx %s: %w", sig, err)
			}
			if tx == nil {
				continue // pruned from history; the next full pass retries
			}
			events, err := ix.parseTransaction(tx, sig, info.Slot, at)
			if err != nil {
				return applied, err
			}
			for _, ev := range events {
				if err := ix.apply(ctx, ev); err != nil {
					return applied, err
				}
				applied++
			}
			if info.Slot > ix.cursor {
				ix.cursor = info.Slot
			}
		}
		if len(page) < 1000 {
			return applied, nil
		}
		before = page[len(page)-1].Signature
	}
}

// transactionOf fetches one confirmed transaction in the wire form the
// parser needs: JSON encoding, so the envelope decodes to a full
// *solana.Transaction with the message's account keys inline (the watcher
// uses the same shape for its token-balance diff). v0 is the only version
// address-lookup-table transactions use.
func (ix *Indexer) transactionOf(ctx context.Context, sig solana.Signature) (*solana.Transaction, error) {
	maxVer := uint64(0)
	tx, err := ix.client.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Encoding:                       solana.EncodingJSON,
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &maxVer,
	})
	if err != nil {
		return nil, err
	}
	if tx == nil || tx.Transaction == nil {
		return nil, nil
	}
	parsed, err := tx.Transaction.GetTransaction()
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return parsed, nil
}

// parseTransaction extracts the slotfare-ledger market instructions from a
// confirmed transaction, offline-testable: it takes the already-decoded
// transaction, so a test can build a synthetic one. It works on the wire
// form: instruction data byte 0 is the program tag, matching the tags the
// Go client and the Rust program both pin (TAG_LIST=7, TAG_BUY=8,
// TAG_CANCEL_LIST=9).
func (ix *Indexer) parseTransaction(tx *solana.Transaction, sig string, slot uint64, at time.Time) ([]MarketEvent, error) {
	accounts := tx.Message.AccountKeys
	var out []MarketEvent
	for _, ixn := range tx.Message.Instructions {
		programIdx := int(ixn.ProgramIDIndex)
		if programIdx >= len(accounts) || accounts[programIdx] != ix.program {
			continue // not our program
		}
		data := []byte(ixn.Data)
		if len(data) == 0 {
			continue
		}
		// Resolve the instruction's account list against the message's keys.
		idxs := ixn.Accounts
		acct := func(i uint16) solana.PublicKey {
			if int(i) < len(accounts) {
				return accounts[i]
			}
			return solana.PublicKey{}
		}
		switch data[0] {
		case ixLedgerList:
			if len(data) < 57 || len(idxs) < 4 {
				continue
			}
			credits := binary.LittleEndian.Uint64(data[1:9])
			price := binary.LittleEndian.Uint64(data[9:17])
			nonce := binary.LittleEndian.Uint64(data[17:25])
			var payMint solana.PublicKey
			copy(payMint[:], data[25:57])
			seller := acct(idxs[0])
			out = append(out, MarketEvent{
				Kind: "list", Signature: sig, Slot: slot, At: at,
				Seller: seller, Nonce: nonce, Credits: credits,
				Price: price, PayMint: payMint,
			})
		case ixLedgerBuy:
			// Accounts: [buyer (signer), buyer_credit, seller_credit, listing,
			// escrow, buyer_token, seller_token, mint, token_program]. The
			// seller is read out of the listing account, not the instruction,
			// so a forged term cannot redirect the sale. Resolving it needs a
			// chain read per event; RunOnce callers pass a resolver.
			if len(idxs) < 4 {
				continue
			}
			buyer := acct(idxs[0])
			listingAddr := acct(idxs[3])
			seller, nonce, ok := ix.resolve(listingAddr)
			if !ok {
				continue
			}
			out = append(out, MarketEvent{
				Kind: "sold", Signature: sig, Slot: slot, At: at,
				Seller: seller, Buyer: buyer, Nonce: nonce,
			})
		case ixLedgerCancelList:
			// Accounts: [seller (signer), listing, escrow, seller_credit].
			if len(idxs) < 2 {
				continue
			}
			seller := acct(idxs[0])
			listingAddr := acct(idxs[1])
			_, nonce, ok := ix.resolve(listingAddr)
			if !ok {
				continue
			}
			out = append(out, MarketEvent{
				Kind: "cancelled", Signature: sig, Slot: slot, At: at,
				Seller: seller, Nonce: nonce,
			})
		}
	}
	return out, nil
}

// resolveListing reads a listing PDA off chain for its seller and nonce, so
// a sale or cancel names its listing unambiguously. A listing the program
// has already closed out (state != open) is still readable, since the
// program never deletes the account.
func (ix *Indexer) resolveListing(addr solana.PublicKey) (seller solana.PublicKey, nonce uint64, ok bool) {
	data, err := ix.client.GetAccountInfoWithOpts(context.Background(), addr, &rpc.GetAccountInfoOpts{
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil || data == nil || data.Value == nil || len(data.Value.Data.GetBinary()) < 125 {
		return solana.PublicKey{}, 0, false
	}
	l, err := DecodeLedgerListing(data.Value.Data.GetBinary())
	if err != nil {
		return solana.PublicKey{}, 0, false
	}
	var sellerKey solana.PublicKey
	copy(sellerKey[:], l.Seller[:])
	return sellerKey, l.Nonce, true
}

// apply pushes one parsed event into the store. A nil store (scan-only
// deployment) counts the event without writing anything.
func (ix *Indexer) apply(ctx context.Context, ev MarketEvent) error {
	if ix.store == nil {
		return nil
	}
	accountID := AccountIDFor(ev.Seller)
	switch ev.Kind {
	case "list":
		listingID := ix.listingIDFor(ev.Seller, ev.Nonce)
		_, err := ix.store.RecordListing(ctx, ConsoleListingForIndex{
			ListingID:   listingID,
			Account:     accountID,
			Seller:      ev.Seller.String(),
			Credits:     ev.Credits,
			PayToken:    ev.PayMint.String(),
			Price:       ev.Price,
			ListedTx:    ev.Signature,
			ListedBlock: ev.Slot,
			ListedAt:    ev.At.Format(time.RFC3339),
			Nonce:       ev.Nonce,
		})
		if err != nil {
			return fmt.Errorf("solana: indexer record listing %s: %w", ev.Signature, err)
		}
	case "cancelled":
		if _, err := ix.store.CancelListingBySeller(ctx, ev.Seller, ev.Nonce, ev.Signature, ev.Slot, ev.At.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("solana: indexer cancel listing %s: %w", ev.Signature, err)
		}
	case "sold":
		buyer := ""
		if ev.Buyer != (solana.PublicKey{}) {
			buyer = ev.Buyer.String()
		}
		if _, err := ix.store.SoldListingBySeller(ctx, ev.Seller, ev.Nonce, buyer, ev.Signature, ev.Slot, ev.At.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("solana: indexer sold listing %s: %w", ev.Signature, err)
		}
	default:
		log.Printf("solana: indexer: unknown event kind %q in %s", ev.Kind, ev.Signature)
	}
	return nil
}

// listingIDFor derives the store's listing_id for a program listing: the
// last 8 bytes of the listing PDA, little-endian - stable across re-scan,
// so the same listing always lands on the same row and the INSERT OR
// IGNORE dedupes replays. The console's MarketMirror re-derives exactly
// this value, which is what pins both sides to one row per listing.
func (ix *Indexer) listingIDFor(seller solana.PublicKey, nonce uint64) uint64 {
	addr, err := LedgerListingAddress(ix.program, seller, nonce)
	if err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(addr[24:32])
}
