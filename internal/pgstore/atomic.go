package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"slotfare/internal/meter"
)

// MeterStore is the Postgres balance authority: every statement below is
// the shared constant from internal/meter, executed against the accounts
// table that every replica's proxy reserves against. No replica-local
// balance authority remains; the conditional UPDATE is what makes reserve,
// release and settle atomic across replicas.
var _ meter.AtomicLedger = (*MeterStore)(nil)
var _ meter.BalanceProvider = (*MeterStore)(nil)

// Reserve moves credits into pending in one conditional UPDATE: it touches
// exactly one row when spendable covers the amount, zero rows otherwise -
// including when the account is unknown, which matches the in-memory
// zero-value account. A concurrent writer that spends the balance first
// wins the row; this call then reports insufficient credits.
func (s *MeterStore) Reserve(ctx context.Context, id string, credits uint64) error {
	res, err := s.db.ExecContext(ctx, s.qf(meter.SQLReserve),
		credits, time.Now().Unix(), id, credits)
	if err != nil {
		return fmt.Errorf("pgstore: reserve %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return meter.ErrInsufficientCredits
	}
	return nil
}

// Release returns a reservation, saturating at zero like Ledger.Release.
// It cannot fail semantically: releasing more than is pending clears
// pending instead of underflowing, and an unknown account is a no-op.
func (s *MeterStore) Release(ctx context.Context, id string, credits uint64) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLRelease),
		credits, credits, time.Now().Unix(), id); err != nil {
		return fmt.Errorf("pgstore: release %s: %w", id, err)
	}
	return nil
}

// SettleCredits moves pending to settled, capped at min(credits, pending,
// unlocked balance) - the MarkSettled cap, atomic in one UPDATE. An unknown
// account is a no-op.
func (s *MeterStore) SettleCredits(ctx context.Context, id string, credits uint64) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLSettle),
		credits, credits, credits, credits, credits, credits, time.Now().Unix(), id); err != nil {
		return fmt.Errorf("pgstore: settle %s: %w", id, err)
	}
	return nil
}

// SetMinted adopts a fresh chain-read minted value: it upserts the account
// row, overwriting only minted when the row exists, so a concurrent reserve
// or settle on the row is never clobbered. It sets the value in either
// direction, which is the settler's intent: the chain program is the
// authority for what an account holds, so a read below the local figure must
// be storable.
func (s *MeterStore) SetMinted(ctx context.Context, id string, mintedMicro uint64) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLUpsertMinted),
		id, mintedMicro, time.Now().Unix(), mintedMicro, time.Now().Unix()); err != nil {
		return fmt.Errorf("pgstore: minted for %s: %w", id, err)
	}
	return nil
}

// RaiseMinted records an absolute deposited total and never lowers the stored
// one: a rescan that saw less than the row already holds - an incomplete or
// bounded RPC history - cannot take credits away.
//
// The guard is in the statement rather than in a read-then-write here, and
// that is the point: two replicas scanning at once would both read the old
// total, so a check in Go could not stop them lowering the row, while the
// statement's CASE sees the row as it is at write time.
func (s *MeterStore) RaiseMinted(ctx context.Context, id string, mintedMicro uint64) error {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLRaiseMinted),
		id, mintedMicro, now, now); err != nil {
		return fmt.Errorf("pgstore: raise minted for %s: %w", id, err)
	}
	return nil
}

// SetLocked mirrors SetMinted for the market lock: it upserts the account
// row, overwriting only locked when the row exists.
func (s *MeterStore) SetLocked(ctx context.Context, id string, lockedMicro uint64) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLUpsertLocked),
		id, lockedMicro, time.Now().Unix(), lockedMicro, time.Now().Unix()); err != nil {
		return fmt.Errorf("pgstore: locked for %s: %w", id, err)
	}
	return nil
}

// Accounts reads the shared account table back out: the loops' view of the
// balance authority, in place of the replica-local ledger snapshot.
func (s *MeterStore) Accounts(ctx context.Context) (map[string]meter.Account, error) {
	return s.Load(ctx)
}

// Balance returns one account's spendable micro-credits from the shared
// table: the balance header's value, read straight from the authority
// rather than any replica cache. An unknown account has no row and
// reports 0 spendable, matching Reserve's unknown-account semantics.
func (s *MeterStore) Balance(ctx context.Context, id string) (uint64, error) {
	a, err := s.Account(ctx, id)
	if err != nil {
		return 0, err
	}
	return a.Spendable(), nil
}

// Account returns one account's full row from the shared table: the figure
// every replica agrees on, since reserve, settle, mint and lock all land
// here. An unknown account has no row and reports the zero Account.
func (s *MeterStore) Account(ctx context.Context, id string) (meter.Account, error) {
	var a meter.Account
	var minted, adjust, settled, pending, locked int64
	err := s.db.QueryRowContext(ctx, s.qf(meter.SQLBalance), id).
		Scan(&a.ID, &minted, &adjust, &settled, &pending, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return meter.Account{}, nil
	}
	if err != nil {
		return meter.Account{}, fmt.Errorf("pgstore: account %s: %w", id, err)
	}
	a.Minted = uint64(minted)
	a.Adjust = adjust
	a.Settled = uint64(settled)
	a.Pending = uint64(pending)
	a.Locked = uint64(locked)
	return a, nil
}

// AdjustMinted applies a signed market movement to the shared row's adjust
// column, creating the row with a zero chain part when absent. The chain
// part stays untouched, so a concurrent SetMinted from the deposit watcher
// never clobbers the movement. Args: id, delta.
func (s *MeterStore) AdjustMinted(ctx context.Context, id string, deltaMicro int64) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLAdjustMinted),
		id, deltaMicro, time.Now().Unix(), deltaMicro, time.Now().Unix()); err != nil {
		return fmt.Errorf("pgstore: adjust minted for %s: %w", id, err)
	}
	return nil
}
