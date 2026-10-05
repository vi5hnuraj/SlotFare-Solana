package console

import "net/http"

// WithChains attaches the configured funding source chains served by
// GET /chains. The console hardcodes no chain metadata; this list is it.
func WithChains(chains []ChainInfo) Option {
	return func(h *Handler) { h.Chains = chains }
}

// handleChains lists the chains the console names: today that is the one
// Solana row. Authenticated like every other console route; 501 when slotfared
// was started without a chain (the offline demo).
func (h *Handler) handleChains(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAccount(w, r); !ok {
		return
	}
	chains := make([]ChainInfo, 0, len(h.Chains))
	chains = append(chains, h.Chains...)
	if len(chains) == 0 {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Chains []ChainInfo `json:"chains"`
	}{Chains: chains})
}
