package meter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the checkpoint for the in-memory ledger. It is a durability
// device, not the request path: counters live in memory and are written
// here periodically by the Flusher. The SQLite driver is SQLite; the
// Postgres driver lives in internal/pgstore.
type Store interface {
	Load(ctx context.Context) (map[string]Account, error)
	Flush(ctx context.Context, accounts map[string]Account) error
	RecordUsage(ctx context.Context, row UsageRow) error
	RecentUsage(ctx context.Context, accountID string, limit int, keyPrefix string, errorsOnly bool, before int64) ([]UsageRow, error)
	UsagePage(ctx context.Context, accountID string, f UsageFilter, page, pageSize int) ([]UsageRow, int, error)
	UsageFacets(ctx context.Context, accountID string) ([]string, []string, error)
	UsageSince(ctx context.Context, accountID string, afterRowID int64) ([]UsageRow, error)
	BatchRows(ctx context.Context, accountID string, afterRowID int64) ([]UsageRow, error)
	BatchCursor(ctx context.Context, accountID string) (int64, error)
	CommitRecord(ctx context.Context, b SettleBatch, settleMicro uint64) error
	UsageByID(ctx context.Context, accountID string, rowID int64) (UsageRow, bool, error)
	BatchForRow(ctx context.Context, accountID string, rowID int64) (SettleBatch, int64, bool, error)
	BatchRangeRows(ctx context.Context, accountID string, firstID, lastID int64) ([]UsageRow, error)
	Ping(ctx context.Context) error
	Close() error
}

// SQLite is the Store driver backed by a single SQLite file. The pure-Go
// driver keeps the binary cgo-free so it cross-compiles.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the SQLite database at path, enables WAL,
// and ensures the accounts table exists.
func Open(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("meter: open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("meter: enable WAL on %s: %w", path, err)
	}
	// All amount columns in this database are stored in micro-credits, the
	// internal metering unit (MicroCreditsPerCredit to the credit); the
	// one-time migration below scales databases written in whole credits.
	schema := `CREATE TABLE IF NOT EXISTS accounts (
		id TEXT PRIMARY KEY,
		minted INTEGER NOT NULL,
		settled INTEGER NOT NULL,
		pending INTEGER NOT NULL,
		locked INTEGER NOT NULL DEFAULT 0,
		adjust INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("meter: create accounts table in %s: %w", path, err)
	}
	// Checkpoints written before the locked column existed keep their rows;
	// add the column so Load can read it back. A database that already has
	// it (fresh or re-opened) reports a duplicate and is left alone.
	if _, err := db.Exec(`ALTER TABLE accounts ADD COLUMN locked INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("meter: migrate accounts table in %s: %w", path, err)
		}
	}
	// The market adjust column: signed ledger-side movements that must
	// survive an absolute SetMinted from the deposit watcher. Same
	// tolerated-duplicate pattern as locked.
	if _, err := db.Exec(`ALTER TABLE accounts ADD COLUMN adjust INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("meter: migrate accounts table in %s: %w", path, err)
		}
	}
	// Usage rows are what the console's /usage view and /stream usage
	// events read back; they record spend, never request or response text.
	// credits is the settled cost of the call in micro-credits.
	usageSchema := `CREATE TABLE IF NOT EXISTS usage (
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
		status TEXT NOT NULL,
		nonce TEXT NOT NULL DEFAULT ''
	)`
	if _, err := db.Exec(usageSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("meter: create usage table in %s: %w", path, err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_account ON usage (account_id, id)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("meter: index usage table in %s: %w", path, err)
	}
	// Batches are the settle bookkeeping: which usage rows a settle batch
	// covered, the root committed on-chain for them, and the tx hash. The
	// settler writes one row per committed batch; the cursor is the MAX
	// last_usage_id per account.
	batchesSchema := `CREATE TABLE IF NOT EXISTS batches (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id TEXT NOT NULL,
		root BLOB NOT NULL,
		first_usage_id INTEGER NOT NULL,
		last_usage_id INTEGER NOT NULL,
		credits INTEGER NOT NULL,
		covered_credits INTEGER NOT NULL,
		tx_hash TEXT NOT NULL DEFAULT '',
		committed_at INTEGER NOT NULL
	)`
	if _, err := db.Exec(batchesSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("meter: create batches table in %s: %w", path, err)
	}
	// Usage rows written before the kind column existed keep their rows;
	// add the column so the service kind can be read back, defaulting to
	// empty. A database that already has it (fresh or re-opened) reports a
	// duplicate and is left alone.
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN kind TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("meter: migrate usage table in %s: %w", path, err)
		}
	}
	// Usage rows written before the duration_ms column existed keep their
	// rows; add the nullable column so recorded durations can be read
	// back. A database that already has it (fresh or re-opened) reports a
	// duplicate and is left alone.
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN duration_ms INTEGER`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("meter: migrate usage table in %s: %w", path, err)
		}
	}
	// Usage rows written before the nonce column existed keep their rows;
	// add the column so the server-assigned request nonce can be read back,
	// defaulting to empty. A database that already has it (fresh or
	// re-opened) reports a duplicate and is left alone.
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN nonce TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("meter: migrate usage table in %s: %w", path, err)
		}
	}
	if err := migrateToMicroCredits(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// migrateToMicroCredits scales a database written before the internal unit
// moved to micro-credits: every whole-credit amount column is multiplied by
// MicroCreditsPerCredit exactly once. PRAGMA user_version guards it - 0
// means unmigrated, 1 means done - so a re-open never scales a second time.
// A fresh database has empty tables, where the UPDATEs touch no rows; it is
// stamped 1 on its first open.
func migrateToMicroCredits(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("meter: read user_version: %w", err)
	}
	if version != 0 {
		return nil
	}
	factor := fmt.Sprintf("%d", MicroCreditsPerCredit)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("meter: begin micro-credit migration: %w", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE accounts SET minted = minted * ` + factor + `, settled = settled * ` + factor +
			`, pending = pending * ` + factor + `, locked = locked * ` + factor,
		`UPDATE usage SET credits = credits * ` + factor,
		`PRAGMA user_version = 1`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("meter: run micro-credit migration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("meter: commit micro-credit migration: %w", err)
	}
	return nil
}

// SQLLoadAccounts reads every account back out of the checkpoint; both
// drivers execute it.
const SQLLoadAccounts = `SELECT id, minted, adjust, settled, pending, locked FROM accounts`

// SQLBalance reads one account's row back out of the shared table; the
// Postgres driver executes it to answer the balance header. An unknown
// account is no row, which the caller reports as 0 spendable.
const SQLBalance = `SELECT id, minted, adjust, settled, pending, locked FROM accounts WHERE id = ?`

// SQLFlushDelete clears the checkpoint; both drivers execute it.
const SQLFlushDelete = `DELETE FROM accounts`

// SQLFlushInsert writes one account into the checkpoint; both drivers
// execute it.
const SQLFlushInsert = `INSERT INTO accounts (id, minted, adjust, settled, pending, locked, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`

// The five statements below are the Postgres balance-authority surface.
// Each is executed by the Postgres driver (internal/pgstore) when
// SLOTFARE_DATABASE_URL is set: the conditional UPDATE is what makes reserve,
// release and settle atomic across replicas, so no replica holds a local
// balance authority. They are dialect-neutral (? placeholders, no engine
// specific functions) so the shared-SQL guard test and the SQLite stand-in
// tests run the identical text; in sqlite mode the in-memory ledger stays
// authoritative and these are never run. The saturating floor and the
// settle cap use CASE instead of GREATEST/LEAST because the pure-Go
// SQLite build omits those functions; the CASE forms evaluate identically
// in both engines.

// SQLReserve moves credits into pending only when spendable covers the
// amount: one UPDATE whose WHERE clause is the check, so a concurrent
// writer that spends the balance first makes this UPDATE touch zero rows.
// Args: credits, now, id, credits.
const SQLReserve = `UPDATE accounts SET pending = pending + ?, updated_at = ? WHERE id = ? AND minted + adjust - settled - pending - locked >= ?`

// SQLRelease returns a reservation, saturating at zero like
// Ledger.Release: releasing more than is pending clears pending instead
// of underflowing. Args: credits, credits, now, id.
const SQLRelease = `UPDATE accounts SET pending = CASE WHEN pending <= ? THEN 0 ELSE pending - ? END, updated_at = ? WHERE id = ?`

// SQLSettle moves pending to settled, capped at min(credits, pending,
// unlocked balance) - the MarkSettled cap, in one statement. The CASE
// evaluates min(credits, pending, max(minted - settled - locked, 0))
// exactly; both SET expressions evaluate against the pre-update row, which
// is what makes the single UPDATE atomic; do not "optimize" them into a
// variable. Args: credits x6 (three per CASE, the CASE appears twice),
// now, id.
const SQLSettle = `UPDATE accounts SET pending = pending - CASE WHEN minted + adjust - settled - locked <= 0 THEN 0 WHEN ? <= pending AND ? <= minted + adjust - settled - locked THEN ? WHEN pending <= minted + adjust - settled - locked THEN pending ELSE minted + adjust - settled - locked END, settled = settled + CASE WHEN minted + adjust - settled - locked <= 0 THEN 0 WHEN ? <= pending AND ? <= minted + adjust - settled - locked THEN ? WHEN pending <= minted + adjust - settled - locked THEN pending ELSE minted + adjust - settled - locked END, updated_at = ? WHERE id = ?`

// SQLUpsertMinted adopts a fresh chain-read minted value: it creates the
// account row with zero settled, pending and locked when absent, and only
// overwrites minted when present, so a concurrent reserve or settle on the
// row is never clobbered. It sets the value in either direction, which is
// the settler's intent: the chain program is the authority, so a read below
// the local figure must be storable. Args: id, minted, now, minted, now.
const SQLUpsertMinted = `INSERT INTO accounts (id, minted, settled, pending, locked, updated_at) VALUES (?, ?, 0, 0, 0, ?) ON CONFLICT(id) DO UPDATE SET minted = ?, updated_at = ?`

// SQLRaiseMinted is the deposit rescan's write and cannot lower the row: when
// the account exists, minted becomes the greater of the stored and incoming
// totals. An incomplete or bounded RPC history therefore cannot take credits
// away, which is the same refusal Ledger.RaiseMinted makes in memory.
//
// The comparison is a CASE rather than GREATEST or MAX(a, b) because this
// statement is executed by both dialects: the Postgres store runs it against
// Postgres, and the offline suite runs the same text against SQLite, where
// GREATEST does not exist (and on Postgres MAX(a, b) is the aggregate, not a
// scalar). CASE is spelled identically in both.
//
// Only minted and updated_at are touched. Args: id, minted, now, now.
const SQLRaiseMinted = `INSERT INTO accounts (id, minted, settled, pending, locked, updated_at) VALUES (?, ?, 0, 0, 0, ?) ON CONFLICT(id) DO UPDATE SET minted = CASE WHEN accounts.minted > excluded.minted THEN accounts.minted ELSE excluded.minted END, updated_at = ?`

// SQLUpsertLocked mirrors SQLUpsertMinted for the market lock: it creates
// the account row with zero minted, settled and pending when absent, and
// only overwrites locked when present. Args: id, locked, now, locked, now.
const SQLUpsertLocked = `INSERT INTO accounts (id, minted, settled, pending, locked, updated_at) VALUES (?, 0, 0, 0, ?, ?) ON CONFLICT(id) DO UPDATE SET locked = ?, updated_at = ?`

// SQLAdjustMinted applies a signed market movement to the adjust column,
// creating the row with a zero chain part when absent. The chain part is
// never touched, so an absolute SQLUpsertMinted from the deposit watcher
// converges on the same row without clobbering the movement. Args: id,
// delta, now, delta, now.
const SQLAdjustMinted = `INSERT INTO accounts (id, minted, adjust, settled, pending, locked, updated_at) VALUES (?, 0, ?, 0, 0, 0, ?) ON CONFLICT(id) DO UPDATE SET adjust = adjust + ?, updated_at = ?`

// Load reads every account back out of the checkpoint. A fresh or empty
// database returns an empty map and a nil error.
func (s *SQLite) Load(ctx context.Context) (map[string]Account, error) {
	rows, err := s.db.QueryContext(ctx, SQLLoadAccounts)
	if err != nil {
		return nil, fmt.Errorf("meter: load accounts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]Account)
	for rows.Next() {
		var a Account
		var minted, adjust, settled, pending, locked int64
		if err := rows.Scan(&a.ID, &minted, &adjust, &settled, &pending, &locked); err != nil {
			return nil, fmt.Errorf("meter: scan account row: %w", err)
		}
		a.Minted = uint64(minted)
		a.Adjust = adjust
		a.Settled = uint64(settled)
		a.Pending = uint64(pending)
		a.Locked = uint64(locked)
		out[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("meter: load accounts: %w", err)
	}
	return out, nil
}

// Flush replaces the checkpoint with the given accounts, atomically: one
// transaction deletes the old rows and inserts the new ones, so a crash
// mid-flush leaves either the old or the new state, never a mix. The
// in-memory ledger is authoritative at flush time.
func (s *SQLite) Flush(ctx context.Context, accounts map[string]Account) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("meter: begin flush: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, SQLFlushDelete); err != nil {
		return fmt.Errorf("meter: clear accounts: %w", err)
	}
	now := time.Now().Unix()
	for id, a := range accounts {
		if _, err := tx.ExecContext(ctx, SQLFlushInsert,
			id, a.Minted, a.Adjust, a.Settled, a.Pending, a.Locked, now); err != nil {
			return fmt.Errorf("meter: insert account %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("meter: commit flush: %w", err)
	}
	return nil
}

// Ping reports whether the underlying database is reachable; slotfared's
// /health probe calls it.
func (s *SQLite) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// BatchRows returns the account's usage rows after afterRowID, oldest
// first; the settle batch walk runs over it. No LIMIT: RecordUsage prunes
// each account to its newest UsageCap rows.
func (s *SQLite) BatchRows(ctx context.Context, accountID string, afterRowID int64) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, SQLBatchRows, accountID, afterRowID)
	if err != nil {
		return nil, fmt.Errorf("meter: batch rows after %d for account %s: %w", afterRowID, accountID, err)
	}
	return scanUsageRows(rows)
}

// UsageByID reads one of the account's usage rows by its row id; found is
// false when no row matches. It is the entry point for serving a charge's
// inclusion proof, scoped to the caller's own rows.
func (s *SQLite) UsageByID(ctx context.Context, accountID string, rowID int64) (UsageRow, bool, error) {
	var r UsageRow
	var ts, prompt, cached, completion, credits int64
	var dur sql.NullInt64
	err := s.db.QueryRowContext(ctx, SQLUsageByID, accountID, rowID).Scan(
		&r.RowID, &r.AccountID, &r.KeyPrefix, &ts, &r.Service, &r.Model,
		&r.Kind, &prompt, &cached, &completion, &credits, &r.Status, &dur, &r.Nonce)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UsageRow{}, false, nil
		}
		return UsageRow{}, false, fmt.Errorf("meter: usage row %d for account %s: %w", rowID, accountID, err)
	}
	r.Timestamp = time.Unix(ts, 0).UTC()
	r.PromptTokens = uint64(prompt)
	r.CachedTokens = uint64(cached)
	r.CompletionTokens = uint64(completion)
	r.Credits = uint64(credits)
	if dur.Valid {
		v := uint64(dur.Int64)
		r.DurationMS = &v
	}
	return r, true, nil
}

// BatchForRow returns the settle batch that covers rowID - first_usage_id <=
// rowID <= last_usage_id - along with its local auto-increment id. found is
// false when no batch covers the row yet (the row is still pending).
func (s *SQLite) BatchForRow(ctx context.Context, accountID string, rowID int64) (SettleBatch, int64, bool, error) {
	var b SettleBatch
	var id int64
	var root []byte
	err := s.db.QueryRowContext(ctx, SQLBatchForRow, accountID, rowID, rowID).Scan(
		&id, &b.AccountID, &root, &b.FirstUsageID, &b.LastUsageID, &b.Credits, &b.CoveredCredits, &b.TXHash, &b.CommittedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SettleBatch{}, 0, false, nil
		}
		return SettleBatch{}, 0, false, fmt.Errorf("meter: batch covering row %d for account %s: %w", rowID, accountID, err)
	}
	if len(root) != 32 {
		return SettleBatch{}, 0, false, fmt.Errorf("meter: batch root is %d bytes, want 32", len(root))
	}
	copy(b.Root[:], root)
	return b, id, true, nil
}

// BatchRangeRows returns the account's usage rows from firstID to lastID
// inclusive, oldest first - exactly the range a settle batch committed over,
// so a proof can be rebuilt on demand without any stored proof material.
func (s *SQLite) BatchRangeRows(ctx context.Context, accountID string, firstID, lastID int64) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, SQLBatchRangeRows, accountID, firstID, lastID)
	if err != nil {
		return nil, fmt.Errorf("meter: batch range rows %d..%d for account %s: %w", firstID, lastID, accountID, err)
	}
	return scanUsageRows(rows)
}

// BatchCursor returns the account's committed cursor - the last usage id a
// previous batch covered - or 0 when the account has no batches yet.
func (s *SQLite) BatchCursor(ctx context.Context, accountID string) (int64, error) {
	var cur int64
	if err := s.db.QueryRowContext(ctx, SQLBatchCursor, accountID).Scan(&cur); err != nil {
		return 0, fmt.Errorf("meter: batch cursor for account %s: %w", accountID, err)
	}
	return cur, nil
}

// CommitRecord persists one settle batch's bookkeeping. In SQLite mode the
// in-memory ledger is the balance authority - the settler marks it settled
// after this returns - so this is the single insert; settleMicro is not
// written here. The Postgres driver instead runs the authoritative settle
// UPDATE and this insert in one transaction.
func (s *SQLite) CommitRecord(ctx context.Context, b SettleBatch, _ uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLRecordBatchInsert,
		b.AccountID, b.Root[:], b.FirstUsageID, b.LastUsageID, b.Credits, b.CoveredCredits, b.TXHash, b.CommittedAt); err != nil {
		return fmt.Errorf("meter: record batch for account %s: %w", b.AccountID, err)
	}
	return nil
}
