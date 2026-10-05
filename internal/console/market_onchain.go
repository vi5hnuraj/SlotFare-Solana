package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/solana"
)

// This file is the daemon's half of the on-chain credit market as the
// console serves it. The program (programs/slotfare-ledger) owns the
// authority: a listing is a program-owned escrow account, a sale is one
// atomic transaction that pays the seller and releases the escrow, and
// only the seller may withdraw. The daemon never holds a user's key, so
// every market instruction is wallet-signed, and each write is therefore
// two round trips:
//
//	POST /listings/onchain/prepare  build the instruction, hand back an
//	                                unsigned transaction for the wallet
//	POST /listings/onchain/confirm  read the listing back from the program
//	                                and only then record the row
//
// The read-back is what makes the row trustworthy: a confirm that finds no
// listing, or one the caller did not cause, is rejected, so the console
// cannot be talked into recording a listing that never existed. The escrow
// is the lock and `Buy` is the verification - there is no second, local
// source of truth to drift.

// LedgerMarket is what the console needs from the deployed market program:
// the three wallet-signed instructions, the transaction preparation that
// hands one to a browser, the read-back that confirms it landed, and the
// slot lookup that dates the row. *solana.Client satisfies it; nil (no
// SLOTFARE_LEDGER_PROGRAM) leaves the on-chain routes answering 501 while
// the local-mirror routes keep working.
type LedgerMarket interface {
	LedgerListIx(seller gsolana.PublicKey, credits, price, nonce uint64, payMint gsolana.PublicKey) (gsolana.Instruction, error)
	LedgerBuyIx(buyer, seller gsolana.PublicKey, nonce uint64, payMint gsolana.PublicKey) ([]gsolana.Instruction, error)
	LedgerCancelListIx(seller gsolana.PublicKey, nonce uint64) (gsolana.Instruction, error)
	PrepareLedgerTx(ctx context.Context, payer gsolana.PublicKey, ixs []gsolana.Instruction) ([]byte, error)
	LedgerListingRead(ctx context.Context, seller gsolana.PublicKey, nonce uint64) (solana.LedgerListing, bool, error)
	LedgerTxSlot(ctx context.Context, signature gsolana.Signature) (uint64, error)
	// LedgerListingID is the store's listing_id for a program listing: the
	// last 8 bytes of the listing PDA, little-endian. confirmList records
	// this id so its row and the indexer's replay of the same on-chain
	// listing land on the same primary key instead of two rows (and two
	// locks) for one escrow.
	LedgerListingID(seller gsolana.PublicKey, nonce uint64) (uint64, error)
}

// The three market writes the console accepts, as they appear on the wire.
const (
	marketActionList   = "list"
	marketActionBuy    = "buy"
	marketActionCancel = "cancel"
)

// wireUint64 carries a uint64 on the wire as a decimal string. A JSON
// number is an IEEE-754 double - a browser's JSON.parse rounds any integer
// above 2^53 - and a listing nonce is a full 64-bit draw, so a
// number-encoded nonce would come back as a different value and derive a
// listing PDA that was never opened. Unmarshal accepts both forms, so an
// older client sending a number still decodes while the exact value stays
// exact.
type wireUint64 uint64

func (n wireUint64) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(n), 10))
}

func (n *wireUint64) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("nonce %q: %w", s, err)
		}
		*n = wireUint64(v)
		return nil
	}
	var v uint64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*n = wireUint64(v)
	return nil
}

// marketRequest is the body both on-chain routes take. prepare reads action
// plus the terms (or the listing id); confirm adds the wallet's signature
// and echoes the nonce prepare handed out - as a decimal string, per
// wireUint64, because the nonce is a full 64-bit draw. The listing id is a
// full 64-bit PDA-derived value too, so it rides as a string for the same
// reason: a JSON number would round it and the lookup would miss the row.
type marketRequest struct {
	Action    string     `json:"action"`
	ListingID wireUint64 `json:"listingId"`
	Credits   uint64     `json:"credits"`
	Price     uint64     `json:"price"`
	Nonce     wireUint64 `json:"nonce"`
	Signature string     `json:"signature"`
}

// preparedTxView is one prepare response: the nonce the transaction carries,
// and the standard Solana wire transaction (base64) for the wallet to sign.
// The user is the fee payer, so the wallet - not the daemon - pays and signs.
// The nonce is marshaled as a string for the same reason it is echoed as
// one: a 64-bit draw does not survive a JSON number.
type preparedTxView struct {
	Action string     `json:"action"`
	Nonce  wireUint64 `json:"nonce"`
	// Transaction is the base64 of the marshaled unsigned transaction; a
	// wallet's Transaction.from(bytes) reads it back with the same accounts
	// and instruction data.
	Transaction string `json:"transaction"`
}

// randomListingNonce draws a listing nonce. It is the caller's to choose and
// only has to be unique to the seller; 2^64 draws make a collision something
// the protocol does not have to reason about. Zero is remapped because a
// zero nonce is what an un-migrated row reads back as, and the two must not
// be confused.
func randomListingNonce() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	if n := binary.LittleEndian.Uint64(b[:]); n != 0 {
		return n, nil
	}
	return 1, nil
}

// sellerPubkeyFor recovers the base58 wallet behind a listing row: the
// recorded seller, or the account id when the row predates the seller
// column. Both are the same key - the id is the hex of the key's bytes.
func sellerPubkeyFor(l market.Listing) (gsolana.PublicKey, error) {
	if l.Seller != "" {
		return gsolana.PublicKeyFromBase58(l.Seller)
	}
	s, err := base58FromAccountID(l.Account)
	if err != nil {
		return gsolana.PublicKey{}, err
	}
	return gsolana.PublicKeyFromBase58(s)
}

// payMintFor is the mint a listing settles in: the row's recorded token, or
// the console's configured deposit mint when the row carries none.
func (h *Handler) payMintFor(l market.Listing) (gsolana.PublicKey, error) {
	addr := l.PayToken
	if addr == "" {
		addr = h.Deposit.Mint
	}
	return gsolana.PublicKeyFromBase58(addr)
}

// handlePrepareMarket builds one market instruction and returns it as an
// unsigned transaction for the caller's wallet. Nothing is written: the
// chain, not the prepare route, is what creates a listing, and a prepare
// that is never signed leaves no trace.
func (h *Handler) handlePrepareMarket(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Program == nil || h.Deposit == nil || h.MarketWrite == nil || h.Market == nil {
		writeError(w, http.StatusNotImplemented, "the on-chain market is not configured on this deployment")
		return
	}
	var in marketRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	caller, err := base58FromAccountID(accountID)
	if err != nil {
		// A session bound to a legacy 20-byte id has no Solana wallet to
		// sign from; nothing about that is a server fault.
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}
	payer, err := gsolana.PublicKeyFromBase58(caller)
	if err != nil {
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}

	ctx := r.Context()
	switch in.Action {
	case marketActionList:
		if in.Credits == 0 || in.Credits > listingCreditCap {
			writeError(w, http.StatusBadRequest, "credits out of range")
			return
		}
		if in.Price == 0 || in.Price > listingPriceCap {
			writeError(w, http.StatusBadRequest, "price out of range")
			return
		}
		mint, err := gsolana.PublicKeyFromBase58(h.Deposit.Mint)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		nonce, err := randomListingNonce()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ix, err := h.Program.LedgerListIx(payer, in.Credits, in.Price, nonce, mint)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.writePrepared(w, ctx, payer, marketActionList, nonce, []gsolana.Instruction{ix})

	case marketActionBuy:
		row, err := h.MarketWrite.ListingByID(ctx, uint64(in.ListingID))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if row.ListingID == 0 {
			writeError(w, http.StatusNotFound, "listing not found")
			return
		}
		if row.State != "open" {
			writeError(w, http.StatusConflict, "listing is not open")
			return
		}
		if row.Account == accountID {
			writeError(w, http.StatusConflict, "cannot buy your own listing")
			return
		}
		if h.now().Unix() < int64(row.MaturesAt) {
			writeError(w, http.StatusConflict, "listing has not matured yet")
			return
		}
		seller, err := sellerPubkeyFor(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		mint, err := h.payMintFor(row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		// The row must still have a live escrow behind it. A row whose
		// listing was already taken or withdrawn is a gone listing, not a
		// transaction the buyer should waste a signature on.
		l, found, err := h.Program.LedgerListingRead(ctx, seller, row.Nonce)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !found || !l.Open() {
			writeError(w, http.StatusConflict, "listing is not open")
			return
		}
		ixs, err := h.Program.LedgerBuyIx(payer, seller, row.Nonce, mint)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.writePrepared(w, ctx, payer, marketActionBuy, row.Nonce, ixs)

	case marketActionCancel:
		row, err := h.MarketWrite.ListingByID(ctx, uint64(in.ListingID))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if row.ListingID == 0 {
			writeError(w, http.StatusNotFound, "listing not found")
			return
		}
		if row.Account != accountID {
			writeError(w, http.StatusForbidden, "not your listing")
			return
		}
		if row.State != "open" {
			writeError(w, http.StatusConflict, "listing is not open")
			return
		}
		ix, err := h.Program.LedgerCancelListIx(payer, row.Nonce)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.writePrepared(w, ctx, payer, marketActionCancel, row.Nonce, []gsolana.Instruction{ix})

	default:
		writeError(w, http.StatusBadRequest, "unknown action")
	}
}

// writePrepared serializes the instructions and answers. A cluster the
// daemon cannot reach is a 500: the wallet would have nothing usable to
// sign, so failing here is better than handing back a stale blockhash.
func (h *Handler) writePrepared(w http.ResponseWriter, ctx context.Context, payer gsolana.PublicKey, action string, nonce uint64, ixs []gsolana.Instruction) {
	raw, err := h.Program.PrepareLedgerTx(ctx, payer, ixs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, preparedTxView{
		Action:      action,
		Nonce:       wireUint64(nonce),
		Transaction: base64.StdEncoding.EncodeToString(raw),
	})
}

// handleConfirmMarket records what the program already did. It reads the
// listing account back before touching a row, so the console's record is a
// consequence of on-chain state rather than a claim the caller made.
func (h *Handler) handleConfirmMarket(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Program == nil || h.Deposit == nil || h.MarketWrite == nil || h.Market == nil {
		writeError(w, http.StatusNotImplemented, "the on-chain market is not configured on this deployment")
		return
	}
	var in marketRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	in.Signature = strings.TrimSpace(in.Signature)
	if in.Signature == "" {
		writeError(w, http.StatusBadRequest, "signature required")
		return
	}
	caller, err := base58FromAccountID(accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}
	payer, err := gsolana.PublicKeyFromBase58(caller)
	if err != nil {
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}
	sig, err := gsolana.SignatureFromBase58(in.Signature)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid signature")
		return
	}
	ctx := r.Context()

	switch in.Action {
	case marketActionList:
		h.confirmList(w, ctx, accountID, caller, payer, in, sig)
	case marketActionBuy:
		h.confirmBuy(w, ctx, accountID, payer, in, sig)
	case marketActionCancel:
		h.confirmCancel(w, ctx, accountID, payer, in, sig)
	default:
		writeError(w, http.StatusBadRequest, "unknown action")
	}
}

// confirmList records a listing the program now holds in escrow. The row is
// built from the listing account's own bytes - credits, price and mint come
// from the chain, not from the request - so a client cannot record terms it
// did not actually commit to.
func (h *Handler) confirmList(w http.ResponseWriter, ctx context.Context, accountID, caller string, payer gsolana.PublicKey, in marketRequest, sig gsolana.Signature) {
	// Confirming the same transaction twice must return the same row, not
	// open a second escrow: a retry after a dropped response is a normal
	// thing for a wallet flow to do.
	if existing, err := h.Market.AccountListings(ctx, accountID, marketPageCap, 0); err == nil {
		for _, row := range existing {
			if row.Nonce == uint64(in.Nonce) {
				writeJSON(w, http.StatusOK, h.listingRowView(row, accountID))
				return
			}
		}
	}
	l, found, err := h.Program.LedgerListingRead(ctx, payer, uint64(in.Nonce))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusBadRequest, "no listing was opened on chain")
		return
	}
	if !l.Open() {
		writeError(w, http.StatusConflict, "listing is not open on chain")
		return
	}
	slot, err := h.Program.LedgerTxSlot(ctx, sig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// Pin the row to the same id the indexer derives from the listing PDA,
	// so this confirm and the indexer's replay of the same on-chain listing
	// dedupe onto one primary key instead of racing two rows (and two
	// locks) for one escrow.
	id, err := h.Program.LedgerListingID(payer, uint64(in.Nonce))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	row := market.Listing{
		ListingID:        id,
		Account:          accountID,
		Seller:           caller,
		Nonce:            uint64(in.Nonce),
		Credits:          l.Credits,
		PayToken:         l.PayMint.String(),
		PayTokenSymbol:   payTokenSymbol,
		PayTokenDecimals: uint64(h.Deposit.Decimals),
		Price:            l.Price,
		MaturesAt:        uint64(now.Add(h.maturation).Unix()),
		State:            "open",
		ListedTx:         in.Signature,
		ListedBlock:      slot,
		ListedAt:         now.Format(time.RFC3339),
	}
	id, err = h.MarketWrite.RecordListing(ctx, row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if id == 0 {
		// The indexer recorded the same PDA-derived id between the nonce
		// check above and this insert: the ON CONFLICT skipped it, and the
		// row that won is this listing - read it back rather than fail.
		id = row.ListingID
	}
	if err := h.refreshLock(ctx, accountID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(ctx, id)
	if err != nil || saved.ListingID == 0 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, h.listingRowView(saved, accountID))
}

// confirmBuy records a sale the program already settled. The program paid
// the seller and moved the escrow in one transaction; this reads the listing
// back, insists it is sold to this caller, and only then moves the row and
// mirrors the credit movement the chain performed.
func (h *Handler) confirmBuy(w http.ResponseWriter, ctx context.Context, accountID string, payer gsolana.PublicKey, in marketRequest, sig gsolana.Signature) {
	row, err := h.MarketWrite.ListingByID(ctx, uint64(in.ListingID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if row.ListingID == 0 {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	if row.State != "open" {
		writeError(w, http.StatusConflict, "listing is not open")
		return
	}
	seller, err := sellerPubkeyFor(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	l, found, err := h.Program.LedgerListingRead(ctx, seller, row.Nonce)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusBadRequest, "no listing was found on chain")
		return
	}
	if l.State != solana.LedgerListingSold {
		writeError(w, http.StatusConflict, "the listing was not sold on chain")
		return
	}
	// The listing records who bought it; a caller who did not sign the sale
	// must not be able to claim it.
	if l.Buyer != payer {
		writeError(w, http.StatusForbidden, "the listing was sold to another wallet")
		return
	}
	slot, err := h.Program.LedgerTxSlot(ctx, sig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	micro := int64(row.Credits * meter.MicroCreditsPerCredit)

	// Shared authority first: a failure here aborts before the row moves,
	// so the buyer (already paid on chain) can simply retry.
	if h.Atomic != nil {
		if err := h.Atomic.AdjustMinted(ctx, row.Account, -micro); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := h.Atomic.AdjustMinted(ctx, accountID, micro); err != nil {
			_ = h.Atomic.AdjustMinted(ctx, row.Account, micro)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	// The ledger the proxy spends from: the same movement, in memory. The
	// seller's credits left at list time (they sit in the escrow) and the
	// escrow went to the buyer, so the net is a flat transfer between the
	// two accounts.
	if h.Ledger != nil {
		h.Ledger.AdjustMinted(row.Account, -micro)
		h.Ledger.AdjustMinted(accountID, micro)
	}

	now := h.now().UTC()
	moved, err := h.MarketWrite.SoldListing(ctx, row.ListingID, accountID, in.Signature, slot, now.Format(time.RFC3339))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !moved {
		if h.Ledger != nil {
			h.Ledger.AdjustMinted(row.Account, micro)
			h.Ledger.AdjustMinted(accountID, -micro)
		}
		if h.Atomic != nil {
			_ = h.Atomic.AdjustMinted(ctx, row.Account, micro)
			_ = h.Atomic.AdjustMinted(ctx, accountID, -micro)
		}
		writeError(w, http.StatusConflict, "listing already moved")
		return
	}
	if err := h.refreshLock(ctx, row.Account); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(ctx, row.ListingID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, h.listingRowView(saved, accountID))
}

// confirmCancel records a withdrawal the program already made: the escrow is
// back in the seller's own credit account, so the row moves to cancelled and
// the lock is recomputed from what is still open.
func (h *Handler) confirmCancel(w http.ResponseWriter, ctx context.Context, accountID string, payer gsolana.PublicKey, in marketRequest, sig gsolana.Signature) {
	row, err := h.MarketWrite.ListingByID(ctx, uint64(in.ListingID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if row.ListingID == 0 {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	if row.Account != accountID {
		writeError(w, http.StatusForbidden, "not your listing")
		return
	}
	if row.State != "open" {
		writeError(w, http.StatusConflict, "listing is not open")
		return
	}
	l, found, err := h.Program.LedgerListingRead(ctx, payer, row.Nonce)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusBadRequest, "no listing was found on chain")
		return
	}
	if l.State != solana.LedgerListingCancelled {
		writeError(w, http.StatusConflict, "the listing was not cancelled on chain")
		return
	}
	slot, err := h.Program.LedgerTxSlot(ctx, sig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	moved, err := h.MarketWrite.CancelListing(ctx, row.ListingID, in.Signature, slot, now.Format(time.RFC3339))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !moved {
		writeError(w, http.StatusConflict, "listing already moved")
		return
	}
	if err := h.refreshLock(ctx, accountID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(ctx, row.ListingID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, h.listingRowView(saved, accountID))
}

// WithLedgerMarket wires the deployed market program. Without it the
// on-chain routes answer 501 and only the local-mirror market is served.
func WithLedgerMarket(m LedgerMarket) Option {
	return func(h *Handler) { h.Program = m }
}
