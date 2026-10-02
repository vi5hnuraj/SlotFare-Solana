// Package schema owns the SQLite DDL for the tables the console-side stores
// share: funding, funding_cursor and listings, plus the column helpers every
// CREATE TABLE IF NOT EXISTS migration needs (a table created before a column
// existed is left alone by that statement, so each column is added when
// missing). One home for the schema means the ALTERs that keep old files
// readable live next to the CREATEs that define them.
//
// Postgres DDL is separate and lives in internal/pgstore/migrate.go; the two
// are written to describe the same columns.
package schema

import (
	"database/sql"
	"fmt"
	"strings"
)

// listingsTableSQL creates the listings table when absent. The nonce
// column is TEXT: a listing nonce is a full 64-bit draw, and SQLite's
// INTEGER affinity would store any decimal too large for an integer as a
// REAL - rounding the very value the listing PDA is derived from.
const listingsTableSQL = `CREATE TABLE IF NOT EXISTS listings (
		listing_id      INTEGER PRIMARY KEY,
		account         TEXT    NOT NULL,
		seller          TEXT    NOT NULL DEFAULT '',
		credits         INTEGER NOT NULL DEFAULT 0,
		pay_token       TEXT    NOT NULL DEFAULT '',
		price           INTEGER NOT NULL DEFAULT 0,
		matures_at      INTEGER NOT NULL DEFAULT 0,
		state           TEXT    NOT NULL DEFAULT 'open',
		listed_tx       TEXT    NOT NULL DEFAULT '',
		listed_block    INTEGER NOT NULL DEFAULT 0,
		listed_at       TEXT    NOT NULL DEFAULT '',
		cancelled_tx    TEXT    NOT NULL DEFAULT '',
		cancelled_block INTEGER NOT NULL DEFAULT 0,
		cancelled_at    TEXT    NOT NULL DEFAULT '',
		sold_tx         TEXT    NOT NULL DEFAULT '',
		sold_block      INTEGER NOT NULL DEFAULT 0,
		sold_at         TEXT    NOT NULL DEFAULT '',
		buyer_account   TEXT    NOT NULL DEFAULT '',
		pay_token_symbol TEXT   NOT NULL DEFAULT '',
		pay_token_decimals INTEGER NOT NULL DEFAULT 0,
		listing_nonce   TEXT    NOT NULL DEFAULT '0'
	)`

// EnsureListings creates the listings table when absent.
func EnsureListings(db *sql.DB) error {
	if _, err := db.Exec(listingsTableSQL); err != nil {
		return err
	}
	// pay_token_symbol and pay_token_decimals were added after the first
	// schema; CREATE TABLE IF NOT EXISTS does not alter an existing file, so
	// add each when missing.
	if err := ensureColumn(db, "listings", "pay_token_symbol TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "listings", "pay_token_decimals INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// listing_nonce arrived with the on-chain market: a row mirrored before
	// it has no program listing behind it and reads back 0.
	return ensureNonceText(db)
}

// ensureNonceText adds the nonce column when missing and converts a column
// that shipped as INTEGER. SQLite has no ALTER COLUMN, so the conversion
// rebuilds the table; every other column already exists by then (the
// callers add them first) and the CAST turns the stored integer 0 into the
// '0' the reader parses.
func ensureNonceText(db *sql.DB) error {
	var ctype string
	rows, err := db.Query(`PRAGMA table_info(listings)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "listing_nonce" {
			found = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !found {
		_, err := db.Exec(`ALTER TABLE listings ADD COLUMN listing_nonce TEXT NOT NULL DEFAULT '0'`)
		return err
	}
	if strings.EqualFold(strings.TrimSpace(ctype), "TEXT") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE listings_nonce_text (
			listing_id      INTEGER PRIMARY KEY,
			account         TEXT    NOT NULL,
			seller          TEXT    NOT NULL DEFAULT '',
			credits         INTEGER NOT NULL DEFAULT 0,
			pay_token       TEXT    NOT NULL DEFAULT '',
			price           INTEGER NOT NULL DEFAULT 0,
			matures_at      INTEGER NOT NULL DEFAULT 0,
			state           TEXT    NOT NULL DEFAULT 'open',
			listed_tx       TEXT    NOT NULL DEFAULT '',
			listed_block    INTEGER NOT NULL DEFAULT 0,
			listed_at       TEXT    NOT NULL DEFAULT '',
			cancelled_tx    TEXT    NOT NULL DEFAULT '',
			cancelled_block INTEGER NOT NULL DEFAULT 0,
			cancelled_at    TEXT    NOT NULL DEFAULT '',
			sold_tx         TEXT    NOT NULL DEFAULT '',
			sold_block      INTEGER NOT NULL DEFAULT 0,
			sold_at         TEXT    NOT NULL DEFAULT '',
			buyer_account   TEXT    NOT NULL DEFAULT '',
			pay_token_symbol TEXT   NOT NULL DEFAULT '',
			pay_token_decimals INTEGER NOT NULL DEFAULT 0,
			listing_nonce   TEXT    NOT NULL DEFAULT '0'
		)`,
		`INSERT INTO listings_nonce_text (listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
			listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
			sold_tx, sold_block, sold_at, buyer_account, listing_nonce)
		 SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
			listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
			sold_tx, sold_block, sold_at, buyer_account, CAST(listing_nonce AS TEXT)
		 FROM listings`,
		`DROP TABLE listings`,
		`ALTER TABLE listings_nonce_text RENAME TO listings`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("schema: listings nonce column to text: %w", err)
		}
	}
	return tx.Commit()
}

// EnsureFunding creates or migrates the funding table. A table without
// the chain_key column (pre-EP-010) is recreated with the new
// (account_id, chain_key, deposit_id) primary key.
func EnsureFunding(db *sql.DB, sourceChainKey uint64) error {
	// Does the table exist?
	var exists int
	err := db.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='funding'`).Scan(&exists)
	if err == sql.ErrNoRows {
		return createFundingTable(db)
	}
	if err != nil {
		return fmt.Errorf("check funding table: %w", err)
	}
	// Does it already have chain_key?
	if hasColumn(db, "funding", "chain_key") {
		return nil // already migrated
	}
	// Migrate: recreate with the new PK.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE funding_new (
		account_id      TEXT    NOT NULL,
		chain_key       INTEGER NOT NULL,
		deposit_id      TEXT    NOT NULL,
		source_tx       TEXT    NOT NULL DEFAULT '',
		source_block    INTEGER NOT NULL DEFAULT 0,
		attested_height INTEGER NOT NULL DEFAULT 0,
		credited        INTEGER NOT NULL DEFAULT 0,
		token           TEXT    NOT NULL DEFAULT '',
		usd_micros      INTEGER NOT NULL DEFAULT 0,
		started_at      TEXT    NOT NULL DEFAULT '',
		cc_tx           TEXT    NOT NULL DEFAULT '',
		cc_block        INTEGER NOT NULL DEFAULT 0,
		credits         INTEGER NOT NULL DEFAULT 0,
		credited_at     TEXT    NOT NULL DEFAULT '',
		swap_token_in   TEXT    NOT NULL DEFAULT '',
		swap_amount_in  TEXT    NOT NULL DEFAULT '',
		swap_native      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, chain_key, deposit_id)
	)`); err != nil {
		return fmt.Errorf("create funding_new: %w", err)
	}
	// All existing rows are source-chain rows: stamp the source chain key.
	// When deposit_id is empty (pre-migration rows that predate that column),
	// fall back to source_tx so the new PK stays unique.
	if _, err := tx.Exec(
		`INSERT INTO funding_new (account_id, chain_key, deposit_id, source_tx, source_block, attested_height, credited, token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at)
		 SELECT account_id, ?, CASE WHEN deposit_id = '' THEN source_tx ELSE deposit_id END, source_tx, source_block, attested_height, credited, token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at
		 FROM funding`, sourceChainKey); err != nil {
		return fmt.Errorf("copy funding rows: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE funding`); err != nil {
		return fmt.Errorf("drop old funding: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE funding_new RENAME TO funding`); err != nil {
		return fmt.Errorf("rename funding_new: %w", err)
	}
	return tx.Commit()
}

func createFundingTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS funding (
		account_id      TEXT    NOT NULL,
		chain_key       INTEGER NOT NULL,
		deposit_id      TEXT    NOT NULL,
		source_tx       TEXT    NOT NULL DEFAULT '',
		source_block    INTEGER NOT NULL DEFAULT 0,
		attested_height INTEGER NOT NULL DEFAULT 0,
		credited        INTEGER NOT NULL DEFAULT 0,
		token           TEXT    NOT NULL DEFAULT '',
		usd_micros      INTEGER NOT NULL DEFAULT 0,
		started_at      TEXT    NOT NULL DEFAULT '',
		cc_tx           TEXT    NOT NULL DEFAULT '',
		cc_block        INTEGER NOT NULL DEFAULT 0,
		credits         INTEGER NOT NULL DEFAULT 0,
		credited_at     TEXT    NOT NULL DEFAULT '',
		swap_token_in   TEXT    NOT NULL DEFAULT '',
		swap_amount_in  TEXT    NOT NULL DEFAULT '',
		swap_native      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, chain_key, deposit_id)
	)`)
	if err != nil {
		return err
	}
	// Columns added after the first schema. CREATE TABLE IF NOT EXISTS does
	// not alter an existing file, so each is added when missing.
	for _, col := range []string{
		"deposit_id TEXT NOT NULL DEFAULT ''",
		"token TEXT NOT NULL DEFAULT ''",
		"usd_micros INTEGER NOT NULL DEFAULT 0",
		"started_at TEXT NOT NULL DEFAULT ''",
		"cc_tx TEXT NOT NULL DEFAULT ''",
		"cc_block INTEGER NOT NULL DEFAULT 0",
		"credits INTEGER NOT NULL DEFAULT 0",
		"credited_at TEXT NOT NULL DEFAULT ''",
		"swap_token_in TEXT NOT NULL DEFAULT ''",
		"swap_amount_in TEXT NOT NULL DEFAULT ''",
		"swap_native INTEGER NOT NULL DEFAULT 0",
	} {
		if err := ensureColumn(db, "funding", col); err != nil {
			return err
		}
	}
	return nil
}

// hasColumn reports whether table has a column of the given name.
func hasColumn(db *sql.DB, table, name string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if cname == name {
			return true
		}
	}
	return false
}

// ensureColumn adds col (a "name TYPE ..." definition) to table when a
// column of that name is absent.
func ensureColumn(db *sql.DB, table, col string) error {
	name := strings.Fields(col)[0]
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if cname == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col)
	return err
}

// EnsureFundingCursor creates the named-scan cursor table when absent.
// Cursors are (name, block) pairs; the upsert that writes them is
// funding.SQLSetCursor.
func EnsureFundingCursor(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS funding_cursor (
		name TEXT PRIMARY KEY,
		block INTEGER NOT NULL
	)`)
	return err
}
