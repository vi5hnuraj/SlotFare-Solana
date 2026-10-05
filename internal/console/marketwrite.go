package console

import (
	"context"
	"encoding/hex"
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

// payTokenSymbol is the market's token symbol: every listing the console
// creates pays in USDC, so rows never need an address-shaped fallback.
const payTokenSymbol = "USDC"

// defaultListingMaturation is the window a fresh listing must age through
// before anyone may buy it - the maturation the original CreditMarket
// enforced at 120 s, kept as the default so the console's copy ("sale
// lands after the maturation window") still describes the contract. It is
// a floor, not a deadline: a matured listing stays buyable until sold or
// cancelled. SLOTFARE_LISTING_MATURATION overrides it per deployment.
const defaultListingMaturation = 2 * time.Minute

// listingWindowSkew is how far before listed_at an accepted payment's block
// time may sit. The row's timestamp is slotfared's clock; the transaction's is
// the cluster's bank time, and the two are not required to agree to the
// second.
const listingWindowSkew = 2 * time.Minute

// listingCreditCap bounds one listing's size: a million whole credits
// ($1,000 at the fixed rate) per row, so a fat-fingered body cannot lock an
// absurd position or overflow the micro-credit conversion.
const listingCreditCap = 1_000_000

// listingPriceCap bounds one listing's price in pay-token base units
// (USDC micro-dollars): $1,000,000, which no honest demo row approaches.
const listingPriceCap = 1_000_000_000_000

// PaymentVerifier checks a market payment on the settlement chain: *solana.Client
// satisfies it. The signature is the seller-visible proof the buyer paid;
// slotfared never moves USDC itself.
type PaymentVerifier interface {
	VerifyPayment(ctx context.Context, sig, mint, payer, payee string, minAmount uint64) (solana.PaymentInfo, error)
}

// base58FromAccountID recovers the base58 wallet behind a Slotfare account
// id: the id is 0x-hex of the raw 32-byte public key, so the reverse is a
// decode, not a lookup. Sessions only ever carry ids built that way.
func base58FromAccountID(accountID string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(accountID, "0x"))
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("console: account %q is not a 32-byte key", accountID)
	}
	return gsolana.PublicKeyFromBytes(raw).String(), nil
}

// listingRowView renders one listing as the write routes return it: the
// read routes' listingView shape, so a create/cancel/pay response is
// byte-compatible with a /listings row the client refreshes anyway.
func (h *Handler) listingRowView(l market.Listing, viewer string) listingView {
	now := h.now().Unix()
	role := "buying"
	if l.Account == viewer {
		role = "selling"
	}
	return listingView{
		ListingID:        wireUint64(l.ListingID),
		Credits:          l.Credits,
		Seller:           l.Seller,
		PayToken:         l.PayToken,
		PayTokenSymbol:   h.payTokenSymbolFor(l),
		PayTokenDecimals: h.payTokenDecimalsFor(l),
		Price:            l.Price,
		MaturesAt:        l.MaturesAt,
		Matured:          now >= int64(l.MaturesAt),
		State:            l.State,
		Role:             role,
		ListedTx:         l.ListedTx,
		ListedBlock:      l.ListedBlock,
		ListedAt:         l.ListedAt,
		CancelledTx:      l.CancelledTx,
		CancelledBlock:   l.CancelledBlock,
		CancelledAt:      l.CancelledAt,
		SoldTx:           l.SoldTx,
		SoldBlock:        l.SoldBlock,
		SoldAt:           l.SoldAt,
		BuyerAccount:     l.BuyerAccount,
	}
}

// refreshLock recomputes an account's lock from the row set (the open
// listings' credits) and pushes it to the ledger and, when configured, the
// shared authority. Recomputing rather than adding means a create, a cancel
// and a sale in flight cannot interleave into a lost update: whoever
// writes last writes the sum of the rows as they now stand.
func (h *Handler) refreshLock(ctx context.Context, accountID string) error {
	lockedCredits, err := h.MarketWrite.LockedFor(ctx, accountID)
	if err != nil {
		return err
	}
	lockedMicro := lockedCredits * meter.MicroCreditsPerCredit
	// The mirror is absent on an on-chain-only deployment, where the escrow
	// itself is the lock and this recompute only keeps the shared authority
	// and any local ledger honest.
	if h.Ledger != nil {
		h.Ledger.SetLocked(accountID, lockedMicro)
	}
	if h.Atomic != nil {
		return h.Atomic.SetLocked(ctx, accountID, lockedMicro)
	}
	return nil
}

// RebuildLocks re-derives every account's lock from the listing rows and
// writes each figure to the ledger and the shared authority. The market's
// write routes refresh per account as they land, but a listing can also be
// recorded by the indexer, or by a confirm that failed after the row was
// written - neither ever reaches refreshLock. The row scan returns every
// account with a listing, open or not, so an account whose last row closed
// while nobody ran the route is re-zeroed rather than left stale.
func (h *Handler) RebuildLocks(ctx context.Context) error {
	if h.MarketWrite == nil {
		return nil
	}
	locks, err := h.MarketWrite.OpenLocks(ctx)
	if err != nil {
		return err
	}
	for accountID, credits := range locks {
		lockedMicro := credits * meter.MicroCreditsPerCredit
		if h.Ledger != nil {
			h.Ledger.SetLocked(accountID, lockedMicro)
		}
		if h.Atomic != nil {
			if err := h.Atomic.SetLocked(ctx, accountID, lockedMicro); err != nil {
				return err
			}
		}
	}
	return nil
}

// handleCreateListing lists credits for sale. The credits are checked
// against spendable, then locked (through the row set) for the window; the
// buyer side happens later, on-chain, in handlePayListing.
func (h *Handler) handleCreateListing(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.MarketWrite == nil || h.Deposit == nil || h.Ledger == nil {
		writeError(w, http.StatusNotImplemented, "market not available")
		return
	}
	var in struct {
		Credits uint64 `json:"credits"`
		Price   uint64 `json:"price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if in.Credits == 0 || in.Credits > listingCreditCap {
		writeError(w, http.StatusBadRequest, "credits out of range")
		return
	}
	if in.Price == 0 || in.Price > listingPriceCap {
		writeError(w, http.StatusBadRequest, "price out of range")
		return
	}
	micro := in.Credits * meter.MicroCreditsPerCredit
	if h.Ledger.Balance(accountID) < micro {
		writeError(w, http.StatusPaymentRequired, "insufficient credits")
		return
	}
	seller, err := base58FromAccountID(accountID)
	if err != nil {
		// A session bound to a legacy 20-byte id has no Solana wallet to
		// list from; nothing about that is a server fault.
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}
	now := h.now().UTC()
	l := market.Listing{
		Account:          accountID,
		Seller:           seller,
		Credits:          in.Credits,
		PayToken:         h.Deposit.Mint,
		PayTokenSymbol:   payTokenSymbol,
		PayTokenDecimals: uint64(h.Deposit.Decimals),
		Price:            in.Price,
		MaturesAt:        uint64(now.Add(h.maturation).Unix()),
		ListedAt:         now.Format(time.RFC3339),
	}
	id, err := h.MarketWrite.RecordListing(r.Context(), l)
	if err != nil || id == 0 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.refreshLock(r.Context(), accountID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(r.Context(), id)
	if err != nil || saved.ListingID == 0 {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, h.listingRowView(saved, accountID))
}

// handleCancelListing withdraws the caller's open listing: the guarded row
// move wins or loses against a concurrent sale (a lost race is 409), then
// the lock is recomputed from what remains open.
func (h *Handler) handleCancelListing(w http.ResponseWriter, r *http.Request, id uint64) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.MarketWrite == nil {
		writeError(w, http.StatusNotImplemented, "market not available")
		return
	}
	l, err := h.MarketWrite.ListingByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if l.ListingID == 0 {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	if l.State != "open" {
		writeError(w, http.StatusConflict, "listing is not open")
		return
	}
	if l.Account != accountID {
		writeError(w, http.StatusForbidden, "not your listing")
		return
	}
	at := h.now().UTC().Format(time.RFC3339)
	moved, err := h.MarketWrite.CancelListing(r.Context(), id, "", 0, at)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !moved {
		writeError(w, http.StatusConflict, "listing already moved")
		return
	}
	if err := h.refreshLock(r.Context(), l.Account); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, h.listingRowView(saved, accountID))
}

// handlePayListing settles a sale. The USDC moves on Solana, wallet to
// wallet - the buyer's session wallet pays the seller's wallet directly -
// and this route only verifies that it did: the signature must net at least
// the price from buyer to seller in the listing's mint, inside the
// listing's window. The guarded sold move is the commit point; the credit
// adjustments follow the order (authorities first, row last) so a failure
// or a lost race leaves nothing half-applied.
//
// Accounting axis: a sale rides the ledger's signed Adjust axis, never the
// Minted axis. The seller's Minted (deposits) is untouched - the credits
// leaving are minted − spent − locked at the buy, not a reversed deposit -
// and the buyer's gain is a positive Adjust, not a Minted write, so a
// later deposit rescan can neither clobber nor be clobbered by it. A
// buyer's acquired credits are therefore distinguishable from deposited
// credits: Minted still means "created by the deposit rail" (docs/
// decisions.md D3), Adjust-with-a-plus means "received on the market".
func (h *Handler) handlePayListing(w http.ResponseWriter, r *http.Request, id uint64) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.MarketWrite == nil || h.Payment == nil || h.Deposit == nil || h.Ledger == nil {
		writeError(w, http.StatusNotImplemented, "market not available")
		return
	}
	var in struct {
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	in.Signature = strings.TrimSpace(in.Signature)
	if in.Signature == "" {
		writeError(w, http.StatusBadRequest, "signature required")
		return
	}
	l, err := h.MarketWrite.ListingByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if l.ListingID == 0 {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	if l.State != "open" {
		writeError(w, http.StatusConflict, "listing is not open")
		return
	}
	now := h.now().UTC()
	// Maturity is the earliest a sale may land, not an expiry: the row
	// stays open - and buyable - after it, until sold or cancelled.
	if now.Unix() < int64(l.MaturesAt) {
		writeError(w, http.StatusConflict, "listing has not matured yet")
		return
	}
	if l.Account == accountID {
		writeError(w, http.StatusConflict, "cannot buy your own listing")
		return
	}
	buyer, err := base58FromAccountID(accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "account has no Solana wallet")
		return
	}
	// The on-chain check: payer is the session wallet, payee the seller's
	// wallet (the owner behind the row's account id, not the hex id
	// itself), mint and price from the row. An unverifiable signature -
	// wrong payer, wrong mint, short amount, failed transaction, unknown
	// signature - is one client-visible rejection.
	seller, err := base58FromAccountID(l.Account)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	info, err := h.Payment.VerifyPayment(r.Context(), in.Signature, l.PayToken, buyer, seller, l.Price)
	if err != nil {
		writeError(w, http.StatusBadRequest, "payment not verified")
		return
	}
	// blockTime + skew must reach listedAt: the cluster's bank time may
	// trail slotfared's clock by the skew, never lead it by more.
	if listed, perr := time.Parse(time.RFC3339, l.ListedAt); perr == nil &&
		info.BlockTime.Add(listingWindowSkew).Before(listed) {
		writeError(w, http.StatusBadRequest, "payment predates the listing")
		return
	}
	// No upper time bound: a matured listing never expires, so the only
	// temporal rule is that the payment cannot precede the listing itself.

	micro := int64(l.Credits * meter.MicroCreditsPerCredit)

	// Shared authority first: a failure here aborts before any state
	// moves, so the buyer (already paid on-chain) can simply retry.
	if h.Atomic != nil {
		if err := h.Atomic.AdjustMinted(r.Context(), l.Account, -micro); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := h.Atomic.AdjustMinted(r.Context(), accountID, micro); err != nil {
			// Compensate the seller-side adjustment before surfacing.
			if cErr := h.Atomic.AdjustMinted(r.Context(), l.Account, micro); cErr != nil {
				// Loudly inconsistent; the row has not moved yet.
			}
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	// The ledger the proxy spends from: same adjustment, in memory.
	h.Ledger.AdjustMinted(l.Account, -micro)
	h.Ledger.AdjustMinted(accountID, micro)

	// The commit point: one winner moves the row; a loser rolls back its
	// adjustments and reports the conflict.
	moved, err := h.MarketWrite.SoldListing(r.Context(), id, accountID, info.Signature, info.Slot, now.Format(time.RFC3339))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !moved {
		h.Ledger.AdjustMinted(l.Account, micro)
		h.Ledger.AdjustMinted(accountID, -micro)
		if h.Atomic != nil {
			if err := h.Atomic.AdjustMinted(r.Context(), l.Account, micro); err != nil {
				// Best-effort compensation; logged by the caller's error
				// surface only through the 409 below - the in-memory
				// ledger, which the proxy spends from, is already correct.
			}
			if err := h.Atomic.AdjustMinted(r.Context(), accountID, -micro); err != nil {
				// Same: best effort.
			}
		}
		writeError(w, http.StatusConflict, "listing already moved")
		return
	}

	// The sale released the seller's lock with the row; recompute it from
	// what is still open (the sold row no longer counts).
	if err := h.refreshLock(r.Context(), l.Account); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	saved, err := h.MarketWrite.ListingByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, h.listingRowView(saved, accountID))
}

// parseListingWritePath splits /listings/{id}/{cancel|pay} into the id and
// the action; false means the path is not a write subroute at all.
func parseListingWritePath(path string) (id uint64, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/listings/")
	if !found {
		return 0, "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return 0, "", false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	switch parts[1] {
	case "cancel", "pay":
		return id, parts[1], true
	default:
		return 0, "", false
	}
}
