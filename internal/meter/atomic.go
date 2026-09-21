package meter

import (
	"context"
)

// Authority is the proxy's narrow view of credit reservation. When
// SLOTFARE_DATABASE_URL is set it is the Postgres store, where a reservation
// succeeds or fails as one conditional UPDATE against the shared account
// row and no replica holds a local balance authority; in sqlite
// single-process mode it is LocalAuthority, wrapping the in-memory ledger
// exactly as today.
type Authority interface {
	Reserve(ctx context.Context, id string, credits uint64) error
	Release(ctx context.Context, id string, credits uint64) error
}

// BalanceProvider is the optional Authority extension that reads an
// account's spendable balance: the micro-credits the account may still
// spend, so a metered response can tell the client its position without a
// second call. Unknown accounts report 0.
type BalanceProvider interface {
	Balance(ctx context.Context, id string) (uint64, error)
}

// AccountReader reads one account's full position (minted, settled, pending,
// locked) from the balance authority. The console reads through it so every
// replica reports the shared figures instead of its own in-memory ledger,
// which only sees the reservations it served and the locks it pushed itself.
// An unknown account comes back as the zero Account with an empty ID.
type AccountReader interface {
	Account(ctx context.Context, id string) (Account, error)
}

// LocalAuthority adapts the in-memory Ledger to Authority for sqlite mode:
// the context is ignored and the calls land on the ledger verbatim, so the
// request path is byte-for-byte today's behavior.
type LocalAuthority struct {
	l *Ledger
}

var _ Authority = (*LocalAuthority)(nil)
var _ BalanceProvider = (*LocalAuthority)(nil)
var _ AccountReader = (*LocalAuthority)(nil)

// NewLocalAuthority wraps l as an Authority.
func NewLocalAuthority(l *Ledger) *LocalAuthority {
	return &LocalAuthority{l: l}
}

// Balance returns the account's spendable micro-credits on the local
// ledger.
func (a *LocalAuthority) Balance(_ context.Context, id string) (uint64, error) {
	return a.l.Balance(id), nil
}

// Account returns the in-memory position; on the single-process SQLite path
// the ledger is the authority.
func (a *LocalAuthority) Account(_ context.Context, id string) (Account, error) {
	acct, ok := a.l.Snapshot()[id]
	if !ok {
		return Account{}, nil
	}
	return acct, nil
}

// Reserve moves credits into pending on the local ledger.
func (a *LocalAuthority) Reserve(_ context.Context, id string, credits uint64) error {
	return a.l.Reserve(id, credits)
}

// Release returns a reservation on the local ledger; it cannot fail.
func (a *LocalAuthority) Release(_ context.Context, id string, credits uint64) error {
	a.l.Release(id, credits)
	return nil
}

// AtomicLedger is the full Postgres balance authority used by the
// background loops: reserve/release plus settle, minted and locked writes
// and the shared account read. The loops mirror every write into their
// replica-local Ledger as a console display cache; the database is the
// authority.
type AtomicLedger interface {
	Authority
	SettleCredits(ctx context.Context, id string, credits uint64) error
	SetMinted(ctx context.Context, id string, mintedMicro uint64) error
	// RaiseMinted is the deposit rescan's write: it records an absolute
	// deposited total and never lowers the stored figure, so an incomplete
	// scan cannot take credits away. SetMinted is the settler's write and may
	// go down, because there the chain program is the authority.
	RaiseMinted(ctx context.Context, id string, mintedMicro uint64) error
	SetLocked(ctx context.Context, id string, lockedMicro uint64) error
	// AdjustMinted applies a signed market movement, separate from the
	// absolute SetMinted so a deposit rescan cannot clobber it.
	AdjustMinted(ctx context.Context, id string, deltaMicro int64) error
	Accounts(ctx context.Context) (map[string]Account, error)
}
