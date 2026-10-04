package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"slotfare/internal/meter"
)

// The slotfare headers: x-slotfare-credits-charged is the amount settled for
// this request, x-slotfare-balance the account's spendable balance after
// settlement; both in integer micro-credits. Streaming responses carry only
// the balance (as of reservation time); free paths and error rejections
// carry no charge header.

// A metered RPC call carries both headers: the charge is the method weight
// (rpcPricer has no ResponseSettler, so it settles the full reservation) and
// the balance is the spendable position after that amount went pending.
func TestSlotfareHeadersOnMeteredRPCCall(t *testing.T) {
	const secret = "slotfare-secret-001"
	const cost = 1 * meter.MicroCreditsPerCredit // eth_chainId is trivial, one credit
	const minted = 100 * meter.MicroCreditsPerCredit
	stub := newStubUpstream(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	h, k, ledger := newTestHandler(t, secret, minted, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerCreditsCharged); got != fmt.Sprintf("%d", cost) {
		t.Errorf("%s = %q, want %d (the settled method weight)", headerCreditsCharged, got, cost)
	}
	wantBalance := ledger.Snapshot()[k.AccountID].Spendable()
	if got := rec.Header().Get(headerBalance); got != fmt.Sprintf("%d", wantBalance) {
		t.Errorf("%s = %q, want %d (spendable after settlement)", headerBalance, got, wantBalance)
	}
}

// A non-streaming inference call with a usage report carries the settled
// true cost as the charge - not the reservation - and the spendable
// balance after the difference was released.
func TestSlotfareHeadersSettleTrueCost(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
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
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if got := rec.Header().Get(headerCreditsCharged); got != fmt.Sprintf("%d", wantTrue) {
		t.Errorf("%s = %q, want %d (the settled true cost, not the reservation)", headerCreditsCharged, got, wantTrue)
	}
	wantBalance := ledger.Snapshot()[k.AccountID].Spendable()
	if got := rec.Header().Get(headerBalance); got != fmt.Sprintf("%d", wantBalance) {
		t.Errorf("%s = %q, want %d (spendable after the release)", headerBalance, got, wantBalance)
	}
}

// A streaming response sends headers before settlement is knowable: it
// carries x-slotfare-balance as of reservation time and omits
// x-slotfare-credits-charged entirely.
func TestSlotfareHeadersStreamingOmitsCharge(t *testing.T) {
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
	reserved, err := meter.DefaultTokenPrices().Reserve([]byte(body))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	h, _, _ := newInferenceTestHandler(t, secret, minted, upstream)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerCreditsCharged); got != "" {
		t.Errorf("%s = %q, want absent (streaming settles after the headers go out)", headerCreditsCharged, got)
	}
	// As of reservation time the hold is pending: spendable is minted minus
	// the reservation, whatever the stream later settles to.
	wantBalance := minted - reserved
	if got := rec.Header().Get(headerBalance); got != fmt.Sprintf("%d", wantBalance) {
		t.Errorf("%s = %q, want %d (as of reservation time)", headerBalance, got, wantBalance)
	}
}

// Free discovery paths reserve nothing and carry neither slotfare header.
func TestSlotfareHeadersAbsentOnFreePaths(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"data":[]}`)
	h, _, _ := newInferenceTestHandler(t, secret, 100, stub.url())

	for _, target := range []string{"/infer/v1/models", "/infer/health"} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body: %s", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(headerCreditsCharged); got != "" {
			t.Errorf("%s: %s = %q, want absent (free path)", target, headerCreditsCharged, got)
		}
		if got := rec.Header().Get(headerBalance); got != "" {
			t.Errorf("%s: %s = %q, want absent (free path)", target, headerBalance, got)
		}
	}
}

// An insufficient-credits rejection carries no charge header but may carry
// the balance: here the account holds less than the weight, so it reports
// its whole spendable position.
func TestSlotfareHeadersOnInsufficientCredits(t *testing.T) {
	const secret = "slotfare-secret-001"
	// eth_getLogs is heavy (20 credits); mint only 10 micro-credits so it
	// cannot cover.
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h, k, ledger := newTestHandler(t, secret, 10, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_getLogs","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if got := rec.Header().Get(headerCreditsCharged); got != "" {
		t.Errorf("%s = %q, want absent (nothing settled)", headerCreditsCharged, got)
	}
	wantBalance := ledger.Snapshot()[k.AccountID].Spendable()
	if got := rec.Header().Get(headerBalance); got != fmt.Sprintf("%d", wantBalance) {
		t.Errorf("%s = %q, want %d (the account's spendable position)", headerBalance, got, wantBalance)
	}
}

// Other rejections carry no slotfare headers at all: a 401 for an unknown
// key and a 502 for an upstream failure.
func TestSlotfareHeadersAbsentOnRejections(t *testing.T) {
	const secret = "slotfare-secret-001"

	t.Run("401 unknown key", func(t *testing.T) {
		stub := newStubUpstream(t, http.StatusOK, `{}`)
		h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

		req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
		req.Header.Set("Authorization", "Bearer not-a-known-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		assertNoSlotfareHeaders(t, rec)
	})

	t.Run("502 upstream failure", func(t *testing.T) {
		stub := newStubUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
		h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

		req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		assertNoSlotfareHeaders(t, rec)
	})
}

func assertNoSlotfareHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get(headerCreditsCharged); got != "" {
		t.Errorf("%s = %q, want absent on a rejection", headerCreditsCharged, got)
	}
	if got := rec.Header().Get(headerBalance); got != "" {
		t.Errorf("%s = %q, want absent on a rejection", headerBalance, got)
	}
}

// A metered Solana call is charged its own class, not the generic standard
// weight: getProgramAccounts is heavy and getBalance is standard. This is the
// end-to-end proof that the table the gateway ships prices the chain it
// actually proxies - the unit tests in internal/meter only prove the table.
func TestSlotfareHeadersPriceSolanaMethods(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		charge uint64
	}{
		{
			name:   "getBalance is standard",
			body:   `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So11111111111111111111111111111111111111112"]}`,
			charge: 2 * meter.MicroCreditsPerCredit,
		},
		{
			name:   "getProgramAccounts is heavy",
			body:   `{"jsonrpc":"2.0","id":1,"method":"getProgramAccounts","params":["TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"]}`,
			charge: 20 * meter.MicroCreditsPerCredit,
		},
		{
			name:   "getSlot is trivial",
			body:   `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[]}`,
			charge: 1 * meter.MicroCreditsPerCredit,
		},
		{
			name:   "getBlock with full transactions is heavy",
			body:   `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100,{"transactionDetails":"full"}]}`,
			charge: 20 * meter.MicroCreditsPerCredit,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const secret = "slotfare-secret-001"
			minted := uint64(1000 * meter.MicroCreditsPerCredit)
			stub := newStubUpstream(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			h, k, ledger := newTestHandler(t, secret, minted, meter.DefaultWeights(), stub.url())

			req := httptest.NewRequest("POST", "/rpc", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(headerCreditsCharged); got != fmt.Sprintf("%d", c.charge) {
				t.Errorf("%s = %q, want %d (the method's own weight)", headerCreditsCharged, got, c.charge)
			}
			wantBalance := ledger.Snapshot()[k.AccountID].Spendable()
			if got := rec.Header().Get(headerBalance); got != fmt.Sprintf("%d", wantBalance) {
				t.Errorf("%s = %q, want %d (spendable after settlement)", headerBalance, got, wantBalance)
			}
		})
	}
}
