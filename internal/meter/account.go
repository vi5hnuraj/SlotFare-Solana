package meter

import (
	"errors"
	"sync"
)

// ErrInsufficientCredits is returned by Reserve when the account's
// spendable micro-credits do not cover the reservation. The account is
// left untouched.
var ErrInsufficientCredits = errors.New("meter: insufficient credits")

// Account is one caller's credit position, in micro-credits - the internal
// metering unit, 10,000 to the credit. Minted is the chain-derived part:
// deposits only, written as an absolute total by the deposit watcher, so a
// rescan converges instead of double-minting. Adjust is the ledger-side
// part: signed market movements (credits acquired or released by a
// listing sale) that the watcher never touches, so a deposit rescan cannot
// clobber them. TotalMinted is what the account owns. Settled and Pending
// are tracked in memory between settlements. Locked is the credits the
// owner has listed for resale, which the proxy must not spend.
type Account struct {
	ID      string
	Minted  uint64
	Adjust  int64
	Settled uint64
	Pending uint64
	Locked  uint64
}

// TotalMinted is the account's effective minted position: the chain part
// plus the signed market adjust, saturating at zero when the adjust has
// released more than the chain part holds. Every display and spend check
// reads this; only the deposit watcher's absolute SetMinted writes Minted.
func (a Account) TotalMinted() uint64 {
	if a.Adjust >= 0 {
		return a.Minted + uint64(a.Adjust)
	}
	released := uint64(-a.Adjust)
	if released >= a.Minted {
		return 0
	}
	return a.Minted - released
}

// Spendable returns the micro-credits this account may still spend: total
// minted minus settled minus pending minus locked, saturating at zero. It
// never underflows, so an account whose settled, pending and locked
// counters outpace a stale Minted value simply cannot spend. Locked
// reduces spendable; it does not reduce minted - the owner still owns
// listed credits until the moment they are sold.
func (a Account) Spendable() uint64 {
	total := a.TotalMinted()
	used := a.Settled + a.Pending + a.Locked
	if used >= total {
		return 0
	}
	return total - used
}

// SettleCap returns the micro-credits a settle pass may move from Pending
// to Settled: total minted minus settled minus locked, saturating at zero.
// Pending usage that cannot settle because of a lock is not an error; it
// stays pending until the lock lifts or new credits arrive.
func (a Account) SettleCap() uint64 {
	total := a.TotalMinted()
	used := a.Settled + a.Locked
	if used >= total {
		return 0
	}
	return total - used
}

// Ledger holds the accounts in memory, behind a mutex. The in-memory map is
// the request path; SQLite is only a checkpoint, reached through the
// Flusher.
type Ledger struct {
	mu       sync.Mutex
	accounts map[string]Account
	notify   chan struct{} // buffered, re-armed on every pending mutation
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{accounts: map[string]Account{}, notify: make(chan struct{}, 1)}
}

// armNotify re-arms the pending-change signal so the flusher wakes on the
// next pending mutation.
func (l *Ledger) armNotify() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// Changed is read-only: it fires (with one buffered signal) whenever a
// mutation changes any account's pending total, so the flusher can flush
// the moment a threshold is crossed rather than waiting for its next tick.
func (l *Ledger) Changed() <-chan struct{} {
	return l.notify
}

// Reserve moves micro-credits into Pending before a request is served, so a
// caller who is served repeatedly across settlement gaps is never debited
// twice for the same spend. It succeeds only when Spendable covers the
// amount; on failure the account is unchanged and
// ErrInsufficientCredits is returned.
func (l *Ledger) Reserve(id string, credits uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	if a.Spendable() < credits {
		return ErrInsufficientCredits
	}
	a.Pending += credits
	l.accounts[id] = a
	l.armNotify()
	return nil
}

// Release returns a reservation, micro-credit for micro-credit, when the upstream call
// failed and the caller was never served. It saturates at zero: releasing
// more than is pending clears pending instead of underflowing.
func (l *Ledger) Release(id string, credits uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	if a.Pending <= credits {
		a.Pending = 0
	} else {
		a.Pending -= credits
	}
	l.accounts[id] = a
	l.armNotify()
}

// MarkSettled moves micro-credits from Pending to Settled after a successful
// on-chain settlement. It moves at most what is pending, so a settle that
// outpaces the in-memory pending (for instance after a restart with a
// stale checkpoint) cannot underflow either counter, and at most the
// account's unlocked balance, so a settle pass never settles into locked
// credits: the remainder stays pending.
func (l *Ledger) MarkSettled(id string, credits uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	if cap := a.SettleCap(); cap < credits {
		credits = cap
	}
	if a.Pending > credits {
		a.Pending -= credits
		a.Settled += credits
	} else {
		a.Settled += a.Pending
		a.Pending = 0
	}
	l.accounts[id] = a
	l.armNotify()
}

// SetMinted adopts an absolute chain-derived minted total, in either
// direction, and an unknown account is created with zero settled, pending and
// adjust. It is the settler's intent: the chain program is the authority for
// what an account holds, so a pass that reads a lower figure than the local
// ledger has must be able to store it - otherwise local state would keep
// claiming credits the chain will not let it settle (see
// TestRefreshLowersMintedNoUnderflow).
//
// Recording a *deposit rescan* is a different intent with a different rule:
// use RaiseMinted, which refuses to lower. Market movements also do not come
// through here; they ride in Adjust, which this leaves untouched.
func (l *Ledger) SetMinted(id string, minted uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	a.ID = id
	a.Minted = minted
	l.accounts[id] = a
}

// RaiseMinted records an absolute deposit total and never lowers what is
// already recorded.
//
// This is the deposit rail's write, and the refusal is the invariant: minted
// is derived from the treasury's transfer history, and a deposit only ever
// adds credits, so a scanned total below what is already recorded can only
// mean an incomplete read - a bounded RPC history, a page of signatures that
// did not arrive - never a withdrawal. Letting it through would silently
// destroy credits the chain still holds.
//
// It is deliberately separate from SetMinted rather than a flag on it: the
// settler adopting the chain's figure and the deposit rail recording a scan
// are different claims about the same field, and only one of them may go
// down.
func (l *Ledger) RaiseMinted(id string, minted uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	if a.Minted >= minted {
		return
	}
	a.ID = id
	a.Minted = minted
	l.accounts[id] = a
}

// Minted returns the chain-derived minted micro-credits recorded for an
// account: the figure SetMinted refuses to lower. A caller that wants to
// report an incomplete scan reads this before writing and compares.
func (l *Ledger) Minted(id string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts[id].Minted
}

// AdjustMinted applies a signed market movement to an account: positive
// when credits were acquired in a listing sale, negative when listed
// credits were released to a buyer. It is deliberately separate from
// SetMinted, whose absolute chain value would otherwise clobber the
// movement on the next deposit rescan. The changed signal arms so the
// console stream emits a balance event.
func (l *Ledger) AdjustMinted(id string, delta int64) {
	if delta == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	a.ID = id
	a.Adjust += delta
	l.accounts[id] = a
	l.armNotify()
}

// SetLocked records the credits the ledger contract has locked for an
// account, mirrored in from the market (EP-011-T04) and converted to
// micro-credits at the call site. An unknown account is created with zero
// minted, settled and pending. A lock change arms the
// changed signal so the console stream emits a balance event on a lock
// change alone, exactly as on any other movement.
func (l *Ledger) SetLocked(id string, locked uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	a.ID = id
	a.Locked = locked
	l.accounts[id] = a
	l.armNotify()
}

// Locked returns the micro-credits currently locked for an account, so a
// market write that just changed the row set (rather than computing a
// delta) can re-read the figure it is about to replace. An unknown account
// has nothing locked.
func (l *Ledger) Locked(id string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts[id].Locked
}

// Balance returns the account's spendable micro-credits at one consistent
// moment. An unknown account has a zero-value position, so it reports 0.
func (l *Ledger) Balance(id string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts[id].Spendable()
}

// SettleCap returns the micro-credits a settle pass may move for an account:
// the account's unlocked balance, capped at its pending.
func (l *Ledger) SettleCap(id string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts[id].SettleCap()
}

// PendingTotal returns a snapshot of each account's pending micro-credits
// for the settlement loop: one entry per account with pending, and no entries
// for accounts at rest.
func (l *Ledger) PendingTotal() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.accounts))
	for id, a := range l.accounts {
		if a.Pending > 0 {
			out[id] = a.Pending
		}
	}
	return out
}

// Snapshot returns every account at one consistent moment, for flushing
// to the checkpoint store.
func (l *Ledger) Snapshot() map[string]Account {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Account, len(l.accounts))
	for id, a := range l.accounts {
		out[id] = a
	}
	return out
}

// Restore loads accounts from the checkpoint store after a restart, so
// spendable reflects settled and pending again. Each entry replaces the
// in-memory account verbatim; no notify arm is needed because pending did
// not change by mutation - it arrived from disk.
func (l *Ledger) Restore(accounts map[string]Account) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, a := range accounts {
		a.ID = id
		l.accounts[id] = a
	}
}
