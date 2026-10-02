// Package market is the credit market's persistence surface: the listing
// row, its state moves (open, cancelled, sold) and the lock sums the meter
// ledger derives from it. The SQLite driver is SQLite; the Postgres driver
// lives in internal/pgstore, which implements the same statements.
package market

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	_ "modernc.org/sqlite"

	"slotfare/internal/schema"
)

// Listing is one credit-market listing observed on-chain: the console
// mirrors it so a customer's bought credits have a record, and the feeder
// sums the open rows into the meter ledger's lock.
type Listing struct {
	ListingID uint64
	// Nonce is the listing's on-chain nonce. The program's listing account
	// is the PDA [b"listing", seller, nonce], so without it a row cannot be
	// mapped back to the account its escrow lives in. Zero on rows mirrored
	// before the on-chain market existed.
	Nonce    uint64
	Account  string // seller account (bytes32 hex)
	Seller   string // seller wallet address that listed (base58)
	Credits  uint64
	PayToken string
	// PayTokenSymbol is the stablecoin symbol recorded by the feeder; empty on
	// pre-existing rows, where the view falls back to the short address.
	PayTokenSymbol string
	// PayTokenDecimals is the token's on-chain decimals recorded by the
	// feeder; 0 means unknown, where the client falls back to its configured
	// token table.
	PayTokenDecimals uint64
	Price            uint64
	MaturesAt        uint64
	State            string // open | cancelled | sold
	ListedTx         string
	ListedBlock      uint64
	ListedAt         string
	CancelledTx      string
	CancelledBlock   uint64
	CancelledAt      string
	SoldTx           string
	SoldBlock        uint64
	SoldAt           string
	BuyerAccount     string // set on a sale; the buyer's only record of the trade
}

// Reader is what the console reads for the market routes: every open
// listing and both sides of one account's trades.
type Reader interface {
	OpenListings(ctx context.Context, limit int, before int64) ([]Listing, error)
	AccountListings(ctx context.Context, accountID string, limit int, before int64) ([]Listing, error)
}

// WriteStore is the write side of the market: the listing row load, the
// insert, and the two guarded state moves, plus the lock recompute the
// moves feed. The read routes keep using Reader so a browse-only
// deployment (and the read tests) never see the write surface.
type WriteStore interface {
	Reader
	RecordListing(ctx context.Context, l Listing) (uint64, error)
	ListingByID(ctx context.Context, id uint64) (Listing, error)
	CancelListing(ctx context.Context, id uint64, tx string, block uint64, at string) (bool, error)
	SoldListing(ctx context.Context, id uint64, buyer, tx string, block uint64, at string) (bool, error)
	LockedFor(ctx context.Context, accountID string) (uint64, error)
	OpenLocks(ctx context.Context) (map[string]uint64, error)
}

// Store is the full listing surface: WriteStore plus the reads the feeder
// and the startup repairs take. On Postgres one pgstore.FundingStore
// satisfies this and funding.Store alike, which is why the two ports can be
// backed by a single object there; on SQLite each package opens its own
// pool on the shared file.
type Store interface {
	WriteStore
	AccountForListing(ctx context.Context, id uint64) (string, error)
	MangledListings(ctx context.Context) ([]MangledListing, error)
	RepairListing(ctx context.Context, id uint64, seller, payToken, payTokenSymbol string, payTokenDecimals uint64) error
	UnknownTokenListings(ctx context.Context) ([]UnknownTokenListing, error)
	SetListingTokenMetadata(ctx context.Context, id uint64, symbol string, decimals uint64) error
	Close() error
}

// SQLite is the Store driver backed by the shared slotfared SQLite file.
// The table DDL it runs is internal/schema's: this package owns the
// statements that read and write the rows, not the columns.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// NewStore opens (creating if needed) the shared database at path, enables
// WAL and ensures the listings table exist. This is a second pool on the
// same file the funding, keys, meter and session stores open: SQLite in WAL
// mode serialises the writers across pools, and each store issues only its
// own statements.
func NewStore(path string) (*SQLite, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("market: open sqlite %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("market: enable WAL on %s: %w", path, err)
	}
	if err := schema.EnsureListings(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("market: ensure listings table in %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// SQLRecordListing inserts one observed listing in state 'open'; both
// drivers execute it. The SQLite driver uses sqlRecordListingIgnore; the
// Postgres driver appends ON CONFLICT (listing_id) DO NOTHING - keyed by
// listing_id, a rescan is a no-op either way.
const SQLRecordListing = `INSERT INTO listings (listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state, listed_tx, listed_block, listed_at, listing_nonce)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?)`

// sqlRecordListingIgnore is the SQLite form of SQLRecordListing.
const sqlRecordListingIgnore = `INSERT OR IGNORE INTO listings (listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state, listed_tx, listed_block, listed_at, listing_nonce)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?)`

// RecordListing persists one observed listing. It reports whether this was
// a fresh row so the caller knows when to fire the priority settle.
func (s *SQLite) RecordListing(ctx context.Context, l Listing) (uint64, error) {
	// A zero id must travel as NULL: INTEGER PRIMARY KEY auto-assigns only
	// for NULL, and an explicit 0 would be stored as rowid 0 - one shared
	// listing forever.
	// A listing id is the listing PDA's last 8 bytes: a full uint64, half
	// of which exceed int64max. The column is signed, so the id travels as
	// its wrapped int64 - the bit pattern round-trips exactly and the driver
	// never sees a uint64 it cannot encode. The zero id still travels as
	// NULL so INTEGER PRIMARY KEY auto-assigns.
	idArg := any(int64(l.ListingID))
	if l.ListingID == 0 {
		idArg = nil
	}
	res, err := s.db.ExecContext(ctx, sqlRecordListingIgnore,
		idArg, l.Account, l.Seller, l.Credits, l.PayToken, l.PayTokenSymbol, l.PayTokenDecimals, l.Price, l.MaturesAt, l.ListedTx, l.ListedBlock, l.ListedAt, strconv.FormatUint(l.Nonce, 10))
	if err != nil {
		return 0, fmt.Errorf("market: record listing %d for account %s: %w", l.ListingID, l.Account, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	// The console's own listings arrive with a zero id and take the
	// auto-generated primary key; a feeder-era row with an explicit id
	// returns that id when fresh, 0 when it already existed.
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("market: listing id for account %s: %w", l.Account, err)
	}
	return uint64(id), nil
}

// SQLListingByID reads one listing row; both drivers execute it.
const SQLListingByID = `SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
	listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
	sold_tx, sold_block, sold_at, buyer_account, listing_nonce
	FROM listings WHERE listing_id = ?`

// ListingByID reads one listing; the zero Listing (ListedID 0) means the
// row does not exist, matching AccountForListing's empty-string shape.
func (s *SQLite) ListingByID(ctx context.Context, id uint64) (Listing, error) {
	var l Listing
	var idCol, listedBlock, cancelledBlock, soldBlock int64
	// The nonce is a full 64-bit draw stored as a decimal string (the
	// INTEGER column keeps it as TEXT whenever it exceeds int64max), so it
	// is read as text and parsed rather than scanned into the uint64. The
	// id column is signed for the same reason: the PDA-derived id wraps
	// through int64 on the way in and unwraps here.
	var nonceText string
	err := s.db.QueryRowContext(ctx, SQLListingByID, int64(id)).Scan(
		&idCol, &l.Account, &l.Seller, &l.Credits, &l.PayToken, &l.PayTokenSymbol, &l.PayTokenDecimals, &l.Price, &l.MaturesAt, &l.State,
		&l.ListedTx, &listedBlock, &l.ListedAt, &l.CancelledTx, &cancelledBlock, &l.CancelledAt,
		&l.SoldTx, &soldBlock, &l.SoldAt, &l.BuyerAccount, &nonceText)
	if err == sql.ErrNoRows {
		return Listing{}, nil
	}
	if err != nil {
		return Listing{}, fmt.Errorf("market: read listing %d: %w", id, err)
	}
	nonce, err := strconv.ParseUint(nonceText, 10, 64)
	if err != nil {
		return Listing{}, fmt.Errorf("market: parse listing %d nonce %q: %w", id, nonceText, err)
	}
	l.ListingID = uint64(idCol)
	l.Nonce = nonce
	l.ListedBlock = uint64(listedBlock)
	l.CancelledBlock = uint64(cancelledBlock)
	l.SoldBlock = uint64(soldBlock)
	return l, nil
}

// AccountForListing returns the account that owns the listing; empty when
// the row is not recorded yet (a cancel or sale seen before its list).
// SQLAccountForListing reads the owner of one listing; both drivers
// execute it.
const SQLAccountForListing = `SELECT account FROM listings WHERE listing_id = ?`

func (s *SQLite) AccountForListing(ctx context.Context, id uint64) (string, error) {
	var account string
	err := s.db.QueryRowContext(ctx, SQLAccountForListing, int64(id)).Scan(&account)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("market: read listing %d: %w", id, err)
	}
	return account, nil
}

// CancelListing moves an open listing to cancelled with the event's
// transaction, block and timestamp. A cancel of an already-moved listing is
// a no-op.
// SQLCancelListing moves an open listing to cancelled; both drivers
// execute it.
const SQLCancelListing = `UPDATE listings SET state = 'cancelled', cancelled_tx = ?, cancelled_block = ?, cancelled_at = ?
	 WHERE listing_id = ? AND state = 'open'`

func (s *SQLite) CancelListing(ctx context.Context, id uint64, tx string, block uint64, at string) (bool, error) {
	res, err := s.db.ExecContext(ctx, SQLCancelListing, tx, block, at, int64(id))
	if err != nil {
		return false, fmt.Errorf("market: cancel listing %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SoldListing moves an open listing to sold, recording the buyer's account
// from the CreditsSold event and the sale's transaction, block and
// timestamp. A sale of an already-moved listing is a no-op.
// SQLSoldListing moves an open listing to sold; both drivers execute it.
const SQLSoldListing = `UPDATE listings SET state = 'sold', buyer_account = ?, sold_tx = ?, sold_block = ?, sold_at = ?
	 WHERE listing_id = ? AND state = 'open'`

func (s *SQLite) SoldListing(ctx context.Context, id uint64, buyer, tx string, block uint64, at string) (bool, error) {
	res, err := s.db.ExecContext(ctx, SQLSoldListing, buyer, tx, block, at, int64(id))
	if err != nil {
		return false, fmt.Errorf("market: mark listing %d sold: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MangledListing is one listing row whose stored address carries the
// leading-zero signature of the old decode bug.
type MangledListing struct {
	ListingID uint64
	Account   string
}

// MangledListings returns every row mirrored while the address decode read
// the leading 20 bytes of a word: a full address hex that starts with twelve
// zero bytes. The startup repair re-derives these from the chain; a clean
// address never matches.
func (s *SQLite) MangledListings(ctx context.Context) ([]MangledListing, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT listing_id, account FROM listings
		 WHERE seller LIKE '0x000000000000000000000000%' OR pay_token LIKE '0x000000000000000000000000%'
		 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("market: list mangled listings: %w", err)
	}
	defer rows.Close()
	out := []MangledListing{}
	for rows.Next() {
		var m MangledListing
		var id int64
		if err := rows.Scan(&id, &m.Account); err != nil {
			return nil, fmt.Errorf("market: scan mangled listing: %w", err)
		}
		m.ListingID = uint64(id)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: list mangled listings: %w", err)
	}
	return out, nil
}

// RepairListing overwrites one listing's seller, pay token and their resolved
// metadata with values re-derived from the chain. It touches only those
// columns: identity (listing id, account), state and the moves stay as they
// were.
// SQLRepairListing re-derives one listing's address columns; both drivers
// execute it.
const SQLRepairListing = `UPDATE listings SET seller = ?, pay_token = ?, pay_token_symbol = ?, pay_token_decimals = ?
	 WHERE listing_id = ?`

func (s *SQLite) RepairListing(ctx context.Context, id uint64, seller, payToken, payTokenSymbol string, payTokenDecimals uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLRepairListing, seller, payToken, payTokenSymbol, payTokenDecimals, int64(id)); err != nil {
		return fmt.Errorf("market: repair listing %d: %w", id, err)
	}
	return nil
}

// UnknownTokenListing is one listing row whose pay token address is present
// but whose recorded metadata is still unknown: an empty or 0x-prefixed
// symbol, or zero decimals.
type UnknownTokenListing struct {
	ListingID uint64
	PayToken  string
}

// UnknownTokenListings returns every row that carries a pay token address
// but no resolved metadata yet: an empty symbol (pre-existing rows), a
// 0x-prefixed string (the bug era's short-address fallback) or zero
// decimals. The startup repair re-resolves these against the row's stored
// address; a row with a real symbol and nonzero decimals never matches.
func (s *SQLite) UnknownTokenListings(ctx context.Context) ([]UnknownTokenListing, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT listing_id, pay_token FROM listings
		 WHERE pay_token != ''
		   AND (pay_token_symbol = '' OR pay_token_symbol LIKE '0x%' OR pay_token_decimals = 0)
		 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("market: list unknown-token listings: %w", err)
	}
	defer rows.Close()
	out := []UnknownTokenListing{}
	for rows.Next() {
		var u UnknownTokenListing
		var id int64
		if err := rows.Scan(&id, &u.PayToken); err != nil {
			return nil, fmt.Errorf("market: scan unknown-token listing: %w", err)
		}
		u.ListingID = uint64(id)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: list unknown-token listings: %w", err)
	}
	return out, nil
}

// SetListingTokenMetadata writes one listing's resolved pay token symbol and
// decimals. It touches only those two columns - unlike RepairListing it does
// not overwrite seller or pay_token, because this also serves rows that were
// never mangled and whose address is already correct.
// SQLSetListingTokenMetadata writes one listing's resolved token metadata;
// both drivers execute it.
const SQLSetListingTokenMetadata = `UPDATE listings SET pay_token_symbol = ?, pay_token_decimals = ?
	 WHERE listing_id = ?`

func (s *SQLite) SetListingTokenMetadata(ctx context.Context, id uint64, symbol string, decimals uint64) error {
	if _, err := s.db.ExecContext(ctx, SQLSetListingTokenMetadata, symbol, decimals, int64(id)); err != nil {
		return fmt.Errorf("market: set token metadata for listing %d: %w", id, err)
	}
	return nil
}

// OpenListings returns every open listing, newest first, capped at limit: the
// market the console's /market route serves to the browser.
func (s *SQLite) OpenListings(ctx context.Context, limit int, before int64) ([]Listing, error) {
	q := `SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
		        listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
		        sold_tx, sold_block, sold_at, buyer_account, listing_nonce
		 FROM listings WHERE state = 'open'`
	args := []any{}
	if before != 0 {
		q += ` AND listing_id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("market: list open listings: %w", err)
	}
	defer rows.Close()
	out := []Listing{}
	for rows.Next() {
		var l Listing
		var idCol, block, ccBlock, soldBlock int64
		var nonceText string
		if err := rows.Scan(&idCol, &l.Account, &l.Seller, &l.Credits, &l.PayToken, &l.PayTokenSymbol, &l.PayTokenDecimals, &l.Price, &l.MaturesAt, &l.State,
			&l.ListedTx, &block, &l.ListedAt, &l.CancelledTx, &ccBlock, &l.CancelledAt,
			&l.SoldTx, &soldBlock, &l.SoldAt, &l.BuyerAccount, &nonceText); err != nil {
			return nil, fmt.Errorf("market: scan open listing: %w", err)
		}
		nonce, err := strconv.ParseUint(nonceText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("market: parse open listing nonce %q: %w", nonceText, err)
		}
		l.ListingID = uint64(idCol)
		l.Nonce = nonce
		l.ListedBlock = uint64(block)
		l.CancelledBlock = uint64(ccBlock)
		l.SoldBlock = uint64(soldBlock)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: list open listings: %w", err)
	}
	return out, nil
}

// AccountListings returns every listing the account is a party to - as the
// seller or, through buyer_account, as the buyer - in every state, newest
// first, capped at limit: the two sides of the account's trades for the
// /listings route.
func (s *SQLite) AccountListings(ctx context.Context, accountID string, limit int, before int64) ([]Listing, error) {
	q := `SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
		        listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
		        sold_tx, sold_block, sold_at, buyer_account, listing_nonce
		 FROM listings WHERE (account = ? OR buyer_account = ?)`
	args := []any{accountID, accountID}
	if before != 0 {
		q += ` AND listing_id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("market: list listings for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []Listing{}
	for rows.Next() {
		var l Listing
		var idCol, block, ccBlock, soldBlock int64
		var nonceText string
		if err := rows.Scan(&idCol, &l.Account, &l.Seller, &l.Credits, &l.PayToken, &l.PayTokenSymbol, &l.PayTokenDecimals, &l.Price, &l.MaturesAt, &l.State,
			&l.ListedTx, &block, &l.ListedAt, &l.CancelledTx, &ccBlock, &l.CancelledAt,
			&l.SoldTx, &soldBlock, &l.SoldAt, &l.BuyerAccount, &nonceText); err != nil {
			return nil, fmt.Errorf("market: scan listing row: %w", err)
		}
		nonce, err := strconv.ParseUint(nonceText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("market: parse listing nonce %q: %w", nonceText, err)
		}
		l.ListingID = uint64(idCol)
		l.Nonce = nonce
		l.ListedBlock = uint64(block)
		l.CancelledBlock = uint64(ccBlock)
		l.SoldBlock = uint64(soldBlock)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: list listings for account %s: %w", accountID, err)
	}
	return out, nil
}

// LockedFor returns the sum of the account's open listings' credits: the
// figure the meter ledger must hold locked for it.
// SQLLockedFor sums one account's open-listing credits; both drivers
// execute it.
const SQLLockedFor = `SELECT COALESCE(SUM(credits), 0) FROM listings WHERE account = ? AND state = 'open'`

func (s *SQLite) LockedFor(ctx context.Context, accountID string) (uint64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, SQLLockedFor, accountID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("market: sum open listings for account %s: %w", accountID, err)
	}
	return uint64(total), nil
}

// OpenLocks returns every account's open-listing sum, for the startup
// rebuild of the meter ledger's locks. Accounts whose rows are all closed
// come back with a zero sum so the rebuild re-zeroes a lock the routes
// never got to clear.
// SQLOpenLocks sums every account's open-listing credits (zero for an
// account with no open row); both drivers execute it.
const SQLOpenLocks = `SELECT account, SUM(CASE WHEN state = 'open' THEN credits ELSE 0 END) FROM listings GROUP BY account`

func (s *SQLite) OpenLocks(ctx context.Context) (map[string]uint64, error) {
	rows, err := s.db.QueryContext(ctx, SQLOpenLocks)
	if err != nil {
		return nil, fmt.Errorf("market: list open locks: %w", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var account string
		var total int64
		if err := rows.Scan(&account, &total); err != nil {
			return nil, fmt.Errorf("market: scan open lock: %w", err)
		}
		out[account] = uint64(total)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("market: list open locks: %w", err)
	}
	return out, nil
}
