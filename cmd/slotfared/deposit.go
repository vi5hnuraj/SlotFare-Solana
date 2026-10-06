package main

import (
	"context"
	"errors"
	"fmt"

	"slotfare/internal/meter"
	"slotfare/internal/solana"
)

// errDepositScanGap reports a deposit rescan that saw less than the balance
// authority already holds. It is a fact about the scan, not about the
// account: an incomplete read, never a withdrawal.
var errDepositScanGap = errors.New("deposit rescan saw less than the recorded total")

// ledgerMirror is the chain side of the deposit rail: it raises an account's
// on-chain credit account to an absolute credited total. *solana.LedgerMirror
// is the production implementation; the interface exists so the rail's retry
// behavior is testable without a cluster.
type ledgerMirror interface {
	Sync(ctx context.Context, accountID string, targetCredits uint64) error
}

// depositRail is the deposit watcher's write path for one account's absolute
// deposited total. It is deliberately the only place that turns a scanned
// total into recorded state, so the two rules below hold for every caller.
//
//  1. A rescan can never lower the recorded total. Both storage layers refuse
//     it themselves - Ledger.RaiseMinted in memory and SQLRaiseMinted on the
//     shared row - and this type reads the recorded figure back so the refusal
//     is reported rather than passed off as a successful write. The rail uses
//     RaiseMinted and never SetMinted: SetMinted is the settler adopting the
//     chain's figure, which may legitimately go down.
//  2. A failed mirror onto the chain program is returned, never swallowed,
//     because the watcher records an account as done once this returns nil
//     and skips any account whose total has not moved. Swallowing it would
//     strand the chain behind the local authority until a process restart.
//
// The read in rule 1 is not the guard: on the Postgres path another replica
// may write between the read and the write, which is exactly why the guard
// lives in the storage layer and the read only decides what to report.
type depositRail struct {
	// ledger is the local authority on the SQLite path and the console's
	// display cache on the Postgres path.
	ledger *meter.Ledger
	// atomic is the shared Postgres authority; nil on the SQLite path, where
	// the in-memory ledger is the authority.
	atomic meter.AtomicLedger
	// reader reads the recorded figures back from whichever store is the
	// authority.
	reader meter.AccountReader
	// mirror raises the on-chain credit account; nil when no ledger program
	// is configured.
	mirror ledgerMirror
	logf   func(format string, args ...any)
}

// newDepositRail wires the rail against the stores main.go opened, choosing
// the authority for reads the same way the rest of the daemon does.
func newDepositRail(ledger *meter.Ledger, atomic meter.AtomicLedger, mirror ledgerMirror, logf func(string, ...any)) *depositRail {
	reader := meter.AccountReader(meter.NewLocalAuthority(ledger))
	if atomic != nil {
		// The shared row is the authority when Postgres is configured.
		if shared, ok := atomic.(meter.AccountReader); ok {
			reader = shared
		}
	}
	return &depositRail{ledger: ledger, atomic: atomic, reader: reader, mirror: mirror, logf: logf}
}

// mint applies one scanned absolute deposit total, in micro-credits.
func (r *depositRail) mint(ctx context.Context, accountID string, scannedMicro uint64) error {
	recorded, err := r.recorded(ctx, accountID)
	if err != nil {
		return err
	}
	if scannedMicro < recorded {
		r.logf("slotfared: deposit scan gap: %s scanned %d micro-credits but %d are recorded; keeping the recorded total (the scan found less history than the balance already reflects)",
			accountID, scannedMicro, recorded)
		return fmt.Errorf("%w: %s scanned %d micro-credits, %d recorded", errDepositScanGap, accountID, scannedMicro, recorded)
	}

	if r.atomic != nil {
		if err := r.atomic.RaiseMinted(ctx, accountID, scannedMicro); err != nil {
			return err
		}
	}
	r.ledger.RaiseMinted(accountID, scannedMicro)

	if r.mirror != nil {
		if err := r.mirror.Sync(ctx, accountID, scannedMicro/meter.MicroCreditsPerCredit); err != nil {
			r.logf("slotfared: ledger mirror %s: %v", accountID, err)
			return fmt.Errorf("ledger mirror %s: %w", accountID, err)
		}
	}

	r.logf("slotfared: deposit mint: %s now %d micro-credits", accountID, scannedMicro)
	return nil
}

// recorded reads the total the balance authority holds for an account.
func (r *depositRail) recorded(ctx context.Context, accountID string) (uint64, error) {
	acct, err := r.reader.Account(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("read recorded minted for %s: %w", accountID, err)
	}
	return acct.Minted, nil
}

// newLedgerMirror builds the chain side of the rail, or a nil interface when
// no ledger program is configured. It returns the interface rather than the
// concrete pointer on purpose: a typed nil *solana.LedgerMirror assigned to
// this interface would compare non-nil and then panic on the first call.
func newLedgerMirror(client *solana.Client) ledgerMirror {
	m := solana.NewLedgerMirror(client)
	if m == nil {
		return nil
	}
	return m
}
