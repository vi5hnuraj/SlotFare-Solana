package console

import (
	"net/http"

	"slotfare/internal/meter"
	"slotfare/internal/solana"
)

// DepositTarget is what the console's deposit screen needs to point a
// wallet at the right place: the treasury token account that receives
// USDC, the mint, the cluster, and the fixed conversion the deposit
// watcher applies. It is served from configuration - there is no chain
// call in the route.
type DepositTarget struct {
	Treasury string `json:"treasury"`
	Mint     string `json:"mint"`
	Cluster  string `json:"cluster"`
	RPC      string `json:"rpc"`
	Explorer string `json:"explorer"`
	// Decimals is the mint's token decimals; USDC-shaped mints are 6, and
	// the frontend formats amounts against it.
	Decimals uint8 `json:"decimals"`
	// CreditsPerUsd is the headline rate (1,000 credits = $1.00), derived
	// from the watcher's fixed conversion so the screen can never drift
	// from what a deposit actually mints.
	CreditsPerUsd uint64 `json:"creditsPerUsd"`
	// MicroCreditsPerUsdBase is the machine form: micro-credits per token
	// base unit, exactly what the watcher applies.
	MicroCreditsPerUsdBase uint64 `json:"microCreditsPerUsdBase"`
}

// DepositTargetFor builds the payload from the loaded chain configuration.
// It returns nil when the chain surface is off, so the route reports 501
// instead of pointing a wallet at an unconfigured treasury.
func DepositTargetFor(cfg solana.Config) *DepositTarget {
	if !cfg.Enabled() || cfg.Treasury == "" {
		return nil
	}
	explorer := "https://explorer.solana.com"
	return &DepositTarget{
		Treasury:               cfg.Treasury,
		Mint:                   cfg.Mint,
		Cluster:                cfg.Cluster,
		RPC:                    cfg.RPC,
		Explorer:               explorer,
		Decimals:               6,
		CreditsPerUsd:          1_000_000 * solana.MicroCreditsPerUSDCBase / meter.MicroCreditsPerCredit,
		MicroCreditsPerUsdBase: solana.MicroCreditsPerUSDCBase,
	}
}

// handleDepositTarget serves the deposit configuration to a signed-in
// console session.
func (h *Handler) handleDepositTarget(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAccount(w, r); !ok {
		return
	}
	if h.Deposit == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	writeJSON(w, http.StatusOK, h.Deposit)
}
