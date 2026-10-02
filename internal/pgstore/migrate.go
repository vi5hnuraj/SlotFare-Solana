package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Migrate applies the Postgres schema idempotently: every statement is a
// CREATE ... IF NOT EXISTS, so it is safe to run on every boot (k8s
// restarts are routine) and a second call is a no-op. All amounts are
// micro-credits; every integer column is BIGINT and every flag a 0/1
// BIGINT, exactly as the SQLite store - Postgres booleans are never used.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pgstore: begin migration: %w", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			minted BIGINT NOT NULL,
			settled BIGINT NOT NULL,
			pending BIGINT NOT NULL,
			locked BIGINT NOT NULL DEFAULT 0,
			adjust BIGINT NOT NULL DEFAULT 0,
			updated_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			key_hash BYTEA PRIMARY KEY,
			account_id TEXT NOT NULL,
			prefix TEXT NOT NULL DEFAULT '',
			label TEXT NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			last_used BIGINT NOT NULL DEFAULT 0,
			request_count BIGINT NOT NULL DEFAULT 0,
			revoked BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS usage (
			id BIGSERIAL PRIMARY KEY,
			account_id TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			ts BIGINT NOT NULL,
			service TEXT NOT NULL,
			model TEXT NOT NULL,
			prompt_tokens BIGINT NOT NULL,
			cached_tokens BIGINT NOT NULL,
			completion_tokens BIGINT NOT NULL,
			credits BIGINT NOT NULL,
			status TEXT NOT NULL,
			nonce TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_account ON usage (account_id, id)`,
		`CREATE TABLE IF NOT EXISTS batches (
			id BIGSERIAL PRIMARY KEY,
			account_id TEXT NOT NULL,
			root BYTEA NOT NULL,
			first_usage_id BIGINT NOT NULL,
			last_usage_id BIGINT NOT NULL,
			credits BIGINT NOT NULL,
			covered_credits BIGINT NOT NULL,
			tx_hash TEXT NOT NULL DEFAULT '',
			committed_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS funding (
			id BIGSERIAL,
			account_id TEXT NOT NULL,
			chain_key BIGINT NOT NULL,
			deposit_id TEXT NOT NULL,
			source_tx TEXT NOT NULL DEFAULT '',
			source_block BIGINT NOT NULL DEFAULT 0,
			attested_height BIGINT NOT NULL DEFAULT 0,
			credited BIGINT NOT NULL DEFAULT 0,
			token TEXT NOT NULL DEFAULT '',
			usd_micros BIGINT NOT NULL DEFAULT 0,
			started_at TEXT NOT NULL DEFAULT '',
			cc_tx TEXT NOT NULL DEFAULT '',
			cc_block BIGINT NOT NULL DEFAULT 0,
			credits BIGINT NOT NULL DEFAULT 0,
			credited_at TEXT NOT NULL DEFAULT '',
			swap_token_in TEXT NOT NULL DEFAULT '',
			swap_amount_in TEXT NOT NULL DEFAULT '',
			swap_native BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (account_id, chain_key, deposit_id)
		)`,
		`CREATE TABLE IF NOT EXISTS funding_cursor (
			name TEXT PRIMARY KEY,
			block BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS listings (
			id BIGSERIAL,
			listing_id BIGINT PRIMARY KEY,			account TEXT NOT NULL,
			seller TEXT NOT NULL DEFAULT '',
			credits BIGINT NOT NULL DEFAULT 0,
			pay_token TEXT NOT NULL DEFAULT '',
			pay_token_symbol TEXT NOT NULL DEFAULT '',
			pay_token_decimals BIGINT NOT NULL DEFAULT 0,
			price BIGINT NOT NULL DEFAULT 0,
			matures_at BIGINT NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'open',
			listed_tx TEXT NOT NULL DEFAULT '',
			listed_block BIGINT NOT NULL DEFAULT 0,
			listed_at TEXT NOT NULL DEFAULT '',
			cancelled_tx TEXT NOT NULL DEFAULT '',
			cancelled_block BIGINT NOT NULL DEFAULT 0,
			cancelled_at TEXT NOT NULL DEFAULT '',
			sold_tx TEXT NOT NULL DEFAULT '',
			sold_block BIGINT NOT NULL DEFAULT 0,
			sold_at TEXT NOT NULL DEFAULT '',
			buyer_account TEXT NOT NULL DEFAULT '',
			listing_nonce TEXT NOT NULL DEFAULT '0'
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash BYTEA PRIMARY KEY,
			account_id TEXT NOT NULL,
			issued_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL,
			last_seen BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS nonces (
			address TEXT PRIMARY KEY,
			nonce TEXT NOT NULL,
			issued_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL
		)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("pgstore: migrate: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pgstore: commit migration: %w", err)
	}
	// The usage.kind ALTER runs outside the transaction above: Postgres
	// aborts a whole transaction on any statement error, so a tolerated
	// duplicate-column error would still poison the commit. Standalone,
	// the duplicate is genuinely ignorable on both dialects (SQLite has
	// no ADD COLUMN IF NOT EXISTS, so the tolerate-error probe stays).
	if _, err := db.ExecContext(ctx, `ALTER TABLE usage ADD COLUMN kind TEXT NOT NULL DEFAULT ''`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
			return fmt.Errorf("pgstore: migrate: %w", err)
		}
	}
	// The nullable duration_ms ALTER runs outside the transaction too, for
	// the same reason as the kind ALTER above: a tolerated duplicate would
	// poison the commit. Pre-existing rows read back as NULL - the column
	// is never defaulted, so an absent duration stays absent.
	if _, err := db.ExecContext(ctx, `ALTER TABLE usage ADD COLUMN duration_ms BIGINT`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
			return fmt.Errorf("pgstore: migrate: %w", err)
		}
	}
	// The nonce ALTER runs outside the transaction too, for the same reason
	// as the kind and duration_ms ALTERs above. Pre-existing rows read back
	// as empty - the receipt nonce is server-assigned per exchange, so an
	// absent one stays absent.
	if _, err := db.ExecContext(ctx, `ALTER TABLE usage ADD COLUMN nonce TEXT NOT NULL DEFAULT ''`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
			return fmt.Errorf("pgstore: migrate: %w", err)
		}
	}
	// The accounts adjust ALTER runs outside the transaction too, same
	// tolerated-duplicate pattern: pre-existing rows read back 0, and the
	// signed market movement rides alongside the absolute chain minted.
	if _, err := db.ExecContext(ctx, `ALTER TABLE accounts ADD COLUMN adjust BIGINT NOT NULL DEFAULT 0`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
			return fmt.Errorf("pgstore: migrate accounts adjust: %w", err)
		}
	}
	// The listing_nonce ALTER runs outside the transaction too, same
	// tolerated-duplicate pattern: it arrived with the on-chain market, so a
	// row mirrored before it has no program listing behind it and reads
	// back 0. The column is TEXT: the nonce is a full 64-bit draw and half
	// of them exceed int64max, which would abort the indexer mid-insert.
	if _, err := db.ExecContext(ctx, `ALTER TABLE listings ADD COLUMN listing_nonce TEXT NOT NULL DEFAULT '0'`); err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
			return fmt.Errorf("pgstore: migrate listings nonce: %w", err)
		}
	}
	// A table that shipped with listing_nonce BIGINT keeps it otherwise, so
	// convert it in place. The default is dropped first: an integer default
	// cannot be cast automatically to text. All three statements are no-ops
	// once the column is TEXT, which makes the block safe to run at boot.
	// The conversion is Postgres syntax and Postgres only: SQLite needs no
	// conversion - its INTEGER-affinity column keeps any decimal too large
	// for an integer as text - and SQLite has no ALTER COLUMN at all, which
	// is what the dialect probe below separates.
	pg, err := postgres(ctx, db)
	if err != nil {
		return err
	}
	if pg {
		for _, q := range []string{
			`ALTER TABLE listings ALTER COLUMN listing_nonce DROP DEFAULT`,
			`ALTER TABLE listings ALTER COLUMN listing_nonce TYPE TEXT USING listing_nonce::text`,
			`ALTER TABLE listings ALTER COLUMN listing_nonce SET DEFAULT '0'`,
		} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("pgstore: migrate listings nonce to text: %w", err)
			}
		}
	}
	// The swap-leg ALTERs run outside the transaction too, for the same reason
	// as the usage ALTERs above: a tolerated duplicate would poison the commit.
	// Pre-existing rows read back as empty markers - a purchase recorded before
	// the swap leg existed stays a bare direct deposit.
	for _, q := range []string{
		`ALTER TABLE funding ADD COLUMN swap_token_in TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE funding ADD COLUMN swap_amount_in TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE funding ADD COLUMN swap_native BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "already exists") && !strings.Contains(msg, "duplicate column name") {
				return fmt.Errorf("pgstore: migrate: %w", err)
			}
		}
	}
	return nil
}

// postgres reports whether db is a live Postgres server. The probe is a
// Postgres-only function: the SQLite stand-in the offline suite migrates
// answers "no such function", which is not an error worth failing on, while
// any other probe failure is a real one - silently skipping a dialect-gated
// migration would let it appear to succeed.
func postgres(ctx context.Context, db *sql.DB) (bool, error) {
	var serverVersion string
	err := db.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&serverVersion)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "no such function") {
		return false, nil
	}
	return false, fmt.Errorf("pgstore: detect dialect: %w", err)
}
