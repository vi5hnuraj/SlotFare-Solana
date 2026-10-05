package console

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"slotfare/internal/meter"
)

// UsageReader is the console's read view over the meter store's usage
// table. *meter.Store satisfies it; the handler takes the narrow interface
// so tests can substitute one.
type UsageReader interface {
	RecentUsage(ctx context.Context, accountID string, limit int, keyPrefix string, errorsOnly bool, before int64) ([]meter.UsageRow, error)
	UsagePage(ctx context.Context, accountID string, f meter.UsageFilter, page, pageSize int) ([]meter.UsageRow, int, error)
	UsageFacets(ctx context.Context, accountID string) ([]string, []string, error)
	UsageSince(ctx context.Context, accountID string, afterRowID int64) ([]meter.UsageRow, error)
}

// parseBeforeCursor reads the optional ?before cursor shared by the usage,
// market and listings routes. Empty means no cursor (0); anything that does
// not parse to a positive integer is rejected, mirroring how limit reports
// an invalid value.
func parseBeforeCursor(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	// A market cursor is a listing id: a full uint64 decimal (half of them
	// exceed int64max), so it parses unsigned and wraps into the signed
	// form the id column stores. Usage ids parse the same way - they are
	// small positives, which the wrap leaves untouched.
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		return 0, errors.New("invalid before")
	}
	return int64(n), nil
}

// WithUsage attaches the usage reader backing the /usage route and the
// stream's usage events.
func WithUsage(r UsageReader) Option {
	return func(h *Handler) { h.Usage = r }
}

// handleUsage lists the account's metered calls. Without ?page it serves
// today's newest batch: ?limit caps it, ?before walks back by row id,
// ?prefix narrows to one key prefix and ?errors=true to failed calls.
// With ?page it switches to numbered pagination over the same filters plus
// ?model and a ?from/?to time range (unix seconds, inclusive), and adds
// total (the matching row count) and facets (the distinct key prefixes and
// model ids for the account, for the dropdowns).
func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Usage == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	limit := meter.UsageCap
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if limit > meter.UsageCap {
		limit = meter.UsageCap
	}
	pageStr := r.URL.Query().Get("page")
	if pageStr != "" {
		handleUsagePaged(w, r, h, accountID, pageStr, limit)
		return
	}
	before, err := parseBeforeCursor(r.URL.Query().Get("before"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid before")
		return
	}
	rows, err := h.Usage.RecentUsage(r.Context(), accountID, limit, r.URL.Query().Get("prefix"), r.URL.Query().Get("errors") == "true", before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := struct {
		Usage      []usageView `json:"usage"`
		NextCursor int64       `json:"nextCursor,omitempty"`
	}{Usage: []usageView{}}
	for _, row := range rows {
		out.Usage = append(out.Usage, h.withSettlement(r.Context(), accountID, row, usageViewOf(row)))
	}
	if len(rows) > 0 {
		out.NextCursor = rows[len(rows)-1].RowID
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUsagePaged serves GET /usage in paged mode (?page present): numbered
// pages over the filtered set, newest first, with the matching total and the
// account's distinct key prefixes and model ids in one round trip. The
// cursor form (?before) is exclusive with it; both present is a bad request.
func handleUsagePaged(w http.ResponseWriter, r *http.Request, h *Handler, accountID, pageStr string, pageSize int) {
	q := r.URL.Query()
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	if q.Get("before") != "" {
		writeError(w, http.StatusBadRequest, "invalid page")
		return
	}
	var from, to int64
	if v := q.Get("from"); v != "" {
		from, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid from")
			return
		}
	}
	if v := q.Get("to"); v != "" {
		to, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid to")
			return
		}
	}
	f := meter.UsageFilter{
		KeyPrefix:  q.Get("prefix"),
		Model:      q.Get("model"),
		From:       from,
		To:         to,
		ErrorsOnly: q.Get("errors") == "true",
	}
	rows, total, err := h.Usage.UsagePage(r.Context(), accountID, f, page, pageSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	prefixes, models, err := h.Usage.UsageFacets(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := struct {
		Usage  []usageView `json:"usage"`
		Total  int         `json:"total"`
		Facets struct {
			Prefixes []string `json:"prefixes"`
			Models   []string `json:"models"`
		} `json:"facets"`
	}{Usage: []usageView{}, Total: total}
	out.Facets.Prefixes = prefixes
	out.Facets.Models = models
	for _, row := range rows {
		out.Usage = append(out.Usage, h.withSettlement(r.Context(), accountID, row, usageViewOf(row)))
	}
	writeJSON(w, http.StatusOK, out)
}

// usageView is one entry of GET /usage: what was spent, never what was
// asked. No request or response content in any field. CreditsMicro is the
// settled cost of the call in micro-credits, the internal metering unit.
type usageView struct {
	// ID is the stored row id: the handle for GET /usage/{id}/proof, so a
	// client can fetch this row's inclusion proof without guessing.
	ID               uint64 `json:"id"`
	Timestamp        string `json:"timestamp"`
	Service          string `json:"service"`
	Model            string `json:"model"`
	Kind             string `json:"kind,omitempty"`
	PromptTokens     uint64 `json:"promptTokens"`
	CachedTokens     uint64 `json:"cachedTokens"`
	CompletionTokens uint64 `json:"completionTokens"`
	Credits          uint64 `json:"creditsMicro"`
	Status           string `json:"status"`
	KeyPrefix        string `json:"keyPrefix"`
	// DurationMS is the recorded wall-clock duration in milliseconds; nil
	// for rows written before the column existed. No omitempty: the key
	// stays present with a null value so old rows read as absent, never
	// fabricated as 0.
	DurationMS *uint64 `json:"duration_ms"`
	// Settlement facts, present only once the row's batch is committed on
	// chain: the batch's zero-based index for this account, its root and the
	// commit transaction. Absent means awaiting settlement.
	BatchIndex *uint64 `json:"batchIndex,omitempty"`
	Root       string  `json:"root,omitempty"`
	CommitTx   string  `json:"commitTx,omitempty"`
}

// withSettlement decorates a rendered row with its committed batch when the
// proof reader is attached and the row is settled; an unsettled row or a
// lookup error leaves the view as it was (the row stays reported, unsettled).
func (h *Handler) withSettlement(ctx context.Context, accountID string, row meter.UsageRow, v usageView) usageView {
	if h.Proof == nil {
		return v
	}
	batch, localID, settled, err := h.Proof.BatchForRow(ctx, accountID, row.RowID)
	if err != nil || !settled {
		return v
	}
	idx := uint64(localID - 1)
	v.BatchIndex = &idx
	v.Root = hex.EncodeToString(batch.Root[:])
	v.CommitTx = batch.TXHash
	return v
}

// usageViewOf renders one stored row for the JSON response. Kind is the
// registry service kind (embeddings / reranker); omitempty leaves every
// existing row's wire payload byte-identical.
func usageViewOf(row meter.UsageRow) usageView {
	return usageView{
		ID:               uint64(row.RowID),
		Timestamp:        row.Timestamp.Format(time.RFC3339),
		Service:          row.Service,
		Model:            row.Model,
		Kind:             row.Kind,
		PromptTokens:     row.PromptTokens,
		CachedTokens:     row.CachedTokens,
		CompletionTokens: row.CompletionTokens,
		Credits:          row.Credits,
		Status:           row.Status,
		KeyPrefix:        row.KeyPrefix,
		DurationMS:       row.DurationMS,
	}
}
