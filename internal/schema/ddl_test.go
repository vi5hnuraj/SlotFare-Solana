package schema

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// openDB opens a private in-memory schema fixture, one per test. The pool is
// capped at one connection so every statement sees the same database.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// hasCol reports whether a table has a column, through the pragma the
// migrations themselves read.
func hasCol(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		if name == col {
			return true
		}
	}
	return false
}

// A fresh file gets the funding table with its (account, chain, deposit)
// primary key, and a second pass changes nothing.
func TestEnsureFundingCreatesAndIsIdempotent(t *testing.T) {
	db := openDB(t)
	if err := EnsureFunding(db, 1); err != nil {
		t.Fatalf("EnsureFunding: %v", err)
	}
	if !hasCol(t, db, "funding", "chain_key") {
		t.Fatal("funding.chain_key missing after EnsureFunding")
	}
	if _, err := db.Exec(`INSERT INTO funding (account_id, chain_key, deposit_id, source_tx)
		VALUES ('0xa', 1, '42', '0xdead')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := EnsureFunding(db, 1); err != nil {
		t.Fatalf("second EnsureFunding: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM funding`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1: a repeat pass must not rewrite the table", n)
	}
}

// A pre-chain_key table is recreated with the new primary key and its rows
// kept, stamped with the configured source chain key; an empty deposit id
// falls back to the source transaction so the key stays unique.
func TestEnsureFundingMigratesALegacyTable(t *testing.T) {
	db := openDB(t)
	legacy := `CREATE TABLE funding (
		account_id      TEXT    NOT NULL,
		deposit_id      TEXT    NOT NULL DEFAULT '',
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
		credited_at     TEXT    NOT NULL DEFAULT ''
	)`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	for _, row := range [][3]string{
		{"0xa", "42", "0xdead"},
		{"0xb", "", "0xbeef"}, // pre-deposit-id row: falls back to source_tx
	} {
		if _, err := db.Exec(`INSERT INTO funding (account_id, deposit_id, source_tx, credits) VALUES (?, ?, ?, 100)`,
			row[0], row[1], row[2]); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}
	if err := EnsureFunding(db, 7); err != nil {
		t.Fatalf("EnsureFunding migration: %v", err)
	}
	if !hasCol(t, db, "funding", "chain_key") {
		t.Fatal("chain_key missing after migration")
	}
	type row struct {
		account, deposit string
		chain            int64
	}
	var got []row
	rows, err := db.Query(`SELECT account_id, chain_key, deposit_id FROM funding ORDER BY account_id`)
	if err != nil {
		t.Fatalf("read migrated rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.account, &r.chain, &r.deposit); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("migrated %d rows, want 2", len(got))
	}
	if got[0].account != "0xa" || got[0].chain != 7 || got[0].deposit != "42" {
		t.Errorf("row = %+v, want 0xa / chain 7 / deposit 42", got[0])
	}
	if got[1].account != "0xb" || got[1].deposit != "0xbeef" {
		t.Errorf("row = %+v, want 0xb stamped with source_tx 0xbeef as its deposit id", got[1])
	}
}

// A listings table created before the metadata and nonce columns existed is
// brought forward: the columns appear, an INTEGER nonce becomes TEXT, and
// stored rows survive both passes.
func TestEnsureListingsAddsColumnsAndConvertsTheNonce(t *testing.T) {
	db := openDB(t)
	if _, err := db.Exec(`CREATE TABLE listings (
		listing_id INTEGER PRIMARY KEY,
		account    TEXT    NOT NULL,
		seller     TEXT    NOT NULL DEFAULT '',
		credits    INTEGER NOT NULL DEFAULT 0,
		pay_token  TEXT    NOT NULL DEFAULT '',
		price      INTEGER NOT NULL DEFAULT 0,
		matures_at INTEGER NOT NULL DEFAULT 0,
		state      TEXT    NOT NULL DEFAULT 'open',
		listed_tx  TEXT    NOT NULL DEFAULT '',
		listed_block INTEGER NOT NULL DEFAULT 0,
		listed_at  TEXT    NOT NULL DEFAULT '',
		cancelled_tx TEXT NOT NULL DEFAULT '',
		cancelled_block INTEGER NOT NULL DEFAULT 0,
		cancelled_at TEXT NOT NULL DEFAULT '',
		sold_tx    TEXT    NOT NULL DEFAULT '',
		sold_block INTEGER NOT NULL DEFAULT 0,
		sold_at    TEXT    NOT NULL DEFAULT '',
		buyer_account TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy listings: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO listings (listing_id, account, credits) VALUES (9, '0xa', 50)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureListings(db); err != nil {
		t.Fatalf("EnsureListings: %v", err)
	}
	for _, col := range []string{"pay_token_symbol", "pay_token_decimals", "listing_nonce"} {
		if !hasCol(t, db, "listings", col) {
			t.Errorf("listings.%s missing after EnsureListings", col)
		}
	}
	var nonceType, symbol string
	if err := db.QueryRow(`SELECT typeof(listing_nonce), pay_token_symbol FROM listings WHERE listing_id = 9`).
		Scan(&nonceType, &symbol); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if nonceType != "text" {
		t.Errorf("listing_nonce typeof = %q, want text", nonceType)
	}
	if symbol != "" {
		t.Errorf("pay_token_symbol = %q, want the empty default", symbol)
	}
	// Re-running is a no-op and the row is still there.
	if err := EnsureListings(db); err != nil {
		t.Fatalf("second EnsureListings: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM listings`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// The cursor table is created once and is safe to ask for again.
func TestEnsureFundingCursorIsIdempotent(t *testing.T) {
	db := openDB(t)
	if err := EnsureFundingCursor(db); err != nil {
		t.Fatalf("EnsureFundingCursor: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO funding_cursor (name, block) VALUES ('deposit', 10)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := EnsureFundingCursor(db); err != nil {
		t.Fatalf("second EnsureFundingCursor: %v", err)
	}
	var block int
	if err := db.QueryRow(`SELECT block FROM funding_cursor WHERE name = 'deposit'`).Scan(&block); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if block != 10 {
		t.Errorf("block = %d, want 10: a repeat pass must not clear the table", block)
	}
}
