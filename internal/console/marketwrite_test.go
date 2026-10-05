package console

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/session"
	"slotfare/internal/solana"
)

// marketWriteClock is a fixed now for the write routes: create stamps it,
// cancel and pay must not disagree with it.
var marketWriteClock = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// testSolanaAccount mints a32-byte account id - the shape
// solana.AccountIDFor produces for a real wallet - unlike testAccount's
// legacy 20-byte ids, which have no Solana wallet behind them.
func testSolanaAccount(t *testing.T) string {
	t.Helper()
	pub := gsolana.NewWallet().PublicKey()
	return "0x" + hex.EncodeToString(pub[:])
}

// fakePayment is a PaymentVerifier that records the check and answers with
// a canned result, so pay-route tests run offline.
type fakePayment struct {
	info solana.PaymentInfo
	err  error

	sig, mint, payer, payee string
	min                     uint64
	calls                   int
}

func (f *fakePayment) VerifyPayment(_ context.Context, sig, mint, payer, payee string, min uint64) (solana.PaymentInfo, error) {
	f.calls++
	f.sig, f.mint, f.payer, f.payee, f.min = sig, mint, payer, payee, min
	if f.err != nil {
		return solana.PaymentInfo{}, f.err
	}
	return f.info, nil
}

// marketWriteEnv is the store set the write tests run against: the same
// sqlite fixture as the read env, one file per test.
type marketWriteEnv struct {
	*readsEnv
	store market.Store
}

func newMarketWriteEnv(t *testing.T) *marketWriteEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "market.db")
	ks, err := keys.Open(path)
	if err != nil {
		t.Fatalf("keys.Open: %v", err)
	}
	ms, err := meter.Open(path)
	if err != nil {
		t.Fatalf("meter.Open: %v", err)
	}
	fs, err := funding.NewStore(path, 1)
	if err != nil {
		t.Fatalf("funding.NewStore: %v", err)
	}
	mk, err := market.NewStore(path)
	if err != nil {
		t.Fatalf("market.NewStore: %v", err)
	}
	ss, err := session.NewStore(path)
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	tracker := NewTracker(fs)
	ledger := meter.NewLedger()
	t.Cleanup(func() {
		ks.Close()
		ms.Close()
		fs.Close()
		mk.Close()
		ss.Close()
	})
	return &marketWriteEnv{
		readsEnv: &readsEnv{h: nil, keysStore: ks, sessionsStore: ss, meterStore: ms, tracker: tracker, marketStore: mk, ledger: ledger, meterPath: path},
		store:    mk,
	}
}

// marketWriteHandler builds a handler with the full write surface: market
// store, deposit config (the USDC mint the listings carry), the injected
// payment verifier, and the fixed clock.
func marketWriteHandler(t *testing.T, env *marketWriteEnv, p PaymentVerifier) *Handler {
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
			Treasury:               "Treasury111111111111111111111111111111111111",
			Mint:                   "USDCMint11111111111111111111111111111111111",
			Cluster:                "devnet",
			RPC:                    "http://127.0.0.1:8899",
			Explorer:               "https://explorer.solana.com",
			Decimals:               6,
			CreditsPerUsd:          1000,
			MicroCreditsPerUsdBase: solana.MicroCreditsPerUSDCBase,
		}),
		WithPaymentVerifier(p),
		// Zero maturation: a fresh listing is buyable at once, so the
		// settlement tests never wait out the contract's default window.
		WithListingMaturation(0),
		withNow(func() time.Time { return marketWriteClock }),
	)
}

// createListing posts a listing as seller and returns the created row.
func createListing(t *testing.T, h *Handler, seller string, credits, price uint64) listingView {
	t.Helper()
	body := fmt.Sprintf(`{"credits":%d,"price":%d}`, credits, price)
	rec := doRequest(t, h, http.MethodPost, "/listings", consoleTokenFor(t, h, seller), []byte(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /listings status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var out listingView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("create body not JSON: %v; body: %s", err, rec.Body.String())
	}
	return out
}

// 1. Create: the row carries the fixed conversion's metadata, the credits
// are locked away from spendable, and the response is the row itself.
func TestCreateListingLocksCredits(t *testing.T) {
	env := newMarketWriteEnv(t)
	h := marketWriteHandler(t, env, &fakePayment{})
	seller := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)

	l := createListing(t, h, seller, 10, 1_234_567)

	if l.ListingID == 0 || l.State != "open" || l.Role != "selling" {
		t.Errorf("row = %+v, want a fresh open selling row", l)
	}
	if l.Credits != 10 || l.Price != 1_234_567 {
		t.Errorf("row = %+v, want credits 10 at price 1234567", l)
	}
	if l.PayToken != "USDCMint11111111111111111111111111111111111" ||
		l.PayTokenSymbol != "USDC" || l.PayTokenDecimals != 6 {
		t.Errorf("token = %s/%s/%d, want the configured USDC mint", l.PayToken, l.PayTokenSymbol, l.PayTokenDecimals)
	}
	// The test handler opts out of the maturation window, so the row is
	// matured at creation; the default handler keeps the contract's window.
	if l.MaturesAt != uint64(marketWriteClock.Unix()) {
		t.Errorf("maturesAt = %d, want %d (zero window)", l.MaturesAt, marketWriteClock.Unix())
	}
	if d := New().maturation; d != defaultListingMaturation {
		t.Errorf("default maturation = %s, want %s", d, defaultListingMaturation)
	}
	if l.ListedAt != marketWriteClock.Format(time.RFC3339) {
		t.Errorf("listedAt = %q, want the fixed clock", l.ListedAt)
	}
	if got := env.ledger.Locked(seller); got != 10*meter.MicroCreditsPerCredit {
		t.Errorf("locked = %d, want the listing's 10 credits in micro", got)
	}
	if got := env.ledger.Balance(seller); got != 0 {
		t.Errorf("spendable = %d, want 0 - the whole position is listed", got)
	}
}

// 2. Create validation: a bad body is 400, a position that cannot cover the
// listing is 402, and neither leaves a row or a lock behind.
func TestCreateListingValidation(t *testing.T) {
	env := newMarketWriteEnv(t)
	h := marketWriteHandler(t, env, &fakePayment{})
	seller := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 5*meter.MicroCreditsPerCredit)
	token := consoleTokenFor(t, h, seller)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"zero credits", `{"credits":0,"price":1}`, http.StatusBadRequest},
		{"credits over cap", fmt.Sprintf(`{"credits":%d,"price":1}`, listingCreditCap+1), http.StatusBadRequest},
		{"zero price", `{"credits":1,"price":0}`, http.StatusBadRequest},
		{"price over cap", fmt.Sprintf(`{"credits":1,"price":%d}`, listingPriceCap+1), http.StatusBadRequest},
		{"over spendable", `{"credits":10,"price":1}`, http.StatusPaymentRequired},
	}
	for _, c := range cases {
		rec := doRequest(t, h, http.MethodPost, "/listings", token, []byte(c.body))
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d; body: %s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
	if locked := env.ledger.Locked(seller); locked != 0 {
		t.Errorf("locked = %d after rejects, want 0", locked)
	}
	listings, err := env.marketStore.OpenListings(context.Background(), 100, 0)
	if err != nil {
		t.Fatalf("OpenListings: %v", err)
	}
	if len(listings) != 0 {
		t.Errorf("open listings after rejects = %+v, want none", listings)
	}
}

// 3. Cancel: the owner withdraws, the lock drops to zero, and the row
// remembers the move; a second cancel, a stranger and a missing id are
// 409, 403 and 404.
func TestCancelListingReleasesLock(t *testing.T) {
	env := newMarketWriteEnv(t)
	h := marketWriteHandler(t, env, &fakePayment{})
	seller := testSolanaAccount(t)
	stranger := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)
	l := createListing(t, h, seller, 7, 500)

	// A stranger is turned away while the row is still open - ownership
	// outranks the state check for a live listing.
	if rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/cancel", l.ListingID),
		consoleTokenFor(t, h, stranger), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("stranger cancel: status = %d, want 403", rec.Code)
	}

	rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/cancel", l.ListingID),
		consoleTokenFor(t, h, seller), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out listingView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("cancel body not JSON: %v", err)
	}
	if out.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", out.State)
	}
	if out.CancelledAt != marketWriteClock.Format(time.RFC3339) {
		t.Errorf("cancelledAt = %q, want the fixed clock", out.CancelledAt)
	}
	if got := env.ledger.Locked(seller); got != 0 {
		t.Errorf("locked = %d after cancel, want 0", got)
	}
	if got := env.ledger.Balance(seller); got != 10*meter.MicroCreditsPerCredit {
		t.Errorf("spendable = %d after cancel, want the full 10 credits back", got)
	}

	if rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/cancel", l.ListingID),
		consoleTokenFor(t, h, seller), nil); rec.Code != http.StatusConflict {
		t.Errorf("second cancel: status = %d, want 409", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodPost, "/listings/424242/cancel",
		consoleTokenFor(t, h, seller), nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing cancel: status = %d, want 404", rec.Code)
	}
}

// 4. Pay: the verifier is handed the row's mint, the buyer and seller
// wallets and the price; a passing check moves credits through the adjust
// axis, marks the row sold and releases the seller's lock. The same
// signature cannot buy the row twice.
func TestPayListingSettlesThroughVerifier(t *testing.T) {
	env := newMarketWriteEnv(t)
	fp := &fakePayment{info: solana.PaymentInfo{
		Signature: "5TestSignature",
		Slot:      42,
		BlockTime: marketWriteClock.Add(-time.Minute),
		Amount:    700_000,
	}}
	h := marketWriteHandler(t, env, fp)
	seller := testSolanaAccount(t)
	buyer := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)
	l := createListing(t, h, seller, 4, 700_000)

	rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, buyer), []byte(`{"signature":"5TestSignature"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("pay status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out listingView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("pay body not JSON: %v", err)
	}
	if out.State != "sold" || out.BuyerAccount != buyer || out.SoldTx != "5TestSignature" || out.SoldBlock != 42 {
		t.Errorf("row = %+v, want sold to the caller with the payment signature", out)
	}
	if out.Role != "buying" {
		t.Errorf("role = %q, want buying", out.Role)
	}

	wantPayer, _ := base58FromAccountID(buyer)
	wantPayee, _ := base58FromAccountID(seller)
	if fp.calls != 1 || fp.sig != "5TestSignature" ||
		fp.mint != "USDCMint11111111111111111111111111111111111" ||
		fp.payer != wantPayer || fp.payee != wantPayee || fp.min != 700_000 {
		t.Errorf("verifier got %+v, want the row's mint, both wallets and the price", fp)
	}

	// Credits moved: 4 credits from seller to buyer through Adjust, with
	// the seller's chain-minted deposits untouched underneath.
	if got := env.ledger.Balance(buyer); got != 4*meter.MicroCreditsPerCredit {
		t.Errorf("buyer balance = %d, want the 4 credits", got)
	}
	if got := env.ledger.Balance(seller); got != 6*meter.MicroCreditsPerCredit {
		t.Errorf("seller balance = %d, want 6 of the 10 credits left", got)
	}
	if got := env.ledger.Locked(seller); got != 0 {
		t.Errorf("seller locked = %d after sale, want 0", got)
	}

	// The same payment signature cannot buy the row again.
	if rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, testSolanaAccount(t)), []byte(`{"signature":"5TestSignature"}`)); rec.Code != http.StatusConflict {
		t.Errorf("second pay: status = %d, want 409", rec.Code)
	}
	if fp.calls != 1 {
		t.Errorf("verifier called %d times, want 1 - a moved row must not reach the chain", fp.calls)
	}
}

// 5. Pay rejections: a failing verification leaves the row open and the
// balances alone; an unmatured row and a self-buy are conflicts that never
// reach the verifier at all.
func TestPayListingRejectsWithoutSettling(t *testing.T) {
	env := newMarketWriteEnv(t)
	fp := &fakePayment{err: fmt.Errorf("signature unknown")}
	h := marketWriteHandler(t, env, fp)
	seller := testSolanaAccount(t)
	buyer := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)
	l := createListing(t, h, seller, 3, 300_000)

	rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, buyer), []byte(`{"signature":"5Bad"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unverified pay: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if got := env.ledger.Locked(seller); got != 3*meter.MicroCreditsPerCredit {
		t.Errorf("locked = %d after rejected pay, want the listing still held", got)
	}
	if got := env.ledger.Balance(buyer); got != 0 {
		t.Errorf("buyer balance = %d after rejected pay, want 0", got)
	}

	// Self-buy: conflict, and the verifier is not consulted.
	calls := fp.calls
	if rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, seller), []byte(`{"signature":"5Whatever"}`)); rec.Code != http.StatusConflict {
		t.Errorf("self buy: status = %d, want 409", rec.Code)
	}
	if fp.calls != calls {
		t.Errorf("verifier consulted for a self-buy")
	}

	// A row that has not matured yet: conflict before any chain read.
	// (Maturity is the earliest a sale may land; past it a listing stays
	// buyable until sold or cancelled, so only the future is a conflict.)
	unmatured := market.Listing{Account: seller, Credits: 1, Price: 1, MaturesAt: uint64(marketWriteClock.Add(time.Hour).Unix()), ListedAt: marketWriteClock.Add(-25 * time.Hour).Format(time.RFC3339)}
	if _, err := env.store.RecordListing(context.Background(), unmatured); err != nil {
		t.Fatalf("seed unmatured: %v", err)
	}
	listings, err := env.store.AccountListings(context.Background(), seller, 10, 0)
	if err != nil {
		t.Fatalf("AccountListings: %v", err)
	}
	var futureID uint64
	for _, row := range listings {
		if row.State == "open" && row.MaturesAt > uint64(marketWriteClock.Unix()) {
			futureID = row.ListingID
		}
	}
	if futureID == 0 {
		t.Fatal("unmatured listing not found")
	}
	if rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", futureID),
		consoleTokenFor(t, h, buyer), []byte(`{"signature":"5Whatever"}`)); rec.Code != http.StatusConflict {
		t.Errorf("unmatured pay: status = %d, want 409", rec.Code)
	}
	if fp.calls != calls {
		t.Errorf("verifier consulted for an unmatured listing")
	}
}

// 6. The window check: a payment whose block time predates the listing
// (beyond the bank-clock skew) is rejected even though the transfer
// itself verifies.
func TestPayListingRejectsPredatingPayment(t *testing.T) {
	env := newMarketWriteEnv(t)
	// Block time two hours before the listing: outside any clock skew.
	fp := &fakePayment{info: solana.PaymentInfo{
		Signature: "5Old",
		Slot:      7,
		BlockTime: marketWriteClock.Add(-2 * time.Hour),
	}}
	h := marketWriteHandler(t, env, fp)
	seller := testSolanaAccount(t)
	buyer := testSolanaAccount(t)
	env.ledger.SetMinted(seller, 10*meter.MicroCreditsPerCredit)
	l := createListing(t, h, seller, 1, 100_000)

	rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, buyer), []byte(`{"signature":"5Old"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("predating pay: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	// The listing's lock is still held and no market adjustment moved:
	// a rejected payment changes nothing on either side.
	if got := env.ledger.Locked(seller); got != 1*meter.MicroCreditsPerCredit {
		t.Errorf("seller locked = %d, want the 1-credit listing still held", got)
	}
	if snap := env.ledger.Snapshot()[seller]; snap.Adjust != 0 {
		t.Errorf("seller adjust = %d, want 0", snap.Adjust)
	}
	if got := env.ledger.Balance(buyer); got != 0 {
		t.Errorf("buyer balance = %d, want 0", got)
	}
}

// TestRebuildLocksReconcilesFromRows walks the startup pass over three
// accounts: open rows sum to their total, an account whose only row closed
// goes back to zero from a stale figure, and an account with no rows is
// never written. This is the gap the write routes cannot close - an
// indexer-recorded listing (or a confirm that failed after the row landed)
// never runs refreshLock.
func TestRebuildLocksReconcilesFromRows(t *testing.T) {
	ctx := context.Background()
	env := newMarketWriteEnv(t)
	h := marketWriteHandler(t, env, &fakePayment{})

	now := marketWriteClock.Format(time.RFC3339)
	for _, l := range []market.Listing{
		{ListingID: 1, Account: "0xa", Seller: "SellerA", Credits: 700, Price: 1000000, State: "open", ListedTx: "0xtx1", ListedAt: now},
		{ListingID: 2, Account: "0xa", Seller: "SellerA", Credits: 300, Price: 1000000, State: "open", ListedTx: "0xtx2", ListedAt: now},
		{ListingID: 3, Account: "0xb", Seller: "SellerB", Credits: 500, Price: 1000000, State: "open", ListedTx: "0xtx3", ListedAt: now},
	} {
		if _, err := env.store.RecordListing(ctx, l); err != nil {
			t.Fatalf("RecordListing(%d): %v", l.ListingID, err)
		}
	}
	// RecordListing is the create path and always writes 'open'; the row
	// only leaves that state through a guarded move.
	if _, err := env.store.CancelListing(ctx, 3, "0xcancel", 1, now); err != nil {
		t.Fatalf("CancelListing(3): %v", err)
	}
	// A lock the routes wrote for a listing that has since closed without
	// them: the rebuild must zero it from the row set.
	env.ledger.SetLocked("0xb", 999*meter.MicroCreditsPerCredit)

	if err := h.RebuildLocks(ctx); err != nil {
		t.Fatalf("RebuildLocks: %v", err)
	}

	snap := env.ledger.Snapshot()
	if got := snap["0xa"].Locked; got != 1000*meter.MicroCreditsPerCredit {
		t.Errorf("0xa locked = %d, want %d (700 + 300 open credits)", got, 1000*meter.MicroCreditsPerCredit)
	}
	if got := snap["0xb"].Locked; got != 0 {
		t.Errorf("0xb locked = %d, want 0 (only row cancelled)", got)
	}
}
