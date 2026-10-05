package console

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/meter"
	"slotfare/internal/solana"
)

// fakeLedgerMarket stands in for the deployed program. It records the
// arguments each builder was called with and answers LedgerListingRead from a
// mutable listing, so a test can drive the whole prepare/confirm flow - open,
// sold, cancelled - without a cluster.
type fakeLedgerMarket struct {
	// The listing LedgerListingRead reports. found false means the program
	// has no account at (seller, nonce).
	listing solana.LedgerListing
	found   bool
	readErr error

	// slot is what LedgerTxSlot dates a row with, and txErr makes it fail.
	slot  uint64
	txErr error

	// The prepared transaction the prepare routes hand back, and the payer
	// it was built for.
	wire    []byte
	payer   gsolana.PublicKey
	txCalls int

	// Recorded builder arguments.
	seller  gsolana.PublicKey
	buyer   gsolana.PublicKey
	credits uint64
	price   uint64
	nonce   uint64
	mint    gsolana.PublicKey

	listCalls, buyCalls, cancelCalls int
	readNonce                        uint64
}

func (f *fakeLedgerMarket) LedgerListIx(seller gsolana.PublicKey, credits, price, nonce uint64, payMint gsolana.PublicKey) (gsolana.Instruction, error) {
	f.listCalls++
	f.seller, f.credits, f.price, f.nonce, f.mint = seller, credits, price, nonce, payMint
	return gsolana.NewInstruction(seller, gsolana.AccountMetaSlice{
		gsolana.NewAccountMeta(seller, true, true),
	}, []byte{7}), nil
}

func (f *fakeLedgerMarket) LedgerBuyIx(buyer, seller gsolana.PublicKey, nonce uint64, payMint gsolana.PublicKey) ([]gsolana.Instruction, error) {
	f.buyCalls++
	f.buyer, f.seller, f.nonce, f.mint = buyer, seller, nonce, payMint
	create := gsolana.NewInstruction(gsolana.SystemProgramID, gsolana.AccountMetaSlice{
		gsolana.NewAccountMeta(buyer, true, true),
	}, []byte{8})
	buy := gsolana.NewInstruction(buyer, gsolana.AccountMetaSlice{
		gsolana.NewAccountMeta(buyer, true, true),
	}, []byte{8})
	return []gsolana.Instruction{create, buy}, nil
}

func (f *fakeLedgerMarket) LedgerCancelListIx(seller gsolana.PublicKey, nonce uint64) (gsolana.Instruction, error) {
	f.cancelCalls++
	f.seller, f.nonce = seller, nonce
	return gsolana.NewInstruction(seller, gsolana.AccountMetaSlice{
		gsolana.NewAccountMeta(seller, true, false),
	}, []byte{9}), nil
}

func (f *fakeLedgerMarket) PrepareLedgerTx(_ context.Context, payer gsolana.PublicKey, ixs []gsolana.Instruction) ([]byte, error) {
	f.txCalls++
	f.payer = payer
	if len(ixs) == 0 {
		return nil, context.Canceled
	}
	return f.wire, nil
}

func (f *fakeLedgerMarket) LedgerListingRead(_ context.Context, _ gsolana.PublicKey, nonce uint64) (solana.LedgerListing, bool, error) {
	f.readNonce = nonce
	if f.readErr != nil {
		return solana.LedgerListing{}, false, f.readErr
	}
	return f.listing, f.found, nil
}

func (f *fakeLedgerMarket) LedgerTxSlot(_ context.Context, _ gsolana.Signature) (uint64, error) {
	if f.txErr != nil {
		return 0, f.txErr
	}
	return f.slot, nil
}

// LedgerListingID stands in for the PDA derivation with a stable value that
// is deliberately a full-width draw (the nonce): it exceeds int64max, so
// every confirm test exercises the signed-column round trip the real
// derivation also hits half the time.
func (f *fakeLedgerMarket) LedgerListingID(_ gsolana.PublicKey, nonce uint64) (uint64, error) {
	return nonce, nil
}

// sigOf mints a well-formed base58 transaction signature: the confirm route
// parses it as one, so a test cannot pass a string the route would reject.
func sigOf(seed byte) string {
	var s gsolana.Signature
	for i := range s {
		s[i] = seed
	}
	return s.String()
}

var (
	sigList   = sigOf(0x11)
	sigBuy    = sigOf(0x22)
	sigCancel = sigOf(0x33)
)

// pubkeyOfAccount recovers the key behind a Slotfare account id, exactly as
// the handler does: the id is 0x-hex of the raw 32-byte public key.
func pubkeyOfAccount(t *testing.T, accountID string) gsolana.PublicKey {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(accountID, "0x"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("account %q is not a 32-byte key: %v", accountID, err)
	}
	return gsolana.PublicKeyFromBytes(raw)
}

// onchainMarketHandler builds a handler with the on-chain market surface: a
// real (parsable) USDC mint, the market stores, the fixed clock, and the
// injected program. Passing m == nil is the "no SLOTFARE_LEDGER_PROGRAM"
// deployment, where the on-chain routes answer 501.
func onchainMarketHandler(t *testing.T, env *marketWriteEnv, m LedgerMarket, mint string) *Handler {
	t.Helper()
	return New(
		WithKeys(env.keysStore),
		WithSessions(env.sessionsStore),
		WithLedger(env.ledger),
		WithUsage(env.meterStore),
		WithFunding(env.tracker),
		WithMarket(env.marketStore),
		WithMarketWrite(env.marketStore),
		WithDepositTarget(&DepositTarget{
			Treasury:               gsolana.NewWallet().PublicKey().String(),
			Mint:                   mint,
			Cluster:                "devnet",
			RPC:                    "http://127.0.0.1:8899",
			Explorer:               "https://explorer.solana.com",
			Decimals:               6,
			CreditsPerUsd:          1000,
			MicroCreditsPerUsdBase: solana.MicroCreditsPerUSDCBase,
		}),
		// Zero maturation: a listed credit is buyable at once, so the
		// prepare-buy tests never wait out the contract's default window.
		WithListingMaturation(0),
		withNow(func() time.Time { return marketWriteClock }),
		WithLedgerMarket(m),
	)
}

// prepareMarket posts to the prepare route and returns the decoded response.
func prepareMarket(t *testing.T, h *Handler, token, body string) preparedTxView {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/prepare", token, []byte(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST prepare status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out preparedTxView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("prepare body not JSON: %v; body: %s", err, rec.Body.String())
	}
	return out
}

// confirmMarket posts to the confirm route and answers the raw recorder, so a
// caller can assert on the status and decode the row itself.
func confirmMarket(t *testing.T, h *Handler, token, body string) (int, listingView, string) {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/confirm", token, []byte(body))
	var out listingView
	if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("confirm body not JSON: %v; body: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out, rec.Body.String()
}

// 1. Prepare-list: the seller gets a wallet-signed transaction, the builder
// sees the caller's key as the seller, and nothing is written.
func TestPrepareListReturnsWalletSignedTransaction(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("prepared-wire")}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	want := pubkeyOfAccount(t, seller)
	wantMint := gsolana.MustPublicKeyFromBase58(mint)

	out := prepareMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"list","credits":10,"price":1234567}`)

	if out.Action != "list" || out.Nonce == 0 {
		t.Errorf("prepared = %+v, want a list action with a drawn nonce", out)
	}
	if got, err := base64.StdEncoding.DecodeString(out.Transaction); err != nil || string(got) != "prepared-wire" {
		t.Errorf("transaction = %q (err %v), want the base64 of the prepared wire bytes", out.Transaction, err)
	}
	if m.listCalls != 1 || m.credits != 10 || m.price != 1234567 {
		t.Errorf("ListIx args = calls %d credits %d price %d, want one call at 10/1234567", m.listCalls, m.credits, m.price)
	}
	if m.seller != want {
		t.Errorf("ListIx seller = %s, want the caller %s", m.seller, want)
	}
	if m.mint != wantMint {
		t.Errorf("ListIx mint = %s, want the configured %s", m.mint, wantMint)
	}
	if m.payer != want {
		t.Errorf("prepare payer = %s, want the caller %s - the wallet pays, not the daemon", m.payer, want)
	}
	// Nothing was recorded: prepare only builds.
	if rows, err := env.store.AccountListings(context.Background(), seller, 10, 0); err != nil || len(rows) != 0 {
		t.Errorf("rows after prepare = %d (err %v), want 0", len(rows), err)
	}
}

// 2. Prepare-list validation: the same caps the local route enforces, and a
// rejected request never reaches the program.
func TestPrepareListValidatesTerms(t *testing.T) {
	env := newMarketWriteEnv(t)
	m := &fakeLedgerMarket{wire: []byte("x")}
	h := onchainMarketHandler(t, env, m, gsolana.NewWallet().PublicKey().String())
	token := consoleTokenFor(t, h, testSolanaAccount(t))

	cases := []struct {
		name string
		body string
	}{
		{"bad json", `{`},
		{"zero credits", `{"action":"list","credits":0,"price":1}`},
		{"credits over cap", `{"action":"list","credits":1000001,"price":1}`},
		{"zero price", `{"action":"list","credits":1,"price":0}`},
		{"price over cap", `{"action":"list","credits":1,"price":1000000000001}`},
		{"unknown action", `{"action":"teleport","credits":1,"price":1}`},
	}
	for _, c := range cases {
		rec := doRequest(t, h, http.MethodPost, "/listings/onchain/prepare", token, []byte(c.body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.name, rec.Code)
		}
	}
	if m.listCalls != 0 {
		t.Errorf("ListIx calls = %d, want 0 - a rejected prepare must not build", m.listCalls)
	}
}

// 3. Prepare-buy builds the buyer's instruction against the row's nonce and
// mint, and refuses the seller's own listing.
func TestPrepareBuyUsesTheRowsEscrow(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("x")}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	sellerKey := pubkeyOfAccount(t, seller)

	// A row the program holds in escrow. The listing's own pay mint is what
	// the buy must settle in, so it is set from the configured USDC.
	m.listing = solana.LedgerListing{Seller: sellerKey, PayMint: gsolana.MustPublicKeyFromBase58(mint), Credits: 4, Price: 99, Nonce: 77, State: solana.LedgerListingOpen}
	m.found = true
	code, row, body := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"list","nonce":77,"signature":"`+sigList+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("confirm list status = %d, want 201; body: %s", code, body)
	}
	if row.Credits != 4 || row.Price != 99 {
		t.Errorf("row = %+v, want the program's 4 credits at 99 (from chain, not the request)", row)
	}

	// The seller cannot buy their own listing.
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/prepare", consoleTokenFor(t, h, seller),
		[]byte(`{"action":"buy","listingId":`+itoa(uint64(row.ListingID))+`}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("self-buy prepare status = %d, want 409", rec.Code)
	}

	// A buyer gets a transaction built from the row's nonce and pay mint.
	buyer := testSolanaAccount(t)
	buyerKey := pubkeyOfAccount(t, buyer)
	out := prepareMarket(t, h, consoleTokenFor(t, h, buyer), `{"action":"buy","listingId":`+itoa(uint64(row.ListingID))+`}`)
	if out.Action != "buy" || out.Nonce != 77 {
		t.Errorf("prepared = %+v, want a buy carrying the row's nonce 77", out)
	}
	if m.buyCalls != 1 || m.buyer != buyerKey || m.seller != sellerKey {
		t.Errorf("BuyIx args = calls %d buyer %s seller %s, want one call buyer %s seller %s", m.buyCalls, m.buyer, m.seller, buyerKey, sellerKey)
	}
	if m.mint != gsolana.MustPublicKeyFromBase58(mint) {
		t.Errorf("BuyIx mint = %s, want the configured %s", m.mint, mint)
	}
}

// 4. Confirm-buy records the sale the program settled and mirrors the credit
// movement; a listing the program does not hold for this caller is refused.
func TestConfirmBuyRecordsTheSale(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("x"), slot: 4242}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	sellerKey := pubkeyOfAccount(t, seller)

	m.listing = solana.LedgerListing{Seller: sellerKey, Credits: 10, Price: 5, Nonce: 5, State: solana.LedgerListingOpen}
	m.found = true
	_, row, body := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"list","nonce":5,"signature":"`+sigList+`"}`)
	if row.ListingID == 0 {
		t.Fatalf("setup: confirm list returned no row; body: %s", body)
	}
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)

	buyer := testSolanaAccount(t)
	buyerKey := pubkeyOfAccount(t, buyer)

	// Sold to a wallet that is not the caller: refused.
	m.listing = solana.LedgerListing{Seller: sellerKey, Buyer: gsolana.NewWallet().PublicKey(), Credits: 10, Price: 5, Nonce: 5, State: solana.LedgerListingSold}
	code, _, _ := confirmMarket(t, h, consoleTokenFor(t, h, buyer), `{"action":"buy","listingId":`+itoa(uint64(row.ListingID))+`,"nonce":5,"signature":"`+sigBuy+`"}`)
	if code != http.StatusForbidden {
		t.Errorf("confirm buy by another wallet = %d, want 403", code)
	}

	// Sold to this buyer: recorded, dated with the transaction's slot.
	m.listing = solana.LedgerListing{Seller: sellerKey, Buyer: buyerKey, Credits: 10, Price: 5, Nonce: 5, State: solana.LedgerListingSold}
	code, sold, body := confirmMarket(t, h, consoleTokenFor(t, h, buyer), `{"action":"buy","listingId":`+itoa(uint64(row.ListingID))+`,"nonce":5,"signature":"`+sigBuy+`"}`)
	if code != http.StatusOK {
		t.Fatalf("confirm buy status = %d, want 200; body: %s", code, body)
	}
	if sold.State != "sold" || sold.SoldTx != sigBuy || sold.SoldBlock != 4242 {
		t.Errorf("sold row = %+v, want state sold with the signature and slot", sold)
	}
	// The credits moved seller -> buyer, the same flat movement the chain made.
	if got := env.ledger.Balance(buyer); got != 10*meter.MicroCreditsPerCredit {
		t.Errorf("buyer spendable = %d, want the listing's 10 credits", got)
	}
	if got := env.ledger.Locked(seller); got != 0 {
		t.Errorf("seller locked = %d, want 0 - the sold row no longer locks", got)
	}
}

// 5. Confirm-cancel withdraws the listing: the row moves and the lock falls
// back to zero, so a cancelled seller can spend again.
func TestConfirmCancelMovesTheRow(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("x"), slot: 9}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	sellerKey := pubkeyOfAccount(t, seller)

	m.listing = solana.LedgerListing{Seller: sellerKey, Credits: 3, Price: 1, Nonce: 3, State: solana.LedgerListingOpen}
	m.found = true
	_, row, body := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"list","nonce":3,"signature":"`+sigList+`"}`)
	if row.ListingID == 0 {
		t.Fatalf("setup: confirm list returned no row; body: %s", body)
	}
	if got := env.ledger.Locked(seller); got != 3*meter.MicroCreditsPerCredit {
		t.Fatalf("locked after list = %d, want the listing's 3 credits", got)
	}

	// A listing the program still reports open cannot be recorded cancelled.
	code, _, _ := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"cancel","listingId":`+itoa(uint64(row.ListingID))+`,"nonce":3,"signature":"`+sigCancel+`"}`)
	if code != http.StatusConflict {
		t.Errorf("cancel of an open listing = %d, want 409", code)
	}

	m.listing.State = solana.LedgerListingCancelled
	code, cancelled, body := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"cancel","listingId":`+itoa(uint64(row.ListingID))+`,"nonce":3,"signature":"`+sigCancel+`"}`)
	if code != http.StatusOK {
		t.Fatalf("confirm cancel status = %d, want 200; body: %s", code, body)
	}
	if cancelled.State != "cancelled" || cancelled.CancelledTx != sigCancel {
		t.Errorf("cancelled row = %+v, want state cancelled with the signature", cancelled)
	}
	if got := env.ledger.Locked(seller); got != 0 {
		t.Errorf("locked after cancel = %d, want 0", got)
	}
}

// 6. Confirm-list is idempotent: a wallet that retries after a dropped
// response gets the same row back, not a second escrow.
func TestConfirmListIsIdempotent(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("x"), slot: 1}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	sellerKey := pubkeyOfAccount(t, seller)
	m.listing = solana.LedgerListing{Seller: sellerKey, Credits: 2, Price: 1, Nonce: 42, State: solana.LedgerListingOpen}
	m.found = true
	token := consoleTokenFor(t, h, seller)

	code, first, body := confirmMarket(t, h, token, `{"action":"list","nonce":42,"signature":"`+sigList+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("first confirm = %d, want 201; body: %s", code, body)
	}
	code, second, body := confirmMarket(t, h, token, `{"action":"list","nonce":42,"signature":"`+sigList+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second confirm = %d, want 200; body: %s", code, body)
	}
	if first.ListingID != second.ListingID {
		t.Errorf("ids = %d then %d, want the same row", first.ListingID, second.ListingID)
	}
	if rows, err := env.store.AccountListings(context.Background(), seller, 10, 0); err != nil || len(rows) != 1 {
		t.Errorf("rows = %d (err %v), want exactly 1", len(rows), err)
	}
}

// 7. Confirm refuses a listing the program does not hold: the row is a
// consequence of on-chain state, never a claim the caller made.
func TestConfirmListRequiresAnOnchainListing(t *testing.T) {
	env := newMarketWriteEnv(t)
	m := &fakeLedgerMarket{wire: []byte("x"), found: false}
	h := onchainMarketHandler(t, env, m, gsolana.NewWallet().PublicKey().String())
	seller := testSolanaAccount(t)

	code, _, _ := confirmMarket(t, h, consoleTokenFor(t, h, seller), `{"action":"list","nonce":1,"signature":"`+sigList+`"}`)
	if code != http.StatusBadRequest {
		t.Errorf("confirm with no on-chain listing = %d, want 400", code)
	}
	// A missing signature is rejected before the program is ever asked.
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/confirm", consoleTokenFor(t, h, seller), []byte(`{"action":"list","nonce":1}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("confirm with no signature = %d, want 400", rec.Code)
	}
}

// 8. Without a configured program the on-chain routes answer 501, so a
// deployment that has no escrow keeps serving only the local mirror.
func TestOnchainMarketUnavailableWithoutAProgram(t *testing.T) {
	env := newMarketWriteEnv(t)
	h := onchainMarketHandler(t, env, nil, gsolana.NewWallet().PublicKey().String())
	token := consoleTokenFor(t, h, testSolanaAccount(t))

	for _, path := range []string{"/listings/onchain/prepare", "/listings/onchain/confirm"} {
		rec := doRequest(t, h, http.MethodPost, path, token, []byte(`{"action":"list","credits":1,"price":1}`))
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, rec.Code)
		}
	}
}

// itoa renders a listing id for a request body without pulling strconv into
// every call site.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// The listing nonce crosses the wire as a decimal string. A JSON number is
// an IEEE-754 double - this draw (0xa8cbfed5a12be772, the one that wedged
// the indexer) sits far above 2^53, so a number-encoded echo would round it
// and the read-back would target a listing PDA that was never opened.
// Confirm must see the exact value, and the row it records must compare
// equal to it on the idempotent retry.
func TestConfirmListNonceSurvivesTheWireAsAString(t *testing.T) {
	env := newMarketWriteEnv(t)
	mint := gsolana.NewWallet().PublicKey().String()
	m := &fakeLedgerMarket{wire: []byte("x"), slot: 7}
	h := onchainMarketHandler(t, env, m, mint)
	seller := testSolanaAccount(t)
	sellerKey := pubkeyOfAccount(t, seller)

	const nonce = uint64(0xa8cbfed5a12be772)
	m.listing = solana.LedgerListing{
		Seller:  sellerKey,
		PayMint: gsolana.MustPublicKeyFromBase58(mint),
		Credits: 10,
		Price:   7,
		Nonce:   nonce,
		State:   solana.LedgerListingOpen,
	}
	m.found = true

	body := `{"action":"list","nonce":"12163095412129654642","signature":"` + sigList + `"}`
	code, first, resp := confirmMarket(t, h, consoleTokenFor(t, h, seller), body)
	if code != http.StatusCreated {
		t.Fatalf("confirm list status = %d, want 201; body: %s", code, resp)
	}
	if m.readNonce != nonce {
		t.Errorf("read-back nonce = %d, want the exact draw %d", m.readNonce, nonce)
	}

	// The retry finds the row by the nonce it echoed: a stored value that
	// had lost a bit would fall through to a second insert and 500.
	code, second, resp := confirmMarket(t, h, consoleTokenFor(t, h, seller), body)
	if code != http.StatusOK {
		t.Fatalf("idempotent confirm status = %d, want 200; body: %s", code, resp)
	}
	if second.ListingID != first.ListingID {
		t.Errorf("retry listing = %d, want the original %d", second.ListingID, first.ListingID)
	}
}
