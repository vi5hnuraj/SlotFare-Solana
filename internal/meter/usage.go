package meter

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// UsageCap is the number of newest usage rows kept per account: the
// console's usage view is capped at this, and RecordUsage prunes to it,
// so one account cannot grow the table without bound.
const UsageCap = 200

// UsageRow is one metered call: what was spent, never what was asked. It
// holds the token counts the upstream reported, the service and model they
// came from, the credits that settled - in micro-credits, the internal
// metering unit - and whether the call failed. No request or response text
// is stored, in any column. KeyPrefix is the 13-char attk_ prefix of the
// key, never key material.
type UsageRow struct {
	RowID            int64
	AccountID        string
	KeyPrefix        string
	Timestamp        time.Time
	Service          string
	Model            string
	Kind             string // registry service kind - "embeddings" / "reranker"; empty for chat and registry-less rows, so existing rows read back unchanged
	PromptTokens     uint64
	CachedTokens     uint64
	CompletionTokens uint64
	Credits          uint64
	// "ok" or "error".
	Status string // "ok" or "error"
	// Nonce is the server-assigned request nonce for this exchange: 32 hex
	// chars, recorded on the row so a receipt's nonce can later be checked
	// against it. It is not a leaf word - the T02 leaf stays the pinned
	// 10-word scheme. Empty for pre-existing rows and for exchanges where
	// receipts are disabled.
	Nonce string
	// DurationMS is the wall-clock duration of the call in whole
	// milliseconds, measured in the proxy from request acceptance to the
	// upstream response fully settling (streaming included); error rows
	// hold the time to failure. Nil means no duration was recorded - all
	// pre-existing rows - and is never fabricated as 0.
	DurationMS *uint64
}

// UsageLogger is the writer view of metered calls that the proxy records
// against.
type UsageLogger interface {
	RecordUsage(ctx context.Context, row UsageRow) error
}

// SQLRecordUsageInsert writes one metered call; both drivers execute it.
const SQLRecordUsageInsert = `INSERT INTO usage (account_id, key_prefix, ts, service, model,
	 prompt_tokens, cached_tokens, completion_tokens, credits, status, kind, duration_ms, nonce)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// SQLPruneUsage keeps only the account's newest UsageCap rows; both drivers
// execute it.
const SQLPruneUsage = `DELETE FROM usage WHERE account_id = ? AND id NOT IN
	 (SELECT id FROM usage WHERE account_id = ? ORDER BY id DESC LIMIT ?)`

// RecordUsage inserts one usage row and then prunes the account back down
// to its newest UsageCap rows, so the table stays capped even when the
// console is never queried.
func (s *SQLite) RecordUsage(ctx context.Context, row UsageRow) error {
	if _, err := s.db.ExecContext(ctx, SQLRecordUsageInsert,
		row.AccountID, row.KeyPrefix, row.Timestamp.Unix(), row.Service, row.Model,
		row.PromptTokens, row.CachedTokens, row.CompletionTokens, row.Credits, row.Status, row.Kind, row.DurationMS, row.Nonce); err != nil {
		return fmt.Errorf("meter: insert usage row: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, SQLPruneUsage,
		row.AccountID, row.AccountID, UsageCap); err != nil {
		return fmt.Errorf("meter: prune usage rows for account %s: %w", row.AccountID, err)
	}
	return nil
}

// UsageFilter narrows UsagePage to a subset of the account's rows; every
// field is optional and the fragments combine with AND. KeyPrefix is the
// exact attk_ prefix stored on rows, Model the exact model id.
// From/To are unix seconds, inclusive (ts >= From AND ts <= To); 0 means
// unset, which leaves the fragment out of the statement.
type UsageFilter struct {
	KeyPrefix  string
	Model      string
	From       int64
	To         int64
	ErrorsOnly bool
}

// SQLUsagePageBase is the fixed part of the paged usage read; both drivers
// assemble the same statement from these fragments, in this order, so the
// page query and the count query can never drift apart. No new index is
// needed: RecordUsage prunes each account to its newest UsageCap rows, so
// filtering an account's rows scans at most 200 rows through
// idx_usage_account - a new index would be write amplification for no
// measurable gain.
const (
	SQLUsagePageBase = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms
	        FROM usage WHERE account_id = ?`
	SQLUsagePageKeyPrefix  = ` AND key_prefix = ?`
	SQLUsagePageModel      = ` AND model = ?`
	SQLUsagePageFrom       = ` AND ts >= ?`
	SQLUsagePageTo         = ` AND ts <= ?`
	SQLUsagePageErrorsOnly = ` AND status = ?`
	SQLUsagePageTail       = ` ORDER BY id DESC LIMIT ? OFFSET ?`
)

// SQLUsageCountBase counts the rows of the same WHERE clause as
// UsagePage; both drivers append the identical filter fragments so total
// always matches the pages.
const SQLUsageCountBase = `SELECT COUNT(*) FROM usage WHERE account_id = ?`

// SQLUsageFacetsPrefixes / SQLUsageFacetsModels list the distinct key
// prefixes and model ids present for the account (not the filtered set) -
// they feed the console's dropdowns; both drivers execute them.
const (
	SQLUsageFacetsPrefixes = `SELECT DISTINCT key_prefix FROM usage WHERE account_id = ? ORDER BY key_prefix`
	SQLUsageFacetsModels   = `SELECT DISTINCT model FROM usage WHERE account_id = ? ORDER BY model`
)

// usagePageWhere appends the filter fragments to q in the fixed order and
// returns the matching arguments; page and count share it.
func usagePageWhere(q string, f UsageFilter) (string, []any) {
	args := []any{}
	if f.KeyPrefix != "" {
		q += SQLUsagePageKeyPrefix
		args = append(args, f.KeyPrefix)
	}
	if f.Model != "" {
		q += SQLUsagePageModel
		args = append(args, f.Model)
	}
	if f.From > 0 {
		q += SQLUsagePageFrom
		args = append(args, f.From)
	}
	if f.To > 0 {
		q += SQLUsagePageTo
		args = append(args, f.To)
	}
	if f.ErrorsOnly {
		q += SQLUsagePageErrorsOnly
		args = append(args, "error")
	}
	return q, args
}

// UsagePage returns one numbered page of the account's usage rows matching
// f, newest first: page is 1-based, pageSize is the rows per page. The
// second result is the TOTAL number of rows matching f, so a caller can
// render page controls. A pageSize below 1 is read as 1; a pageSize above
// UsageCap is clamped to it; a page below 1 is read as 1.
func (s *SQLite) UsagePage(ctx context.Context, accountID string, f UsageFilter, page, pageSize int) ([]UsageRow, int, error) {
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > UsageCap {
		pageSize = UsageCap
	}
	if page < 1 {
		page = 1
	}
	countQ, countArgs := usagePageWhere(SQLUsageCountBase, f)
	var total int
	if err := s.db.QueryRowContext(ctx, countQ, append([]any{accountID}, countArgs...)...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("meter: count usage for account %s: %w", accountID, err)
	}
	q, args := usagePageWhere(SQLUsagePageBase, f)
	q += SQLUsagePageTail
	args = append(append([]any{accountID}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("meter: paged usage for account %s: %w", accountID, err)
	}
	out, err := scanUsageRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// UsageFacets returns the distinct key prefixes and model ids present for
// the account, each sorted; both are empty slices, never nil.
func (s *SQLite) UsageFacets(ctx context.Context, accountID string) ([]string, []string, error) {
	rows, err := s.db.QueryContext(ctx, SQLUsageFacetsPrefixes, accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("meter: usage prefixes for account %s: %w", accountID, err)
	}
	prefixes, err := scanStringColumn(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("meter: usage prefixes for account %s: %w", accountID, err)
	}
	rows, err = s.db.QueryContext(ctx, SQLUsageFacetsModels, accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("meter: usage models for account %s: %w", accountID, err)
	}
	models, err := scanStringColumn(rows)
	if err != nil {
		return nil, nil, fmt.Errorf("meter: usage models for account %s: %w", accountID, err)
	}
	return prefixes, models, nil
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

// SQLRecentUsageBase is the fixed part of the RecentUsage query; both
// drivers assemble the same statement from these fragments.
const (
	SQLRecentUsageBase = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms
	        FROM usage WHERE account_id = ?`
	SQLRecentUsageKeyPrefix  = ` AND key_prefix = ?`
	SQLRecentUsageErrorsOnly = ` AND status = ?`
	SQLRecentUsageBefore     = ` AND id < ?`
	SQLRecentUsageTail       = ` ORDER BY id DESC LIMIT ?`
)

// RecentUsage returns up to limit of the account's newest usage rows,
// newest first, optionally narrowed to one key prefix or to error rows.
// before > 0 keeps only rows with id < before, so a caller can page back
// through the history; before == 0 leaves the statement byte-identical to
// the cursor-less form. A limit below 1 is read as 1; a limit above UsageCap
// is clamped to it.
func (s *SQLite) RecentUsage(ctx context.Context, accountID string, limit int, keyPrefix string, errorsOnly bool, before int64) ([]UsageRow, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > UsageCap {
		limit = UsageCap
	}
	q := SQLRecentUsageBase
	args := []any{accountID}
	if keyPrefix != "" {
		q += SQLRecentUsageKeyPrefix
		args = append(args, keyPrefix)
	}
	if errorsOnly {
		q += SQLRecentUsageErrorsOnly
		args = append(args, "error")
	}
	if before > 0 {
		q += SQLRecentUsageBefore
		args = append(args, before)
	}
	q += SQLRecentUsageTail
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("meter: recent usage for account %s: %w", accountID, err)
	}
	return scanUsageRows(rows)
}

// SQLUsageSince returns the account's usage rows after one row id; both
// drivers execute it.
const SQLUsageSince = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms
	        FROM usage WHERE account_id = ? AND id > ? ORDER BY id ASC LIMIT 500`

// UsageSince returns the account's usage rows inserted after afterRowID,
// oldest first, capped at 500 (one poll window for the console stream).
func (s *SQLite) UsageSince(ctx context.Context, accountID string, afterRowID int64) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, SQLUsageSince, accountID, afterRowID)
	if err != nil {
		return nil, fmt.Errorf("meter: usage since row %d for account %s: %w", afterRowID, accountID, err)
	}
	return scanUsageRows(rows)
}

// scanUsageRows drains a usage result set, keeping the query's order, and
// returns an empty slice (never nil) when no rows match.
func scanUsageRows(rows *sql.Rows) ([]UsageRow, error) {
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		var ts, prompt, cached, completion, credits int64
		var dur sql.NullInt64
		if err := rows.Scan(&r.RowID, &r.AccountID, &r.KeyPrefix, &ts, &r.Service, &r.Model,
			&r.Kind, &prompt, &cached, &completion, &credits, &r.Status, &dur); err != nil {
			return nil, fmt.Errorf("meter: scan usage row: %w", err)
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
		return nil, fmt.Errorf("meter: scan usage rows: %w", err)
	}
	return out, nil
}
