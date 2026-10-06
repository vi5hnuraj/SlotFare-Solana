package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"slotfare/internal/meter"
)

// quiet swallows the rail's log lines in tests that assert on state rather
// than on what was printed.
func quiet(string, ...any) {}

// collector captures the rail's log lines.
type collector struct{ lines []string }

func (c *collector) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *collector) joined() string { return strings.Join(c.lines, "\n") }

// fakeAtomic stands in for the Postgres authority. Its RaiseMinted mirrors
// the guard the real SQL makes with its CASE, so a rail that wrote through
// the wrong method would still be caught here; SetMinted is absolute, like
// the real statement, so the two intents stay distinguishable in tests.
type fakeAtomic struct {
	minted   uint64
	writes   int
	failWith error
}

func (f *fakeAtomic) Reserve(context.Context, string, uint64) error { return nil }
func (f *fakeAtomic) Release(context.Context, string, uint64) error { return nil }
func (f *fakeAtomic) SettleCredits(context.Context, string, uint64) error {
	return nil
}
func (f *fakeAtomic) SetMinted(_ context.Context, _ string, mintedMicro uint64) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.writes++
	f.minted = mintedMicro
	return nil
}

func (f *fakeAtomic) RaiseMinted(_ context.Context, _ string, mintedMicro uint64) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.writes++
	if mintedMicro > f.minted {
		f.minted = mintedMicro
	}
	return nil
}
func (f *fakeAtomic) SetLocked(context.Context, string, uint64) error   { return nil }
func (f *fakeAtomic) AdjustMinted(context.Context, string, int64) error { return nil }
func (f *fakeAtomic) Accounts(context.Context) (map[string]meter.Account, error) {
	return nil, nil
}

// Account is the AccountReader half: the shared row's figures.
func (f *fakeAtomic) Account(_ context.Context, id string) (meter.Account, error) {
	return meter.Account{ID: id, Minted: f.minted}, nil
}

// fakeMirror counts attempts and fails while err is set.
type fakeMirror struct {
	attempts int
	targets  []uint64
	err      error
}

func (m *fakeMirror) Sync(_ context.Context, _ string, targetCredits uint64) error {
	m.attempts++
	m.targets = append(m.targets, targetCredits)
	return m.err
}

var _ meter.AtomicLedger = (*fakeAtomic)(nil)

// TestDepositRailNeverLowersARecordedTotal is the regression test for the
// audit's critical finding: an incomplete rescan must not reduce an account's
// recorded minted balance. It runs against both storage backends, because the
// guard has to hold in each one - the in-memory ledger for SQLite and the
// shared row for Postgres - and not only where the caller remembers to check.
func TestDepositRailNeverLowersARecordedTotal(t *testing.T) {
	const acct = "0xacct"

	cases := []struct {
		name     string
		recorded uint64
		scanned  uint64
		want     uint64
		wantGap  bool
	}{
		{"a lower scan keeps the recorded total", 2500, 1000, 2500, true},
		{"an equal scan changes nothing", 2500, 2500, 2500, false},
		{"a higher scan raises the total", 2500, 3500, 3500, false},
	}

	for _, tc := range cases {
		for _, backend := range []string{"sqlite", "postgres"} {
			t.Run(tc.name+"/"+backend, func(t *testing.T) {
				ledger := meter.NewLedger()
				ledger.SetMinted(acct, tc.recorded)

				var atomic *fakeAtomic
				rail := newDepositRail(ledger, nil, nil, quiet)
				if backend == "postgres" {
					atomic = &fakeAtomic{minted: tc.recorded}
					rail = newDepositRail(ledger, atomic, nil, quiet)
				}

				err := rail.mint(context.Background(), acct, tc.scanned)

				if tc.wantGap && !errors.Is(err, errDepositScanGap) {
					t.Errorf("err = %v, want a scan gap", err)
				}
				if !tc.wantGap && err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				if got := ledger.Minted(acct); got != tc.want {
					t.Errorf("ledger minted = %d, want %d", got, tc.want)
				}
				if atomic != nil && atomic.minted != tc.want {
					t.Errorf("shared minted = %d, want %d", atomic.minted, tc.want)
				}
			})
		}
	}
}

// TestDepositRailSurfacesAScanGap pins the reporting half: the refusal has to
// be visible, not a silent no-op that reads like a successful mint.
func TestDepositRailSurfacesAScanGap(t *testing.T) {
	ledger := meter.NewLedger()
	ledger.SetMinted("0xacct", 2500)
	logs := &collector{}
	mirror := &fakeMirror{}
	rail := newDepositRail(ledger, nil, mirror, logs.logf)

	err := rail.mint(context.Background(), "0xacct", 1000)

	if !errors.Is(err, errDepositScanGap) {
		t.Fatalf("err = %v, want a scan gap the caller can detect", err)
	}
	if !strings.Contains(err.Error(), "2500") || !strings.Contains(err.Error(), "1000") {
		t.Errorf("the gap must name both totals: %v", err)
	}
	if !strings.Contains(logs.joined(), "deposit scan gap") {
		t.Errorf("the gap must be logged, got: %q", logs.joined())
	}
	if strings.Contains(logs.joined(), "deposit mint:") {
		t.Errorf("an incomplete scan must not be logged as a mint: %q", logs.joined())
	}
	if mirror.attempts != 0 {
		t.Errorf("a scan gap must not touch the chain, mirror attempts = %d", mirror.attempts)
	}
}

// TestRestartRescanCannotLowerABalance is the audit's scenario end to end: a
// balance credited before a restart, a checkpoint to rebuild from, and a
// first pass afterwards that sees only a truncated RPC history. The balance
// must survive the restart and the incomplete rescan.
func TestRestartRescanCannotLowerABalance(t *testing.T) {
	const acct = "0xacct"

	before := meter.NewLedger()
	before.SetMinted(acct, 3500)
	checkpoint := before.Snapshot()

	// The restart: a fresh ledger restored from the checkpoint.
	after := meter.NewLedger()
	after.Restore(checkpoint)

	logs := &collector{}
	rail := newDepositRail(after, nil, nil, logs.logf)

	if err := rail.mint(context.Background(), acct, 1000); !errors.Is(err, errDepositScanGap) {
		t.Fatalf("first pass after restart: err = %v, want a scan gap", err)
	}
	if got := after.Minted(acct); got != 3500 {
		t.Errorf("after an incomplete rescan the balance is %d, want the restored 3500", got)
	}
	// The scan becoming complete again is the normal case and still applies.
	if err := rail.mint(context.Background(), acct, 4200); err != nil {
		t.Fatalf("a complete scan must be accepted: %v", err)
	}
	if got := after.Minted(acct); got != 4200 {
		t.Errorf("balance = %d, want 4200", got)
	}
}

// TestDepositRailReturnsMirrorFailureSoTheNextPassRetries covers the second
// audit finding: the mirror error must reach the watcher, because the watcher
// records an account as done the moment this path returns nil and then skips
// it while its total is unchanged.
func TestDepositRailReturnsMirrorFailureSoTheNextPassRetries(t *testing.T) {
	const acct = "0xacct"

	ledger := meter.NewLedger()
	mirror := &fakeMirror{err: errors.New("rpc unavailable")}
	logs := &collector{}
	rail := newDepositRail(ledger, nil, mirror, logs.logf)

	// Pass one: the deposit is recorded locally, the chain mirror fails.
	err := rail.mint(context.Background(), acct, 2500)
	if err == nil {
		t.Fatal("a failed mirror must be returned: returning nil marks the deposit done and the retry never runs")
	}
	if errors.Is(err, errDepositScanGap) {
		t.Errorf("a mirror failure is not a scan gap: %v", err)
	}
	if got := ledger.Minted(acct); got != 2500 {
		t.Errorf("the recorded deposit must survive the failed mirror: got %d", got)
	}
	if mirror.attempts != 1 {
		t.Fatalf("mirror attempts = %d, want 1", mirror.attempts)
	}

	// Pass two: the watcher retries the same absolute total, the chain
	// answers, and success is reported so the watcher can advance.
	mirror.err = nil
	if err := rail.mint(context.Background(), acct, 2500); err != nil {
		t.Fatalf("the retry must succeed once the mirror recovers: %v", err)
	}
	if mirror.attempts != 2 {
		t.Errorf("mirror attempts = %d, want 2: a failed pass must be retried", mirror.attempts)
	}
	if !strings.Contains(logs.joined(), "deposit mint:") {
		t.Errorf("a completed mint must be logged: %q", logs.joined())
	}
}

// TestDepositRailMirrorsWholeCredits pins the unit conversion across the
// chain boundary: the program speaks whole credits, the rail micro-credits.
func TestDepositRailMirrorsWholeCredits(t *testing.T) {
	ledger := meter.NewLedger()
	mirror := &fakeMirror{}
	rail := newDepositRail(ledger, nil, mirror, quiet)

	if err := rail.mint(context.Background(), "0xacct", 25_000_000); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(mirror.targets) != 1 || mirror.targets[0] != 2500 {
		t.Errorf("mirrored targets = %v, want [2500] credits", mirror.targets)
	}
}

// TestDepositRailSurfacesAStoreFailure makes sure a failing authority write is
// reported rather than looking like a completed deposit.
func TestDepositRailSurfacesAStoreFailure(t *testing.T) {
	ledger := meter.NewLedger()
	atomic := &fakeAtomic{failWith: errors.New("store down")}
	rail := newDepositRail(ledger, atomic, nil, quiet)

	err := rail.mint(context.Background(), "0xacct", 2500)
	if err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("err = %v, want the store failure", err)
	}
	if got := ledger.Minted("0xacct"); got != 0 {
		t.Errorf("a failed authority write must not be recorded locally as a success: got %d", got)
	}
}
