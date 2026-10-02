package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// ImportSpec describes the tables Import copies, in order. Columns holds
// each table's explicit column list - the same lists the shared INSERT
// constants use, so no auto-generated id is ever copied. Sequences names
// the Postgres sequences to re-align after the copy (usage_id_seq,
// funding_id_seq, listings_id_seq); it is empty when the target is a SQLite
// stand-in, where there is nothing to re-align.
type ImportSpec struct {
	Tables    []string
	Columns   map[string][]string
	Sequences []string
}

// PGImportSpec is the spec for copying a slotfared database into Postgres.
var PGImportSpec = ImportSpec{
	Tables: []string{"api_keys", "accounts", "usage", "funding", "funding_cursor", "listings", "sessions"},
	Columns: map[string][]string{
		"api_keys":       {"key_hash", "account_id", "prefix", "label", "created_at", "last_used", "request_count", "revoked"},
		"accounts":       {"id", "minted", "settled", "pending", "locked", "updated_at"},
		"usage":          {"account_id", "key_prefix", "ts", "service", "model", "kind", "prompt_tokens", "cached_tokens", "completion_tokens", "credits", "status", "duration_ms"},
		"funding":        {"account_id", "chain_key", "deposit_id", "source_tx", "source_block", "attested_height", "credited", "token", "usd_micros", "started_at", "cc_tx", "cc_block", "credits", "credited_at"},
		"funding_cursor": {"name", "block"},
		"listings":       {"listing_id", "account", "seller", "credits", "pay_token", "pay_token_symbol", "pay_token_decimals", "price", "matures_at", "state", "listed_tx", "listed_block", "listed_at", "cancelled_tx", "cancelled_block", "cancelled_at", "sold_tx", "sold_block", "sold_at", "buyer_account"},
		"sessions":       {"token_hash", "account_id", "issued_at", "expires_at", "last_seen"},
	},
	Sequences: []string{"usage_id_seq", "funding_id_seq", "listings_id_seq"},
}

// importBatchSize is the number of rows per INSERT statement inside one
// table's transaction.
const importBatchSize = 500

// Import copies every row of spec.Tables from src into dst. It first runs
// Migrate on dst (idempotent), then refuses to run when any target table
// already holds rows - naming the table and its count - before writing
// anything. Each table is copied in one transaction using the shared
// INSERT constants (no auto ids on either dialect); afterwards each named
// sequence is re-aligned with setval. A per-table row-count parity check
// runs before anything is reported; any mismatch or error returns an error
// carrying the counts seen so far.
func Import(ctx context.Context, src, dst *sql.DB, spec ImportSpec) (map[string]int64, error) {
	if err := Migrate(ctx, dst); err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, table := range spec.Tables {
		n, err := countRows(ctx, dst, table)
		if err != nil {
			return counts, err
		}
		if n > 0 {
			return counts, fmt.Errorf("pgstore: import refused: target table %s already holds %d rows; import only runs against an empty target", table, n)
		}
	}
	for _, table := range spec.Tables {
		n, err := copyTable(ctx, src, dst, table, spec.Columns[table])
		if err != nil {
			return counts, fmt.Errorf("pgstore: import %s: %w", table, err)
		}
		counts[table] = n
	}
	for _, seq := range spec.Sequences {
		table := strings.TrimSuffix(seq, "_id_seq")
		if counts[table] == 0 {
			continue
		}
		if err := alignSequence(ctx, dst, seq, table); err != nil {
			return counts, err
		}
	}
	for _, table := range spec.Tables {
		s, err := countRows(ctx, src, table)
		if err != nil {
			return counts, fmt.Errorf("pgstore: count source %s: %w", table, err)
		}
		d, err := countRows(ctx, dst, table)
		if err != nil {
			return counts, fmt.Errorf("pgstore: count target %s: %w", table, err)
		}
		if s != d {
			return counts, fmt.Errorf("pgstore: row count mismatch for %s: source %d, target %d", table, s, d)
		}
	}
	return counts, nil
}

// copyTable streams one table out of src and inserts it into dst in one
// transaction, returning the number of rows copied.
func copyTable(ctx context.Context, src, dst *sql.DB, table string, cols []string) (int64, error) {
	colsSQL := strings.Join(cols, ", ")
	rows, err := src.QueryContext(ctx, `SELECT `+colsSQL+` FROM `+table)
	if err != nil {
		return 0, fmt.Errorf("read source %s: %w", table, err)
	}
	defer rows.Close()

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin copy of %s: %w", table, err)
	}
	rollback := func() error { return tx.Rollback() }
	// $n placeholders bind on both dialects (SQLite numbers named
	// parameters in order of appearance), so the test's sqlite stand-in
	// target and the real Postgres target share this statement.
	insert := PG(`INSERT INTO ` + table + ` (` + colsSQL + `) VALUES (` + strings.TrimRight(strings.Repeat("?, ", len(cols)), ", ") + `)`)

	var n int64
	batch := make([][]any, 0, importBatchSize)
	flush := func() error {
		for _, args := range batch {
			if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
				return fmt.Errorf("insert into %s: %w", table, err)
			}
		}
		batch = batch[:0]
		return nil
	}
	values := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range dest {
		dest[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			_ = rollback()
			return 0, fmt.Errorf("scan source %s: %w", table, err)
		}
		args := make([]any, len(cols))
		copy(args, values)
		batch = append(batch, args)
		n++
		if len(batch) == importBatchSize {
			if err := flush(); err != nil {
				_ = rollback()
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rollback()
		return 0, fmt.Errorf("iterate source %s: %w", table, err)
	}
	if err := flush(); err != nil {
		_ = rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit copy of %s: %w", table, err)
	}
	return n, nil
}

// alignSequence sets seq to MAX(id) of table so later inserts continue the
// imported numbering instead of colliding with it.
func alignSequence(ctx context.Context, db *sql.DB, seq, table string) error {
	q := `SELECT setval('` + seq + `', COALESCE((SELECT MAX(id) FROM ` + table + `), 1))`
	if _, err := db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("pgstore: align sequence %s: %w", seq, err)
	}
	return nil
}

// countRows returns the number of rows a table holds.
func countRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}
