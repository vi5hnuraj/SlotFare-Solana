package console

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
)

// keyPrefix is the plaintext prefix of every issued API key. It keeps an API
// key from ever passing the console middleware's 64-hex-char token check, so
// the two credential types cannot cross.
const keyPrefix = "attk_"

// requireAccount resolves the caller's account id from the bearer token,
// answering through failBearer when the token is not valid or the session
// store itself failed.
func (h *Handler) requireAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	accountID, ok, reason, err := h.resolveBearer(r)
	if err != nil || !ok {
		failBearer(w, reason, err)
		return "", false
	}
	return accountID, true
}

// mintKey returns a fresh plaintext key and the prefix that identifies it:
// attk_ plus 32 lowercase hex chars, prefix = first 13 chars.
func mintKey() (string, string) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("console: generate key: %v", err))
	}
	plaintext := keyPrefix + hex.EncodeToString(raw[:])
	return plaintext, plaintext[:13]
}

// handleIssueKey mints a key for the caller's account and returns the
// plaintext exactly once. Only its hash is stored.
func (h *Handler) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Keys == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	var in struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}

	existing, err := h.Keys.ListByAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := h.now().UTC()
	var plaintext, prefix string
	for attempt := 0; attempt < 16; attempt++ {
		plaintext, prefix = mintKey()
		if !prefixInUse(existing, prefix) {
			break
		}
		if attempt == 15 {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	k := keys.Key{
		Hash:      keys.HashKey(plaintext),
		AccountID: accountID,
		Prefix:    prefix,
		Label:     in.Label,
		CreatedAt: now.Unix(),
	}
	if err := h.Keys.Add(r.Context(), k); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Key       string `json:"key"`
		Prefix    string `json:"prefix"`
		Label     string `json:"label"`
		CreatedAt string `json:"createdAt"`
	}{plaintext, prefix, in.Label, now.Format(time.RFC3339)})
}

// handleListKeys lists the caller's keys: prefix, label, created, last used,
// request count and revoked flag. Never key material.
func (h *Handler) handleListKeys(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Keys == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	list, err := h.Keys.ListByAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := struct {
		Keys []keyView `json:"keys"`
	}{Keys: []keyView{}}
	for _, k := range list {
		out.Keys = append(out.Keys, keyView{
			Prefix:       k.Prefix,
			Label:        k.Label,
			CreatedAt:    time.Unix(k.CreatedAt, 0).UTC().Format(time.RFC3339),
			LastUsed:     unixOrZeroToNull(k.LastUsed),
			RequestCount: k.RequestCount,
			Revoked:      k.Revoked,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeKey revokes the key with the prefix after /keys/ when it
// belongs to the caller's account. A prefix owned by another account is a
// 404, not a 403: do not confirm that someone else's key exists.
func (h *Handler) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Keys == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	prefix := strings.TrimPrefix(r.URL.Path, "/keys/")
	if prefix == "" || strings.Contains(prefix, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	revoked, err := h.Keys.RevokeByPrefix(r.Context(), accountID, prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !revoked {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
}

// handleAccount reports the caller's credit position from one ledger
// snapshot; spendable is the ledger's own arithmetic. An account absent from
// the snapshot reports zeros.
func (h *Handler) handleAccount(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Ledger == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	a, known := h.accountPosition(r.Context(), accountID)
	if !known && h.Minted != nil {
		// First sight of this account: seed Minted from the contract so a
		// funded-but-idle wallet does not read as empty until it spends.
		// The contract holds whole credits; the ledger speaks micro-credits.
		if minted, err := h.Minted.BalanceOf(r.Context(), accountID); err == nil {
			h.Ledger.SetMinted(accountID, minted*meter.MicroCreditsPerCredit)
			if h.LedgerSink != nil {
				// Best-effort mirror into the Postgres authority: a failed
				// write is re-derived by the settler's next mint refresh,
				// so it never fails the route.
				_ = h.LedgerSink(r.Context(), accountID, minted*meter.MicroCreditsPerCredit)
			}
			a, _ = h.accountPosition(r.Context(), accountID)
		}
	}
	a.ID = accountID
	// The balance is reported in micro-credits under explicit field names,
	// so the console can render fractional credits without a unit the name
	// lies about.
	writeJSON(w, http.StatusOK, struct {
		AccountID string `json:"accountId"`
		Minted    uint64 `json:"mintedMicro"`
		Settled   uint64 `json:"settledMicro"`
		Pending   uint64 `json:"pendingMicro"`
		Locked    uint64 `json:"lockedMicro"`
		Spendable uint64 `json:"spendableMicro"`
	}{accountID, a.TotalMinted(), a.Settled, a.Pending, a.Locked, a.Spendable()})
}

// keyView is one entry of the GET /keys list. No key material in any field.
type keyView struct {
	Prefix       string  `json:"prefix"`
	Label        string  `json:"label"`
	CreatedAt    string  `json:"createdAt"`
	LastUsed     *string `json:"lastUsed"`
	RequestCount int64   `json:"requestCount"`
	Revoked      bool    `json:"revoked"`
}

// prefixInUse reports whether any stored key already carries the prefix.
func prefixInUse(list []keys.Key, prefix string) bool {
	for _, k := range list {
		if k.Prefix == prefix {
			return true
		}
	}
	return false
}

// unixOrZeroToNull renders a unix timestamp as RFC3339, or JSON null for the
// zero value (never used).
func unixOrZeroToNull(unix int64) *string {
	if unix == 0 {
		return nil
	}
	s := time.Unix(unix, 0).UTC().Format(time.RFC3339)
	return &s
}
