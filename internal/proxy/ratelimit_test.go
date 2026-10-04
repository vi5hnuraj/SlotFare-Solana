package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
)

// setClock gives the handler's rate limiter a deterministic clock and
// returns the setter, so tests advance time without sleeping.
func setClock(t *testing.T, h *Handler) func(time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	h.rateLimiter.now = func() time.Time { return now }
	return func(when time.Time) { now = when }
}

// Per-key limit: the fourth rapid request on one key is a 429 with
// Retry-After, uncharged and unserved; the window rolls over; another key
// is unaffected.
func TestRateLimitPerKey(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"result":"0x1"}`)
	h, k, ledger := newTestHandler(t, secret, 100_000, meter.DefaultWeights(), stub.url())
	usage := &recUsage{}
	h.Usage = usage
	h.KeyRateLimit = 3
	h.setRateLimits()
	advance := setClock(t, h)

	call := func(key string) int {
		req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for i := 1; i <= 3; i++ {
		if code := call(secret); code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth request: status = %d, want 429; body: %s", rec.Code, rec.Body.String())
	}
	if retryAfter := rec.Result().Header.Get("Retry-After"); retryAfter == "" || retryAfter[0] < '1' {
		t.Errorf("Retry-After = %q, want whole seconds >= 1", retryAfter)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeRateLimited {
		t.Errorf("error code = %d, want %d", code, codeRateLimited)
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded for key") {
		t.Errorf("error message %q does not name the key layer", rec.Body.String())
	}
	if n := stub.requests.Load(); n != 3 {
		t.Errorf("upstream received %d requests, want 3 (the 429 must not forward)", n)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 3*meter.MicroCreditsPerCredit {
		t.Errorf("pending = %d, want %d (three trivial calls in micro-credits; the rejected request reserves nothing)", pending, 3*meter.MicroCreditsPerCredit)
	}
	// The three served calls recorded one row each; the rejected fourth
	// recorded none.
	if len(usage.rows) != 3 {
		t.Errorf("usage rows = %d, want 3 (one per served call; the rate-limited request is never metered)", len(usage.rows))
	}

	// The window rolls over: the key is allowed again.
	advance(time.Now().Add(61 * time.Second))
	if code := call(secret); code != http.StatusOK {
		t.Errorf("after window rollover: status = %d, want 200", code)
	}

	// A second key has its own bucket.
	advance(time.Now().Add(61 * time.Second))
	other := keys.Issue("slotfare-secret-002")
	if err := h.Keys.Add(context.Background(), other); err != nil {
		t.Fatalf("keys.Add: %v", err)
	}
	ledger.SetMinted(other.AccountID, 100_000)
	if code := call("slotfare-secret-002"); code != http.StatusOK {
		t.Errorf("second key: status = %d, want 200 (separate bucket)", code)
	}
}

// A bucket whose window has closed is swept: it does not stay in the map
// for the life of the process, and the sweep does not disturb a bucket that
// is still open.
func TestRateLimitSweepsClosedWindows(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	rl := newRateLimiter(func() time.Time { return now })
	rl.setLimit("key", 5)

	if ok, _ := rl.allow("key", "k1"); !ok {
		t.Fatal("first request on k1 was refused")
	}
	if _, seen := rl.buckets["key/k1"]; !seen {
		t.Fatal("k1 bucket missing after its first request")
	}
	// Inside the same window the bucket stays and keeps counting.
	rl.setLimit("key", 1)
	if ok, wait := rl.allow("key", "k1"); ok || wait <= 0 {
		t.Fatalf("k1 at its limit = (%v, %v), want a refusal with a wait", ok, wait)
	}
	// Past the window a request for another key sweeps the dead bucket.
	rl.setLimit("key", 5)
	now = now.Add(2 * time.Minute)
	if ok, _ := rl.allow("key", "k2"); !ok {
		t.Fatal("first request on k2 was refused")
	}
	if _, seen := rl.buckets["key/k1"]; seen {
		t.Error("k1's closed window survived the sweep")
	}
	if _, seen := rl.buckets["key/k2"]; !seen {
		t.Error("k2's open bucket was swept with the dead one")
	}
}

// Per-model cap: three requests to model A from two different keys hit the
// global model budget on the third; model B is unaffected.
func TestRateLimitPerModel(t *testing.T) {
	const minted = uint64(100_000)
	stub := newStubUpstream(t, http.StatusOK, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	ctx := context.Background()
	store, err := keys.Open(filepath.Join(t.TempDir(), "keys2.db"))
	if err != nil {
		t.Fatalf("keys.OpenInMemory: %v", err)
	}
	k1 := keys.Issue("slotfare-secret-001")
	k2 := keys.Issue("slotfare-secret-002")
	for _, k := range []keys.Key{k1, k2} {
		if err := store.Add(ctx, k); err != nil {
			t.Fatalf("keys.Add: %v", err)
		}
	}
	ledger := meter.NewLedger()
	ledger.SetMinted(k1.AccountID, minted)
	ledger.SetMinted(k2.AccountID, minted)
	h := New(store, meter.NewLocalAuthority(ledger), []Service{
		{Name: "inference", Prefix: "/infer", Upstream: stub.url(), Pricer: NewInferencePricerWithModels(meter.DefaultTokenPrices(), registryModels), Allow: inferenceAllowlist},
	})
	usage := &recUsage{}
	h.Usage = usage
	h.KeyRateLimit = 100
	h.ModelRateLimit = 2
	h.setRateLimits()
	setClock(t, h)

	call := func(secret, model string) *httptest.ResponseRecorder {
		body := `{"model":"` + model + `","max_tokens":10,"messages":[]}`
		req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("slotfare-secret-001", "modelA"); rec.Code != http.StatusOK {
		t.Fatalf("A request 1: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec := call("slotfare-secret-002", "modelA"); rec.Code != http.StatusOK {
		t.Fatalf("A request 2: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	rec := call("slotfare-secret-001", "modelA")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A request 3: status = %d, want 429; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded for model modelA") {
		t.Errorf("error message %q does not name the model layer and the model", rec.Body.String())
	}
	if retryAfter := rec.Result().Header.Get("Retry-After"); retryAfter == "" || retryAfter[0] < '1' {
		t.Errorf("Retry-After = %q, want whole seconds >= 1", retryAfter)
	}
	// Model B has its own budget.
	if rec := call("slotfare-secret-001", "modelB"); rec.Code != http.StatusOK {
		t.Errorf("B request 1: status = %d, want 200 (separate model budget); body: %s", rec.Code, rec.Body.String())
	}
	if n := stub.requests.Load(); n != 3 {
		t.Errorf("upstream received %d requests, want 3", n)
	}
	if len(usage.rows) != 3 {
		t.Errorf("usage rows = %d, want 3 (the rate-limited request records nothing)", len(usage.rows))
	}
}
