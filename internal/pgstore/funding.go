package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"slotfare/internal/funding"
	"slotfare/internal/market"
	"strconv"
)

// FundingStore implements funding.Store on Postgres. Dialect-neutral
// statements use the shared constants from internal/console; the two INSERT
// OR IGNORE statements become ON CONFLICT ... DO NOTHING with identical
// RowsAffected semantics, and the seven ORDER BY rowid statements order by
// the serial id column instead - the only intentional divergence, since
// rowid has no Postgres equivalent.
type FundingStore struct {
	db *sql.DB
}

// pgSQLRecordPurchase is SQLRecordPurchase with the duplicate-key no-op.
const pgSQLRecordPurchase = funding.SQLRecordPurchase + ` ON CONFLICT (account_id, chain_key, deposit_id) DO NOTHING`

// pgSQLRecordListing is SQLRecordListing with the duplicate-key no-op.
const pgSQLRecordListing = market.SQLRecordListing + ` ON CONFLICT (listing_id) DO NOTHING`

// Record persists one observed purchase. Repeated observations of the same
// purchase are no-ops, so a rescanner cannot double-count it. The key is
// (account_id, chain_key, deposit_id).
func (s *FundingStore) Record(ctx context.Context, p funding.Purchase) error {
	tokenIn, amountIn, native := funding.SwapFields(p)
	if _, err := s.db.ExecContext(ctx, PG(pgSQLRecordPurchase),
		p.AccountID, p.ChainKey, p.DepositID, p.SourceTx, p.SourceBlock, p.AttestedHeight, boolToInt(p.Credited),
		p.Token, p.UsdMicros, p.StartedAt, p.CCTx, p.CCBlock, p.Credits, p.CreditedAt,
		tokenIn, amountIn, boolToInt(native)); err != nil {
		return fmt.Errorf("pgstore: record purchase chain %d deposit %s for account %s: %w", p.ChainKey, p.DepositID, p.AccountID, err)
	}
	return nil
}

// OpenPurchases returns the account's purchases that have not been credited
// yet, oldest first.
func (s *FundingStore) OpenPurchases(ctx context.Context, accountID string) ([]funding.Purchase, error) {
	rows, err := s.db.QueryContext(ctx,
		PG(`SELECT source_tx, source_block, attested_height, chain_key, deposit_id FROM funding
		 WHERE account_id = ? AND credited = 0 ORDER BY id`),
		accountID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list purchases for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []funding.Purchase{}
	for rows.Next() {
		var p funding.Purchase
		var chainKey, block, height int64
		if err := rows.Scan(&p.SourceTx, &block, &height, &chainKey, &p.DepositID); err != nil {
			return nil, fmt.Errorf("pgstore: scan purchase row: %w", err)
		}
		p.AccountID = accountID
		p.ChainKey = uint64(chainKey)
		p.SourceBlock = uint64(block)
		p.AttestedHeight = uint64(height)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list purchases for account %s: %w", accountID, err)
	}
	return out, nil
}

// AllPurchases returns every purchase of the account, newest first: the
// open ones the trace follows and the credited ones that are its history.
func (s *FundingStore) AllPurchases(ctx context.Context, accountID string) ([]funding.Purchase, error) {
	rows, err := s.db.QueryContext(ctx,
		PG(`SELECT chain_key, source_tx, source_block, attested_height, credited,
		        deposit_id, token, usd_micros, started_at, cc_tx, cc_block, credits, credited_at,
		        swap_token_in, swap_amount_in, swap_native
		 FROM funding WHERE account_id = ? ORDER BY id DESC`),
		accountID)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list purchases for account %s: %w", accountID, err)
	}
	defer rows.Close()
	out := []funding.Purchase{}
	for rows.Next() {
		var p funding.Purchase
		var chainKey, block, height, credited, ccBlock, credits, usd int64
		var swapTokenIn, swapAmountIn string
		var swapNative int64
		if err := rows.Scan(&chainKey, &p.SourceTx, &block, &height, &credited,
			&p.DepositID, &p.Token, &usd, &p.StartedAt, &p.CCTx, &ccBlock, &credits, &p.CreditedAt,
			&swapTokenIn, &swapAmountIn, &swapNative); err != nil {
			return nil, fmt.Errorf("pgstore: scan purchase row: %w", err)
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
			p.Swap = &funding.SwapLeg{TokenIn: swapTokenIn, AmountIn: swapAmountIn, Native: swapNative != 0}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list purchases for account %s: %w", accountID, err)
	}
	return out, nil
}

// OpenAccounts returns the accounts that have at least one uncredited
// purchase: the only accounts whose mints the ledger scan needs to see.
func (s *FundingStore) OpenAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, PG(funding.SQLOpenAccounts))
	if err != nil {
		return nil, fmt.Errorf("pgstore: list open accounts: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pgstore: scan open account: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// StampAttested records tip as the attested height of every open purchase on
// chainKey whose source block the tip has passed and that has no height yet.
// The chain key keeps one chain's tip from stamping another chain's rows.
func (s *FundingStore) StampAttested(ctx context.Context, chainKey, tip uint64) error {
	if _, err := s.db.ExecContext(ctx, PG(funding.SQLStampAttested), tip, tip, chainKey); err != nil {
		return fmt.Errorf("pgstore: stamp attested height %d on chain %d: %w", tip, chainKey, err)
	}
	return nil
}

// MarkCreditedByDeposit closes the purchase the ledger's CreditsMinted event
// names by account, chain key and deposit id, recording the Solana side
// of each deposit: the treasury transfer signature and slot.
// A mint for a purchase the source scan has not recorded yet is a no-op; the
// next pass records the purchase first and the ledger cursor is not advanced
// past an unmatched mint, so it is retried.
func (s *FundingStore) MarkCreditedByDeposit(ctx context.Context, accountID string, chainKey uint64, depositID, ccTx string, ccBlock, credits uint64, creditedAt string) (bool, error) {
	res, err := s.db.ExecContext(ctx, PG(funding.SQLMarkCreditedByDeposit),
		ccTx, ccBlock, credits, creditedAt, accountID, chainKey, depositID)
	if err != nil {
		return false, fmt.Errorf("pgstore: mark deposit %s (chain %d) credited for account %s: %w", depositID, chainKey, accountID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// IncompleteDirectPurchases returns every credited chain-0 row with an empty
// price and a known mint transaction, oldest first: the set the startup
// repair fills. A row with a price - paid or derived - never matches, so a
// completed repair stays completed.
func (s *FundingStore) IncompleteDirectPurchases(ctx context.Context) ([]funding.IncompleteDirectPurchase, error) {
	rows, err := s.db.QueryContext(ctx,
		PG(`SELECT account_id, chain_key, deposit_id, cc_tx, cc_block, credits
		 FROM funding WHERE chain_key = 0 AND credited = 1 AND usd_micros = 0 AND cc_tx != ''
		 ORDER BY id`))
	if err != nil {
		return nil, fmt.Errorf("pgstore: list incomplete direct purchases: %w", err)
	}
	defer rows.Close()
	out := []funding.IncompleteDirectPurchase{}
	for rows.Next() {
		var r funding.IncompleteDirectPurchase
		var chainKey, ccBlock, credits int64
		if err := rows.Scan(&r.AccountID, &chainKey, &r.DepositID, &r.CCTx, &ccBlock, &credits); err != nil {
			return nil, fmt.Errorf("pgstore: scan incomplete direct purchase: %w", err)
		}
		r.ChainKey = uint64(chainKey)
		r.CCBlock = uint64(ccBlock)
		r.Credits = uint64(credits)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list incomplete direct purchases: %w", err)
	}
	return out, nil
}

// RepairDirectPurchase writes one direct row's token and price when the row
// still has no price. The usd_micros = 0 in the WHERE keeps complete rows
// untouched and makes a repeated repair a no-op; identity is never changed.
func (s *FundingStore) RepairDirectPurchase(ctx context.Context, accountID string, chainKey uint64, depositID, token string, usdMicros uint64) error {
	if _, err := s.db.ExecContext(ctx, PG(funding.SQLRepairDirectPurchase),
		token, usdMicros, accountID, chainKey, depositID); err != nil {
		return fmt.Errorf("pgstore: repair direct deposit %s (chain %d) for account %s: %w", depositID, chainKey, accountID, err)
	}
	return nil
}

// RecordListing persists one observed listing. A rescan is a no-op; it
// reports whether this was a fresh row so the caller knows when to fire the
// priority settle.
func (s *FundingStore) RecordListing(ctx context.Context, l market.Listing) (uint64, error) {
	// The table's own id sequence hands out ids for console-created
	// listings (listing_id is a plain primary key with no default); a
	// feeder-era row arrives with an explicit id and skips the draw.
	// An explicit id is a PDA-derived full uint64 - half exceed int64max -
	// so it travels as its wrapped int64: the bit pattern round-trips and
	// the driver can encode it into the signed column.
	id := int64(l.ListingID)
	if l.ListingID == 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT nextval(pg_get_serial_sequence('listings', 'id'))`).Scan(&id); err != nil {
			return 0, fmt.Errorf("pgstore: listing id for account %s: %w", l.Account, err)
		}
	}
	res, err := s.db.ExecContext(ctx, PG(pgSQLRecordListing),
		id, l.Account, l.Seller, l.Credits, l.PayToken, l.PayTokenSymbol, l.PayTokenDecimals, l.Price, l.MaturesAt, l.ListedTx, l.ListedBlock, l.ListedAt, strconv.FormatUint(l.Nonce, 10))
	if err != nil {
		return 0, fmt.Errorf("pgstore: record listing %d for account %s: %w", id, l.Account, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		// Lost the race on the explicit id: the row already exists.
		return 0, err
	}
	return uint64(id), nil
}

// ListingByID reads one listing; the zero Listing (ListedID 0) means the
// row does not exist, matching AccountForListing's empty-string shape.
func (s *FundingStore) ListingByID(ctx context.Context, id uint64) (market.Listing, error) {
	rows, err := s.db.QueryContext(ctx, PG(market.SQLListingByID), int64(id))
	if err != nil {
		return market.Listing{}, fmt.Errorf("pgstore: read listing %d: %w", id, err)
	}
	defer rows.Close()
	list, err := scanListings(rows)
	if err != nil {
		return market.Listing{}, fmt.Errorf("pgstore: read listing %d: %w", id, err)
	}
	if len(list) == 0 {
		return market.Listing{}, nil
	}
	return list[0], nil
}

// AccountForListing returns the account that owns the listing; empty when
// the row is not recorded yet (a cancel or sale seen before its list).
func (s *FundingStore) AccountForListing(ctx context.Context, id uint64) (string, error) {
	var account string
	err := s.db.QueryRowContext(ctx, PG(market.SQLAccountForListing), int64(id)).Scan(&account)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pgstore: read listing %d: %w", id, err)
	}
	return account, nil
}

// CancelListing moves an open listing to cancelled with the event's
// transaction, block and timestamp. A cancel of an already-moved listing is
// a no-op.
func (s *FundingStore) CancelListing(ctx context.Context, id uint64, tx string, block uint64, at string) (bool, error) {
	res, err := s.db.ExecContext(ctx, PG(market.SQLCancelListing), tx, block, at, int64(id))
	if err != nil {
		return false, fmt.Errorf("pgstore: cancel listing %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SoldListing moves an open listing to sold, recording the buyer's account
// from the CreditsSold event and the sale's transaction, block and
// timestamp. A sale of an already-moved listing is a no-op.
func (s *FundingStore) SoldListing(ctx context.Context, id uint64, buyer, tx string, block uint64, at string) (bool, error) {
	res, err := s.db.ExecContext(ctx, PG(market.SQLSoldListing), buyer, tx, block, at, int64(id))
	if err != nil {
		return false, fmt.Errorf("pgstore: mark listing %d sold: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MangledListings returns every row mirrored while the address decode read
// the leading 20 bytes of a word: a full address hex that starts with twelve
// zero bytes. The startup repair re-derives these from the chain; a clean
// address never matches.
func (s *FundingStore) MangledListings(ctx context.Context) ([]market.MangledListing, error) {
	rows, err := s.db.QueryContext(ctx,
		PG(`SELECT listing_id, account FROM listings
		 WHERE seller LIKE '0x000000000000000000000000%' OR pay_token LIKE '0x000000000000000000000000%'
		 ORDER BY id`))
	if err != nil {
		return nil, fmt.Errorf("pgstore: list mangled listings: %w", err)
	}
	defer rows.Close()
	out := []market.MangledListing{}
	for rows.Next() {
		var m market.MangledListing
		var id int64
		if err := rows.Scan(&id, &m.Account); err != nil {
			return nil, fmt.Errorf("pgstore: scan mangled listing: %w", err)
		}
		m.ListingID = uint64(id)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list mangled listings: %w", err)
	}
	return out, nil
}

// RepairListing overwrites one listing's seller, pay token and their resolved
// metadata with values re-derived from the chain. It touches only those
// columns: identity (listing id, account), state and the moves stay as they
// were.
func (s *FundingStore) RepairListing(ctx context.Context, id uint64, seller, payToken, payTokenSymbol string, payTokenDecimals uint64) error {
	if _, err := s.db.ExecContext(ctx, PG(market.SQLRepairListing), seller, payToken, payTokenSymbol, payTokenDecimals, int64(id)); err != nil {
		return fmt.Errorf("pgstore: repair listing %d: %w", id, err)
	}
	return nil
}

// UnknownTokenListings returns every row that carries a pay token address
// but no resolved metadata yet: an empty symbol (pre-existing rows), a
// 0x-prefixed string (the bug era's short-address fallback) or zero
// decimals. The startup repair re-resolves these against the row's stored
// address; a row with a real symbol and nonzero decimals never matches.
func (s *FundingStore) UnknownTokenListings(ctx context.Context) ([]market.UnknownTokenListing, error) {
	rows, err := s.db.QueryContext(ctx,
		PG(`SELECT listing_id, pay_token FROM listings
		 WHERE pay_token != ''
		   AND (pay_token_symbol = '' OR pay_token_symbol LIKE '0x%' OR pay_token_decimals = 0)
		 ORDER BY id`))
	if err != nil {
		return nil, fmt.Errorf("pgstore: list unknown-token listings: %w", err)
	}
	defer rows.Close()
	out := []market.UnknownTokenListing{}
	for rows.Next() {
		var u market.UnknownTokenListing
		var id int64
		if err := rows.Scan(&id, &u.PayToken); err != nil {
			return nil, fmt.Errorf("pgstore: scan unknown-token listing: %w", err)
		}
		u.ListingID = uint64(id)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list unknown-token listings: %w", err)
	}
	return out, nil
}

// SetListingTokenMetadata writes one listing's resolved pay token symbol and
// decimals. It touches only those two columns - unlike RepairListing it does
// not overwrite seller or pay_token, because this also serves rows that were
// never mangled and whose address is already correct.
func (s *FundingStore) SetListingTokenMetadata(ctx context.Context, id uint64, symbol string, decimals uint64) error {
	if _, err := s.db.ExecContext(ctx, PG(market.SQLSetListingTokenMetadata), symbol, decimals, int64(id)); err != nil {
		return fmt.Errorf("pgstore: set token metadata for listing %d: %w", id, err)
	}
	return nil
}

// OpenListings returns every open listing, newest first, capped at limit: the
// market the console's /market route serves to the browser. before > 0 keeps
// only rows with listing_id < before so a caller can page back through the
// history; before == 0 leaves the statement byte-identical to today.
func (s *FundingStore) OpenListings(ctx context.Context, limit int, before int64) ([]market.Listing, error) {
	q := `SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
		        listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
		        sold_tx, sold_block, sold_at, buyer_account, listing_nonce
		 FROM listings WHERE state = 'open'`
	args := []any{}
	if before != 0 {
		q += ` AND listing_id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY listing_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, PG(q), args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list open listings: %w", err)
	}
	return scanListings(rows)
}

// AccountListings returns every listing the account is a party to - as the
// seller or, through buyer_account, as the buyer - in every state, newest
// first, capped at limit: the two sides of the account's trades for the
// /listings route. before > 0 keeps only rows with listing_id < before so a
// caller can page back through the history; before == 0 leaves the statement
// byte-identical to today.
func (s *FundingStore) AccountListings(ctx context.Context, accountID string, limit int, before int64) ([]market.Listing, error) {
	q := `SELECT listing_id, account, seller, credits, pay_token, pay_token_symbol, pay_token_decimals, price, matures_at, state,
		        listed_tx, listed_block, listed_at, cancelled_tx, cancelled_block, cancelled_at,
		        sold_tx, sold_block, sold_at, buyer_account, listing_nonce
		 FROM listings WHERE (account = ? OR buyer_account = ?)`
	args := []any{accountID, accountID}
	if before != 0 {
		q += ` AND listing_id < ?`
		args = append(args, before)
	}
	q += ` ORDER BY listing_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, PG(q), args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list listings for account %s: %w", accountID, err)
	}
	return scanListings(rows)
}

// LockedFor returns the sum of the account's open listings' credits: the
// figure the meter ledger must hold locked for it.
func (s *FundingStore) LockedFor(ctx context.Context, accountID string) (uint64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, PG(market.SQLLockedFor), accountID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("pgstore: sum open listings for account %s: %w", accountID, err)
	}
	return uint64(total), nil
}

// OpenLocks returns every account's open-listing sum, for the startup
// rebuild of the meter ledger's locks.
func (s *FundingStore) OpenLocks(ctx context.Context) (map[string]uint64, error) {
	rows, err := s.db.QueryContext(ctx, PG(market.SQLOpenLocks))
	if err != nil {
		return nil, fmt.Errorf("pgstore: list open locks: %w", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var account string
		var total int64
		if err := rows.Scan(&account, &total); err != nil {
			return nil, fmt.Errorf("pgstore: scan open lock: %w", err)
		}
		out[account] = uint64(total)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: list open locks: %w", err)
	}
	return out, nil
}

// Cursor returns the last block the named scan has covered; ok is false
// when the scan has never run.
func (s *FundingStore) Cursor(ctx context.Context, name string) (uint64, bool, error) {
	var block int64
	err := s.db.QueryRowContext(ctx, PG(funding.SQLCursor), name).Scan(&block)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("pgstore: read cursor %s: %w", name, err)
	}
	return uint64(block), true, nil
}

// SetCursor records the last block the named scan has covered.
func (s *FundingStore) SetCursor(ctx context.Context, name string, block uint64) error {
	if _, err := s.db.ExecContext(ctx, PG(funding.SQLSetCursor), name, block); err != nil {
		return fmt.Errorf("pgstore: write cursor %s: %w", name, err)
	}
	return nil
}

// MarkCredited flags a purchase as credited so /funding stops reporting it.
func (s *FundingStore) MarkCredited(ctx context.Context, accountID, sourceTx string) error {
	if _, err := s.db.ExecContext(ctx, PG(funding.SQLMarkCredited), accountID, sourceTx); err != nil {
		return fmt.Errorf("pgstore: mark purchase %s credited for account %s: %w", sourceTx, accountID, err)
	}
	return nil
}

// Close closes the underlying pool.
func (s *FundingStore) Close() error {
	return s.db.Close()
}

// scanListings drains a listings result set, keeping the query's order, and
// returns an empty slice (never nil) when no rows match.
func scanListings(rows *sql.Rows) ([]market.Listing, error) {
	defer rows.Close()
	out := []market.Listing{}
	for rows.Next() {
		var l market.Listing
		var idCol, block, ccBlock, soldBlock int64
		// The nonce column is TEXT - a full 64-bit draw, and half of them
		// exceed int64max - so it is read as the decimal it stores and
		// parsed, rather than scanned straight into the uint64. The id
		// column is signed for the same reason: the PDA-derived id wraps
		// through int64 on the way in and unwraps here.
		var nonceText string
		if err := rows.Scan(&idCol, &l.Account, &l.Seller, &l.Credits, &l.PayToken, &l.PayTokenSymbol, &l.PayTokenDecimals, &l.Price, &l.MaturesAt, &l.State,
			&l.ListedTx, &block, &l.ListedAt, &l.CancelledTx, &ccBlock, &l.CancelledAt,
			&l.SoldTx, &soldBlock, &l.SoldAt, &l.BuyerAccount, &nonceText); err != nil {
			return nil, fmt.Errorf("pgstore: scan listing row: %w", err)
		}
		nonce, err := strconv.ParseUint(nonceText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pgstore: parse listing nonce %q: %w", nonceText, err)
		}
		l.ListingID = uint64(idCol)
		l.Nonce = nonce
		l.ListedBlock = uint64(block)
		l.CancelledBlock = uint64(ccBlock)
		l.SoldBlock = uint64(soldBlock)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgstore: scan listing rows: %w", err)
	}
	return out, nil
}
