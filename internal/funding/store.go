// Package funding persists observed funding purchases: the /funding history
// the console reads, the deposit-rail writes that close one, and the named
// scan cursors behind them. Listings are not theirs - the credit market has
// its own package, internal/market - so the store interface here is exactly
// the purchase surface. The SQLite driver is SQLite; the Postgres driver
// lives in internal/pgstore.
package funding

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"

	"slotfare/internal/schema"
)

// SwapLeg is the swap side of a purchase that landed through the
// SwapPeripheral: the input token actually consumed, its amount in base
// units as a decimal string, and whether the input was native ETH (the
// payment transaction's value > 0) rather than an ERC-20 WETH. A nil leg
// means a direct stablecoin deposit: no swap was involved.
type SwapLeg struct {
	TokenIn  string `json:"tokenIn"`  // input token address, hex
	AmountIn string `json:"amountIn"` // input amount, base units, decimal string
	Native   bool   `json:"native"`   // true when paid in native ETH (tx value > 0)
}

// Purchase is one funding transaction observed for an account: the console's
// /funding route reports it until its credits land in the ledger.
type Purchase struct {
	AccountID string
	// ChainKey is the chain key of the source: the configured
	// source chain for a proven purchase, or 0 for a direct deposit on
	// the slotfare chain itself. Together with AccountID and DepositID it forms the
	// purchase identity that closes one.
	ChainKey       uint64
	SourceTx       string
	SourceBlock    uint64
	AttestedHeight uint64
	Credited       bool
	// DepositID is the deposit contract's monotonic id, as a decimal string;
	// it is what the ledger's CreditsMinted event carries, so it is the key
	// that closes a purchase.
	DepositID string
	// Token is the stablecoin symbol paid; UsdMicros the amount in millionths
	// of a dollar, integer so no float ever reaches the store.
	Token     string
	UsdMicros uint64
	// StartedAt is the source block's timestamp, RFC 3339 UTC.
	StartedAt string
	// The deposit rail's side once credited: mint transaction, block, its
	// timestamp (RFC 3339 UTC) and credits.
	CCTx       string
	CCBlock    uint64
	CreditedAt string
	Credits    uint64
	// Swap is the swap leg when the deposit landed through the peripheral;
	// nil for a direct stablecoin payment.
	Swap *SwapLeg
}

// Store persists observed funding purchases in the shared slotfared
// storage, alongside the keys and meter tables. The SQLite driver is
// SQLite; the Postgres driver lives in internal/pgstore.
type Store interface {
	Record(ctx context.Context, p Purchase) error
	OpenPurchases(ctx context.Context, accountID string) ([]Purchase, error)
	AllPurchases(ctx context.Context, accountID string) ([]Purchase, error)
	OpenAccounts(ctx context.Context) ([]string, error)
	StampAttested(ctx context.Context, chainKey, tip uint64) error
	MarkCreditedByDeposit(ctx context.Context, accountID string, chainKey uint64, depositID, ccTx string, ccBlock, credits uint64, creditedAt string) (bool, error)
	IncompleteDirectPurchases(ctx context.Context) ([]IncompleteDirectPurchase, error)
	RepairDirectPurchase(ctx context.Context, accountID string, chainKey uint64, depositID, token string, usdMicros uint64) error
	// Cursor is the named scan position behind the feeder; MarkCredited
	// closes a purchase by its source transaction.
	Cursor(ctx context.Context, name string) (uint64, bool, error)
	SetCursor(ctx context.Context, name string, block uint64) error
	MarkCredited(ctx context.Context, accountID, sourceTx string) error
	Close() error
}

// SQLite is the Store driver backed by the shared slotfared
// SQLite file. The table DDL it runs is internal/schema's: this package
// owns the statements that read and write the rows, not the columns.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewStore opens (creating if needed) the shared database at path,
// enables WAL and ensures the funding tables exist. sourceChainKey is the
// the chain key of the configured source chain; it is stamped on every
// row written before this change so the new (account, chain, deposit) primary
// key stays unique.
func NewStore(path string, sourceChainKey uint64) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("funding: open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("funding: enable WAL on %s: %w", path, err)
	}
	if err := schema.EnsureFunding(db, sourceChainKey); err != nil {
		db.Close()
		return nil, fmt.Errorf("funding: ensure funding table in %s: %w", path, err)
	}
	if err := schema.EnsureFundingCursor(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("funding: create funding_cursor table in %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// SQLRecordPurchase inserts one observed purchase; both drivers execute it.
// The SQLite driver prepends OR IGNORE (sqlRecordPurchaseIgnore); the
// Postgres driver appends ON CONFLICT ... DO NOTHING - repeated observations
// of the same purchase are no-ops either way, so a rescanner cannot
// double-count it. The key is (account_id, chain_key, deposit_id).
const SQLRecordPurchase = `INSERT INTO funding (account_id, chain_key, deposit_id, source_tx, source_block, attested_height, credited,
	  token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at,
	  swap_token_in, swap_amount_in, swap_native)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// sqlRecordPurchaseIgnore is the SQLite form of SQLRecordPurchase.
const sqlRecordPurchaseIgnore = `INSERT OR IGNORE INTO funding (account_id, chain_key, deposit_id, source_tx, source_block, attested_height, credited,
	  token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at,
	  swap_token_in, swap_amount_in, swap_native)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SwapFields flattens a purchase's optional swap leg into the three storage
// columns; a nil leg stores empty markers so direct deposits stay bare.
func SwapFields(p Purchase) (tokenIn, amountIn string, native bool) {
	if p.Swap == nil {
		return "", "", false
	}
	return p.Swap.TokenIn, p.Swap.AmountIn, p.Swap.Native
}

// Record persists one observed purchase.
func (s *SQLite) Record(ctx context.Context, p Purchase) error {
	tokenIn, amountIn, native := SwapFields(p)
	if _, err := s.db.ExecContext(ctx, sqlRecordPurchaseIgnore,
		p.AccountID, p.ChainKey, p.DepositID, p.SourceTx, p.SourceBlock, p.AttestedHeight, boolToInt(p.Credited),
		p.Token, p.UsdMicros, p.StartedAt, p.CCTx, p.CCBlock, p.Credits, p.CreditedAt,
		tokenIn, amountIn, boolToInt(native)); err != nil {
		return fmt.Errorf("funding: record purchase chain %d deposit %s for account %s: %w", p.ChainKey, p.DepositID, p.AccountID, err)
	}
	return nil
}

// OpenPurchases returns the account's purchases that have not been credited
// yet, oldest first.
func (s *SQLite) OpenPurchases(ctx context.Context, accountID string) ([]Purchase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_tx, source_block, attested_height, chain_key, deposit_id FROM funding
		 WHERE account_id = ? AND credited = 0 ORDER BY rowid`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("funding: list purchases for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []Purchase{}
	for rows.Next() {
		var p Purchase
		var chainKey, block, height int64
		if err := rows.Scan(&p.SourceTx, &block, &height, &chainKey, &p.DepositID); err != nil {
			return nil, fmt.Errorf("funding: scan purchase row: %w", err)
		}
		p.AccountID = accountID
		p.ChainKey = uint64(chainKey)
		p.SourceBlock = uint64(block)
		p.AttestedHeight = uint64(height)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("funding: list purchases for account %s: %w", accountID, err)
	}
	return out, nil
}

// AllPurchases returns every purchase of the account, newest first: the
// open ones the trace follows and the credited ones that are its history.
func (s *SQLite) AllPurchases(ctx context.Context, accountID string) ([]Purchase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT chain_key, source_tx, source_block, attested_height, credited,
		        deposit_id, token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at,
		        swap_token_in, swap_amount_in, swap_native
		 FROM funding WHERE account_id = ? ORDER BY rowid DESC`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("funding: list purchases for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []Purchase{}
	for rows.Next() {
		var p Purchase
		var chainKey, block, height, credited, ccBlock, credits, usd int64
		var swapTokenIn, swapAmountIn string
		var swapNative int64
		if err := rows.Scan(&chainKey, &p.SourceTx, &block, &height, &credited,
			&p.DepositID, &p.Token, &usd, &p.StartedAt, &p.CCTx, &ccBlock, &credits, &p.CreditedAt,
			&swapTokenIn, &swapAmountIn, &swapNative); err != nil {
			return nil, fmt.Errorf("funding: scan purchase row: %w", err)
		}
		p.AccountID = accountID
		p.ChainKey = uint64(chainKey)
		p.SourceBlock = uint64(block)
		p.AttestedHeight = uint64(height)
		p.Credited = credited != 0
		p.UsdMicros = uint64(usd)
		p.CCBlock = uint64(ccBlock)
		p.Credits = uint64(credits)
		if swapTokenIn != "" {
			p.Swap = &SwapLeg{TokenIn: swapTokenIn, AmountIn: swapAmountIn, Native: swapNative != 0}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("funding: list purchases for account %s: %w", accountID, err)
	}
	return out, nil
}

// OpenAccounts returns the accounts that have at least one uncredited
// purchase: the only accounts whose mints the ledger scan needs to see.
// SQLOpenAccounts lists the accounts with at least one uncredited purchase;
// both drivers execute it.
const SQLOpenAccounts = `SELECT DISTINCT account_id FROM funding WHERE credited = 0`

func (s *SQLite) OpenAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, SQLOpenAccounts)
	if err != nil {
		return nil, fmt.Errorf("funding: list open accounts: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("funding: scan open account: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StampAttested records tip as the attested height of every open purchase on
// chainKey whose source block the tip has passed and that has no height yet.
// The chain key keeps one chain's tip from stamping another chain's rows.
// SQLStampAttested records a tip as the attested height of open purchases;
// both drivers execute it.
const SQLStampAttested = `UPDATE funding SET attested_height = ?
	 WHERE credited = 0 AND attested_height = 0 AND source_block > 0 AND source_block <= ? AND chain_key = ?`

func (s *SQLite) StampAttested(ctx context.Context, chainKey, tip uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLStampAttested, tip, tip, chainKey); err != nil {
		return fmt.Errorf("funding: stamp attested height %d on chain %d: %w", tip, chainKey, err)
	}
	return nil
}

// MarkCreditedByDeposit closes the purchase the ledger's CreditsMinted event
// names by account, chain key and deposit id, recording the deposit rail's side.
// A mint for a purchase the source scan has not recorded yet is a no-op; the
// next pass records the purchase first and the ledger cursor is not advanced
// past an unmatched mint, so it is retried.
// SQLMarkCreditedByDeposit closes one purchase by its identity; both
// drivers execute it.
const SQLMarkCreditedByDeposit = `UPDATE funding SET credited = 1, cc_tx = ?, cc_block = ?, credits = ?, credited_at = ?
	 WHERE account_id = ? AND chain_key = ? AND deposit_id = ?`

func (s *SQLite) MarkCreditedByDeposit(ctx context.Context, accountID string, chainKey uint64, depositID, ccTx string, ccBlock, credits uint64, creditedAt string) (bool, error) {
	res, err := s.db.ExecContext(ctx, SQLMarkCreditedByDeposit,
		ccTx, ccBlock, credits, creditedAt, accountID, chainKey, depositID)
	if err != nil {
		return false, fmt.Errorf("funding: mark deposit %s (chain %d) credited for account %s: %w", depositID, chainKey, accountID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IncompleteDirectPurchase is one credited chain-0 row that still has no
// price: a direct deposit recorded before the feeder filled direct rows.
type IncompleteDirectPurchase struct {
	AccountID string
	ChainKey  uint64
	DepositID string
	CCTx      string
	CCBlock   uint64
	Credits   uint64
}

// IncompleteDirectPurchases returns every credited chain-0 row with an empty
// price and a known mint transaction, oldest first: the set the startup
// repair fills. A row with a price - paid or derived - never matches, so a
// completed repair stays completed.
func (s *SQLite) IncompleteDirectPurchases(ctx context.Context) ([]IncompleteDirectPurchase, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, chain_key, deposit_id, cc_tx, cc_block, credits
		 FROM funding WHERE chain_key = 0 AND credited = 1 AND usd_micros = 0 AND cc_tx != ''
		 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("funding: list incomplete direct purchases: %w", err)
	}
	defer rows.Close()
	out := []IncompleteDirectPurchase{}
	for rows.Next() {
		var r IncompleteDirectPurchase
		var chainKey, ccBlock, credits int64
		if err := rows.Scan(&r.AccountID, &chainKey, &r.DepositID, &r.CCTx, &ccBlock, &credits); err != nil {
			return nil, fmt.Errorf("funding: scan incomplete direct purchase: %w", err)
		}
		r.ChainKey = uint64(chainKey)
		r.CCBlock = uint64(ccBlock)
		r.Credits = uint64(credits)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("funding: list incomplete direct purchases: %w", err)
	}
	return out, nil
}

// RepairDirectPurchase writes one direct row's token and price when the row
// still has no price. The usd_micros = 0 in the WHERE keeps complete rows
// untouched and makes a repeated repair a no-op; identity is never changed.
// SQLRepairDirectPurchase fills one direct row's price; both drivers
// execute it.
const SQLRepairDirectPurchase = `UPDATE funding SET token = ?, usd_micros = ?
	 WHERE account_id = ? AND chain_key = ? AND deposit_id = ? AND usd_micros = 0`

func (s *SQLite) RepairDirectPurchase(ctx context.Context, accountID string, chainKey uint64, depositID, token string, usdMicros uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLRepairDirectPurchase,
		token, usdMicros, accountID, chainKey, depositID); err != nil {
		return fmt.Errorf("funding: repair direct deposit %s (chain %d) for account %s: %w", depositID, chainKey, accountID, err)
	}
	return nil
}

// Cursor returns the last block the named scan has covered; ok is false
// when the scan has never run.
// SQLCursor reads one named scan cursor; both drivers execute it.
const SQLCursor = `SELECT block FROM funding_cursor WHERE name = ?`

// SQLSetCursor upserts one named scan cursor; both drivers execute it.
const SQLSetCursor = `INSERT INTO funding_cursor (name, block) VALUES (?, ?)
	 ON CONFLICT(name) DO UPDATE SET block = excluded.block`

// SQLMarkCredited flags one purchase credited by source transaction; both
// drivers execute it.
const SQLMarkCredited = `UPDATE funding SET credited = 1 WHERE account_id = ? AND source_tx = ?`

func (s *SQLite) Cursor(ctx context.Context, name string) (uint64, bool, error) {
	var block int64
	err := s.db.QueryRowContext(ctx, SQLCursor, name).Scan(&block)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("funding: read cursor %s: %w", name, err)
	}
	return uint64(block), true, nil
}

// SetCursor records the last block the named scan has covered.
func (s *SQLite) SetCursor(ctx context.Context, name string, block uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLSetCursor, name, block); err != nil {
		return fmt.Errorf("funding: write cursor %s: %w", name, err)
	}
	return nil
}

// MarkCredited flags a purchase as credited so /funding stops reporting it.
func (s *SQLite) MarkCredited(ctx context.Context, accountID, sourceTx string) error {
	if _, err := s.db.ExecContext(ctx, SQLMarkCredited, accountID, sourceTx); err != nil {
		return fmt.Errorf("funding: mark purchase %s credited for account %s: %w", sourceTx, accountID, err)
	}
	return nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// boolToInt stores a bool as 1 or 0 for SQLite.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
