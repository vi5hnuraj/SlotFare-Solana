package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slotfare/internal/meter"
	"time"
)

// Stream intervals. The heartbeat stays under slotfared's 30 s WriteTimeout so
// both intermediaries and the server keep an idle connection open; the poll
// interval bounds delivery latency, and Ledger.Changed() only shortens it.
const (
	defaultStreamPoll      = 2 * time.Second
	defaultStreamHeartbeat = 20 * time.Second
)

// WithFunding attaches the tracker backing the /funding route.
func WithFunding(t *Tracker) Option {
	return func(h *Handler) { h.Funding = t }
}

// withStreamIntervals shortens the stream's poll and heartbeat intervals;
// tests use it to observe events without waiting real seconds. Production
// keeps the 2 s / 20 s defaults.
func withStreamIntervals(poll, heartbeat time.Duration) Option {
	return func(h *Handler) {
		if poll > 0 {
			h.streamPoll = poll
		}
		if heartbeat > 0 {
			h.streamHeartbeat = heartbeat
		}
	}
}

// balanceView is one account's credit position as a balance event payload.
// Every amount is in micro-credits, the internal metering unit, under
// explicit *Micro field names so the console can render fractional credits
// without a unit a field name lies about.
type balanceView struct {
	Minted    uint64 `json:"mintedMicro"`
	Settled   uint64 `json:"settledMicro"`
	Pending   uint64 `json:"pendingMicro"`
	Locked    uint64 `json:"lockedMicro"`
	Spendable uint64 `json:"spendableMicro"`
}

func (h *Handler) balanceView(ctx context.Context, accountID string) balanceView {
	a, _ := h.accountPosition(ctx, accountID)
	return balanceView{Minted: a.TotalMinted(), Settled: a.Settled, Pending: a.Pending, Locked: a.Locked, Spendable: a.Spendable()}
}

// accountPosition is the one place the console reads a credit position. With
// an AccountReader attached it is the shared authority's row, so every
// replica answers alike; otherwise (single process, SQLite) the in-memory
// ledger. A failed authority read falls back to the ledger rather than
// failing the route, and logs once per occurrence. The boolean reports
// whether the account is known anywhere.
func (h *Handler) accountPosition(ctx context.Context, accountID string) (meter.Account, bool) {
	if h.Accounts != nil {
		a, err := h.Accounts.Account(ctx, accountID)
		if err == nil {
			return a, a.ID != ""
		}
		log.Printf("slotfared: console: account %s from authority: %v (using ledger)", accountID, err)
	}
	a, known := h.Ledger.Snapshot()[accountID]
	return a, known
}

// handleStream feeds the console's live view to one browser connection as a
// server-sent event stream: a balance snapshot on connect, a balance event
// whenever the account's position changes, a usage event per settled usage
// row (new rows only - no history replay), and a heartbeat every
// streamHeartbeat while idle so intermediaries do not close the
// connection. Every read is scoped to this token's account id, so one
// connection can never see another account's events.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	// EventSource cannot set headers, so the browser passes the console
	// token as ?token=; it is folded into the same bearer path as every
	// other route, and an Authorization header still wins.
	if r.Header.Get("Authorization") == "" {
		if t := r.URL.Query().Get("token"); t != "" {
			r.Header.Set("Authorization", "Bearer "+t)
		}
	}
	accountID, ok := h.requireAccount(w, r)
	if !ok {
		return
	}
	if h.Ledger == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	_ = flusher
	// The cursor starts after the account's newest row: the stream reports
	// new usage, it does not replay history.
	var cursor int64
	if h.Usage != nil {
		recent, err := h.Usage.RecentUsage(r.Context(), accountID, 1, "", false, 0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if len(recent) > 0 {
			cursor = recent[0].RowID
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	last := h.balanceView(r.Context(), accountID)
	writeSSE(w, "balance", last)
	lastWrite := time.Now()

	// A context that dies when the client goes away even if the server's
	// request context does not (for instance when the connection is closed
	// without the server noticing): the stream must not outlive its reader.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		<-w.(http.CloseNotifier).CloseNotify()
		cancel()
	}()

	poll := time.NewTicker(h.streamPoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(h.streamHeartbeat)
	defer heartbeat.Stop()
	var changed <-chan struct{}
	if h.Ledger != nil {
		changed = h.Ledger.Changed()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-heartbeat.C:
		case <-changed:
		}
		cur := h.balanceView(ctx, accountID)
		if cur != last {
			writeSSE(w, "balance", cur)
			last = cur
			lastWrite = time.Now()
		}
		if h.Usage != nil {
			rows, err := h.Usage.UsageSince(r.Context(), accountID, cursor)
			if err == nil {
				for _, row := range rows {
					writeSSE(w, "usage", usageViewOf(row))
					cursor = row.RowID
					lastWrite = time.Now()
				}
			}
			// A transient database error keeps the stream alive: the
			// next wake retries from the same cursor.
		}
		if time.Since(lastWrite) >= h.streamHeartbeat {
			writeSSE(w, "heartbeat", struct {
				TS string `json:"ts"`
			}{TS: time.Now().UTC().Format(time.RFC3339)})
			lastWrite = time.Now()
		}
	}
}

// writeSSE writes one server-sent event and flushes it through to the
// client, so each event is delivered the moment it is produced. The writer
// must support flushing; handleStream has already asserted that.
func writeSSE(w http.ResponseWriter, name string, data any) {
	body, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body)
	w.(http.Flusher).Flush()
}
