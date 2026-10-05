package console

import (
	"context"
	"net/http"
	"time"
)

// AttestChainHealth is one chain's attestation picture at a moment: how far
// the deposit watcher has attested relative to where the chain actually is.
// Known is false when no scan has completed yet (chain surface off, or the
// RPC has never answered); such a row carries its key and name only, every
// other field zero - nothing fabricated.
type AttestChainHealth struct {
	ChainKey    uint64 `json:"chainKey"`
	Name        string `json:"name"`
	Known       bool   `json:"known"`
	AttestedTip uint64 `json:"attestedTip"`
	Head        uint64 `json:"head"`
	LagBlocks   uint64 `json:"lagBlocks"`
	LagSeconds  uint64 `json:"lagSeconds"`
}

// AttestHealthView is the console's /attestation payload: one row per
// configured chain, in configuration order. Solana is the only chain, so
// there is exactly one row.
type AttestHealthView struct {
	Chains []AttestChainHealth `json:"chains"`
}

// WatcherHealth is the deposit watcher's self-reported frontier, as
// solana.Watcher.Health reports it: the last completed scan, the slot it
// observed, and the most recent failure (empty when healthy).
type WatcherHealth struct {
	LastScan time.Time
	Head     uint64
	Err      string
}

// AttestHealth produces the console's /attestation payload from the
// deposit watcher's own heartbeat. The watcher rescans full history each
// pass, so an attested tip equals the head it observed in that pass; lag
// is therefore time-based - how long since the last completed pass - and
// the block lag stays zero. Failures do not blank the row: lastScan keeps
// its last-success stamp, so lagSeconds grows through an outage and the
// card degrades normal -> lagging -> stalled on its own thresholds.
type AttestHealth struct {
	health func() WatcherHealth
	now    func() time.Time
}

// NewAttestHealth builds the producer over a health function (the watcher's
// Health method, projected onto this package's struct). The clock defaults
// to time.Now; tests inject one through withAttestNow.
func NewAttestHealth(h func() WatcherHealth) *AttestHealth {
	return &AttestHealth{health: h, now: time.Now}
}

// withAttestNow injects the clock; tests use it to control the reported lag.
func withAttestNow(now func() time.Time) func(*AttestHealth) {
	return func(a *AttestHealth) { a.now = now }
}

// View reports the watcher's current health as the single Solana row.
// Everything here is read from the in-memory snapshot: no chain RPC, no
// cache, no failure path that could 500 the card.
func (a *AttestHealth) View(_ context.Context) AttestHealthView {
	h := a.health()
	// Nothing to report: the chain surface is off (no watcher at all), or
	// no pass has ever run and none has failed. An empty list hides the
	// card outright - quieter than an unreachable row for a deployment
	// that never claimed to watch a chain.
	if h.LastScan.IsZero() && h.Err == "" {
		return AttestHealthView{Chains: []AttestChainHealth{}}
	}
	row := AttestChainHealth{ChainKey: 0, Name: "Solana"}
	if !h.LastScan.IsZero() {
		row.Known = true
		row.AttestedTip = h.Head
		row.Head = h.Head
		// Full rescan: the pass covered everything up to head, so there is
		// no block lag to report.
		row.LagBlocks = 0
		if lag := a.now().Sub(h.LastScan).Seconds(); lag > 0 {
			row.LagSeconds = uint64(lag)
		}
	}
	// LastScan zero with an error present: the watcher is configured but
	// has never completed a pass. Known stays false - name and key only.
	return AttestHealthView{Chains: []AttestChainHealth{row}}
}

// handleAttestation reports the deposit watcher's attestation health.
// Authenticated like the other console routes; 501 when slotfared was started
// without the chain surface (the offline demo path).
func (h *Handler) handleAttestation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAccount(w, r); !ok {
		return
	}
	if h.Attestation == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	writeJSON(w, http.StatusOK, h.Attestation.View(r.Context()))
}
