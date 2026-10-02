package pgstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/session"
)

// sqliteImportSpec is the ImportSpec for two SQLite databases standing in
// for source and target: the same tables and column lists as PGImportSpec,
// but no sequences to re-align.
var sqliteImportSpec = ImportSpec{
	Tables:    PGImportSpec.Tables,
	Columns:   PGImportSpec.Columns,
	Sequences: nil,
}

// openSQLiteStore opens one shared-file store quadruple at path.
func openSQLiteStore(t *testing.T, path string) (*keys.SQLite, *meter.SQLite, *funding.SQLite, *market.SQLite, *session.SQLite) {
	t.Helper()
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
	return ks, ms, fs, mk, ss
}

// seedSource fills a fresh store quadruple with one row per table,
// including usage past the cap so pruning is exercised, and both listing
// states.
func seedSource(t *testing.T, ks *keys.SQLite, ms *meter.SQLite, fs *funding.SQLite, mk *market.SQLite, ss *session.SQLite) {
	t.Helper()
	ctx := context.Background()
	k := keys.Key{Hash: [32]byte{0xab}, AccountID: "acct-1", Prefix: "attk_0123456789", Label: "test", CreatedAt: 1700000000} // gitleaks:allow
	if err := ks.Add(ctx, k); err != nil {
		t.Fatalf("keys.Add: %v", err)
	}
	accts := map[string]meter.Account{"acct-1": {ID: "acct-1", Minted: 1000, Settled: 400, Pending: 600, Locked: 50}}
	if err := ms.Flush(ctx, accts); err != nil {
		t.Fatalf("meter.Flush: %v", err)
	}
	for i := 0; i < meter.UsageCap+5; i++ {
		row := meter.UsageRow{
			AccountID: "acct-1", KeyPrefix: "attk_0123456789", // gitleaks:allow
			Timestamp: time.Unix(int64(1700000000+i), 0).UTC(),
			Service:   "rpc", Model: "eth_call",
			PromptTokens: 10, CachedTokens: 2, CompletionTokens: 3,
			Credits: uint64(i + 1), Status: "ok",
		}
		if i%7 == 0 {
			row.Status = "error"
		}
		if err := ms.RecordUsage(ctx, row); err != nil {
			t.Fatalf("meter.RecordUsage %d: %v", i, err)
		}
	}
	purchase := funding.Purchase{
		AccountID: "acct-1", ChainKey: 1, DepositID: "42",
		SourceTx: "0xdead", SourceBlock: 100, AttestedHeight: 120,
		Token: "USDC", UsdMicros: 2500000, StartedAt: "2026-01-01T00:00:00Z",
	}
	if err := fs.Record(ctx, purchase); err != nil {
		t.Fatalf("funding.Record: %v", err)
	}
	if err := fs.SetCursor(ctx, "source", 555); err != nil {
		t.Fatalf("funding.SetCursor: %v", err)
	}
	listing := market.Listing{
		ListingID: 7, Account: "seller-1", Seller: "0xseller",
		Credits: 50, PayToken: "0xtoken", PayTokenSymbol: "USDC", PayTokenDecimals: 6,
		Price: 55, MaturesAt: 1700000999, ListedTx: "0xlist", ListedBlock: 200,
		ListedAt: "2026-01-02T00:00:00Z",
	}
	if _, err := mk.RecordListing(ctx, listing); err != nil {
		t.Fatalf("market.RecordListing: %v", err)
	}
	sold := market.Listing{
		ListingID: 8, Account: "seller-2", Seller: "0xseller2",
		Credits: 10, Price: 12, MaturesAt: 1700001000,
		ListedTx: "0xlist2", ListedBlock: 201, ListedAt: "2026-01-03T00:00:00Z",
	}
	if _, err := mk.RecordListing(ctx, sold); err != nil {
		t.Fatalf("market.RecordListing (sold): %v", err)
	}
	if _, err := mk.SoldListing(ctx, 8, "buyer-1", "0xsale", 210, "2026-01-04T00:00:00Z"); err != nil {
		t.Fatalf("market.SoldListing: %v", err)
	}
	if err := ss.Save(ctx, session.Session{
		Hash:      [32]byte{0xcd},
		AccountID: "acct-1",
		IssuedAt:  1700000000, ExpiresAt: 1700121600, LastSeen: 1700000000,
	}); err != nil {
		t.Fatalf("sessions.Save: %v", err)
	}
}

func TestImportRowAndDataParity(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	srcKS, srcMS, srcFS, srcMK, srcSS := openSQLiteStore(t, srcPath)
	defer srcKS.Close()
	defer srcMS.Close()
	defer srcFS.Close()
	defer srcMK.Close()
	defer srcSS.Close()
	seedSource(t, srcKS, srcMS, srcFS, srcMK, srcSS)

	dstKS, dstMS, dstFS, dstMK, dstSS := openSQLiteStore(t, dstPath)
	defer dstKS.Close()
	defer dstMS.Close()
	defer dstFS.Close()
	defer dstMK.Close()
	defer dstSS.Close()

	srcDB, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	defer srcDB.Close()
	dstDB, err := sql.Open("sqlite", "file:"+dstPath)
	if err != nil {
		t.Fatalf("open dst db: %v", err)
	}
	defer dstDB.Close()

	counts, err := Import(ctx, srcDB, dstDB, sqliteImportSpec)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for table, want := range map[string]int64{"api_keys": 1, "accounts": 1, "usage": int64(meter.UsageCap), "funding": 1, "funding_cursor": 1, "listings": 2, "sessions": 1} {
		if counts[table] != want {
			t.Errorf("Import count for %s = %d, want %d", table, counts[table], want)
		}
	}

	// Data parity through the store APIs on the destination.
	gotK, ok, err := dstKS.Lookup(ctx, [32]byte{0xab})
	if err != nil {
		t.Fatalf("destination key lookup: %v", err)
	}
	if !ok {
		t.Fatal("destination key not found")
	}
	if gotK.AccountID != "acct-1" || gotK.Prefix != "attk_0123456789" || gotK.Label != "test" || gotK.CreatedAt != 1700000000 {
		t.Errorf("destination key mismatch: %+v", gotK)
	}
	accts, err := dstMS.Load(ctx)
	if err != nil {
		t.Fatalf("destination Load: %v", err)
	}
	a, ok := accts["acct-1"]
	if !ok || a.Minted != 1000 || a.Settled != 400 || a.Pending != 600 || a.Locked != 50 {
		t.Errorf("destination account mismatch: %+v (present=%v)", a, ok)
	}
	usage, err := dstMS.RecentUsage(ctx, "acct-1", 10, "", false, 0)
	if err != nil {
		t.Fatalf("destination RecentUsage: %v", err)
	}
	if len(usage) != 10 {
		t.Fatalf("destination RecentUsage returned %d rows, want 10", len(usage))
	}
	// Newest first: the last five seeded rows are the ones that survived
	// pruning, so the newest row is credits UsageCap+5.
	if usage[0].Credits != uint64(meter.UsageCap+5) {
		t.Errorf("newest destination usage row has credits %d, want %d", usage[0].Credits, meter.UsageCap+5)
	}
	// The seeded rows predate the duration_ms column: the import carries
	// NULL through, never a fabricated 0.
	for i, u := range usage {
		if u.DurationMS != nil {
			t.Errorf("destination usage row %d has DurationMS %d, want nil (imported NULL)", i, *u.DurationMS)
		}
	}
	errsOnly, err := dstMS.RecentUsage(ctx, "acct-1", 100, "", true, 0)
	if err != nil {
		t.Fatalf("destination RecentUsage errors-only: %v", err)
	}
	if len(errsOnly) == 0 {
		t.Error("destination lost its error rows")
	}
	purchases, err := dstFS.AllPurchases(ctx, "acct-1")
	if err != nil {
		t.Fatalf("destination AllPurchases: %v", err)
	}
	if len(purchases) != 1 || purchases[0].DepositID != "42" || purchases[0].ChainKey != 1 || purchases[0].UsdMicros != 2500000 {
		t.Errorf("destination purchase mismatch: %+v", purchases)
	}
	block, ok, err := dstFS.Cursor(ctx, "source")
	if err != nil {
		t.Fatalf("destination Cursor: %v", err)
	}
	if !ok || block != 555 {
		t.Errorf("destination cursor = (%d, %v), want (555, true)", block, ok)
	}
	listings, err := dstMK.AccountListings(ctx, "seller-1", 10, 0)
	if err != nil {
		t.Fatalf("destination AccountListings: %v", err)
	}
	if len(listings) != 1 || listings[0].ListingID != 7 || listings[0].State != "open" || listings[0].PayTokenDecimals != 6 {
		t.Errorf("destination listing mismatch: %+v", listings)
	}
	soldListings, err := dstMK.AccountListings(ctx, "buyer-1", 10, 0)
	if err != nil {
		t.Fatalf("destination AccountListings (buyer): %v", err)
	}
	if len(soldListings) != 1 || soldListings[0].State != "sold" || soldListings[0].BuyerAccount != "buyer-1" {
		t.Errorf("destination sold listing mismatch: %+v", soldListings)
	}
	locked, err := dstMK.LockedFor(ctx, "seller-1")
	if err != nil {
		t.Fatalf("destination LockedFor: %v", err)
	}
	if locked != 50 {
		t.Errorf("destination LockedFor = %d, want 50", locked)
	}
	sess, ok, err := dstSS.Lookup(ctx, [32]byte{0xcd})
	if err != nil {
		t.Fatalf("destination session lookup: %v", err)
	}
	if !ok {
		t.Fatal("destination session not found")
	}
	if sess.AccountID != "acct-1" || sess.IssuedAt != 1700000000 || sess.ExpiresAt != 1700121600 || sess.LastSeen != 1700000000 {
		t.Errorf("destination session mismatch: %+v", sess)
	}
}

func TestImportRefusesNonEmptyTarget(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	srcKS, srcMS, srcFS, srcMK, srcSS := openSQLiteStore(t, srcPath)
	defer srcKS.Close()
	defer srcMS.Close()
	defer srcFS.Close()
	defer srcMK.Close()
	defer srcSS.Close()
	seedSource(t, srcKS, srcMS, srcFS, srcMK, srcSS)

	dstKS, dstMS, dstFS, dstMK, dstSS := openSQLiteStore(t, dstPath)
	defer dstKS.Close()
	defer dstMS.Close()
	defer dstFS.Close()
	defer dstMK.Close()
	defer dstSS.Close()
	// Pre-seed one row in the target so it is not empty.
	if err := dstFS.SetCursor(ctx, "existing", 1); err != nil {
		t.Fatalf("pre-seed target: %v", err)
	}

	srcDB, err := sql.Open("sqlite", "file:"+srcPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open src db: %v", err)
	}
	defer srcDB.Close()
	dstDB, err := sql.Open("sqlite", "file:"+dstPath)
	if err != nil {
		t.Fatalf("open dst db: %v", err)
	}
	defer dstDB.Close()

	_, err = Import(ctx, srcDB, dstDB, sqliteImportSpec)
	if err == nil {
		t.Fatal("Import succeeded against a non-empty target, want refusal")
	}
	if !contains(err.Error(), "funding_cursor") || !contains(err.Error(), "already holds 1 rows") {
		t.Errorf("refusal error does not name the table and count: %v", err)
	}
	// The target must be unchanged: no api_keys row was copied.
	if _, ok, err := dstKS.Lookup(ctx, [32]byte{0xab}); err != nil || ok {
		t.Errorf("refused import still wrote to the target: ok=%v err=%v", ok, err)
	}
	if n, err := dstFS.AllPurchases(ctx, "acct-1"); err != nil || len(n) != 0 {
		t.Errorf("refused import copied funding rows: %v / %v", n, err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
