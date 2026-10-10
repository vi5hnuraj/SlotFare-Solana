package solana

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

// The tests build synthetic transactions directly against the wire format
// ledger_market.rs and instruction.rs pin, so the parser is exercised
// offline: no RPC, no validator. The store half is a recording stub, which
// is where the idempotency contract lives (replaying the same signature
// must move each row at most once).

type recordedEvent struct {
	kind   string
	sig    string
	seller string
	buyer  string
	nonce  uint64
}

type stubStore struct {
	events    []recordedEvent
	cancelled map[string]bool // listing id -> moved
	sold      map[string]bool
}

func (s *stubStore) RecordListing(ctx context.Context, l ConsoleListingForIndex) (uint64, error) {
	for _, e := range s.events {
		if e.kind == "list" && e.nonce == l.Nonce {
			return l.ListingID, nil // duplicate list replay: store IGNOREs
		}
	}
	s.events = append(s.events, recordedEvent{kind: "list", sig: l.ListedTx, seller: l.Seller, nonce: l.Nonce})
	return l.ListingID, nil
}

func (s *stubStore) CancelListingBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64, sig string, slot uint64, at string) (bool, error) {
	key := seller.String() + "/" + itoa(nonce)
	if s.cancelled[key] || s.sold[key] {
		return false, nil // already moved: the guarded SQL's "no rows"
	}
	s.cancelled[key] = true
	s.events = append(s.events, recordedEvent{kind: "cancelled", sig: sig, seller: seller.String(), nonce: nonce})
	return true, nil
}

func (s *stubStore) SoldListingBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64, buyer string, sig string, slot uint64, at string) (bool, error) {
	key := seller.String() + "/" + itoa(nonce)
	if s.cancelled[key] || s.sold[key] {
		return false, nil
	}
	s.sold[key] = true
	s.events = append(s.events, recordedEvent{kind: "sold", sig: sig, seller: seller.String(), buyer: buyer, nonce: nonce})
	return true, nil
}

func (s *stubStore) ListingExistsBySeller(ctx context.Context, seller solana.PublicKey, nonce uint64) (bool, error) {
	key := seller.String() + "/" + itoa(nonce)
	return s.cancelled[key] || s.sold[key] || s.hasList(nonce), nil
}

func (s *stubStore) hasList(nonce uint64) bool {
	for _, e := range s.events {
		if e.kind == "list" && e.nonce == nonce {
			return true
		}
	}
	return false
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// mustProgram derives a fixed program key for the tests.
var testProgram = solana.MustPublicKeyFromBase58("2GiCtSotdJ5PtCRvLtkYnrd4JT5TjkkugzJzqbmDw226")

func mustWallet(t *testing.T, name string) solana.PublicKey {
	t.Helper()
	w := solana.NewWallet()
	return w.PublicKey()
}

// listTx builds one synthetic transaction carrying a List instruction with
// the exact account order ledger_market.go's LedgerListIx pins.
func listTx(t *testing.T, program, seller solana.PublicKey, credits, price, nonce uint64, payMint solana.PublicKey) *solana.Transaction {
	t.Helper()
	data := ledgerListData(credits, price, nonce, payMint)
	sellerCredit, err := LedgerCreditAddress(program, seller)
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	listing, err := LedgerListingAddress(program, seller, nonce)
	if err != nil {
		t.Fatalf("listing address: %v", err)
	}
	escrow, err := LedgerEscrowAddress(program, listing)
	if err != nil {
		t.Fatalf("escrow address: %v", err)
	}
	// Message keys, in index order: [seller, seller_credit, listing, escrow, system, program].
	keys := solana.PublicKeySlice{seller, sellerCredit, listing, escrow, solana.SystemProgramID, program}
	return &solana.Transaction{
		Message: solana.Message{
			AccountKeys: keys,
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 5,
				Accounts:       []uint16{0, 1, 2, 3, 4},
				Data:           data,
			}},
		},
	}
}

// buyTx carries the Buy instruction's exact account order, per
// LedgerBuyIx: [buyer, buyer_credit, seller_credit, listing, escrow,
// buyer_token, seller_token, mint, token_program].
func buyTx(t *testing.T, program, buyer, listing solana.PublicKey) *solana.Transaction {
	t.Helper()
	buyerCredit, err := LedgerCreditAddress(program, buyer)
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	sellerCredit, err := LedgerCreditAddress(program, testSeller)
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	escrow, err := LedgerEscrowAddress(program, listing)
	if err != nil {
		t.Fatalf("escrow address: %v", err)
	}
	buyerToken, _, err := solana.FindAssociatedTokenAddress(buyer, testPayMint)
	if err != nil {
		t.Fatalf("ata: %v", err)
	}
	sellerToken, _, err := solana.FindAssociatedTokenAddress(testSeller, testPayMint)
	if err != nil {
		t.Fatalf("ata: %v", err)
	}
	keys := solana.PublicKeySlice{buyer, buyerCredit, sellerCredit, listing, escrow, buyerToken, sellerToken, testPayMint, solana.TokenProgramID, program}
	return &solana.Transaction{
		Message: solana.Message{
			AccountKeys: keys,
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 9,
				Accounts:       []uint16{0, 1, 2, 3, 4, 5, 6, 7, 8},
				Data:           []byte{ixLedgerBuy},
			}},
		},
	}
}

// cancelTx carries CancelList's account order, per LedgerCancelListIx:
// [seller, listing, escrow, seller_credit].
func cancelTx(t *testing.T, program, seller solana.PublicKey, nonce uint64) *solana.Transaction {
	t.Helper()
	listing, err := LedgerListingAddress(program, seller, nonce)
	if err != nil {
		t.Fatalf("listing address: %v", err)
	}
	escrow, err := LedgerEscrowAddress(program, listing)
	if err != nil {
		t.Fatalf("escrow address: %v", err)
	}
	sellerCredit, err := LedgerCreditAddress(program, seller)
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	keys := solana.PublicKeySlice{seller, listing, escrow, sellerCredit, program}
	return &solana.Transaction{
		Message: solana.Message{
			AccountKeys: keys,
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 4,
				Accounts:       []uint16{0, 1, 2, 3},
				Data:           []byte{ixLedgerCancelList},
			}},
		},
	}
}

func newTestIndexer(store MarketIndexerStore) *Indexer {
	ix := &Indexer{program: testProgram, store: store, seen: map[string]bool{}}
	// Offline by default: an unresolved listing is skipped, which the buy
	// test pins. Tests that need resolution set ix.resolve themselves.
	ix.resolve = func(solana.PublicKey) (solana.PublicKey, uint64, bool) {
		return solana.PublicKey{}, 0, false
	}
	return ix
}

var (
	testSeller = func() solana.PublicKey {
		w := solana.NewWallet()
		return w.PublicKey()
	}()
	testBuyer = func() solana.PublicKey {
		w := solana.NewWallet()
		return w.PublicKey()
	}()
	testPayMint = func() solana.PublicKey {
		w := solana.NewWallet()
		return w.PublicKey()
	}()
)

var _ = errors.New // keep errors imported for future negative cases

func TestIndexerParsesListInstruction(t *testing.T) {
	const credits, price, nonce = 5_000, 9_500, 1
	tx := listTx(t, testProgram, testSeller, credits, price, nonce, testPayMint)
	ix := newTestIndexer(nil)
	evs, err := ix.parseTransaction(tx, "sig-list", 77, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != "list" || ev.Seller != testSeller || ev.Nonce != nonce || ev.Credits != credits || ev.Price != price || ev.PayMint != testPayMint {
		t.Fatalf("event mismatch: %+v", ev)
	}
	if ev.Signature != "sig-list" || ev.Slot != 77 {
		t.Fatalf("identity fields: %+v", ev)
	}
}

func TestIndexerParsesBuyNeedsListingResolution(t *testing.T) {
	listing, err := LedgerListingAddress(testProgram, testSeller, 2)
	if err != nil {
		t.Fatalf("listing address: %v", err)
	}
	tx := buyTx(t, testProgram, testBuyer, listing)
	ix := newTestIndexer(nil)
	// Wire a resolver that reads the listing's seller and nonce back out of
	// the same PDA derivation, standing in for the chain read.
	ix.resolve = func(addr solana.PublicKey) (solana.PublicKey, uint64, bool) {
		var l LedgerListing
		copy(l.Seller[:], testSeller[:])
		l.Nonce = 2
		l.State = LedgerListingSold // even closed listings stay readable
		return testSeller, 2, true
	}
	evs, err := ix.parseTransaction(tx, "sig-buy", 78, time.Unix(1700000001, 0).UTC())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Kind != "sold" || ev.Seller != testSeller || ev.Buyer != testBuyer || ev.Nonce != 2 {
		t.Fatalf("event mismatch: %+v", ev)
	}
	// And the unresolved case skip, not error: a buy of a listing no longer
	// (or never) readable names no seller and cannot be applied.
	ix.resolve = func(solana.PublicKey) (solana.PublicKey, uint64, bool) {
		return solana.PublicKey{}, 0, false
	}
	evs, err = ix.parseTransaction(tx, "sig-buy-2", 79, time.Unix(1700000001, 0).UTC())
	if err != nil {
		t.Fatalf("parse unresolved: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("unresolved listing produced %d events, want 0", len(evs))
	}
}

func TestIndexerSkipsForeignProgramInstructions(t *testing.T) {
	tx := listTx(t, testProgram, testSeller, 100, 100, 3, testPayMint)
	// Rewrite the instruction's program index to the system program.
	tx.Message.Instructions[0].ProgramIDIndex = 4
	ix := newTestIndexer(nil)
	evs, err := ix.parseTransaction(tx, "sig-x", 1, time.Now())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("foreign instruction produced %d events, want 0", len(evs))
	}
}

func TestIndexerApplyIsIdempotentAcrossReplays(t *testing.T) {
	const nonce = uint64(4)
	store := &stubStore{cancelled: map[string]bool{}, sold: map[string]bool{}}
	ix := newTestIndexer(store)

	listEv := MarketEvent{
		Kind: "list", Signature: "sig-list-4", Slot: 10,
		At:     time.Unix(1700000010, 0).UTC(),
		Seller: testSeller, Nonce: nonce,
		Credits: 2_000, Price: 2_100, PayMint: testPayMint,
	}
	// Replay the same list event three times.
	for i := 0; i < 3; i++ {
		if err := ix.apply(context.Background(), listEv); err != nil {
			t.Fatalf("apply list: %v", err)
		}
	}
	lists := 0
	for _, e := range store.events {
		if e.kind == "list" && e.nonce == nonce && e.sig == "sig-list-4" {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("store holds %d list rows for one listing, want 1", lists)
	}

	// One sale, applied twice: exactly one "sold" may record.
	soldEv := MarketEvent{
		Kind: "sold", Signature: "sig-sold-4", Slot: 12,
		At:     time.Unix(1700000012, 0).UTC(),
		Seller: testSeller, Buyer: testBuyer, Nonce: nonce,
	}
	for i := 0; i < 2; i++ {
		if err := ix.apply(context.Background(), soldEv); err != nil {
			t.Fatalf("apply sold: %v", err)
		}
	}
	sales := 0
	for _, e := range store.events {
		if e.kind == "sold" && e.sig == "sig-sold-4" {
			sales++
		}
	}
	if sales != 1 {
		t.Fatalf("store holds %d sold rows for one sale, want 1", sales)
	}

	// A cancelled replay after a sold row must not resurrect or overwrite
	// anything: the guarded store reports "already moved" and applies 0.
	cancelEv := MarketEvent{
		Kind: "cancelled", Signature: "sig-cancel-4", Slot: 13,
		At:     time.Unix(1700000013, 0).UTC(),
		Seller: testSeller, Nonce: nonce,
	}
	if err := ix.apply(context.Background(), cancelEv); err != nil {
		t.Fatalf("apply cancel: %v", err)
	}
	for _, e := range store.events {
		if e.kind == "cancelled" && e.nonce == nonce {
			t.Fatalf("a sold listing was cancelled on replay: %+v", e)
		}
	}
}

func TestIndexerListingIDIsStable(t *testing.T) {
	a, err := LedgerListingAddress(testProgram, testSeller, 9)
	if err != nil {
		t.Fatalf("listing address: %v", err)
	}
	ix := newTestIndexer(nil)
	id := ix.listingIDFor(testSeller, 9)
	if id != binary.LittleEndian.Uint64(a[24:32]) {
		t.Fatalf("listingIDFor %d != PDA tail %d", id, binary.LittleEndian.Uint64(a[24:32]))
	}
	// Same derivation twice: identical, so a rescan lands on the same row.
	if id != ix.listingIDFor(testSeller, 9) {
		t.Fatal("listing id is not stable across calls")
	}
}
