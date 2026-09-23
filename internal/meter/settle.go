package meter

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ChainClient is the on-chain settlement surface the meter needs. It moves
// whole credits, which is what the contract takes; internal/chain.Client
// satisfies it and tests use a fake.
type ChainClient interface {
	BalanceOf(ctx context.Context, accountID string) (uint64, error)
	Settle(ctx context.Context, accountID string, credits uint64) (string, error)
}

// UsageCommitter is the on-chain commitment surface: it stores a settle
// batch's usage root and burns its credits in one
// transaction, so there is no failure ordering to reason about - both
// land or neither. internal/chain.Client satisfies it when an attestor
// address is configured; tests use a fake.
type UsageCommitter interface {
	CommitAndSettle(ctx context.Context, root [32]byte, accountID string, firstUsageId, lastUsageId uint64, credits uint64) (string, error)
}

// SettleBatchStore is the batch bookkeeping surface the committed-settle
// path needs: the account's rows after its committed cursor, the cursor
// itself, and the record write. meter.Store satisfies it in both dialects.
type SettleBatchStore interface {
	BatchRows(ctx context.Context, accountID string, afterRowID int64) ([]UsageRow, error)
	BatchCursor(ctx context.Context, accountID string) (int64, error)
	CommitRecord(ctx context.Context, b SettleBatch, settleMicro uint64) error
}

// Settler closes the metering loop against the credit ledger: on an
// interval it refreshes Minted for every known account from the chain and
// settles pending usage that reaches the threshold. The ledger speaks
// micro-credits and the threshold is in micro-credits too; the chain moves
// whole credits, so each pass settles only the whole-credit part of the
// pending and leaves the sub-credit remainder pending locally. A settlement
// failure never loses usage: pending stays pending, the reservation still
// blocks respending, and the next pass retries.
type Settler struct {
	l         *Ledger
	c         ChainClient
	interval  time.Duration
	threshold uint64

	// Atomic, when set, makes Postgres the balance authority for this
	// loop: the pass reads and writes the shared accounts table, and every
	// write is mirrored into l as a console display cache. Nil leaves the
	// sqlite single-process path exactly as before.
	Atomic AtomicLedger
	// Locker, when set, single-flights this loop across replicas via a
	// Postgres advisory lock: a tick that cannot take the lock is skipped,
	// and a dead holder's lock dies with its connection so any replica
	// inherits on its next tick. Nil means no gating.
	Locker Locker
	// Attestor, together with BatchStore, switches the settle from the
	// direct ledger.settle to the on-chain commitAndSettle: every
	// batch commits the Merkle root of the exact usage rows whose spend it
	// burns, in the same transaction as the burn. Either one nil - which
	// is the default - keeps today's direct-settle behavior byte-for-byte.
	Attestor   UsageCommitter
	BatchStore SettleBatchStore
}

// NewSettler returns a Settler that refreshes and settles l through c
// every interval. An account is settled when its pending reaches
// threshold; a threshold of zero settles every account with any pending
// on every pass.
func NewSettler(l *Ledger, c ChainClient, interval time.Duration, threshold uint64) *Settler {
	return &Settler{l: l, c: c, interval: interval, threshold: threshold}
}

// Run blocks until ctx is done, running one settlement pass per tick.
// Errors are reported through onErr (which may be nil) and never stop the
// loop; there is no shutdown settle, because leaving usage pending is the
// safe state.
func (s *Settler) Run(ctx context.Context, onErr func(error)) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.tick(ctx, onErr)
		case <-ctx.Done():
			return nil
		}
	}
}

// tick runs one settlement pass, gated by the advisory lock when a locker
// is attached: a replica that cannot take the lock skips the tick silently
// - the holder is running it, and when the holder dies its session-level
// lock dies with its connection so any replica inherits on its next tick.
func (s *Settler) tick(ctx context.Context, onErr func(error)) {
	if s.Locker == nil {
		s.step(ctx, onErr)
		return
	}
	release, ok, err := s.Locker.Try(ctx, "settler")
	if err != nil {
		if onErr != nil {
			onErr(fmt.Errorf("meter: settle: acquire loop lock: %w", err))
		}
		return
	}
	if !ok {
		return
	}
	s.step(ctx, onErr)
	if err := release(ctx); err != nil && onErr != nil {
		onErr(fmt.Errorf("meter: settle: release loop lock: %w", err))
	}
}

// step runs one pass: refresh Minted for every known account from the
// chain (converting whole chain credits to micro-credits at that boundary),
// then settle each account whose pending reaches the threshold. Per-account
// failures are reported and skipped; they never abort the pass and never
// lose usage. When Atomic is set the pass drives off the shared accounts
// table and mirrors every write into the local ledger; otherwise it runs on
// the local ledger exactly as before.
func (s *Settler) step(ctx context.Context, onErr func(error)) {
	if ctx.Err() != nil {
		return
	}
	var accts map[string]Account
	if s.Atomic != nil {
		var err error
		if accts, err = s.Atomic.Accounts(ctx); err != nil {
			if onErr != nil {
				onErr(fmt.Errorf("meter: settle: load accounts: %w", err))
			}
			return
		}
	}
	setMinted := func(id string, mintedMicro uint64) {
		if s.Atomic != nil {
			if err := s.Atomic.SetMinted(ctx, id, mintedMicro); err != nil && onErr != nil {
				onErr(fmt.Errorf("meter: settle: store minted for %s: %w", id, err))
			}
			// Keep the pass's view current so the settle cap below sees
			// the refreshed minted, as the local-ledger path does.
			a := accts[id]
			a.Minted = mintedMicro
			accts[id] = a
		}
		s.l.SetMinted(id, mintedMicro)
	}
	markSettled := func(id string, credits uint64) {
		if s.Atomic != nil {
			if err := s.Atomic.SettleCredits(ctx, id, credits); err != nil && onErr != nil {
				onErr(fmt.Errorf("meter: settle: store settle for %s: %w", id, err))
			}
		}
		s.l.MarkSettled(id, credits)
	}
	if s.Atomic != nil {
		for id := range accts {
			minted, err := s.c.BalanceOf(ctx, id)
			if err != nil {
				if onErr != nil {
					onErr(fmt.Errorf("meter: settle: refresh %s: %w", id, err))
				}
				continue
			}
			setMinted(id, minted*MicroCreditsPerCredit)
		}
	} else {
		for id := range s.l.Snapshot() {
			minted, err := s.c.BalanceOf(ctx, id)
			if err != nil {
				if onErr != nil {
					onErr(fmt.Errorf("meter: settle: refresh %s: %w", id, err))
				}
				continue
			}
			setMinted(id, minted*MicroCreditsPerCredit)
		}
	}
	var pendings map[string]uint64
	if s.Atomic != nil {
		pendings = make(map[string]uint64, len(accts))
		for id, a := range accts {
			if a.Pending > 0 {
				pendings[id] = a.Pending
			}
		}
	} else {
		pendings = s.l.PendingTotal()
	}
	for id, pending := range pendings {
		if pending < s.threshold {
			continue
		}
		// A settle pass never settles into locked credits: cap what this
		// pass moves at the account's unlocked balance and leave the
		// remainder pending. That is not an error - it waits for the lock
		// to lift or new credits to arrive.
		var cap uint64
		if s.Atomic != nil {
			cap = accts[id].SettleCap()
		} else {
			cap = s.l.SettleCap(id)
		}
		if cap < pending {
			pending = cap
		}
		// The chain burns whole credits only: settle the whole-credit part
		// of the pending and carry the sub-credit remainder in the local
		// ledger, where it accumulates until it reaches a whole credit.
		whole := pending / MicroCreditsPerCredit
		if whole == 0 {
			continue
		}
		if s.Attestor != nil && s.BatchStore != nil {
			s.settleCommitted(ctx, onErr, id, pending)
			continue
		}
		txHash, err := s.c.Settle(ctx, id, whole)
		if err != nil {
			if onErr != nil {
				onErr(fmt.Errorf("meter: settle: %s: %w", id, err))
			}
			continue
		}
		markSettled(id, whole*MicroCreditsPerCredit)
		_ = txHash // the caller logs it; step keeps its surface narrow
	}
}

// settleCommitted settles one account through the on-chain committer: it walks
// the account's usage rows after the committed cursor, forms the batch per
// SelectBatch against the capped pending P, commits the batch root and the
// burn in one on-chain transaction, records the batch bookkeeping, and
// marks the covered spend settled locally. A failed commit defers the
// settle: nothing is marked, the pending stays, and the next pass retries
// with the same cursor. No row is ever covered twice: the cursor only
// moves when CommitRecord lands.
func (s *Settler) settleCommitted(ctx context.Context, onErr func(error), id string, P uint64) {
	report := func(err error) {
		if onErr != nil {
			onErr(fmt.Errorf("meter: settle: %s: %w", id, err))
		}
	}
	cursor, err := s.BatchStore.BatchCursor(ctx, id)
	if err != nil {
		report(err)
		return
	}
	rows, err := s.BatchStore.BatchRows(ctx, id, cursor)
	if err != nil {
		report(err)
		return
	}
	sel, covered, ok := SelectBatch(rows, P)
	if !ok {
		if len(sel) == 0 {
			report(errors.New("no usage rows to commit"))
		}
		// A sub-credit-only covered sum defers silently, exactly like
		// today's whole == 0 case.
		return
	}
	root, err := UsageRoot(sel)
	if err != nil {
		report(err)
		return
	}
	credits := covered / MicroCreditsPerCredit
	txHash, err := s.Attestor.CommitAndSettle(ctx, root, id, uint64(sel[0].RowID), uint64(sel[len(sel)-1].RowID), credits)
	if err != nil {
		report(err)
		return
	}
	b := SettleBatch{
		AccountID:      id,
		Root:           root,
		FirstUsageID:   uint64(sel[0].RowID),
		LastUsageID:    uint64(sel[len(sel)-1].RowID),
		Credits:        credits,
		CoveredCredits: covered,
		TXHash:         txHash,
		CommittedAt:    time.Now().Unix(),
	}
	if err := s.BatchStore.CommitRecord(ctx, b, covered); err != nil {
		// The commit landed on-chain but the local bookkeeping did not:
		// report it and leave the pending alone - the next pass retries,
		// the same window today's direct settle already has.
		report(err)
		return
	}
	// l.MarkSettled only: in Postgres mode the authoritative settle UPDATE
	// already ran inside CommitRecord's transaction, so calling
	// Atomic.SettleCredits again would double-settle the shared table.
	s.l.MarkSettled(id, covered)
}
