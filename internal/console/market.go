package console

import (
	"net/http"
	"slotfare/internal/market"
)

// marketPageCap is the newest number of rows each market route returns, like
// the usage cap: enough for a browse screen, bounded so a busy market cannot
// grow the response without bound.
const marketPageCap = 200

// marketView is one entry of /market: the listing's identity and price plus
// the two figures the client must not compute itself - whether it is the
// caller's own row and whether it has matured on slotfared's clock.
type marketView struct {
	ListingID wireUint64 `json:"listingId"`
	Credits   uint64     `json:"credits"`
	// Seller is the seller's base58 wallet - where a buyer's payment
	// lands. It is public on-chain data; the buyer needs it before the
	// signature exists.
	Seller         string `json:"seller"`
	PayToken       string `json:"payToken"`
	PayTokenSymbol string `json:"payTokenSymbol"`
	// PayTokenDecimals is the token's on-chain decimals; 0 means unknown, and
	// the client falls back to its configured token table.
	PayTokenDecimals uint64 `json:"payTokenDecimals"`
	Price            uint64 `json:"price"`
	MaturesAt        uint64 `json:"maturesAt"`
	Matured          bool   `json:"matured"`
	Mine             bool   `json:"mine"`
}

// listingView is one entry of /listings: the market fields minus mine, plus
// the state, which side of the trade the caller sits on, and the transactions
// that moved the row. The cancel and sale moves are omitted when they never
// happened; buyerAccount when there is no buyer yet.
type listingView struct {
	ListingID wireUint64 `json:"listingId"`
	Credits   uint64     `json:"credits"`
	// Seller is the seller's base58 wallet (empty on legacy rows).
	Seller         string `json:"seller"`
	PayToken       string `json:"payToken"`
	PayTokenSymbol string `json:"payTokenSymbol"`
	// PayTokenDecimals is the token's on-chain decimals; 0 means unknown, and
	// the client falls back to its configured token table.
	PayTokenDecimals uint64 `json:"payTokenDecimals"`
	Price            uint64 `json:"price"`
	MaturesAt        uint64 `json:"maturesAt"`
	Matured          bool   `json:"matured"`
	State            string `json:"state"`
	Role             string `json:"role"`
	ListedTx         string `json:"listedTx"`
	ListedBlock      uint64 `json:"listedBlock"`
	ListedAt         string `json:"listedAt"`
	CancelledTx      string `json:"cancelledTx,omitempty"`
	CancelledBlock   uint64 `json:"cancelledBlock,omitempty"`
	CancelledAt      string `json:"cancelledAt,omitempty"`
	SoldTx           string `json:"soldTx,omitempty"`
	SoldBlock        uint64 `json:"soldBlock,omitempty"`
	SoldAt           string `json:"soldAt,omitempty"`
	BuyerAccount     string `json:"buyerAccount,omitempty"`
}

// payTokenSymbolFor resolves a listing's stablecoin symbol: the feeder-recorded
// value, falling back to the short address form so a pre-existing row is never
// blank.
// payTokenSymbolFor names the token a listing pays in. A row mirrored from
// chain records only the mint - its symbol and decimals columns stay at
// their defaults - and every listing prices in the deposit the console
// accepts, so the configured deposit resolves the metadata the event never
// carried. A row that recorded a symbol keeps it.
func (h *Handler) payTokenSymbolFor(l market.Listing) string {
	if l.PayTokenSymbol != "" {
		return l.PayTokenSymbol
	}
	if _, _, ok := h.depositedToken(l.PayToken); ok {
		return payTokenSymbol
	}
	if len(l.PayToken) >= 10 {
		return l.PayToken[:10]
	}
	return l.PayToken
}

// payTokenDecimalsFor resolves a listing's pay-token decimals the same way:
// the stored value when the row recorded one, the configured deposit's
// otherwise - a mirrored row's 0 would render its price as raw units.
func (h *Handler) payTokenDecimalsFor(l market.Listing) uint64 {
	if l.PayTokenDecimals != 0 {
		return l.PayTokenDecimals
	}
	if _, decimals, ok := h.depositedToken(l.PayToken); ok {
		return decimals
	}
	return 0
}

// depositedToken reports whether payToken is the mint the console's deposit
// target accepts, returning the symbol and decimals configured with it.
func (h *Handler) depositedToken(payToken string) (string, uint64, bool) {
	if payToken == "" || h.Deposit == nil || payToken != h.Deposit.Mint {
		return "", 0, false
	}
	return payTokenSymbol, uint64(h.Deposit.Decimals), true
}

// handleMarket lists every open listing, newest first, marking the caller's
// own and computing maturity server-side against slotfared's clock.
func (h *Handler) handleMarket(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Market == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	before, err := parseBeforeCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	listings, err := h.Market.OpenListings(r.Context(), marketPageCap, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().Unix()
	out := struct {
		Market     []marketView `json:"market"`
		NextCursor wireUint64   `json:"nextCursor,omitempty"`
	}{Market: []marketView{}}
	for _, l := range listings {
		out.Market = append(out.Market, marketView{
			ListingID:        wireUint64(l.ListingID),
			Credits:          l.Credits,
			Seller:           l.Seller,
			PayToken:         l.PayToken,
			PayTokenSymbol:   h.payTokenSymbolFor(l),
			PayTokenDecimals: h.payTokenDecimalsFor(l),
			Price:            l.Price,
			MaturesAt:        l.MaturesAt,
			Matured:          now >= int64(l.MaturesAt),
			Mine:             l.Account == accountID,
		})
	}
	if len(listings) > 0 {
		out.NextCursor = wireUint64(listings[len(listings)-1].ListingID)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListings lists both sides of the caller's trades in every state,
// newest first: rows where the caller sold carry role "selling", rows where
// the caller bought (through buyer_account) carry role "buying".
func (h *Handler) handleListings(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Market == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	before, err := parseBeforeCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	listings, err := h.Market.AccountListings(r.Context(), accountID, marketPageCap, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().Unix()
	out := struct {
		Listings   []listingView `json:"listings"`
		NextCursor wireUint64    `json:"nextCursor,omitempty"`
	}{Listings: []listingView{}}
	for _, l := range listings {
		role := "buying"
		if l.Account == accountID {
			role = "selling"
		}
		out.Listings = append(out.Listings, listingView{
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
		})
	}
	if len(listings) > 0 {
		out.NextCursor = wireUint64(listings[len(listings)-1].ListingID)
	}
	writeJSON(w, http.StatusOK, out)
}
