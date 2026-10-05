package console

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// healthAt builds an AttestHealth over a fixed health snapshot and clock.
func healthAt(h WatcherHealth, now time.Time) *AttestHealth {
	a := NewAttestHealth(func() WatcherHealth { return h })
	withAttestNow(func() time.Time { return now })(a)
	return a
}

// TestAttestViewHealthy checks the healthy row: known, tip and head from
// the watcher's last pass, and the lag derived from the clock.
func TestAttestViewHealthy(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a := healthAt(WatcherHealth{LastScan: now.Add(-4 * time.Second), Head: 1234}, now)
	out := a.View(context.Background())
	if len(out.Chains) != 1 {
		t.Fatalf("chains = %d rows, want 1", len(out.Chains))
	}
	c := out.Chains[0]
	if c.ChainKey != 0 || c.Name != "Solana" {
		t.Errorf("row identity = %d/%q, want 0/Solana", c.ChainKey, c.Name)
	}
	if !c.Known {
		t.Error("Known = false, want true for a completed scan")
	}
	if c.AttestedTip != 1234 || c.Head != 1234 {
		t.Errorf("tip/head = %d/%d, want 1234/1234 (full rescan covers head)", c.AttestedTip, c.Head)
	}
	if c.LagBlocks != 0 {
		t.Errorf("LagBlocks = %d, want 0 (full rescan leaves no block lag)", c.LagBlocks)
	}
	if c.LagSeconds != 4 {
		t.Errorf("LagSeconds = %d, want 4", c.LagSeconds)
	}
}

// TestAttestViewStale checks that lag grows with wall time since the last
// completed pass - this is how an outage degrades the card's grade.
func TestAttestViewStale(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a := healthAt(WatcherHealth{LastScan: now.Add(-400 * time.Second), Head: 7}, now)
	c := a.View(context.Background()).Chains[0]
	if !c.Known || c.LagSeconds != 400 {
		t.Errorf("row = %+v, want known with 400 s lag", c)
	}
}

// TestAttestViewNeverScanned checks both silent states: nothing configured
// (no scan, no error) reports an empty row list so the card hides; a
// watcher that has never completed a pass but has failed reports a single
// unknown row - name and key only, nothing fabricated.
func TestAttestViewNeverScanned(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	silent := healthAt(WatcherHealth{}, now)
	if rows := silent.View(context.Background()).Chains; len(rows) != 0 {
		t.Errorf("silent watcher rows = %+v, want none (card hides)", rows)
	}

	failing := healthAt(WatcherHealth{Err: "signatures: connection refused"}, now)
	rows := failing.View(context.Background()).Chains
	if len(rows) != 1 {
		t.Fatalf("failing watcher rows = %d, want 1", len(rows))
	}
	if rows[0].Known {
		t.Error("Known = true for a watcher that never completed a pass")
	}
	if rows[0].ChainKey != 0 || rows[0].Name != "Solana" {
		t.Errorf("row identity = %d/%q, want key and name only", rows[0].ChainKey, rows[0].Name)
	}
	if rows[0].AttestedTip != 0 || rows[0].Head != 0 || rows[0].LagSeconds != 0 {
		t.Errorf("unknown row carries data: %+v", rows[0])
	}
}

// TestAttestRoute checks GET /attestation serves the envelope, POST is 405,
// auth is required, and an absent producer answers 501.
func TestAttestRoute(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	a := healthAt(WatcherHealth{LastScan: now.Add(-2 * time.Second), Head: 99}, now)
	h := New(WithSessions(testSessionStore(t)), WithAttestation(a))
	token := consoleTokenFor(t, h, "0xtest")

	rec := doRequest(t, h, http.MethodGet, "/attestation", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /attestation status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out AttestHealthView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /attestation body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Chains) != 1 || !out.Chains[0].Known || out.Chains[0].Head != 99 {
		t.Errorf("view = %+v, want one known Solana row at head 99", out)
	}

	if rec := doRequest(t, h, http.MethodPost, "/attestation", token, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /attestation: status = %d, want 405", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/attestation", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /attestation without token: status = %d, want 401", rec.Code)
	}

	hNil := New(WithSessions(testSessionStore(t)))
	if rec := doRequest(t, hNil, http.MethodGet, "/attestation", consoleTokenFor(t, hNil, "0xtest"), nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /attestation without Attestation: status = %d, want 501", rec.Code)
	}
}
