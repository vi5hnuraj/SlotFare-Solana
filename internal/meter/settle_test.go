package meter

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeChain is a ChainClient for tests: configurable balances and
// per-account settle failures, with every call recorded.
type fakeChain struct {
	balances   map[string]uint64
	settleFail map[string]error
	balanceErr map[string]error
	settled    map[string]uint64 // account -> total credits settled
	calls      []string
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		balances:   map[string]uint64{},
		settleFail: map[string]error{},
		balanceErr: map[string]error{},
		settled:    map[string]uint64{},
	}
}

func (f *fakeChain) BalanceOf(_ context.Context, id string) (uint64, error) {
	f.calls = append(f.calls, "balance:"+id)
	if err := f.balanceErr[id]; err != nil {
		return 0, err
	}
	return f.balances[id], nil
}

func (f *fakeChain) Settle(_ context.Context, id string, credits uint64) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("settle:%s:%d", id, credits))
	if err := f.settleFail[id]; err != nil {
		return "", err
	}
	f.settled[id] += credits
	return "0xtest", nil
}

// runPass drives one settlement pass and returns the errors reported.
func runPass(t *testing.T, s *Settler) []error {
	t.Helper()
	var errs []error
	s.step(context.Background(), func(err error) {
		errs = append(errs, err)
	})
	return errs
}

// fakeLocker is a Locker for tests: busy controls Try's ok result, release
// controls whether release reports an error, and takes/releases count the
// attempts.
type fakeLocker struct {
	busy     bool
	release  error
	takes    int
	releases int
}

func (f *fakeLocker) Try(ctx context.Context, key string) (func(context.Context) error, bool, error) {
	f.takes++
	if f.busy {
		return nil, false, nil
	}
	return func(context.Context) error {
		f.releases++
		return f.release
	}, true, nil
}

// TestSettlerBusyLockerSkipsTicks checks single-flight: while another
// replica holds the lock, ticks are skipped silently and no step body runs.
func TestSettlerBusyLockerSkipsTicks(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100*MicroCreditsPerCredit)
	fc := newFakeChain()
	fc.balances["a"] = 100
	s := NewSettler(l, fc, 0, 0)
	s.Locker = &fakeLocker{busy: true}

	var errs []error
	for i := 0; i < 3; i++ {
		s.tick(context.Background(), func(err error) { errs = append(errs, err) })
	}
	if len(errs) != 0 {
		t.Fatalf("busy ticks reported %d errors, want none (silent skip)", len(errs))
	}
	if len(fc.calls) != 0 {
		t.Errorf("step body ran %d times behind a busy lock, want 0", len(fc.calls))
	}
}

// TestSettlerFreeLockerRunsStepEachTick checks the holder side: with the
// lock free, every tick runs exactly one step and releases the lock.
func TestSettlerFreeLockerRunsStepEachTick(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100*MicroCreditsPerCredit)
	fc := newFakeChain()
	fc.balances["a"] = 100
	s := NewSettler(l, fc, 0, 0)
	locker := &fakeLocker{}
	s.Locker = locker

	var errs []error
	for i := 0; i < 3; i++ {
		s.tick(context.Background(), func(err error) { errs = append(errs, err) })
	}
	if len(errs) != 0 {
		t.Fatalf("ticks reported %d errors, want none", len(errs))
	}
	if locker.takes != 3 || locker.releases != 3 {
		t.Errorf("locker takes/releases = %d/%d, want 3/3", locker.takes, locker.releases)
	}
	balances := 0
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 'b' {
			balances++
		}
	}
	if balances != 3 {
		t.Errorf("step ran %d refreshes over three ticks, want 3 (one per tick)", balances)
	}
}

// TestSettlerReleaseErrorReported checks that a failed lock release is
// reported through onErr rather than swallowed.
func TestSettlerReleaseErrorReported(t *testing.T) {
	l := NewLedger()
	fc := newFakeChain()
	s := NewSettler(l, fc, 0, 0)
	s.Locker = &fakeLocker{release: errors.New("connection lost")}

	var errs []error
	s.tick(context.Background(), func(err error) { errs = append(errs, err) })
	if len(errs) != 1 {
		t.Fatalf("reported %d errors, want exactly 1 (the release failure)", len(errs))
	}
}

func TestSettleAboveThresholdOnce(t *testing.T) {
	l := NewLedger()
	// The chain holds whole credits; the ledger holds micro-credits.
	l.SetMinted("a", 1000*MicroCreditsPerCredit)
	if err := l.Reserve("a", 120*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances["a"] = 1000 // whole credits
	s := NewSettler(l, fc, 0, 100*MicroCreditsPerCredit)

	runPass(t, s)
	a := l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 120*MicroCreditsPerCredit {
		t.Errorf("after first pass: pending=%d settled=%d, want 0/%d", a.Pending, a.Settled, 120*MicroCreditsPerCredit)
	}
	if fc.settled["a"] != 120 {
		t.Errorf("chain settled %d credits, want 120", fc.settled["a"])
	}
	before := l.Snapshot()
	runPass(t, s)
	after := l.Snapshot()
	if after["a"].Pending != before["a"].Pending || after["a"].Settled != before["a"].Settled {
		t.Errorf("second pass changed the ledger: %+v -> %+v", before["a"], after["a"])
	}
}

func TestSettleBelowThresholdLeftAlone(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 1000*MicroCreditsPerCredit)
	if err := l.Reserve("a", 40*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances["a"] = 1000
	s := NewSettler(l, fc, 0, 100*MicroCreditsPerCredit)

	runPass(t, s)
	a := l.Snapshot()["a"]
	if a.Pending != 40*MicroCreditsPerCredit || a.Settled != 0 {
		t.Errorf("after pass: pending=%d settled=%d, want %d/0", a.Pending, a.Settled, 40*MicroCreditsPerCredit)
	}
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			t.Errorf("below-threshold pass made a settle call: %s", c)
		}
	}
}

func TestFailingSettleLeavesPending(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 1000*MicroCreditsPerCredit)
	if err := l.Reserve("a", 200*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances["a"] = 1000
	fc.settleFail["a"] = errors.New("gas too low")
	s := NewSettler(l, fc, 0, 100*MicroCreditsPerCredit)

	errs := runPass(t, s)
	a := l.Snapshot()["a"]
	if a.Pending != 200*MicroCreditsPerCredit || a.Settled != 0 {
		t.Errorf("after failing pass: pending=%d settled=%d, want %d/0", a.Pending, a.Settled, 200*MicroCreditsPerCredit)
	}
	if len(errs) != 1 {
		t.Fatalf("reported %d errors, want exactly 1", len(errs))
	}
	// The next pass retries: clear the failure and settle succeeds.
	delete(fc.settleFail, "a")
	runPass(t, s)
	a = l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 200*MicroCreditsPerCredit {
		t.Errorf("after retry: pending=%d settled=%d, want 0/%d", a.Pending, a.Settled, 200*MicroCreditsPerCredit)
	}
}

func TestRefreshRaisesMintedRaisesSpendable(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 0)
	fc := newFakeChain()
	fc.balances["a"] = 500                                  // whole credits
	s := NewSettler(l, fc, 0, 10_000*MicroCreditsPerCredit) // threshold high enough that nothing settles

	runPass(t, s)
	a := l.Snapshot()["a"]
	if a.Minted != 500*MicroCreditsPerCredit {
		t.Errorf("Minted after refresh = %d, want %d", a.Minted, 500*MicroCreditsPerCredit)
	}
	if got := a.Spendable(); got != 500*MicroCreditsPerCredit {
		t.Errorf("Spendable after refresh = %d, want %d", got, 500*MicroCreditsPerCredit)
	}
}

func TestRefreshLowersMintedNoUnderflow(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 1000*MicroCreditsPerCredit)
	if err := l.Reserve("a", 800*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	l.MarkSettled("a", 600*MicroCreditsPerCredit) // pending 200 credits' worth, settled 600
	fc := newFakeChain()
	fc.balances["a"] = 500 // below settled+pending (800 credits)
	s := NewSettler(l, fc, 0, 10_000*MicroCreditsPerCredit)

	runPass(t, s)
	a := l.Snapshot()["a"]
	if a.Minted != 500*MicroCreditsPerCredit {
		t.Errorf("Minted after refresh = %d, want %d", a.Minted, 500*MicroCreditsPerCredit)
	}
	if got := a.Spendable(); got != 0 {
		t.Errorf("Spendable after lowering refresh = %d, want 0 (no underflow)", got)
	}
	if a.Settled != 600*MicroCreditsPerCredit || a.Pending != 200*MicroCreditsPerCredit {
		t.Errorf("settled/pending changed by refresh: %+v", a)
	}
}

// TestSettlePassCapsAtUnlockedBalance checks that a settle pass with
// pending 100 credits and unlocked 40 settles 40 whole credits and leaves
// 60 pending, without reporting an error, and settles the rest once the
// lock lifts.
func TestSettlePassCapsAtUnlockedBalance(t *testing.T) {
	l := NewLedger()
	l.Restore(map[string]Account{"a": {ID: "a", Minted: 100 * MicroCreditsPerCredit, Pending: 100 * MicroCreditsPerCredit}})
	l.SetLocked("a", 60*MicroCreditsPerCredit) // unlocked balance is 40 credits
	fc := newFakeChain()
	fc.balances["a"] = 100 // the refresh keeps minted at 100 credits
	s := NewSettler(l, fc, 0, 0)

	err := runPass(t, s)
	if len(err) != 0 {
		t.Fatalf("capped pass reported %d errors, want none", len(err))
	}
	a := l.Snapshot()["a"]
	if a.Pending != 60*MicroCreditsPerCredit || a.Settled != 40*MicroCreditsPerCredit {
		t.Errorf("after capped pass: pending=%d settled=%d, want %d/%d", a.Pending, a.Settled,
			60*MicroCreditsPerCredit, 40*MicroCreditsPerCredit)
	}
	if fc.settled["a"] != 40 {
		t.Errorf("chain settled %d credits, want 40", fc.settled["a"])
	}

	// Lift the lock: the next pass settles the remainder.
	l.SetLocked("a", 0)
	runPass(t, s)
	a = l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 100*MicroCreditsPerCredit {
		t.Errorf("after lock lifted: pending=%d settled=%d, want 0/%d", a.Pending, a.Settled, 100*MicroCreditsPerCredit)
	}
	if fc.settled["a"] != 100 {
		t.Errorf("total chain settled = %d credits, want 100", fc.settled["a"])
	}
}

// TestSettleWholeCreditsRemainderSurvives checks the chain boundary: the
// settle burns only whole credits and the sub-credit remainder stays
// pending locally across settle cycles, never lost.
func TestSettleWholeCreditsRemainderSurvives(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 6*MicroCreditsPerCredit)
	// Five whole credits of usage plus 44 micro-credits from small calls.
	if err := l.Reserve("a", 5*MicroCreditsPerCredit+44); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances["a"] = 6
	s := NewSettler(l, fc, 0, 0)

	runPass(t, s)
	a := l.Snapshot()["a"]
	if fc.settled["a"] != 5 {
		t.Errorf("chain settled %d credits, want exactly 5 whole credits", fc.settled["a"])
	}
	if a.Pending != 44 || a.Settled != 5*MicroCreditsPerCredit {
		t.Errorf("after pass: pending=%d settled=%d, want 44/%d (the remainder survives locally)",
			a.Pending, a.Settled, 5*MicroCreditsPerCredit)
	}

	// The next cycle settles nothing new: the remainder is sub-credit and
	// no further chain call may be attempted for it.
	settlesBefore := 0
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			settlesBefore++
		}
	}
	runPass(t, s)
	settlesAfter := 0
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			settlesAfter++
		}
	}
	if settlesAfter != settlesBefore {
		t.Errorf("second pass made %d more settle calls, want none for the sub-credit remainder", settlesAfter-settlesBefore)
	}
	a = l.Snapshot()["a"]
	if a.Pending != 44 || a.Settled != 5*MicroCreditsPerCredit {
		t.Errorf("after second pass: pending=%d settled=%d, want 44/%d", a.Pending, a.Settled, 5*MicroCreditsPerCredit)
	}
}

// A pending total with no whole-credit part never reaches the chain, even
// when the threshold is zero.
func TestSubCreditRemainderNeverBurns(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", MicroCreditsPerCredit)
	if err := l.Reserve("a", 44); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances["a"] = 1
	s := NewSettler(l, fc, 0, 0)

	runPass(t, s)
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			t.Errorf("sub-credit pending made a chain settle call: %s", c)
		}
	}
	if a := l.Snapshot()["a"]; a.Pending != 44 || a.Settled != 0 {
		t.Errorf("pending/settled = %d/%d, want 44/0", a.Pending, a.Settled)
	}
}

func TestManyAccountsOneFailureContinues(t *testing.T) {
	l := NewLedger()
	for _, id := range []string{"a", "b", "c"} {
		l.SetMinted(id, 1000*MicroCreditsPerCredit)
		if err := l.Reserve(id, 300*MicroCreditsPerCredit); err != nil {
			t.Fatalf("Reserve(%s): %v", id, err)
		}
	}
	fc := newFakeChain()
	for _, id := range []string{"a", "b", "c"} {
		fc.balances[id] = 1000
	}
	fc.settleFail["b"] = errors.New("revert")
	s := NewSettler(l, fc, 0, 100*MicroCreditsPerCredit)

	errs := runPass(t, s)
	got := l.Snapshot()
	if got["a"].Pending != 0 || got["a"].Settled != 300*MicroCreditsPerCredit {
		t.Errorf("account a not settled: %+v", got["a"])
	}
	if got["b"].Pending != 300*MicroCreditsPerCredit || got["b"].Settled != 0 {
		t.Errorf("failed account b should keep its pending: %+v", got["b"])
	}
	if got["c"].Pending != 0 || got["c"].Settled != 300*MicroCreditsPerCredit {
		t.Errorf("account c not settled past b's failure: %+v", got["c"])
	}
	if len(errs) != 1 {
		t.Errorf("reported %d errors, want exactly 1 (the failed account)", len(errs))
	}
	// All three settle calls were attempted in one pass.
	attempted := 0
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			attempted++
		}
	}
	if attempted != 3 {
		t.Errorf("pass attempted %d settles, want 3", attempted)
	}
}

// committedAcct is a valid ledger-form account id for the committed-path
// tests: UsageLeaf rejects ids that are not 0x plus 64 hex chars.
const committedAcct = "0x00000000000000000000000000000000000000000000000000000000000000aa"

// fakeBatchStore is a SettleBatchStore for tests: preloaded rows and
// cursor, every CommitRecord recorded, optional record error.
type fakeBatchStore struct {
	rows        []UsageRow
	cursor      int64
	recordErr   error
	records     []SettleBatch
	cursorCalls int
	rowsCalls   int
}

func (f *fakeBatchStore) BatchRows(_ context.Context, _ string, _ int64) ([]UsageRow, error) {
	f.rowsCalls++
	return f.rows, nil
}

func (f *fakeBatchStore) BatchCursor(_ context.Context, _ string) (int64, error) {
	f.cursorCalls++
	return f.cursor, nil
}

func (f *fakeBatchStore) CommitRecord(_ context.Context, b SettleBatch, _ uint64) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.records = append(f.records, b)
	return nil
}

// fakeCommitter is a UsageCommitter for tests: every call recorded, an
// optional error.
type fakeCommitter struct {
	err   error
	calls []string
}

func (f *fakeCommitter) CommitAndSettle(_ context.Context, root [32]byte, id string, first, last, credits uint64) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("commit:%s:%d:%d:%d", id, first, last, credits))
	if f.err != nil {
		return "", f.err
	}
	return "0xcommitted", nil
}

// TestSettleWithAttestorCommitsBatch checks the committed path end to end:
// one commit with the batch's row range and whole credits, one record with
// the root and covered sum, the ledger marked settled - and no direct
// ledger settle call.
func TestSettleWithAttestorCommitsBatch(t *testing.T) {
	l := NewLedger()
	l.SetMinted(committedAcct, 10*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, 2*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances[committedAcct] = 10
	store := &fakeBatchStore{rows: []UsageRow{
		{RowID: 1, AccountID: committedAcct, Credits: MicroCreditsPerCredit},
		{RowID: 2, AccountID: committedAcct, Credits: MicroCreditsPerCredit},
	}}
	comm := &fakeCommitter{}
	s := NewSettler(l, fc, 0, 0)
	s.Attestor = comm
	s.BatchStore = store

	err := runPass(t, s)
	if len(err) != 0 {
		t.Fatalf("committed pass reported %d errors, want none", len(err))
	}
	if len(comm.calls) != 1 || comm.calls[0] != fmt.Sprintf("commit:%s:%d:%d:%d", committedAcct, 1, 2, 2) {
		t.Fatalf("committer calls = %v, want exactly one commit of rows 1..2 for 2 credits", comm.calls)
	}
	if len(store.records) != 1 {
		t.Fatalf("CommitRecord called %d times, want 1", len(store.records))
	}
	b := store.records[0]
	wantRoot, rerr := UsageRoot(store.rows)
	if rerr != nil {
		t.Fatalf("UsageRoot: %v", rerr)
	}
	if b.Root != wantRoot || b.FirstUsageID != 1 || b.LastUsageID != 2 ||
		b.Credits != 2 || b.CoveredCredits != 2*MicroCreditsPerCredit || b.TXHash != "0xcommitted" {
		t.Errorf("recorded batch = %+v, want root %x rows 1..2 credits 2 covered %d tx 0xcommitted",
			b, wantRoot[:], 2*MicroCreditsPerCredit)
	}
	a := l.Snapshot()[committedAcct]
	if a.Pending != 0 || a.Settled != 2*MicroCreditsPerCredit {
		t.Errorf("after committed pass: pending=%d settled=%d, want 0/%d", a.Pending, a.Settled, 2*MicroCreditsPerCredit)
	}
	for _, c := range fc.calls {
		if len(c) > 0 && c[0] == 's' {
			t.Errorf("committed pass made a direct settle call: %s", c)
		}
	}

	// A second pass with a fresh row settles it from cursor+1: the covered
	// rows are never re-covered.
	store.cursor = 2
	store.rows = []UsageRow{{RowID: 3, AccountID: committedAcct, Credits: MicroCreditsPerCredit}}
	l.SetMinted(committedAcct, 11*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc.balances[committedAcct] = 11
	err = runPass(t, s)
	if len(err) != 0 {
		t.Fatalf("second committed pass reported %d errors, want none", len(err))
	}
	if len(comm.calls) != 2 || comm.calls[1] != fmt.Sprintf("commit:%s:%d:%d:%d", committedAcct, 3, 3, 1) {
		t.Errorf("second pass comitter calls = %v, want one commit of row 3 for 1 credit", comm.calls)
	}
	if len(store.records) != 2 || store.records[1].FirstUsageID != 3 || store.records[1].LastUsageID != 3 {
		t.Errorf("second pass records = %+v, want row 3..3 only", store.records)
	}
}

// TestSettleAttestorCommitFailureDefers checks a failed commit: exactly one
// error, nothing marked settled, no record, and the next pass retries with
// the same rows and cursor.
func TestSettleAttestorCommitFailureDefers(t *testing.T) {
	l := NewLedger()
	l.SetMinted(committedAcct, 5*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, 2*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances[committedAcct] = 5
	store := &fakeBatchStore{rows: []UsageRow{
		{RowID: 1, AccountID: committedAcct, Credits: MicroCreditsPerCredit},
		{RowID: 2, AccountID: committedAcct, Credits: MicroCreditsPerCredit},
	}}
	comm := &fakeCommitter{err: errors.New("revert")}
	s := NewSettler(l, fc, 0, 0)
	s.Attestor = comm
	s.BatchStore = store

	err := runPass(t, s)
	if len(err) != 1 {
		t.Fatalf("failing commit reported %d errors, want exactly 1", len(err))
	}
	if len(store.records) != 0 {
		t.Errorf("failed commit recorded %d batches, want none", len(store.records))
	}
	a := l.Snapshot()[committedAcct]
	if a.Pending != 2*MicroCreditsPerCredit || a.Settled != 0 {
		t.Errorf("after failing commit: pending=%d settled=%d, want %d/0", a.Pending, a.Settled, 2*MicroCreditsPerCredit)
	}

	// The next pass retries with the same cursor and rows.
	comm.err = nil
	store.cursorCalls = 0
	store.rowsCalls = 0
	err = runPass(t, s)
	if len(err) != 0 {
		t.Fatalf("retry pass reported %d errors, want none", len(err))
	}
	if store.cursorCalls != 1 || store.rowsCalls != 1 {
		t.Errorf("retry consulted cursor/rows %d/%d times, want 1/1 (same cursor, no progress)", store.cursorCalls, store.rowsCalls)
	}
	if len(comm.calls) != 2 || len(store.records) != 1 {
		t.Errorf("retry = %d commits, %d records, want 2 commits and 1 record", len(comm.calls), len(store.records))
	}
}

// TestSettleBatchRecordFailureDefersMark checks a failed CommitRecord:
// the on-chain commit landed but the local bookkeeping did not - the
// pending stays, an error is reported, and nothing is marked settled.
func TestSettleBatchRecordFailureDefersMark(t *testing.T) {
	l := NewLedger()
	l.SetMinted(committedAcct, 5*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances[committedAcct] = 5
	store := &fakeBatchStore{
		rows:      []UsageRow{{RowID: 1, AccountID: committedAcct, Credits: MicroCreditsPerCredit}},
		recordErr: errors.New("connection lost"),
	}
	comm := &fakeCommitter{}
	s := NewSettler(l, fc, 0, 0)
	s.Attestor = comm
	s.BatchStore = store

	err := runPass(t, s)
	if len(err) != 1 {
		t.Fatalf("failed record reported %d errors, want exactly 1", len(err))
	}
	if len(comm.calls) != 1 {
		t.Errorf("failed record made %d commits, want 1 (the commit landed on-chain)", len(comm.calls))
	}
	a := l.Snapshot()[committedAcct]
	if a.Pending != MicroCreditsPerCredit || a.Settled != 0 {
		t.Errorf("after failed record: pending=%d settled=%d, want %d/0 (nothing marked)",
			a.Pending, a.Settled, MicroCreditsPerCredit)
	}
}

// TestSettleAttestorUnsetKeepsTodayBehavior checks that with both fields
// nil the direct-settle path runs and neither the committer nor the batch
// store is consulted.
func TestSettleAttestorUnsetKeepsTodayBehavior(t *testing.T) {
	l := NewLedger()
	l.SetMinted(committedAcct, 5*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, 2*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances[committedAcct] = 5
	store := &fakeBatchStore{rows: []UsageRow{{RowID: 1, AccountID: committedAcct, Credits: MicroCreditsPerCredit}}}
	comm := &fakeCommitter{}
	s := NewSettler(l, fc, 0, 0)
	// Attestor and BatchStore stay nil: today's behavior.

	err := runPass(t, s)
	if len(err) != 0 {
		t.Fatalf("unset-attestor pass reported %d errors, want none", len(err))
	}
	if fc.settled[committedAcct] != 2 {
		t.Errorf("direct chain settled %d credits, want 2", fc.settled[committedAcct])
	}
	if len(comm.calls) != 0 || store.cursorCalls != 0 || store.rowsCalls != 0 {
		t.Errorf("unset attestor consulted committer/batch store (%d commits, cursor %d, rows %d), want none",
			len(comm.calls), store.cursorCalls, store.rowsCalls)
	}
}

// TestSettleNoRowsAfterCursorDefers checks an empty BatchRows result: the
// account has pending but no usage rows after its cursor - it defers loudly
// instead of committing an empty or mismatched root.
func TestSettleNoRowsAfterCursorDefers(t *testing.T) {
	l := NewLedger()
	l.SetMinted(committedAcct, 5*MicroCreditsPerCredit)
	if err := l.Reserve(committedAcct, 2*MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fc := newFakeChain()
	fc.balances[committedAcct] = 5
	store := &fakeBatchStore{} // no rows after the cursor
	comm := &fakeCommitter{}
	s := NewSettler(l, fc, 0, 0)
	s.Attestor = comm
	s.BatchStore = store

	err := runPass(t, s)
	if len(err) != 1 {
		t.Fatalf("no-rows pass reported %d errors, want exactly 1", len(err))
	}
	if len(comm.calls) != 0 || len(store.records) != 0 {
		t.Errorf("no-rows pass made %d commits and %d records, want none", len(comm.calls), len(store.records))
	}
	a := l.Snapshot()[committedAcct]
	if a.Pending != 2*MicroCreditsPerCredit || a.Settled != 0 {
		t.Errorf("after no-rows pass: pending=%d settled=%d, want %d/0", a.Pending, a.Settled, 2*MicroCreditsPerCredit)
	}
}
