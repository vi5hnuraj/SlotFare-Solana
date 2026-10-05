package console

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"slotfare/internal/meter"
)

// ProofReader is the narrow read view the usage-proof route needs: one row by
// id, the batch that covers it, and the rows of that batch's range. Both
// meter.Store drivers satisfy it; the handler takes the interface so tests can
// substitute one.
type ProofReader interface {
	UsageByID(ctx context.Context, accountID string, rowID int64) (meter.UsageRow, bool, error)
	BatchForRow(ctx context.Context, accountID string, rowID int64) (meter.SettleBatch, int64, bool, error)
	BatchRangeRows(ctx context.Context, accountID string, firstID, lastID int64) ([]meter.UsageRow, error)
}

// WithProof attaches the reader backing GET /usage/{id}/proof. Without it the
// route answers 501 like every other unattached subsystem.
func WithProof(r ProofReader) Option {
	return func(h *Handler) { h.Proof = r }
}

// proofLeafView is the canonical leaf of one usage row: exactly the ten words
// fed to UsageLeaf's hash. key_prefix, kind and duration_ms are not leaf words
// and are deliberately absent, so a verifier recomputing the leaf from this
// object gets the same value the batch root was committed over.
type proofLeafView struct {
	ID               uint64 `json:"id"`
	AccountID        string `json:"accountId"`
	Timestamp        int64  `json:"ts"`
	Service          string `json:"service"`
	Model            string `json:"model"`
	PromptTokens     uint64 `json:"promptTokens"`
	CachedTokens     uint64 `json:"cachedTokens"`
	CompletionTokens uint64 `json:"completionTokens"`
	Credits          uint64 `json:"creditsMicro"`
	Status           string `json:"status"`
}

// proofBatchView is the committed batch record a charge belongs to. BatchIndex
// is the on-chain array offset of that batch: the local
// auto-increment id starts at 1 while the on-chain array starts at 0, so it is
// localID - 1.
type proofBatchView struct {
	Root         string `json:"root"`
	FirstUsageID uint64 `json:"firstUsageId"`
	LastUsageID  uint64 `json:"lastUsageId"`
	Credits      uint64 `json:"credits"`
	TXHash       string `json:"txHash"`
	CommittedAt  int64  `json:"committedAt"`
	BatchIndex   uint64 `json:"batchIndex"`
}

// proofStepView is one combine on the leaf-to-root path.
type proofStepView struct {
	Hash      string `json:"hash"`
	Direction string `json:"direction"`
}

// proofView is the body of GET /usage/{id}/proof. Settled is false when no
// batch has committed over the row yet: the leaf and its hash are still served
// so the caller can confirm the row's canonical encoding, but there is no proof
// path or batch record to check against the chain.
type proofView struct {
	Settled  bool          `json:"settled"`
	Leaf     proofLeafView `json:"leaf"`
	LeafHash string        `json:"leafHash"`
	// Nonce is the row's server-assigned request nonce, exposed so a
	// receipt's nonce can be checked against the recorded row. It is not
	// a leaf word - the T02 leaf stays the pinned 10-word scheme - and it
	// is empty for rows recorded before receipts existed.
	Nonce string          `json:"nonce,omitempty"`
	Proof []proofStepView `json:"proof,omitempty"`
	Batch *proofBatchView `json:"batch,omitempty"`
}

// handleUsageProof serves GET /usage/{id}/proof for the caller's own row: the
// canonical leaf, the Merkle proof path, and the committed batch record - or an
// honest "not yet settled" answer when the row is still pending. The proof is
// rebuilt on demand from the batch's stored row range; nothing beyond T02's
// batches table is kept.
func (h *Handler) handleUsageProof(w http.ResponseWriter, r *http.Request, rowID int64) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Proof == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	row, found, err := h.Proof.UsageByID(r.Context(), accountID, rowID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "row not found")
		return
	}
	leafHash, err := meter.UsageLeaf(row)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	view := proofView{Settled: false, Leaf: proofLeafOf(row), LeafHash: hex.EncodeToString(leafHash[:]), Nonce: row.Nonce}

	batch, localID, settled, err := h.Proof.BatchForRow(r.Context(), accountID, rowID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !settled {
		writeJSON(w, http.StatusOK, view)
		return
	}

	rows, err := h.Proof.BatchRangeRows(r.Context(), accountID, int64(batch.FirstUsageID), int64(batch.LastUsageID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	root, proof, err := meter.BuildBatchProof(rows, rowID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	view.Settled = true
	view.Proof = make([]proofStepView, len(proof))
	for i, s := range proof {
		view.Proof[i] = proofStepView{Hash: hex.EncodeToString(s.Hash[:]), Direction: s.Direction}
	}
	view.Batch = &proofBatchView{
		Root:         hex.EncodeToString(root[:]),
		FirstUsageID: batch.FirstUsageID,
		LastUsageID:  batch.LastUsageID,
		Credits:      batch.Credits,
		TXHash:       batch.TXHash,
		CommittedAt:  batch.CommittedAt,
		BatchIndex:   uint64(localID - 1),
	}
	writeJSON(w, http.StatusOK, view)
}

// proofLeafOf renders the ten canonical leaf words of a stored row.
func proofLeafOf(row meter.UsageRow) proofLeafView {
	return proofLeafView{
		ID:               uint64(row.RowID),
		AccountID:        row.AccountID,
		Timestamp:        row.Timestamp.Unix(),
		Service:          row.Service,
		Model:            row.Model,
		PromptTokens:     row.PromptTokens,
		CachedTokens:     row.CachedTokens,
		CompletionTokens: row.CompletionTokens,
		Credits:          row.Credits,
		Status:           row.Status,
	}
}

// parseUsageProofPath splits /usage/{id}/proof into the numeric row id. It
// reports ok=false for any path that is not exactly that shape.
func parseUsageProofPath(path string) (int64, bool) {
	rest := strings.TrimPrefix(path, "/usage/")
	if rest == "" || rest == path {
		return 0, false
	}
	idPart, tail, found := strings.Cut(rest, "/")
	if !found || tail != "proof" {
		return 0, false
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
