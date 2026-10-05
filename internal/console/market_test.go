package console

import (
	"context"
	"encoding/json"
	"net/http"
	"slotfare/internal/market"
	"strconv"
	"testing"
	"time"
)

// marketClock is a fixed clock the handler's maturity computation reads.
type marketClock struct{ now time.Time }

func (c *marketClock) Now() time.Time { return c.now }

// marketHandler rebuilds env's handler with Market attached and the clock
// injected, so maturity assertions are deterministic. The original env.h is
// left untouched; tests that need a specific clock use this one instead.
func marketHandler(t *testing.T, env *readsEnv, clock time.Time) *Handler {
	t.Helper()
	return New(
		WithKeys(env.keysStore),
		WithSessions(env.sessionsStore),
		WithLedger(env.ledger),
		WithUsage(env.meterStore),
		WithFunding(env.tracker),
		WithMarket(env.marketStore),
		withNow(func() time.Time { return clock }),
	)
}

// seedListing records an open listing directly through the store, bypassing
// the feeder so the test is offline and deterministic.
func seedListing(t *testing.T, env *readsEnv, l market.Listing) {
	t.Helper()
	if _, err := env.marketStore.RecordListing(context.Background(), l); err != nil {
		t.Fatalf("RecordListing %d: %v", l.ListingID, err)
	}
}

// 1. The market route lists open listings only, newest first, marks the
// caller's own, and computes maturity on both sides of the boundary.
func TestMarketRouteListsOpenNewestFirstAndMaturity(t *testing.T) {
	env := newReadsEnv(t)
	caller := testAccount(t)
	other := testAccount(t)
	ctx := context.Background()

	// Insertion order defines "newest": the last recorded row comes first.
	seedListing(t, env, market.Listing{
		ListingID: 1, Account: other, Credits: 100, PayToken: "0x1111111111111111111111111111111111111111",
		PayTokenSymbol: "USDC", PayTokenDecimals: 6, Price: 50, MaturesAt: 1_000_000, ListedTx: "0xa1", ListedBlock: 10, ListedAt: "2026-01-01T00:00:00Z",
	})
	seedListing(t, env, market.Listing{
		ListingID: 2, Account: caller, Credits: 200, PayToken: "0x2222222222222222222222222222222222222222",
		Price: 90, MaturesAt: 2_000_000, ListedTx: "0xa2", ListedBlock: 11, ListedAt: "2026-01-01T00:01:00Z",
	})
	seedListing(t, env, market.Listing{
		ListingID: 3, Account: other, Credits: 300, PayToken: "0x3333333333333333333333333333333333333333",
		PayTokenSymbol: "USDT", PayTokenDecimals: 18, Price: 120, MaturesAt: 3_000_000, ListedTx: "0xa3", ListedBlock: 12, ListedAt: "2026-01-01T00:02:00Z",
	})
	// A cancelled and a sold listing for the caller must not appear in /market.
	seedListing(t, env, market.Listing{ListingID: 4, Account: caller, Credits: 400, PayToken: "0x4444444444444444444444444444444444444444", MaturesAt: 4_000_000})
	if _, err := env.marketStore.CancelListing(ctx, 4, "0xcancel", 13, "2026-01-01T00:03:00Z"); err != nil {
		t.Fatalf("CancelListing: %v", err)
	}
	seedListing(t, env, market.Listing{ListingID: 5, Account: caller, Credits: 500, PayToken: "0x5555555555555555555555555555555555555555", MaturesAt: 5_000_000})
	if _, err := env.marketStore.SoldListing(ctx, 5, other, "0xsold", 14, "2026-01-01T00:04:00Z"); err != nil {
		t.Fatalf("SoldListing: %v", err)
	}

	h := marketHandler(t, env, time.Unix(2_000_000, 0).UTC())
	token := consoleTokenFor(t, h, caller)
	rec := doRequest(t, h, http.MethodGet, "/market", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /market status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Market []marketView `json:"market"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /market body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Market) != 3 {
		t.Fatalf("market = %+v, want 3 open entries", out.Market)
	}
	// Newest first: reverse of insertion order.
	if out.Market[0].ListingID != 3 || out.Market[1].ListingID != 2 || out.Market[2].ListingID != 1 {
		t.Errorf("order = %d,%d,%d, want 3,2,1 (newest first)", out.Market[0].ListingID, out.Market[1].ListingID, out.Market[2].ListingID)
	}
	byID := map[uint64]marketView{}
	for _, m := range out.Market {
		byID[uint64(m.ListingID)] = m
	}
	// The caller's own row is marked mine; others' are not.
	if !byID[2].Mine || byID[1].Mine || byID[3].Mine {
		t.Errorf("mine flags = %v/%v/%v, want false/true/false for ids 1/2/3", byID[1].Mine, byID[2].Mine, byID[3].Mine)
	}
	// Fields match the seeded values; symbol falls back to the short address
	// when the column is empty.
	if byID[1].Credits != 100 || byID[1].Price != 50 || byID[1].MaturesAt != 1_000_000 || byID[1].PayTokenSymbol != "USDC" {
		t.Errorf("id 1 = %+v, want credits 100 price 50 maturesAt 1000000 symbol USDC", byID[1])
	}
	if byID[2].PayTokenSymbol != "0x22222222" {
		t.Errorf("id 2 payTokenSymbol = %q, want short-address fallback 0x22222222", byID[2].PayTokenSymbol)
	}
	if byID[3].PayTokenSymbol != "USDT" {
		t.Errorf("id 3 payTokenSymbol = %q, want USDT", byID[3].PayTokenSymbol)
	}
	// Decimals ride the wire for scaling the raw price; absent means 0 and
	// the client falls back to its configured token table.
	if byID[1].PayTokenDecimals != 6 {
		t.Errorf("id 1 payTokenDecimals = %d, want 6", byID[1].PayTokenDecimals)
	}
	if byID[2].PayTokenDecimals != 0 {
		t.Errorf("id 2 payTokenDecimals = %d, want 0 (unresolved)", byID[2].PayTokenDecimals)
	}
	if byID[3].PayTokenDecimals != 18 {
		t.Errorf("id 3 payTokenDecimals = %d, want 18", byID[3].PayTokenDecimals)
	}
	// Maturity at the boundary: now == id2's maturesAt counts as matured; id1's
	// earlier boundary has passed too; id3's later one has not.
	if !byID[2].Matured || !byID[1].Matured || byID[3].Matured {
		t.Errorf("ids 1,2,3 matured = %v/%v/%v at now=2000000, want true/true/false", byID[1].Matured, byID[2].Matured, byID[3].Matured)
	}

	// Just before id 3's boundary it is not matured; exactly at it, it is.
	hBefore := marketHandler(t, env, time.Unix(2_999_999, 0).UTC())
	recBefore := doRequest(t, hBefore, http.MethodGet, "/market", consoleTokenFor(t, hBefore, caller), nil)
	var outBefore struct {
		Market []marketView `json:"market"`
	}
	json.Unmarshal(recBefore.Body.Bytes(), &outBefore)
	for _, m := range outBefore.Market {
		if m.ListingID == 3 && m.Matured {
			t.Errorf("id 3 matured = true just before its boundary, want false")
		}
	}
	hAt := marketHandler(t, env, time.Unix(3_000_000, 0).UTC())
	recAt := doRequest(t, hAt, http.MethodGet, "/market", consoleTokenFor(t, hAt, caller), nil)
	var outAt struct {
		Market []marketView `json:"market"`
	}
	json.Unmarshal(recAt.Body.Bytes(), &outAt)
	for _, m := range outAt.Market {
		if m.ListingID == 3 && !m.Matured {
			t.Errorf("id 3 matured = false at its boundary, want true")
		}
	}
}

// 2. The market route caps at 200 rows, newest first.
func TestMarketRouteCapsAt200(t *testing.T) {
	env := newReadsEnv(t)
	caller := testAccount(t)
	ctx := context.Background()
	for i := uint64(1); i <= 205; i++ {
		if _, err := env.marketStore.RecordListing(ctx, market.Listing{
			ListingID: i, Account: caller, Credits: i, PayToken: "0xabc", MaturesAt: i,
		}); err != nil {
			t.Fatalf("RecordListing %d: %v", i, err)
		}
	}
	h := marketHandler(t, env, time.Unix(0, 0).UTC())
	token := consoleTokenFor(t, h, caller)
	rec := doRequest(t, h, http.MethodGet, "/market", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /market status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Market []marketView `json:"market"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Market) != 200 {
		t.Fatalf("market returned %d rows, want 200", len(out.Market))
	}
	// Newest first: the highest inserted id leads.
	if out.Market[0].ListingID != 205 || out.Market[199].ListingID != 6 {
		t.Errorf("cap window = %d..%d, want 205..6 (newest 200 of 205)", out.Market[0].ListingID, out.Market[199].ListingID)
	}
}

// 3. The listings route returns every state for the caller and nothing of
// anyone else's; a bought listing appears once per side with opposite roles.
func TestListingsRouteBothSidesAllStates(t *testing.T) {
	env := newReadsEnv(t)
	seller := testAccount(t)
	buyer := testAccount(t)
	stranger := testAccount(t)
	ctx := context.Background()

	// Seller lists three rows: open, cancelled, sold.
	seedListing(t, env, market.Listing{
		ListingID: 10, Account: seller, Credits: 100, PayToken: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PayTokenSymbol: "USDC", PayTokenDecimals: 6, Price: 50, MaturesAt: 1_000_000, ListedTx: "0xlist10", ListedBlock: 100, ListedAt: "2026-01-01T00:00:00Z",
	})
	seedListing(t, env, market.Listing{
		ListingID: 11, Account: seller, Credits: 200, PayToken: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Price: 60, MaturesAt: 2_000_000, ListedTx: "0xlist11", ListedBlock: 101, ListedAt: "2026-01-01T00:01:00Z",
	})
	if _, err := env.marketStore.CancelListing(ctx, 11, "0xcancel11", 102, "2026-01-01T00:02:00Z"); err != nil {
		t.Fatalf("CancelListing: %v", err)
	}
	seedListing(t, env, market.Listing{
		ListingID: 12, Account: seller, Credits: 300, PayToken: "0xcccccccccccccccccccccccccccccccccccccccc",
		PayTokenSymbol: "USDT", PayTokenDecimals: 18, Price: 70, MaturesAt: 3_000_000, ListedTx: "0xlist12", ListedBlock: 103, ListedAt: "2026-01-01T00:03:00Z",
	})
	if _, err := env.marketStore.SoldListing(ctx, 12, buyer, "0xsold12", 104, "2026-01-01T00:04:00Z"); err != nil {
		t.Fatalf("SoldListing: %v", err)
	}

	h := marketHandler(t, env, time.Unix(0, 0).UTC())

	// The seller sees all three states, all role selling, with the moves that
	// happened populated and the ones that did not omitted.
	sellerRec := doRequest(t, h, http.MethodGet, "/listings", consoleTokenFor(t, h, seller), nil)
	if sellerRec.Code != http.StatusOK {
		t.Fatalf("seller GET /listings status = %d, want 200; body: %s", sellerRec.Code, sellerRec.Body.String())
	}
	var sellerOut struct {
		Listings []listingView `json:"listings"`
	}
	if err := json.Unmarshal(sellerRec.Body.Bytes(), &sellerOut); err != nil {
		t.Fatalf("seller body not JSON: %v; body: %s", err, sellerRec.Body.String())
	}
	if len(sellerOut.Listings) != 3 {
		t.Fatalf("seller listings = %+v, want 3", sellerOut.Listings)
	}
	sellerByID := map[uint64]listingView{}
	for _, l := range sellerOut.Listings {
		sellerByID[uint64(l.ListingID)] = l
	}
	for id, l := range sellerByID {
		if l.Role != "selling" {
			t.Errorf("seller listing %d role = %q, want selling", id, l.Role)
		}
		if l.ListedTx == "" || l.ListedBlock == 0 {
			t.Errorf("seller listing %d missing listed move: %+v", id, l)
		}
	}
	if l := sellerByID[10]; l.State != "open" || l.CancelledTx != "" || l.SoldTx != "" {
		t.Errorf("listing 10 = state %q cancelledTx %q soldTx %q, want open with no cancel/sale", l.State, l.CancelledTx, l.SoldTx)
	}
	if l := sellerByID[11]; l.State != "cancelled" || l.CancelledTx != "0xcancel11" || l.SoldTx != "" {
		t.Errorf("listing 11 = state %q cancelledTx %q soldTx %q, want cancelled with cancel move only", l.State, l.CancelledTx, l.SoldTx)
	}
	if l := sellerByID[12]; l.State != "sold" || l.SoldTx != "0xsold12" || l.BuyerAccount != buyer {
		t.Errorf("listing 12 = state %q soldTx %q buyer %q, want sold with sale move and buyer", l.State, l.SoldTx, l.BuyerAccount)
	}
	// Decimals ride the wire in /listings too; unresolved stays 0.
	if sellerByID[10].PayTokenDecimals != 6 || sellerByID[11].PayTokenDecimals != 0 || sellerByID[12].PayTokenDecimals != 18 {
		t.Errorf("decimals = %d/%d/%d for ids 10/11/12, want 6/0/18", sellerByID[10].PayTokenDecimals, sellerByID[11].PayTokenDecimals, sellerByID[12].PayTokenDecimals)
	}

	// The buyer sees the sold listing exactly once, role buying, nothing else.
	buyerRec := doRequest(t, h, http.MethodGet, "/listings", consoleTokenFor(t, h, buyer), nil)
	if buyerRec.Code != http.StatusOK {
		t.Fatalf("buyer GET /listings status = %d, want 200; body: %s", buyerRec.Code, buyerRec.Body.String())
	}
	var buyerOut struct {
		Listings []listingView `json:"listings"`
	}
	if err := json.Unmarshal(buyerRec.Body.Bytes(), &buyerOut); err != nil {
		t.Fatalf("buyer body not JSON: %v; body: %s", err, buyerRec.Body.String())
	}
	if len(buyerOut.Listings) != 1 {
		t.Fatalf("buyer listings = %+v, want exactly the one bought row", buyerOut.Listings)
	}
	b := buyerOut.Listings[0]
	if b.ListingID != 12 || b.Role != "buying" || b.State != "sold" || b.BuyerAccount != buyer {
		t.Errorf("buyer row = %+v, want listing 12 role buying state sold", b)
	}
	if b.SoldTx != "0xsold12" || b.SoldBlock != 104 {
		t.Errorf("buyer row sold move = %q/%d, want 0xsold12/104", b.SoldTx, b.SoldBlock)
	}

	// A stranger sees nothing.
	strangerRec := doRequest(t, h, http.MethodGet, "/listings", consoleTokenFor(t, h, stranger), nil)
	if strangerRec.Code != http.StatusOK {
		t.Fatalf("stranger GET /listings status = %d, want 200; body: %s", strangerRec.Code, strangerRec.Body.String())
	}
	var strangerOut struct {
		Listings []listingView `json:"listings"`
	}
	json.Unmarshal(strangerRec.Body.Bytes(), &strangerOut)
	if len(strangerOut.Listings) != 0 {
		t.Errorf("stranger listings = %+v, want empty", strangerOut.Listings)
	}
}

// 4. Auth on the market routes: no token is 401 with the unchanged reason.
func TestMarketRoutesAuth(t *testing.T) {
	env := newReadsEnv(t)
	h := marketHandler(t, env, time.Unix(0, 0).UTC())
	for _, path := range []string{"/market", "/listings"} {
		rec := doRequest(t, h, http.MethodGet, path, "", nil)
		if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "missing token" {
			t.Errorf("%s no token: status=%d reason=%q, want 401/\"missing token\"", path, rec.Code, errorReason(t, rec))
		}
	}
}

// listingsPage is one parsed /listings or /market response with its cursor.
type listingsPage struct {
	Listings   []listingView `json:"listings"`
	Market     []marketView  `json:"market"`
	NextCursor wireUint64    `json:"nextCursor"`
	HasNext    bool
}

// fetchPage GETs path and decodes the rows plus the nextCursor presence.
func fetchPage(t *testing.T, h *Handler, token, path string) listingsPage {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, path, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body: %s", path, rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body not JSON: %v; body: %s", err, rec.Body.String())
	}
	var p listingsPage
	if ls, ok := raw["listings"]; ok {
		json.Unmarshal(ls, &p.Listings)
	}
	if mk, ok := raw["market"]; ok {
		json.Unmarshal(mk, &p.Market)
	}
	if nc, ok := raw["nextCursor"]; ok {
		json.Unmarshal(nc, &p.NextCursor)
		p.HasNext = true
	}
	return p
}

// listingIDs returns the row ids of a page in wire order (newest first).
func pageIDs(p listingsPage) []uint64 {
	out := []uint64{}
	for _, l := range p.Listings {
		out = append(out, uint64(l.ListingID))
	}
	for _, m := range p.Market {
		out = append(out, uint64(m.ListingID))
	}
	return out
}

// TestListingsCursorPagination walks one account's full listing history with
// a before-id cursor: stable newest-first order, no gaps or repeats across
// batch boundaries, nextCursor naming the last row's id, and exhaustion
// omitting it. An absent cursor serves the newest page.
func TestListingsCursorPagination(t *testing.T) {
	env := newReadsEnv(t)
	seller := testAccount(t)
	ctx := context.Background()
	const total = 5
	// Seed ids 1..total; newest-first order is total..1.
	for i := uint64(1); i <= total; i++ {
		seedListing(t, env, market.Listing{
			ListingID: i, Account: seller, Credits: i, PayToken: "0xabc",
			MaturesAt: i, ListedTx: "0xlist", ListedBlock: i, ListedAt: "2026-01-01T00:00:00Z",
		})
	}
	// Move two to non-open states so /listings sees every state but /market
	// does not.
	if _, err := env.marketStore.CancelListing(ctx, 2, "0xcancel", 1, "2026-01-01T00:01:00Z"); err != nil {
		t.Fatalf("CancelListing: %v", err)
	}
	if _, err := env.marketStore.SoldListing(ctx, 3, testAccount(t), "0xsold", 2, "2026-01-01T00:02:00Z"); err != nil {
		t.Fatalf("SoldListing: %v", err)
	}

	h := marketHandler(t, env, time.Unix(0, 0).UTC())
	token := consoleTokenFor(t, h, seller)

	// All-states walk: five rows, ids 5,4,3,2,1. The route caps at
	// marketPageCap, so a single request returns all five; the cursor still
	// must name the last row and an exhausted walk must stop.
	wantAll := []uint64{5, 4, 3, 2, 1}
	seen := []uint64{}
	dup := map[uint64]bool{}
	before := int64(0)
	steps := 0
	for {
		path := "/listings"
		if before > 0 {
			path += "?before=" + strconv.FormatInt(before, 10)
		}
		p := fetchPage(t, h, token, path)
		ids := pageIDs(p)
		if len(ids) == 0 {
			if p.HasNext {
				t.Errorf("exhausted /listings carries nextCursor=%d, want absent", p.NextCursor)
			}
			break
		}
		for _, id := range ids {
			if dup[id] {
				t.Errorf("listing id %d repeated across batches", id)
			}
			dup[id] = true
			seen = append(seen, id)
		}
		if !p.HasNext || p.NextCursor == 0 {
			t.Fatalf("non-exhausted /listings has no nextCursor")
		}
		before = int64(ids[len(ids)-1])
		if before != int64(p.NextCursor) {
			t.Fatalf("nextCursor=%d, want last row id %d", p.NextCursor, ids[len(ids)-1])
		}
		steps++
		if steps > 10 {
			t.Fatalf("walk did not terminate after %d steps", steps)
		}
	}
	if len(seen) != len(wantAll) {
		t.Fatalf("walk visited %d listings, want %d; seen=%v", len(seen), len(wantAll), seen)
	}
	for i := range wantAll {
		if seen[i] != wantAll[i] {
			t.Errorf("walk order diverges at %d: got %d, want %d (seen=%v)", i, seen[i], wantAll[i], seen)
		}
	}

	// Absent cursor serves the newest page: all five, ids 5..1.
	first := fetchPage(t, h, token, "/listings")
	if got := pageIDs(first); len(got) != 5 || got[0] != 5 || got[4] != 1 {
		t.Errorf("no-cursor /listings = %v, want [5 4 3 2 1] (newest page)", got)
	}

	// Open-only market walk: open set is 5,4,1 (2 cancelled, 3 sold), newest
	// first.
	wantOpen := []uint64{5, 4, 1}
	openSeen := []uint64{}
	openDup := map[uint64]bool{}
	mbefore := int64(0)
	msteps := 0
	for {
		path := "/market"
		if mbefore > 0 {
			path += "?before=" + strconv.FormatInt(mbefore, 10)
		}
		p := fetchPage(t, h, token, path)
		ids := pageIDs(p)
		if len(ids) == 0 {
			if p.HasNext {
				t.Errorf("exhausted /market carries nextCursor=%d, want absent", p.NextCursor)
			}
			break
		}
		for _, id := range ids {
			if openDup[id] {
				t.Errorf("market listing id %d repeated across batches", id)
			}
			openDup[id] = true
			openSeen = append(openSeen, id)
		}
		if !p.HasNext || p.NextCursor == 0 {
			t.Fatalf("non-exhausted /market has no nextCursor")
		}
		mbefore = int64(ids[len(ids)-1])
		if mbefore != int64(p.NextCursor) {
			t.Fatalf("market nextCursor=%d, want last row id %d", p.NextCursor, ids[len(ids)-1])
		}
		msteps++
		if msteps > 10 {
			t.Fatalf("market walk did not terminate after %d steps", msteps)
		}
	}
	if len(openSeen) != len(wantOpen) {
		t.Fatalf("market walk visited %d, want %d; seen=%v want=%v", len(openSeen), len(wantOpen), openSeen, wantOpen)
	}
	for i := range wantOpen {
		if openSeen[i] != wantOpen[i] {
			t.Errorf("market walk order diverges at %d: got %d, want %d (seen=%v)", i, openSeen[i], wantOpen[i], openSeen)
		}
	}
}
