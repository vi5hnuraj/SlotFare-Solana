package meter

import (
	"errors"
	"sync"
	"testing"
)

func TestSpendableSaturates(t *testing.T) {
	// Settled plus pending exceeds minted: saturate at zero, never underflow.
	a := Account{Minted: 100, Settled: 80, Pending: 40}
	if got := a.Spendable(); got != 0 {
		t.Errorf("Spendable() = %d, want 0 when settled+pending exceed minted", got)
	}
	// Exactly used up.
	a = Account{Minted: 100, Settled: 60, Pending: 40}
	if got := a.Spendable(); got != 0 {
		t.Errorf("Spendable() = %d, want 0 when fully used", got)
	}
	// Normal case.
	a = Account{Minted: 100, Settled: 30, Pending: 20}
	if got := a.Spendable(); got != 50 {
		t.Errorf("Spendable() = %d, want 50", got)
	}
	// Fresh account with nothing minted.
	a = Account{}
	if got := a.Spendable(); got != 0 {
		t.Errorf("Spendable() = %d, want 0 for empty account", got)
	}
}

func TestSpendableWithLocks(t *testing.T) {
	cases := []struct {
		name string
		a    Account
		want uint64
	}{
		{"no lock", Account{Minted: 100}, 100},
		{"lock reduces spendable", Account{Minted: 100, Locked: 30}, 70},
		{"lock and usage", Account{Minted: 100, Settled: 10, Pending: 20, Locked: 30}, 40},
		{"lock exactly the balance", Account{Minted: 100, Locked: 100}, 0},
		{"lock larger than balance", Account{Minted: 100, Locked: 500}, 0},
		{"lock plus usage exceeds minted", Account{Minted: 100, Settled: 80, Pending: 40, Locked: 10}, 0},
		{"lock does not reduce minted", Account{Minted: 100, Locked: 90}, 10},
	}
	for _, c := range cases {
		if got := c.a.Spendable(); got != c.want {
			t.Errorf("%s: Spendable(%+v) = %d, want %d", c.name, c.a, got, c.want)
		}
	}
}

func TestSettleCap(t *testing.T) {
	cases := []struct {
		name string
		a    Account
		want uint64
	}{
		{"no lock settles everything up to minted", Account{Minted: 100}, 100},
		{"settled counts against the cap", Account{Minted: 100, Settled: 60}, 40},
		{"locked counts against the cap", Account{Minted: 100, Locked: 60}, 40},
		{"pending does not count against the cap", Account{Minted: 100, Pending: 100}, 100},
		{"lock larger than balance", Account{Minted: 100, Settled: 10, Locked: 90}, 0},
	}
	for _, c := range cases {
		if got := c.a.SettleCap(); got != c.want {
			t.Errorf("%s: SettleCap(%+v) = %d, want %d", c.name, c.a, got, c.want)
		}
	}
}

func TestReserveInsufficient(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	if err := l.Reserve("a", 101); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("Reserve(101) error = %v, want ErrInsufficientCredits", err)
	}
	if got := l.Snapshot()["a"].Pending; got != 0 {
		t.Errorf("Pending after failed reserve = %d, want 0", got)
	}
}

func TestReserveThenMarkSettled(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	if err := l.Reserve("a", 30); err != nil {
		t.Fatalf("Reserve(30): %v", err)
	}
	a := l.Snapshot()["a"]
	if a.Pending != 30 || a.Spendable() != 70 {
		t.Fatalf("after reserve: pending=%d spendable=%d, want 30/70", a.Pending, a.Spendable())
	}
	l.MarkSettled("a", 30)
	a = l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 30 || a.Spendable() != 70 {
		t.Errorf("after settle: pending=%d settled=%d spendable=%d, want 0/30/70",
			a.Pending, a.Settled, a.Spendable())
	}
	// Settling more than pending cannot underflow either counter.
	l.Reserve("a", 20)
	l.MarkSettled("a", 999)
	a = l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 50 {
		t.Errorf("after over-settle: pending=%d settled=%d, want 0/50", a.Pending, a.Settled)
	}
}

// TestReserveRefusesLockedCredits checks that a reservation which fits the
// balance but not the unlocked balance is refused, and that releasing the
// lock lets the same reservation pass.
func TestReserveRefusesLockedCredits(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	l.SetLocked("a", 60) // unlocked balance is 40
	if err := l.Reserve("a", 40); err != nil {
		t.Fatalf("Reserve(40): %v", err)
	}
	if err := l.Reserve("a", 20); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("Reserve(20) into locked credits error = %v, want ErrInsufficientCredits", err)
	}
	if got := l.Snapshot()["a"].Pending; got != 40 {
		t.Errorf("Pending after refused reserve = %d, want 40", got)
	}
	l.SetLocked("a", 0)
	if err := l.Reserve("a", 20); err != nil {
		t.Fatalf("Reserve(20) after releasing the lock: %v", err)
	}
}

// TestSettledNeverEntersLockedCredits checks that MarkSettled caps what it
// moves at the unlocked balance: pending 100 and unlocked 40 settles 40
// and leaves 60 pending, then settles the rest once the lock lifts.
func TestSettledNeverEntersLockedCredits(t *testing.T) {
	l := NewLedger()
	l.Restore(map[string]Account{"a": {ID: "a", Minted: 100, Pending: 100}})
	l.SetLocked("a", 60) // unlocked balance is 40

	l.MarkSettled("a", 100)
	a := l.Snapshot()["a"]
	if a.Pending != 60 || a.Settled != 40 {
		t.Errorf("after capped settle: pending=%d settled=%d, want 60/40", a.Pending, a.Settled)
	}

	l.SetLocked("a", 0)
	l.MarkSettled("a", 60)
	a = l.Snapshot()["a"]
	if a.Pending != 0 || a.Settled != 100 {
		t.Errorf("after lock lifted: pending=%d settled=%d, want 0/100", a.Pending, a.Settled)
	}
}

// TestSetLockedArmsNotify checks that a lock change alone fires the changed
// signal, so the console stream emits a balance event on it.
func TestSetLockedArmsNotify(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	l.SetLocked("a", 40)
	select {
	case <-l.Changed():
	default:
		t.Fatal("SetLocked did not arm the changed signal")
	}
	if got := l.Snapshot()["a"].Locked; got != 40 {
		t.Errorf("Locked = %d, want 40", got)
	}
	// SetLocked on an unknown account creates it with zero other counters.
	l.SetLocked("new", 5)
	if got := l.Snapshot()["new"]; got.Locked != 5 || got.Minted != 0 {
		t.Errorf("SetLocked on unknown account = %+v, want locked 5 and zero counters", got)
	}
}

func TestReleaseReturnsReservation(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	if err := l.Reserve("a", 30); err != nil {
		t.Fatalf("Reserve(30): %v", err)
	}
	l.Release("a", 30)
	if got := l.Snapshot()["a"].Pending; got != 0 {
		t.Errorf("Pending after release = %d, want 0", got)
	}
	// Releasing more than reserved saturates at zero without panic.
	l.Release("a", 999)
	if got := l.Snapshot()["a"].Pending; got != 0 {
		t.Errorf("Pending after over-release = %d, want 0", got)
	}
}

func TestReserveConcurrentNoOversell(t *testing.T) {
	const (
		id      = "a"
		mint    = 500
		callerN = 100
		perCall = 10
		wantOK  = mint / perCall // 50
	)
	l := NewLedger()
	l.SetMinted(id, mint)

	var wg sync.WaitGroup
	ok := make(chan struct{}, callerN)
	for i := 0; i < callerN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Reserve(id, perCall); err == nil {
				ok <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(ok)

	succeeded := 0
	for range ok {
		succeeded++
	}
	if succeeded != wantOK {
		t.Errorf("concurrent reserves succeeded %d times, want exactly %d", succeeded, wantOK)
	}
	if got := l.Snapshot()[id].Pending; got != mint {
		t.Errorf("Pending after concurrent reserves = %d, want %d", got, mint)
	}
}

func TestPendingTotalOnlyPendingAccounts(t *testing.T) {
	l := NewLedger()
	l.SetMinted("resting", 100)
	l.SetMinted("busy", 100)
	l.Reserve("busy", 40)
	got := l.PendingTotal()
	if len(got) != 1 || got["busy"] != 40 {
		t.Errorf("PendingTotal() = %v, want map[busy:40]", got)
	}
}

// Repeated small calls accumulate: N identical reservations of a small
// nonzero micro-credit cost charge N times that cost, none of them
// truncating to zero.
func TestRepeatedSmallCallsAccumulate(t *testing.T) {
	const (
		smallCost = uint64(44) // one small inference call, in micro-credits
		calls     = 10
	)
	l := NewLedger()
	l.SetMinted("a", 100*MicroCreditsPerCredit)
	for i := 0; i < calls; i++ {
		if err := l.Reserve("a", smallCost); err != nil {
			t.Fatalf("call %d: Reserve(%d): %v", i, smallCost, err)
		}
	}
	got := l.Snapshot()["a"].Pending
	want := smallCost * calls
	if got != want {
		t.Errorf("Pending after %d identical small calls = %d, want %d (N times the single-call price)", calls, got, want)
	}
}

func TestRestoreMatchesPreSnapshotSpendable(t *testing.T) {
	l := NewLedger()
	l.SetMinted("alice", 1000)
	if err := l.Reserve("alice", 340); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	l.MarkSettled("alice", 100)
	want := l.Snapshot()["alice"].Spendable()

	// Restart: a fresh ledger restored from the snapshot must agree.
	l2 := NewLedger()
	l2.Restore(l.Snapshot())
	got := l2.Snapshot()["alice"].Spendable()
	if got != want {
		t.Errorf("restored Spendable() = %d, want %d (the pre-snapshot value)", got, want)
	}
	// Restoring into a ledger that already holds the account replaces it
	// verbatim rather than merging counters.
	l2.SetMinted("alice", 99999)
	l2.Restore(map[string]Account{"alice": {ID: "alice", Minted: 1000, Settled: 100, Pending: 240}})
	if got := l2.Snapshot()["alice"]; got.Minted != 1000 || got.Pending != 240 {
		t.Errorf("Restore did not replace verbatim: %+v", got)
	}
}

// TestAdjustMintedSurvivesRescan pins the market/chain split: the deposit
// watcher's absolute SetMinted writes only the chain part, so a market
// movement rides alongside it and survives a full rescan.
func TestAdjustMintedSurvivesRescan(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	l.AdjustMinted("a", 50)
	// Rescan pushes the same absolute chain figure again.
	l.SetMinted("a", 100)
	got := l.Snapshot()["a"]
	if got.TotalMinted() != 150 {
		t.Errorf("TotalMinted = %d after rescan, want 150", got.TotalMinted())
	}
	if got.Minted != 100 || got.Adjust != 50 {
		t.Errorf("chain/adjust split = minted %d adjust %d, want 100/50", got.Minted, got.Adjust)
	}
	if got.Spendable() != 150 {
		t.Errorf("Spendable = %d, want 150", got.Spendable())
	}
}

// TestAdjustMintedSaturates pins the negative edge: releasing more than the
// chain part holds zeroes the position instead of underflowing.
func TestAdjustMintedSaturates(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	l.AdjustMinted("a", -200)
	got := l.Snapshot()["a"]
	if got.TotalMinted() != 0 {
		t.Errorf("TotalMinted = %d, want 0", got.TotalMinted())
	}
	if got.Spendable() != 0 {
		t.Errorf("Spendable = %d, want 0", got.Spendable())
	}
	if err := l.Reserve("a", 1); err != ErrInsufficientCredits {
		t.Errorf("Reserve on a saturated account = %v, want ErrInsufficientCredits", err)
	}
}

// TestAdjustMintedArmsNotify: a market movement is a balance change, so the
// console stream must see it.
func TestAdjustMintedArmsNotify(t *testing.T) {
	l := NewLedger()
	l.SetMinted("a", 100)
	l.AdjustMinted("a", -10)
	select {
	case <-l.Changed():
	default:
		t.Fatal("AdjustMinted did not arm the changed signal")
	}
}

// TestAdjustMintedZeroIsNoop keeps a zero delta from creating rows or
// signals.
func TestAdjustMintedZeroIsNoop(t *testing.T) {
	l := NewLedger()
	l.AdjustMinted("ghost", 0)
	if _, ok := l.Snapshot()["ghost"]; ok {
		t.Error("zero AdjustMinted created an account row")
	}
}
