package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
)

// inferenceAllowlist is the set of paths the inference service accepts.
var inferenceAllowlist = []string{"/v1/models", "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/rerank", "/health"}

// newInferenceTestHandler builds a Handler with one token-priced inference
// service under /infer against the given upstream.
func newInferenceTestHandler(t *testing.T, secret string, minted uint64, upstream *url.URL) (*Handler, keys.Key, *meter.Ledger) {
	t.Helper()
	return newMultiTestHandler(t, secret, minted, []Service{
		{Name: "inference", Prefix: "/infer", Upstream: upstream, Pricer: NewInferencePricer(meter.DefaultTokenPrices()), Allow: inferenceAllowlist},
	})
}

// /v1/models and /health cost nothing and are forwarded with the prefix
// stripped.
func TestInferenceFreePathsCostZeroAndForward(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"data":[]}`)
	h, k, ledger := newInferenceTestHandler(t, secret, 100, stub.url())

	for _, target := range []string{"/infer/v1/models", "/infer/health"} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body: %s", target, rec.Code, rec.Body.String())
		}
		if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
			t.Errorf("%s: pending = %d, want 0 (free path)", target, pending)
		}
	}
	if got := <-stub.paths; got != "/v1/models" {
		t.Errorf("upstream path = %q, want %q (prefix stripped)", got, "/v1/models")
	}
}

// A path outside the allowlist is refused without reaching the upstream.
func TestInferenceAllowlistRefusesUnknownPath(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"ok":true}`)
	h, k, ledger := newInferenceTestHandler(t, secret, 100, stub.url())

	req := httptest.NewRequest("POST", "/infer/v1/other", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeNotFound {
		t.Errorf("error code = %d, want %d", code, codeNotFound)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
		t.Errorf("pending = %d, want 0 (nothing reserved)", pending)
	}
}

// A completion that used fewer tokens than reserved releases the difference:
// the pending balance ends at the true cost and spendable returns to
// minted minus that.
func TestInferenceSettlesTrueCostReleasesDifference(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	// max_tokens of one million reserves exactly 3000 credits (the prompt
	// estimate is under a million tokens, so it prices to 0).
	body := `{"model":"qwen38-27b","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	respBody := `{"id":"acfb41c5...","object":"chat.completion","model":"qwen38-27b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},
   "finish_reason":"stop"}],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000,
   "prompt_tokens_details":null}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	h, k, ledger := newInferenceTestHandler(t, secret, minted, stub.url())

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != respBody {
		t.Errorf("served body differs from the upstream response byte-for-byte")
	}
	// True cost: 400000 prompt at 1000/M plus 300000 completion at 3000/M,
	// in micro-credits.
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if wantTrue != 1300*meter.MicroCreditsPerCredit {
		t.Fatalf("sanity: true cost = %d, want %d micro-credits", wantTrue, 1300*meter.MicroCreditsPerCredit)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (difference released)", pending, wantTrue)
	}
	if spendable := ledger.Snapshot()[k.AccountID].Spendable(); spendable != minted-wantTrue {
		t.Errorf("spendable = %d, want %d", spendable, minted-wantTrue)
	}
}

// A completion that reports no usage settles the full reservation rather
// than releasing it: serving happened and the cost is unknown.
func TestInferenceNoUsageSettlesFullReservation(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	body := `{"model":"qwen38-27b","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	reserved, err := meter.DefaultTokenPrices().Reserve([]byte(body))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	respBody := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	h, k, ledger := newInferenceTestHandler(t, secret, minted, stub.url())

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != reserved {
		t.Errorf("pending after no-usage response = %d, want the full reservation %d", pending, reserved)
	}
	if spendable := ledger.Snapshot()[k.AccountID].Spendable(); spendable != minted-reserved {
		t.Errorf("spendable = %d, want %d", spendable, minted-reserved)
	}
}

// An upstream failure still releases the reservation: the new settlement
// path must not have broken the release path.
func TestInferenceUpstreamFailureStillReleases(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	h, k, ledger := newInferenceTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url())

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"max_tokens":100,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeUpstreamError {
		t.Errorf("error code = %d, want %d", code, codeUpstreamError)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
		t.Errorf("pending after upstream failure = %d, want 0 (reservation released)", pending)
	}
}

// A streaming request copies the chunks through byte-for-byte and leaves
// the full reservation pending when the upstream reports no usage in the
// stream - the shape of an upstream without stream_options support.
func TestInferenceStreamingLeavesFullReservation(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	body := `{"stream":true,"max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	sse := "data: {\"id\":\"1\",\"choices\":[{}]}\n\ndata: [DONE]\n\n"

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse)
	}))
	defer srv.Close()
	upstream, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := meter.DefaultTokenPrices().Reserve([]byte(body))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	h, k, ledger := newInferenceTestHandler(t, secret, minted, upstream)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != sse {
		t.Errorf("served body = %q, want the SSE chunks byte-for-byte %q", got, sse)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want %q", gotPath, "/v1/chat/completions")
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != reserved {
		t.Errorf("pending after streaming = %d, want the full reservation %d (T03 settles)", pending, reserved)
	}
}

// A streaming request whose final chunk carries a usage report settles at
// the true cost instead of the full reservation: the body is served
// byte-for-byte and only the difference between hold and truth is released.
func TestInferenceStreamingSettlesFromFinalChunk(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	body := `{"stream":true,"max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	sse := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":13,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse)
	}))
	defer srv.Close()
	upstream, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	h, k, ledger := newInferenceTestHandler(t, secret, minted, upstream)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != sse {
		t.Errorf("served body = %q, want the SSE chunks byte-for-byte %q", got, sse)
	}
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 5, CompletionTokens: 13})
	if wantTrue == 0 {
		t.Fatalf("sanity: true cost = 0 micro-credits for a 5+13 token stream")
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after streaming settle = %d, want %d (the final chunk's usage, not the reservation)", pending, wantTrue)
	}
	if spendable := ledger.Snapshot()[k.AccountID].Spendable(); spendable != minted-wantTrue {
		t.Errorf("spendable = %d, want %d", spendable, minted-wantTrue)
	}
}

// A small call settles at its exact, nonzero micro-credit price: the
// live-inference bug where a 5-prompt + 13-completion-token call settled at
// 0 credits under whole-credit integer math.
func TestInferenceSmallCallSettlesNonzero(t *testing.T) {
	const secret = "slotfare-secret-001"
	body := `{"model":"qwen38-27b","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`
	respBody := `{"id":"x","object":"chat.completion","model":"qwen38-27b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":5,"total_tokens":18,"completion_tokens":13}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	h, k, ledger := newInferenceTestHandler(t, secret, 1000*meter.MicroCreditsPerCredit, stub.url())

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 5, CompletionTokens: 13})
	if wantTrue == 0 {
		t.Fatalf("sanity: true cost = 0 micro-credits for a 5+13 token call")
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (the exact nonzero micro-credit price)", pending, wantTrue)
	}
}
