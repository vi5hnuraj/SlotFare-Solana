package meter

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	accounts := map[string]Account{
		"alice": {ID: "alice", Minted: 1000, Adjust: -40, Settled: 250, Pending: 120, Locked: 80},
		"bob":   {ID: "bob", Minted: 50, Settled: 50, Pending: 0},
	}
	if err := s.Flush(ctx, accounts); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(accounts) {
		t.Fatalf("Load returned %d accounts, want %d", len(got), len(accounts))
	}
	for id, want := range accounts {
		if got[id] != want {
			t.Errorf("account %s = %+v, want %+v", id, got[id], want)
		}
	}
}

func TestLoadFreshEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on fresh database: %v", err)
	}
	if got == nil {
		t.Fatal("Load returned nil map, want empty non-nil map")
	}
	if len(got) != 0 {
		t.Errorf("Load returned %d accounts, want 0", len(got))
	}
}

func TestFlushAfterRestartPreservesPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	ctx := context.Background()

	l := NewLedger()
	l.SetMinted("alice", 1000)
	if err := l.Reserve("alice", 340); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	l.MarkSettled("alice", 100)
	l.SetLocked("alice", 200)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Flush(ctx, l.Snapshot()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Restart: open again and load. Pending is the only durability that
	// matters here.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after restart: %v", err)
	}
	defer s2.Close()
	got, err := s2.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := l.Snapshot()["alice"]
	if got["alice"] != want {
		t.Errorf("after restart account = %+v, want %+v", got["alice"], want)
	}
	if got["alice"].Pending != 240 {
		t.Errorf("pending after restart = %d, want 240", got["alice"].Pending)
	}
	if got["alice"].Locked != 200 {
		t.Errorf("locked after restart = %d, want 200", got["alice"].Locked)
	}

	// A ledger restored from the checkpoint honours the lock too: spendable
	// is 1000-100-240-200 = 460, so reserving past it is refused.
	l2 := NewLedger()
	l2.Restore(got)
	if err := l2.Reserve("alice", 460); err != nil {
		t.Fatalf("Reserve(460) after restore: %v", err)
	}
	if err := l2.Reserve("alice", 1); err != ErrInsufficientCredits {
		t.Errorf("reserve into locked credits after restore error = %v, want ErrInsufficientCredits", err)
	}
}

// TestOpenMigratesPreLockedSchema checks that a checkpoint database written
// before the locked column existed still opens and loads, with locked
// defaulting to zero.
func TestOpenMigratesPreLockedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	oldSchema := `CREATE TABLE accounts (
		id TEXT PRIMARY KEY,
		minted INTEGER NOT NULL,
		settled INTEGER NOT NULL,
		pending INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (id, minted, settled, pending, updated_at) VALUES ('a', 100, 10, 20, 0)`); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	// This test is about the locked-column migration, not the micro-credit
	// unit change: mark the database already migrated so Open does not
	// scale the row's whole-credit values.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-locked database: %v", err)
	}
	defer s.Close()
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["a"] != (Account{ID: "a", Minted: 100, Settled: 10, Pending: 20}) {
		t.Errorf("migrated account = %+v, want locked defaulting to 0", got["a"])
	}
	if err := s.Flush(context.Background(), got); err != nil {
		t.Fatalf("Flush on migrated database: %v", err)
	}
}

// TestOpenMigratesCreditsToMicroOnce checks that a database written in
// whole credits is scaled to micro-credits exactly once on open, that a
// re-open is a no-op, and that a fresh database opens at version 1.
func TestOpenMigratesCreditsToMicroOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-credits.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	oldSchema := `CREATE TABLE accounts (
		id TEXT PRIMARY KEY,
		minted INTEGER NOT NULL,
		settled INTEGER NOT NULL,
		pending INTEGER NOT NULL,
		locked INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	usageSchema := `CREATE TABLE usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL,
		key_prefix TEXT NOT NULL,
		ts INTEGER NOT NULL,
		service TEXT NOT NULL,
		model TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL,
		cached_tokens INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL,
		credits INTEGER NOT NULL,
		status TEXT NOT NULL
	)`
	if _, err := db.Exec(usageSchema); err != nil {
		t.Fatalf("create old usage schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (id, minted, settled, pending, locked, updated_at) VALUES ('a', 1000, 10, 20, 3, 0)`); err != nil {
		t.Fatalf("insert old account row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO usage (account_id, key_prefix, ts, service, model, prompt_tokens, cached_tokens, completion_tokens, credits, status) VALUES ('a', 'attk_abcdefg', 0, 'inference', 'm', 5, 0, 13, 7, 'ok')`); err != nil {
		t.Fatalf("insert old usage row: %v", err)
	}
	// user_version is left at 0: an unmigrated database.
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on whole-credit database: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Account{ID: "a",
		Minted: 1000 * MicroCreditsPerCredit, Settled: 10 * MicroCreditsPerCredit,
		Pending: 20 * MicroCreditsPerCredit, Locked: 3 * MicroCreditsPerCredit}
	if got["a"] != want {
		t.Errorf("migrated account = %+v, want %+v (every amount x%d)", got["a"], want, MicroCreditsPerCredit)
	}
	rows, err := s.RecentUsage(ctx, "a", 1, "", false, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("RecentUsage: %v, %d rows", err, len(rows))
	}
	if rows[0].Credits != 7*MicroCreditsPerCredit {
		t.Errorf("migrated usage credits = %d, want %d", rows[0].Credits, 7*MicroCreditsPerCredit)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version after migration = %d, want 1", version)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: the guard must keep the values where the migration left
	// them - a second scaling would be a double charge.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()
	got2, err := s2.Load(ctx)
	if err != nil {
		t.Fatalf("Load after re-open: %v", err)
	}
	if got2["a"] != want {
		t.Errorf("account after re-open = %+v, want %+v (no second scaling)", got2["a"], want)
	}
	rows2, err := s2.RecentUsage(ctx, "a", 1, "", false, 0)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("RecentUsage after re-open: %v, %d rows", err, len(rows2))
	}
	if rows2[0].Credits != 7*MicroCreditsPerCredit {
		t.Errorf("usage credits after re-open = %d, want %d (no second scaling)", rows2[0].Credits, 7*MicroCreditsPerCredit)
	}
}

// A fresh database is stamped migrated on its first open; its empty tables
// make the UPDATEs no-ops.
func TestOpenFreshDatabaseIsVersionOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh-micro.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("fresh database user_version = %d, want 1", version)
	}
}

// A usage row's service kind survives the write/read round trip, and a
// row written before the kind column existed (no kind in the INSERT)
// reads back as empty - existing rows stay unchanged.
func TestUsageRowKindRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	row := UsageRow{
		AccountID: "acct-1", KeyPrefix: "attk_abcdefg",
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Service:   "inference", Model: "embed", Kind: "embeddings",
		PromptTokens: 120, Credits: 120, Status: "ok",
	}
	if err := s.RecordUsage(ctx, row); err != nil {
		t.Fatalf("RecordUsage(kind): %v", err)
	}
	// A legacy row: written without the kind column, exactly as an old
	// binary would have.
	if _, err := s.db.Exec(`INSERT INTO usage (account_id, key_prefix, ts, service, model,
		prompt_tokens, cached_tokens, completion_tokens, credits, status)
		VALUES ('acct-1', 'attk_abcdefg', 0, 'inference', 'old', 5, 0, 13, 7, 'ok')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	rows, err := s.RecentUsage(ctx, "acct-1", 10, "", false, 0)
	if err != nil {
		t.Fatalf("RecentUsage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("RecentUsage returned %d rows, want 2: %+v", len(rows), rows)
	}
	// Newest first by row id: the legacy row was inserted second.
	newest, oldest := rows[0], rows[1]
	if newest.Kind != "" || newest.Model != "old" {
		t.Errorf("legacy row = %+v, want empty kind reading back unchanged", newest)
	}
	if oldest.Kind != "embeddings" || oldest.Model != "embed" || oldest.PromptTokens != 120 || oldest.Credits != 120 {
		t.Errorf("kind row = %+v, want kind embeddings model embed 120/120", oldest)
	}
}

// A usage row's recorded duration survives the write/read round trip, and
// a row written before the duration_ms column existed (no duration in the
// INSERT) reads back as nil - absent, never fabricated as 0.
func TestUsageRowDurationRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-duration.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	dur := uint64(1234)
	row := UsageRow{
		AccountID: "acct-1", KeyPrefix: "attk_abcdefg",
		Timestamp: time.Unix(1700000000, 0).UTC(),
		Service:   "inference", Model: "m", Status: "ok",
		Credits: 120, DurationMS: &dur,
	}
	if err := s.RecordUsage(ctx, row); err != nil {
		t.Fatalf("RecordUsage(duration): %v", err)
	}
	// A legacy row: written without the duration_ms column, exactly as an
	// old binary would have.
	if _, err := s.db.Exec(`INSERT INTO usage (account_id, key_prefix, ts, service, model,
		prompt_tokens, cached_tokens, completion_tokens, credits, status)
		VALUES ('acct-1', 'attk_abcdefg', 0, 'inference', 'old', 5, 0, 13, 7, 'ok')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	rows, err := s.RecentUsage(ctx, "acct-1", 10, "", false, 0)
	if err != nil {
		t.Fatalf("RecentUsage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("RecentUsage returned %d rows, want 2: %+v", len(rows), rows)
	}
	// Newest first by row id: the legacy row was inserted second.
	newest, oldest := rows[0], rows[1]
	if newest.Model != "old" || newest.DurationMS != nil {
		t.Errorf("legacy row = %+v, want nil duration reading back absent, not zero", newest)
	}
	if oldest.Model != "m" || oldest.DurationMS == nil || *oldest.DurationMS != 1234 {
		t.Errorf("duration row = %+v, want DurationMS 1234", oldest)
	}
}

// TestOpenMigratesPreDurationSchema checks that a usage table written
// before the duration_ms column existed still opens (the duplicate-column
// ALTER is tolerated), that re-opening is a no-op, and that pre-existing
// rows read back with a nil duration.
func TestOpenMigratesPreDurationSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-usage.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	usageSchema := `CREATE TABLE usage (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL,
		key_prefix TEXT NOT NULL,
		ts INTEGER NOT NULL,
		service TEXT NOT NULL,
		model TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL,
		cached_tokens INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL,
		credits INTEGER NOT NULL,
		status TEXT NOT NULL
	)`
	if _, err := db.Exec(usageSchema); err != nil {
		t.Fatalf("create old usage schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO usage (account_id, key_prefix, ts, service, model, prompt_tokens, cached_tokens, completion_tokens, credits, status) VALUES ('a', 'attk_abcdefg', 0, 'rpc', 'm', 5, 0, 13, 7, 'ok')`); err != nil {
		t.Fatalf("insert old usage row: %v", err)
	}
	// Skip the micro-credit scaling; this test is about the column
	// migration, not the unit change.
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-duration database: %v", err)
	}
	rows, err := s.RecentUsage(context.Background(), "a", 10, "", false, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("RecentUsage on migrated database: %v, %d rows", err, len(rows))
	}
	if rows[0].DurationMS != nil {
		t.Errorf("pre-existing row duration = %d, want nil (absent, not zero)", *rows[0].DurationMS)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: the tolerated duplicate-column ALTER must be a no-op.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()
	rows2, err := s2.RecentUsage(context.Background(), "a", 10, "", false, 0)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("RecentUsage after re-open: %v, %d rows", err, len(rows2))
	}
	if rows2[0].DurationMS != nil {
		t.Errorf("pre-existing row duration after re-open = %d, want nil", *rows2[0].DurationMS)
	}
}

func TestFlusherThresholdAndShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	l := NewLedger()
	l.SetMinted("alice", 10000)
	// Interval is long; only the threshold may trigger a flush here.
	f := NewFlusher(l, s, time.Hour, 100)
	done := make(chan struct{})
	go func() {
		f.Run(ctx, func(err error) { t.Errorf("flush error: %v", err) })
		close(done)
	}()

	// Drive pending past the threshold and wait for a flush to land in the
	// store (assert on store contents, not wall-clock timing).
	for i := 0; i < 50; i++ {
		if err := l.Reserve("alice", 10); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	var pending uint64
	for {
		got, err := s.Load(ctx)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		pending = got["alice"].Pending
		if pending == 500 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("threshold flush did not land within deadline; store pending = %d", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Shutdown flush: add more pending, cancel, and require the final state.
	l.Reserve("alice", 10)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after shutdown: %v", err)
	}
	if got["alice"].Pending != 510 {
		t.Errorf("pending after shutdown flush = %d, want 510", got["alice"].Pending)
	}
}

// TestUsagePageAndFacets covers the paged usage read: each filter alone and
// combined, paging that walks the filtered set with correct totals, facets
// that match the seeded rows, and account isolation.
func TestUsagePageAndFacets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-page.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	const baseTS = 1_700_000_000
	const n = 30
	// i even -> prefix attk_aaaa, else attk_bbbb; i < 15 -> model-x, else
	// model-y; ts ascending with i so row id order equals ts order; every
	// 7th row is an error.
	prefixOf := func(i int) string {
		if i%2 == 0 {
			return "attk_aaaa"
		}
		return "attk_bbbb"
	}
	modelOf := func(i int) string {
		if i < 15 {
			return "model-x"
		}
		return "model-y"
	}
	statusOf := func(i int) string {
		if i%7 == 0 {
			return "error"
		}
		return "ok"
	}
	for i := 0; i < n; i++ {
		row := UsageRow{
			AccountID: "acct-1", KeyPrefix: prefixOf(i),
			Timestamp: time.Unix(baseTS+int64(i), 0).UTC(),
			Service:   "rpc", Model: modelOf(i),
			Credits: uint64(i + 1), Status: statusOf(i),
		}
		if err := s.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}
	// A second account's rows must never leak into results or facets.
	for i := 0; i < 2; i++ {
		row := UsageRow{
			AccountID: "acct-2", KeyPrefix: "attk_cccc",
			Timestamp: time.Unix(baseTS+int64(i), 0).UTC(),
			Service:   "rpc", Model: "model-z", Credits: 99, Status: "ok",
		}
		if err := s.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage acct-2 %d: %v", i, err)
		}
	}
	assertRows := func(desc string, got []UsageRow, wantCredits ...uint64) {
		t.Helper()
		if len(got) != len(wantCredits) {
			t.Errorf("%s: %d rows, want %d", desc, len(got), len(wantCredits))
			return
		}
		for i, r := range got {
			if r.AccountID != "acct-1" {
				t.Errorf("%s: row %d leaked from another account: %+v", desc, i, r)
			}
			if i < len(wantCredits) && r.Credits != wantCredits[i] {
				t.Errorf("%s: row %d credits = %d, want %d (newest first)", desc, i, r.Credits, wantCredits[i])
			}
		}
	}

	// No filter: total is the full count and pages walk newest first.
	rows, total, err := s.UsagePage(ctx, "acct-1", UsageFilter{}, 1, 7)
	if err != nil {
		t.Fatalf("UsagePage no filter: %v", err)
	}
	if total != n {
		t.Errorf("no filter total = %d, want %d", total, n)
	}
	wantPage1 := []uint64{}
	for i := n - 1; i >= n-7; i-- {
		wantPage1 = append(wantPage1, uint64(i+1))
	}
	assertRows("page 1 no filter", rows, wantPage1...)

	// Consecutive pages partition the set; the last page is short.
	seen := map[uint64]bool{}
	for p := 1; p <= 5; p++ {
		got, gotTotal, err := s.UsagePage(ctx, "acct-1", UsageFilter{}, p, 7)
		if err != nil {
			t.Fatalf("UsagePage page %d: %v", p, err)
		}
		if gotTotal != n {
			t.Errorf("page %d total = %d, want %d", p, gotTotal, n)
		}
		wantLen := 7
		if p == 5 {
			wantLen = 2 // 30 = 4*7 + 2
		}
		if len(got) != wantLen {
			t.Errorf("page %d has %d rows, want %d", p, len(got), wantLen)
		}
		for _, r := range got {
			if seen[r.Credits] {
				t.Errorf("row credits %d repeated across pages", r.Credits)
			}
			seen[r.Credits] = true
		}
	}
	if len(seen) != n {
		t.Errorf("walk visited %d distinct rows, want %d", len(seen), n)
	}

	// Out-of-range page: empty rows but the correct total.
	got, gotTotal, err := s.UsagePage(ctx, "acct-1", UsageFilter{}, 100, 7)
	if err != nil {
		t.Fatalf("UsagePage out of range: %v", err)
	}
	if len(got) != 0 || gotTotal != n {
		t.Errorf("out-of-range page = (%d rows, total %d), want (0 rows, total %d)", len(got), gotTotal, n)
	}

	// Each filter alone. Prefix: i even -> 15 rows, newest first.
	got, total, err = s.UsagePage(ctx, "acct-1", UsageFilter{KeyPrefix: "attk_aaaa"}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage prefix: %v", err)
	}
	if total != 15 {
		t.Errorf("prefix total = %d, want 15", total)
	}
	assertRows("prefix filter", got, 29, 27, 25, 23, 21, 19, 17, 15, 13, 11, 9, 7, 5, 3, 1)

	// Model: i < 15 -> 15 rows.
	got, total, err = s.UsagePage(ctx, "acct-1", UsageFilter{Model: "model-x"}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage model: %v", err)
	}
	if total != 15 {
		t.Errorf("model total = %d, want 15", total)
	}
	for _, r := range got {
		if r.Model != "model-x" {
			t.Errorf("model filter returned %q", r.Model)
		}
	}

	// From: ts >= baseTS+10 -> i >= 10 -> 20 rows, inclusive at the boundary.
	got, total, err = s.UsagePage(ctx, "acct-1", UsageFilter{From: baseTS + 10}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage from: %v", err)
	}
	if total != 20 {
		t.Errorf("from total = %d, want 20", total)
	}
	if len(got) != 20 || got[len(got)-1].Timestamp.Unix() != baseTS+10 {
		t.Errorf("from filter oldest boundary = %d, want %d", got[len(got)-1].Timestamp.Unix(), baseTS+10)
	}

	// To: ts <= baseTS+10 -> i <= 10 -> 11 rows, inclusive at the boundary.
	got, total, err = s.UsagePage(ctx, "acct-1", UsageFilter{To: baseTS + 10}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage to: %v", err)
	}
	if total != 11 {
		t.Errorf("to total = %d, want 11", total)
	}
	if len(got) != 11 || got[0].Timestamp.Unix() != baseTS+10 {
		t.Errorf("to filter newest boundary = %d, want %d", got[0].Timestamp.Unix(), baseTS+10)
	}

	// Errors only: i % 7 == 0 -> 5 rows.
	got, total, err = s.UsagePage(ctx, "acct-1", UsageFilter{ErrorsOnly: true}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage errors: %v", err)
	}
	if total != 5 {
		t.Errorf("errors total = %d, want 5", total)
	}
	for _, r := range got {
		if r.Status != "error" {
			t.Errorf("errors filter returned status %q", r.Status)
		}
	}

	// Combined filters intersect: prefix attk_aaaa + model-y + errors=true
	// leaves exactly i=28 (credits 29).
	got, total, err = s.UsagePage(ctx, "acct-1",
		UsageFilter{KeyPrefix: "attk_aaaa", Model: "model-y", ErrorsOnly: true}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage combined: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].Credits != 29 {
		t.Errorf("combined filters = %d rows (total %d), want exactly credits 29", len(got), total)
	}

	// Time range plus prefix: i in [5,9] AND i even -> i in {6,8} -> 2 rows.
	got, total, err = s.UsagePage(ctx, "acct-1",
		UsageFilter{KeyPrefix: "attk_aaaa", From: baseTS + 5, To: baseTS + 9}, 1, UsageCap)
	if err != nil {
		t.Fatalf("UsagePage range+prefix: %v", err)
	}
	if total != 2 {
		t.Errorf("range+prefix total = %d, want 2", total)
	}
	assertRows("range+prefix", got, 9, 7)

	// Paging inside a filtered set: prefix attk_bbbb (i odd, 15 rows, even
	// credits) with size 4 walks 4/4/4/3 and keeps the filtered total on
	// every page.
	var oddSeen []uint64
	for p := 1; p <= 4; p++ {
		got, total, err := s.UsagePage(ctx, "acct-1", UsageFilter{KeyPrefix: "attk_bbbb"}, p, 4)
		if err != nil {
			t.Fatalf("UsagePage filtered page %d: %v", p, err)
		}
		if total != 15 {
			t.Errorf("filtered page %d total = %d, want 15", p, total)
		}
		wantLen := 4
		if p == 4 {
			wantLen = 3
		}
		if len(got) != wantLen {
			t.Errorf("filtered page %d has %d rows, want %d", p, len(got), wantLen)
		}
		for _, r := range got {
			oddSeen = append(oddSeen, r.Credits)
		}
	}
	if len(oddSeen) != 15 {
		t.Errorf("filtered walk visited %d rows, want 15", len(oddSeen))
	}
	if oddSeen[0] != 30 || oddSeen[14] != 2 {
		t.Errorf("filtered walk order = %v, want newest-first evens 30..2", oddSeen)
	}

	// Facets: the distinct prefixes and models actually seeded, sorted, and
	// nothing from the other account.
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

	// An unknown account: zero total, empty rows and empty facets.
	got, total, err = s.UsagePage(ctx, "nobody", UsageFilter{}, 1, 7)
	if err != nil {
		t.Fatalf("UsagePage unknown account: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Errorf("unknown account = (%d rows, total %d), want (0, 0)", len(got), total)
	}
	prefixes, models, err = s.UsageFacets(ctx, "nobody")
	if err != nil {
		t.Fatalf("UsageFacets unknown account: %v", err)
	}
	if len(prefixes) != 0 || len(models) != 0 {
		t.Errorf("unknown account facets = (%v, %v), want empty", prefixes, models)
	}

}

// TestBatchBookkeepingRoundTrip checks the settle batch bookkeeping on the
// SQLite driver: RecordUsage -> BatchRows after the cursor (ASC order,
// correct columns) -> CommitRecord -> BatchCursor returns the last covered
// usage id; an account with no batches has cursor 0.
func TestBatchBookkeepingRoundTrip(t *testing.T) {
	batchAcct := "0x00000000000000000000000000000000000000000000000000000000000000bb"
	path := filepath.Join(t.TempDir(), "batches.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	baseTS := int64(1_700_000_000)
	for i := 1; i <= 3; i++ {
		row := UsageRow{
			AccountID:        batchAcct,
			KeyPrefix:        "attk_ccccc",
			Timestamp:        time.Unix(baseTS+int64(i), 0).UTC(),
			Service:          "rpc",
			Model:            "model-z",
			PromptTokens:     uint64(i),
			CachedTokens:     uint64(2 * i),
			CompletionTokens: uint64(3 * i),
			Credits:          uint64(10 * i),
			Status:           "ok",
		}
		if err := s.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}
	// A second account's rows must not leak into the first's batch walk.
	if err := s.RecordUsage(ctx, UsageRow{
		AccountID: "other", KeyPrefix: "attk_ddddd", Timestamp: time.Unix(baseTS+99, 0).UTC(),
		Service: "rpc", Model: "m", Credits: 1, Status: "ok",
	}); err != nil {
		t.Fatalf("RecordUsage(other): %v", err)
	}

	cur, err := s.BatchCursor(ctx, batchAcct)
	if err != nil {
		t.Fatalf("BatchCursor before any batch: %v", err)
	}
	if cur != 0 {
		t.Errorf("fresh account cursor = %d, want 0", cur)
	}

	rows, err := s.BatchRows(ctx, batchAcct, 0)
	if err != nil {
		t.Fatalf("BatchRows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("BatchRows returned %d rows, want 3", len(rows))
	}
	for i, r := range rows {
		want := i + 1
		if r.RowID != int64(want) || r.AccountID != batchAcct || r.KeyPrefix != "attk_ccccc" ||
			r.Timestamp.Unix() != baseTS+int64(want) || r.Service != "rpc" || r.Model != "model-z" ||
			r.PromptTokens != uint64(want) || r.CachedTokens != uint64(2*want) ||
			r.CompletionTokens != uint64(3*want) || r.Credits != uint64(10*want) || r.Status != "ok" {
			t.Errorf("row %d = %+v, want id %d with its seeded columns", i, r, want)
		}
	}

	// After the cursor: only rows 2 and 3, still ASC.
	rows, err = s.BatchRows(ctx, batchAcct, 1)
	if err != nil {
		t.Fatalf("BatchRows after cursor: %v", err)
	}
	if len(rows) != 2 || rows[0].RowID != 2 || rows[1].RowID != 3 {
		t.Errorf("BatchRows after cursor 1 = ids %v, want [2 3]", func() []int64 {
			out := make([]int64, len(rows))
			for i, r := range rows {
				out[i] = r.RowID
			}
			return out
		}())
	}

	root, err := UsageRoot(rows)
	if err != nil {
		t.Fatalf("UsageRoot: %v", err)
	}
	b := SettleBatch{
		AccountID:      batchAcct,
		Root:           root,
		FirstUsageID:   2,
		LastUsageID:    3,
		Credits:        30/MicroCreditsPerCredit + 1, // whole credits burned; value is opaque here
		CoveredCredits: 30,
		TXHash:         "0xtestbatch",
		CommittedAt:    baseTS + 500,
	}
	if err := s.CommitRecord(ctx, b, b.CoveredCredits); err != nil {
		t.Fatalf("CommitRecord: %v", err)
	}
	cur, err = s.BatchCursor(ctx, batchAcct)
	if err != nil {
		t.Fatalf("BatchCursor after commit: %v", err)
	}
	if cur != 3 {
		t.Errorf("cursor after commit = %d, want 3 (last_usage_id)", cur)
	}
	// The other account is untouched by this batch.
	if other, oerr := s.BatchCursor(ctx, "other"); oerr != nil || other != 0 {
		t.Errorf("other account cursor = (%d, %v), want (0, nil)", other, oerr)
	}
}
