package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"slotfare/internal/meter"
)

// MeterStore implements meter.Store on Postgres. Every statement is the
// shared constant from internal/meter, rewritten for $n placeholders. The
// serial id column mirrors the SQLite AUTOINCREMENT row order.
type MeterStore struct {
	db *sql.DB
	// qf rewrites the shared ? placeholders for this pool's driver.
	// NewMeterStore sets PG; tests may replace it with the identity
	// function to run the exact shared SQL against a SQLite stand-in.
	qf func(string) string
}

// Load reads every account back out of the checkpoint. A fresh or empty
// database returns an empty map and a nil error.
func (s *MeterStore) Load(ctx context.Context) (map[string]meter.Account, error) {
	rows, err := s.db.QueryContext(ctx, s.qf(meter.SQLLoadAccounts))
	if err != nil {
		return nil, fmt.Errorf("pgstore: load accounts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]meter.Account)
	for rows.Next() {
		var a meter.Account
		var minted, adjust, settled, pending, locked int64
		if err := rows.Scan(&a.ID, &minted, &adjust, &settled, &pending, &locked); err != nil {
			return nil, fmt.Errorf("pgstore: scan account row: %w", err)
		}
		a.Minted = uint64(minted)
		a.Adjust = adjust
		a.Settled = uint64(settled)
		a.Pending = uint64(pending)
		a.Locked = uint64(locked)
		out[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: load accounts: %w", err)
	}
	return out, nil
}

// Flush replaces the checkpoint with the given accounts, atomically: one
// transaction deletes the old rows and inserts the new ones, so a crash
// mid-flush leaves either the old or the new state, never a mix. The
// in-memory ledger is authoritative at flush time.
func (s *MeterStore) Flush(ctx context.Context, accounts map[string]meter.Account) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pgstore: begin flush: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.qf(meter.SQLFlushDelete)); err != nil {
		return fmt.Errorf("pgstore: clear accounts: %w", err)
	}
	now := time.Now().Unix()
	for id, a := range accounts {
		if _, err := tx.ExecContext(ctx, s.qf(meter.SQLFlushInsert),
			id, a.Minted, a.Adjust, a.Settled, a.Pending, a.Locked, now); err != nil {
			return fmt.Errorf("pgstore: insert account %s: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pgstore: commit flush: %w", err)
	}
	return nil
}

// RecordUsage inserts one usage row and then prunes the account back down
// to its newest meter.UsageCap rows, so the table stays capped even when
// the console is never queried.
func (s *MeterStore) RecordUsage(ctx context.Context, row meter.UsageRow) error {
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLRecordUsageInsert),
		row.AccountID, row.KeyPrefix, row.Timestamp.Unix(), row.Service, row.Model,
		row.PromptTokens, row.CachedTokens, row.CompletionTokens, row.Credits, row.Status, row.Kind, row.DurationMS, row.Nonce); err != nil {
		return fmt.Errorf("pgstore: insert usage row: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, s.qf(meter.SQLPruneUsage),
		row.AccountID, row.AccountID, meter.UsageCap); err != nil {
		return fmt.Errorf("pgstore: prune usage rows for account %s: %w", row.AccountID, err)
	}
	return nil
}

// RecentUsage returns up to limit of the account's newest usage rows,
// newest first, optionally narrowed to one key prefix or to error rows.
// before > 0 keeps only rows with id < before so a caller can page back
// through the history; before == 0 leaves the statement byte-identical to the
// cursor-less form. A limit below 1 is read as 1; a limit above meter.UsageCap
// is clamped to it.
func (s *MeterStore) RecentUsage(ctx context.Context, accountID string, limit int, keyPrefix string, errorsOnly bool, before int64) ([]meter.UsageRow, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > meter.UsageCap {
		limit = meter.UsageCap
	}
	q := meter.SQLRecentUsageBase
	args := []any{accountID}
	if keyPrefix != "" {
		q += meter.SQLRecentUsageKeyPrefix
		args = append(args, keyPrefix)
	}
	if errorsOnly {
		q += meter.SQLRecentUsageErrorsOnly
		args = append(args, "error")
	}
	if before > 0 {
		q += meter.SQLRecentUsageBefore
		args = append(args, before)
	}
	q += meter.SQLRecentUsageTail
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.qf(q), args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: recent usage for account %s: %w", accountID, err)
	}
	return scanUsageRows(rows)
}

// UsagePage returns one numbered page of the account's usage rows matching
// f, newest first, plus the total number of matching rows; see
// meter.UsagePage. The statement is the shared constant from internal/meter,
// rewritten for $n placeholders.
func (s *MeterStore) UsagePage(ctx context.Context, accountID string, f meter.UsageFilter, page, pageSize int) ([]meter.UsageRow, int, error) {
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > meter.UsageCap {
		pageSize = meter.UsageCap
	}
	if page < 1 {
		page = 1
	}
	countQ, countArgs := usagePageWhere(meter.SQLUsageCountBase, f)
	var total int
	if err := s.db.QueryRowContext(ctx, s.qf(countQ), append([]any{accountID}, countArgs...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("pgstore: count usage for account %s: %w", accountID, err)
	}
	q, args := usagePageWhere(meter.SQLUsagePageBase, f)
	q += meter.SQLUsagePageTail
	args = append(append([]any{accountID}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, s.qf(q), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("pgstore: paged usage for account %s: %w", accountID, err)
	}
	out, err := scanUsageRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// UsageFacets returns the distinct key prefixes and model ids present for
// the account, each sorted; both are empty slices, never nil.
func (s *MeterStore) UsageFacets(ctx context.Context, accountID string) ([]string, []string, error) {
	rows, err := s.db.QueryContext(ctx, s.qf(meter.SQLUsageFacetsPrefixes), accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("pgstore: usage prefixes for account %s: %w", accountID, err)
	}
	prefixes, err := scanStringColumn(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("pgstore: usage prefixes for account %s: %w", accountID, err)
	}
	rows, err = s.db.QueryContext(ctx, s.qf(meter.SQLUsageFacetsModels), accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("pgstore: usage models for account %s: %w", accountID, err)
	}
	models, err := scanStringColumn(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("pgstore: usage models for account %s: %w", accountID, err)
	}
	return prefixes, models, nil
}

// usagePageWhere appends the shared filter fragments to q in the fixed order
// and returns the matching arguments; page and count share it so total
// always matches the pages.
func usagePageWhere(q string, f meter.UsageFilter) (string, []any) {
	args := []any{}
	if f.KeyPrefix != "" {
		q += meter.SQLUsagePageKeyPrefix
		args = append(args, f.KeyPrefix)
	}
	if f.Model != "" {
		q += meter.SQLUsagePageModel
		args = append(args, f.Model)
	}
	if f.From > 0 {
		q += meter.SQLUsagePageFrom
		args = append(args, f.From)
	}
	if f.To > 0 {
		q += meter.SQLUsagePageTo
		args = append(args, f.To)
	}
	if f.ErrorsOnly {
		q += meter.SQLUsagePageErrorsOnly
		args = append(args, "error")
	}
	return q, args
}

// scanStringColumn drains a single-column string result set, keeping the
// query's order, and returns an empty slice (never nil) when no rows match.
func scanStringColumn(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UsageSince returns the account's usage rows inserted after afterRowID,
// oldest first, capped at 500 (one poll window for the console stream).
func (s *MeterStore) UsageSince(ctx context.Context, accountID string, afterRowID int64) ([]meter.UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, s.qf(meter.SQLUsageSince), accountID, afterRowID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: usage since row %d for account %s: %w", afterRowID, accountID, err)
	}
	return scanUsageRows(rows)
}

// BatchRows returns the account's usage rows after afterRowID, oldest
// first; the settle batch walk runs over it. No LIMIT: RecordUsage prunes
// each account to its newest meter.UsageCap rows.
func (s *MeterStore) BatchRows(ctx context.Context, accountID string, afterRowID int64) ([]meter.UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, s.qf(meter.SQLBatchRows), accountID, afterRowID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: batch rows after %d for account %s: %w", afterRowID, accountID, err)
	}
	return scanUsageRows(rows)
}

// BatchCursor returns the account's committed cursor - the last usage id a
// previous batch covered - or 0 when the account has no batches yet.
func (s *MeterStore) BatchCursor(ctx context.Context, accountID string) (int64, error) {
	var cur int64
	if err := s.db.QueryRowContext(ctx, s.qf(meter.SQLBatchCursor), accountID).Scan(&cur); err != nil {
		return 0, fmt.Errorf("pgstore: batch cursor for account %s: %w", accountID, err)
	}
	return cur, nil
}

// UsageByID reads one of the account's usage rows by its row id; found is
// false when no row matches. See meter.UsageByID.
func (s *MeterStore) UsageByID(ctx context.Context, accountID string, rowID int64) (meter.UsageRow, bool, error) {
	var r meter.UsageRow
	var ts, prompt, cached, completion, credits int64
	var dur sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.qf(meter.SQLUsageByID), accountID, rowID).Scan(
		&r.RowID, &r.AccountID, &r.KeyPrefix, &ts, &r.Service, &r.Model,
		&r.Kind, &prompt, &cached, &completion, &credits, &r.Status, &dur, &r.Nonce)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return meter.UsageRow{}, false, nil
		}
		return meter.UsageRow{}, false, fmt.Errorf("pgstore: usage row %d for account %s: %w", rowID, accountID, err)
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

// BatchForRow returns the settle batch covering rowID and its local
// auto-increment id; found is false when no batch covers the row yet. See
// meter.BatchForRow.
func (s *MeterStore) BatchForRow(ctx context.Context, accountID string, rowID int64) (meter.SettleBatch, int64, bool, error) {
	var b meter.SettleBatch
	var id int64
	var root []byte
	err := s.db.QueryRowContext(ctx, s.qf(meter.SQLBatchForRow), accountID, rowID, rowID).Scan(
		&id, &b.AccountID, &root, &b.FirstUsageID, &b.LastUsageID, &b.Credits, &b.CoveredCredits, &b.TXHash, &b.CommittedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return meter.SettleBatch{}, 0, false, nil
		}
		return meter.SettleBatch{}, 0, false, fmt.Errorf("pgstore: batch covering row %d for account %s: %w", rowID, accountID, err)
	}
	if len(root) != 32 {
		return meter.SettleBatch{}, 0, false, fmt.Errorf("pgstore: batch root is %d bytes, want 32", len(root))
	}
	copy(b.Root[:], root)
	return b, id, true, nil
}

// BatchRangeRows returns the account's usage rows over a committed batch's
// row range, oldest first. See meter.BatchRangeRows.
func (s *MeterStore) BatchRangeRows(ctx context.Context, accountID string, firstID, lastID int64) ([]meter.UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, s.qf(meter.SQLBatchRangeRows), accountID, firstID, lastID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: batch range rows %d..%d for account %s: %w", firstID, lastID, accountID, err)
	}
	return scanUsageRows(rows)
}

// CommitRecord persists one settle batch's bookkeeping and, in the same
// transaction, runs the authoritative settle UPDATE on the shared accounts
// table: both land or neither, so bookkeeping can never be lost for
// settled credits. The settler mirrors the settle into its local display
// cache after this returns; it must not run SQLSettle a second time.
func (s *MeterStore) CommitRecord(ctx context.Context, b meter.SettleBatch, settleMicro uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pgstore: begin commit record: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	// SQLSettle caps at min(settleMicro, pending, unlocked balance), so a
	// stale read can never over-settle the shared table.
	if _, err := tx.ExecContext(ctx, s.qf(meter.SQLSettle),
		settleMicro, settleMicro, settleMicro, settleMicro, settleMicro, settleMicro, now, b.AccountID); err != nil {
		return fmt.Errorf("pgstore: settle for account %s: %w", b.AccountID, err)
	}
	if _, err := tx.ExecContext(ctx, s.qf(meter.SQLRecordBatchInsert),
		b.AccountID, b.Root[:], b.FirstUsageID, b.LastUsageID, b.Credits, b.CoveredCredits, b.TXHash, b.CommittedAt); err != nil {
		return fmt.Errorf("pgstore: record batch for account %s: %w", b.AccountID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pgstore: commit record: %w", err)
	}
	return nil
}

// Ping reports whether the underlying pool is reachable; slotfared's /health
// probe calls it.
func (s *MeterStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying pool.
func (s *MeterStore) Close() error {
	return s.db.Close()
}

// DB returns the underlying pool; slotfared builds the advisory-lock locker on
// top of it, so the loops and the balance authority share one pool.
func (s *MeterStore) DB() *sql.DB {
	return s.db
}

// scanUsageRows drains a usage result set, keeping the query's order, and
// returns an empty slice (never nil) when no rows match.
func scanUsageRows(rows *sql.Rows) ([]meter.UsageRow, error) {
	defer rows.Close()
	out := []meter.UsageRow{}
	for rows.Next() {
		var r meter.UsageRow
		var ts, prompt, cached, completion, credits int64
		var dur sql.NullInt64
		if err := rows.Scan(&r.RowID, &r.AccountID, &r.KeyPrefix, &ts, &r.Service, &r.Model,
			&r.Kind, &prompt, &cached, &completion, &credits, &r.Status, &dur); err != nil {
			return nil, fmt.Errorf("pgstore: scan usage row: %w", err)
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
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: scan usage rows: %w", err)
	}
	return out, nil
}
