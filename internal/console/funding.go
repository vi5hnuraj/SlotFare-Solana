// Package console's funding read model: the Tracker that holds each source
// chain's frontiers beside the purchases they stage, and the /funding view
// assembled from them. Persistence lives in internal/funding; this file is
// the state the HTTP route reads.
package console

import (
	"context"
	"net/http"
	"sync"

	"slotfare/internal/funding"
)

// Tracker is what the console reads for funding progress: the observed
// purchases plus, per source chain, the two frontiers that decide how far
// each one has gone - the chain tip and the proof builder ceiling, held
// separately because a block can be attested while the builder is still
// behind it. A later task feeds SetTip/SetCeiling/Record/MarkCredited; this
// task ships the reader.
type Tracker struct {
	store     funding.Store
	mu        sync.Mutex
	tip       map[uint64]uint64 // attested tip per source chain key
	ceiling   map[uint64]uint64 // builder ceiling per source chain key
	names     map[uint64]string // chain name per source chain key
	explorers map[uint64]string // explorer base per source chain key
}

// NewTracker wraps a funding store with no frontiers and no chain names yet.
func NewTracker(store funding.Store) *Tracker {
	return &Tracker{
		store:     store,
		tip:       map[uint64]uint64{},
		ceiling:   map[uint64]uint64{},
		names:     map[uint64]string{},
		explorers: map[uint64]string{},
	}
}

// SetTip records the current attested tip for one source chain.
func (t *Tracker) SetTip(chainKey, height uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tip[chainKey] = height
}

// SetCeiling records the proof builder's current ceiling for one source chain.
func (t *Tracker) SetCeiling(chainKey, height uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ceiling[chainKey] = height
}

// SetChainNames records the human names of the configured source chains so
// the funding view can label each purchase without a lookup table of its own.
func (t *Tracker) SetChainNames(names map[uint64]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range names {
		t.names[k] = v
	}
}

// SetChainExplorers records the explorer base URL of each configured source
// chain so the funding view can point at the source transaction without a
// lookup table of its own. A chain key with no entry serves an empty base.
func (t *Tracker) SetChainExplorers(explorers map[uint64]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range explorers {
		t.explorers[k] = v
	}
}

// Record persists one observed purchase.
func (t *Tracker) Record(ctx context.Context, p funding.Purchase) error {
	return t.store.Record(ctx, p)
}

// Store exposes the funding store to the feeder, which is the only writer.
func (t *Tracker) Store() funding.Store {
	return t.store
}

// MarkCredited flags a purchase as credited.
func (t *Tracker) MarkCredited(ctx context.Context, accountID, sourceTx string) error {
	return t.store.MarkCredited(ctx, accountID, sourceTx)
}

// FundingView is one account's funding picture at one moment: every purchase,
// newest first, with the stage its own chain's frontiers have reached.
// Credited purchases stay in the list at stage 5: they are the account's
// history. The frontiers are per row - a global tip no longer exists once
// several source chains are scanned.
type FundingView struct {
	Purchases []PurchaseView `json:"purchases"`
	// Tip and Ceiling are the top-level chain frontiers the browser reads
	// directly (chain-tip badge); the per-row copies stay for entries that
	// describe themselves. Solana is the only chain, so chain key 0 is the
	// frontier that matters; a deployment with no deposits reports 0.
	Tip     uint64 `json:"tip"`
	Ceiling uint64 `json:"ceiling"`
}

// PurchaseView is one purchase. Tip and Ceiling repeat that purchase's chain
// frontiers per row so each entry is self-describing. ChainName labels the
// source chain without a lookup table on the client. AmountUsd is derived
// from the integer micros at the edge, for display only.
type PurchaseView struct {
	ChainKey       uint64  `json:"chainKey"`
	ChainName      string  `json:"chainName"`
	Explorer       string  `json:"explorer"`
	SourceTx       string  `json:"sourceTx"`
	SourceBlock    uint64  `json:"sourceBlock"`
	AttestedHeight uint64  `json:"attestedHeight"`
	Tip            uint64  `json:"tip"`
	Ceiling        uint64  `json:"ceiling"`
	Stage          uint8   `json:"stage"`
	DepositID      string  `json:"depositId"`
	Token          string  `json:"token"`
	AmountUsd      float64 `json:"amountUsd"`
	StartedAt      string  `json:"startedAt"`
	Credited       bool    `json:"credited"`
	CCTx           string  `json:"ccTx"`
	CCBlock        uint64  `json:"ccBlock"`
	CreditedAt     string  `json:"creditedAt"`
	Credits        uint64  `json:"credits"`
	// Swap is the swap leg when the purchase landed through the peripheral;
	// omitted on the wire for a direct deposit so those entries stay
	// byte-identical to before.
	Swap *funding.SwapLeg `json:"swap,omitempty"`
}

// View builds the account's funding picture from the store and the current
// per-chain frontiers.
func (t *Tracker) View(ctx context.Context, accountID string) (FundingView, error) {
	purchases, err := t.store.AllPurchases(ctx, accountID)
	if err != nil {
		return FundingView{}, err
	}
	t.mu.Lock()
	tip, ceiling, names, explorers := t.tip, t.ceiling, t.names, t.explorers
	t.mu.Unlock()
	out := FundingView{
		Purchases: []PurchaseView{},
		Tip:       tip[0],
		Ceiling:   ceiling[0],
	}
	for _, p := range purchases {
		out.Purchases = append(out.Purchases, PurchaseView{
			ChainKey:       p.ChainKey,
			ChainName:      names[p.ChainKey],
			Explorer:       explorers[p.ChainKey],
			SourceTx:       p.SourceTx,
			SourceBlock:    p.SourceBlock,
			AttestedHeight: p.AttestedHeight,
			Tip:            tip[p.ChainKey],
			Ceiling:        ceiling[p.ChainKey],
			Stage:          fundingStage(p, tip[p.ChainKey], ceiling[p.ChainKey]),
			DepositID:      p.DepositID,
			Token:          p.Token,
			AmountUsd:      float64(p.UsdMicros) / 1e6,
			StartedAt:      p.StartedAt,
			Credited:       p.Credited,
			CCTx:           p.CCTx,
			CCBlock:        p.CCBlock,
			CreditedAt:     p.CreditedAt,
			Credits:        p.Credits,
			Swap:           p.Swap,
		})
	}
	return out, nil
}

// fundingStage reports which of the five stages a purchase has reached:
// 1 recorded (in the store, no source block yet), 2 confirmed (source block
// known), 3 attested (the chain tip has passed it), 4 provable (the builder
// ceiling has passed it too - both frontiers, not just one), 5 credited.
// A purchase whose block the tip has passed but the ceiling has not stays
// at stage 3, so the trace shows where it is stuck instead of advancing
// for no visible reason.
func fundingStage(p funding.Purchase, tip, ceiling uint64) uint8 {
	if p.Credited {
		return 5
	}
	if p.SourceBlock > 0 && tip >= p.SourceBlock && ceiling >= p.SourceBlock {
		return 4
	}
	if p.SourceBlock > 0 && tip >= p.SourceBlock {
		return 3
	}
	if p.SourceBlock > 0 {
		return 2
	}
	return 1
}

// handleFunding reports the account's uncredited purchases with their
// stages, plus the tip and the builder ceiling as separate top-level
// fields. The browser reads this endpoint; it never talks to the proof
// builder directly.
func (h *Handler) handleFunding(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Funding == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	view, err := h.Funding.View(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, view)
}
