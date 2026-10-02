package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"slotfare/internal/meter"
)

// These tests run the shared atomic statements against a SQLite stand-in:
// live Postgres is unavailable in the offline suite, and the stand-in is
// the proof the statements are dialect-neutral and behave atomically. The
// exact same text runs on Postgres through the PG placeholder rewrite; the
// live-Postgres-only fact that pg_try_advisory_lock excludes across real
// sessions is named in the runbook instead.

// standinStore opens one *sql.DB pool on the SQLite file at path and
// returns a MeterStore on it. The store's qf is the identity function, so
// the shared ?-placeholder SQL runs unmodified, exactly as the SQLite
// drivers run it.
func standinStore(t *testing.T, path string) *MeterStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open stand-in pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &MeterStore{db: db, qf: func(q string) string { return q }}
}

// seedAccounts opens the shared schema at path (the same one meter.Open
// creates) and flushes one account row into it.
func seedAccounts(t *testing.T, path string, accts map[string]meter.Account) {
	t.Helper()
	base, err := meter.Open(path)
	if err != nil {
		t.Fatalf("meter.Open: %v", err)
	}
	defer base.Close()
	if err := base.Flush(context.Background(), accts); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
}

// TestConcurrentReserveNeverOverCommits races two store pools against one
// account: with spendable for exactly two holds, concurrent reserves may
// succeed at most twice, and the account ends at the spent balance with
// everything else untouched. Two writers prove the conditional UPDATE,
// not just the pool: both pools hold independent connections to the same
// table.
func TestConcurrentReserveNeverOverCommits(t *testing.T) {
	ctx := context.Background()
	minted := uint64(100) * meter.MicroCreditsPerCredit
	hold := minted / 2
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{"a": {ID: "a", Minted: minted}})
	s1, s2 := standinStore(t, path), standinStore(t, path)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, insufficient, other int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var s *MeterStore
			if i%2 == 0 {
				s = s1
			} else {
				s = s2
			}
			err := s.Reserve(ctx, "a", hold)
			mu.Lock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, meter.ErrInsufficientCredits):
				insufficient++
			default:
				other++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("successful reserves = %d, want exactly 2 (never over-commit)", ok)
	}
	if insufficient != 6 {
		t.Errorf("insufficient-credit rejections = %d, want 6", insufficient)
	}
	if other != 0 {
		t.Errorf("%d reserves failed with an unexpected error", other)
	}
	accts, err := s1.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := accts["a"]
	if a.Pending != minted {
		t.Errorf("pending = %d, want %d (the two holds)", a.Pending, minted)
	}
	if a.Spendable() != 0 {
		t.Errorf("spendable = %d, want 0", a.Spendable())
	}
}

// TestReserveUnknownAccountIsInsufficient mirrors the in-memory zero-value
// account: reserving from an account the table has never seen fails as
// insufficient credits, not as an error.
func TestReserveUnknownAccountIsInsufficient(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{})
	s := standinStore(t, path)
	err := s.Reserve(ctx, "ghost", meter.MicroCreditsPerCredit)
	if !errors.Is(err, meter.ErrInsufficientCredits) {
		t.Fatalf("Reserve(unknown) = %v, want ErrInsufficientCredits", err)
	}
}

// TestReleaseSaturatesAtZero checks the GREATEST floor: releasing more
// than is pending clears pending instead of underflowing, and releasing
// from an unknown account is a silent no-op.
func TestReleaseSaturatesAtZero(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{"a": {ID: "a", Pending: 100}})
	s := standinStore(t, path)
	if err := s.Release(ctx, "a", 500); err != nil {
		t.Fatalf("Release: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := accts["a"].Pending; got != 0 {
		t.Errorf("pending after over-release = %d, want 0 (saturated)", got)
	}
	if err := s.Release(ctx, "ghost", 100); err != nil {
		t.Errorf("Release(unknown) = %v, want nil", err)
	}
}

// TestSettleCreditsCapsAtUnlockedBalance checks the MarkSettled cap in SQL:
// a settle moves min(credits, pending, minted - settled - locked), so a
// lock blocks settling into locked credits and the remainder stays pending.
func TestSettleCreditsCapsAtUnlockedBalance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 100, Pending: 100, Locked: 40},
	})
	s := standinStore(t, path)
	// min(100, 100, 100-0-40=60) = 60 moves.
	if err := s.SettleCredits(ctx, "a", 100); err != nil {
		t.Fatalf("SettleCredits: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := accts["a"]
	if a.Settled != 60 || a.Pending != 40 {
		t.Errorf("settled/pending = %d/%d, want 60/40 (capped at the unlocked balance)", a.Settled, a.Pending)
	}
}

// TestSettleCreditsCapsAtPending checks the pending side of the cap: a
// settle asks for more credits than the account has pending and moves only
// the pending.
func TestSettleCreditsCapsAtPending(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 100, Pending: 30},
	})
	s := standinStore(t, path)
	if err := s.SettleCredits(ctx, "a", 100); err != nil {
		t.Fatalf("SettleCredits: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := accts["a"]
	if a.Settled != 30 || a.Pending != 0 {
		t.Errorf("settled/pending = %d/%d, want 30/0 (capped at pending)", a.Settled, a.Pending)
	}
}

// TestBalanceReturnsSpendable checks the shared balance read: one account's
// row comes back as its spendable position - minted minus settled minus
// pending minus locked, saturating at zero.
func TestBalanceReturnsSpendable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 100, Settled: 40, Pending: 30, Locked: 5},
	})
	s := standinStore(t, path)
	got, err := s.Balance(ctx, "a")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := uint64(25); got != want {
		t.Errorf("Balance = %d, want %d (100-40-30-5)", got, want)
	}
}

// TestAccountReadsSharedRow checks the console's read path: the full row,
// locked included, comes from the shared table, and an unknown account is
// the zero Account with an empty ID (the "not known" signal).
func TestAccountReadsSharedRow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 100, Settled: 40, Pending: 30, Locked: 5},
	})
	s := standinStore(t, path)
	got, err := s.Account(ctx, "a")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	want := meter.Account{ID: "a", Minted: 100, Settled: 40, Pending: 30, Locked: 5}
	if got != want {
		t.Errorf("Account = %+v, want %+v", got, want)
	}
	if err := s.SetLocked(ctx, "a", 12); err != nil {
		t.Fatalf("SetLocked: %v", err)
	}
	if got, _ = s.Account(ctx, "a"); got.Locked != 12 {
		t.Errorf("Locked after SetLocked = %d, want 12", got.Locked)
	}
	unknown, err := s.Account(ctx, "nobody")
	if err != nil || unknown != (meter.Account{}) {
		t.Errorf("unknown account = %+v, %v; want zero Account, nil", unknown, err)
	}
}

// TestBalanceSaturatesAtZero mirrors Account.Spendable: counters that
// outpace a stale minted value report 0, not an underflow.
func TestBalanceSaturatesAtZero(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 10, Settled: 4, Pending: 8},
	})
	s := standinStore(t, path)
	got, err := s.Balance(ctx, "a")
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got != 0 {
		t.Errorf("Balance = %d, want 0 (saturated at zero)", got)
	}
}

// TestBalanceUnknownAccountIsZero mirrors Reserve's unknown-account
// semantics: no row is a zero-value account, so it reports 0 with no error.
func TestBalanceUnknownAccountIsZero(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{})
	s := standinStore(t, path)
	got, err := s.Balance(ctx, "ghost")
	if err != nil {
		t.Fatalf("Balance(unknown) = %v, want no error", err)
	}
	if got != 0 {
		t.Errorf("Balance(unknown) = %d, want 0", got)
	}
}

// TestSetMintedUpsertCreatesUnknownAccount checks the upsert's create side:
// an account the table has never seen arrives with zero settled, pending
// and locked.
func TestSetMintedUpsertCreatesUnknownAccount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{})
	s := standinStore(t, path)
	if err := s.SetMinted(ctx, "a", 7); err != nil {
		t.Fatalf("SetMinted: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, ok := accts["a"]
	if !ok || a.Minted != 7 || a.Settled != 0 || a.Pending != 0 || a.Locked != 0 {
		t.Errorf("upserted account = %+v (present %v), want minted 7, zeros elsewhere", a, ok)
	}
}

// TestSetMintedUpsertKeepsCounters checks the upsert's update side: only
// minted moves, so a concurrent reserve or settle on the row is never
// clobbered.
func TestSetMintedUpsertKeepsCounters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 100, Settled: 40, Pending: 30, Locked: 5},
	})
	s := standinStore(t, path)
	if err := s.SetMinted(ctx, "a", 200); err != nil {
		t.Fatalf("SetMinted: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := accts["a"]
	if a.Minted != 200 || a.Settled != 40 || a.Pending != 30 || a.Locked != 5 {
		t.Errorf("account after mint upsert = %+v, want minted 200 with counters kept", a)
	}
}

// TestSetLockedUpsertCreatesUnknownAccount mirrors the mint upsert test for
// the lock: an account the table has never seen arrives with zero minted,
// settled and pending.
func TestSetLockedUpsertCreatesUnknownAccount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{})
	s := standinStore(t, path)
	if err := s.SetLocked(ctx, "a", 9); err != nil {
		t.Fatalf("SetLocked: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a, ok := accts["a"]
	if !ok || a.Locked != 9 || a.Minted != 0 || a.Settled != 0 || a.Pending != 0 {
		t.Errorf("upserted account = %+v (present %v), want locked 9, zeros elsewhere", a, ok)
	}
}

// TestUsagePageAndFacetsStandin runs the paged usage read and the facet
// reads through the Postgres driver against the SQLite stand-in, proving
// the shared statements execute from this driver too.
func TestUsagePageAndFacetsStandin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-page.db")
	base, err := meter.Open(path)
	if err != nil {
		t.Fatalf("meter.Open: %v", err)
	}
	defer base.Close()
	ctx := context.Background()
	const baseTS = 1_700_000_000
	for i := 0; i < 10; i++ {
		prefix, model := "attk_aaaa", "model-x"
		if i%2 == 1 {
			prefix, model = "attk_bbbb", "model-y"
		}
		status := "ok"
		if i%5 == 0 {
			status = "error"
		}
		row := meter.UsageRow{
			AccountID: "acct-1", KeyPrefix: prefix,
			Timestamp: time.Unix(baseTS+int64(i), 0).UTC(),
			Service:   "rpc", Model: model, Credits: uint64(i + 1), Status: status,
		}
		if err := base.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}
	s := standinStore(t, path)

	rows, total, err := s.UsagePage(ctx, "acct-1", meter.UsageFilter{}, 2, 3)
	if err != nil {
		t.Fatalf("UsagePage: %v", err)
	}
	if total != 10 || len(rows) != 3 {
		t.Fatalf("UsagePage page 2 size 3 = (%d rows, total %d), want (3 rows, total 10)", len(rows), total)
	}
	if rows[0].Credits != 7 || rows[2].Credits != 5 {
		t.Errorf("page 2 credits = [%d..%d], want [7..5] newest first", rows[0].Credits, rows[2].Credits)
	}

	filtered, total, err := s.UsagePage(ctx, "acct-1",
		meter.UsageFilter{KeyPrefix: "attk_aaaa", Model: "model-x", From: baseTS + 4, To: baseTS + 8, ErrorsOnly: true}, 1, 10)
	if err != nil {
		t.Fatalf("UsagePage filtered: %v", err)
	}
	// i even, ts in [4,8], error (i%5==0): only i=0 -> excluded by From;
	// no row matches.
	if total != 0 || len(filtered) != 0 {
		t.Errorf("filtered UsagePage = (%d rows, total %d), want (0, 0)", len(filtered), total)
	}
	filtered, total, err = s.UsagePage(ctx, "acct-1",
		meter.UsageFilter{KeyPrefix: "attk_bbbb", From: baseTS + 4}, 1, 10)
	if err != nil {
		t.Fatalf("UsagePage prefix+from: %v", err)
	}
	// i odd, ts >= base+4: i in {5,7,9} -> 3 rows.
	if total != 3 || len(filtered) != 3 || filtered[0].Credits != 10 {
		t.Errorf("prefix+from UsagePage = (%d rows, total %d), want newest-first 3 rows starting at credits 10", len(filtered), total)
	}

	prefixes, models, err := s.UsageFacets(ctx, "acct-1")
	if err != nil {
		t.Fatalf("UsageFacets: %v", err)
	}
	if len(prefixes) != 2 || prefixes[0] != "attk_aaaa" || prefixes[1] != "attk_bbbb" {
		t.Errorf("facets prefixes = %v, want [attk_aaaa attk_bbbb]", prefixes)
	}
	if len(models) != 2 || models[0] != "model-x" || models[1] != "model-y" {
		t.Errorf("facets models = %v, want [model-x model-y]", models)
	}
}
