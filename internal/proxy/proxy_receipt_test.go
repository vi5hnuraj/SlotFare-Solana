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
	"slotfare/internal/receipt"
)

// The x-slotfare-receipt header: a compact base64 JSON envelope of the
// canonical usage fields the recorded row will hold, signed by the dedicated
// receipt keypair. Metered, non-streaming responses carry it; streaming
// responses, rejections and free paths omit it. When no receipt signer is
// attached (h.Receipt == nil) no receipt is ever emitted.

// assertReceiptMatchesRow verifies the x-slotfare-receipt header against the
// signer's address and checks every signed field against the recorded row.
func assertReceiptMatchesRow(t *testing.T, rec *httptest.ResponseRecorder, s *receipt.Signer, row meter.UsageRow) {
	t.Helper()
	value := rec.Header().Get(headerReceipt)
	if value == "" {
		t.Fatalf("%s header absent", headerReceipt)
	}
	p, ok, err := receipt.Verify(value, s.PublicKey())
	if err != nil {
		t.Fatalf("receipt.Verify: %v", err)
	}
	if !ok {
		t.Fatal("receipt does not verify against the signer's address")
	}
	want := receipt.PayloadFromRow(row)
	if p != want {
		t.Errorf("verified payload = %+v, want %+v (the recorded row's fields)", p, want)
	}
	if p.Nonce != row.Nonce || row.Nonce == "" {
		t.Errorf("nonce = %q, want %q (a non-empty server-assigned nonce on the row)", p.Nonce, row.Nonce)
	}
}

// A metered RPC call with a receipt signer carries x-slotfare-receipt, and it
// verifies against the served address with exactly the recorded row's fields.
func TestReceiptOnMeteredRPCCall(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = 100 * meter.MicroCreditsPerCredit
	stub := newStubUpstream(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	h, k, _ := newTestHandler(t, secret, minted, meter.DefaultWeights(), stub.url())
	s, err := receipt.Generate()
	if err != nil {
		t.Fatalf("receipt.Generate: %v", err)
	}
	h.Receipt = s
	usage := &recUsage{}
	h.Usage = usage

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(usage.rows) != 1 {
		t.Fatalf("recorded %d usage rows, want 1", len(usage.rows))
	}
	assertReceiptMatchesRow(t, rec, s, usage.rows[0])
	_ = k
}

// A non-streaming inference settle path carries a receipt whose credits are
// the settled true cost - the same figure the row records.
func TestReceiptOnInferenceSettlePath(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	body := `{"model":"qwen38-27b","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	respBody := `{"id":"acfb41c5...","object":"chat.completion","model":"qwen38-27b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},
   "finish_reason":"stop"}],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000,
   "prompt_tokens_details":null}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	h, _, _ := newInferenceTestHandler(t, secret, minted, stub.url())
	s, err := receipt.Generate()
	if err != nil {
		t.Fatalf("receipt.Generate: %v", err)
	}
	h.Receipt = s
	usage := &recUsage{}
	h.Usage = usage

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(usage.rows) != 1 {
		t.Fatalf("recorded %d usage rows, want 1", len(usage.rows))
	}
	assertReceiptMatchesRow(t, rec, s, usage.rows[0])
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if got := rec.Header().Get(headerCreditsCharged); got != fmt.Sprintf("%d", wantTrue) {
		t.Errorf("%s = %q, want %d (the settled true cost)", headerCreditsCharged, got, wantTrue)
	}
}

// A streaming response omits the receipt: its final usage is not knowable at
// header time, so it cannot be signed honestly.
func TestReceiptOmittedOnStreaming(t *testing.T) {
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
	h, _, _ := newInferenceTestHandler(t, secret, minted, upstream)
	s, err := receipt.Generate()
	if err != nil {
		t.Fatalf("receipt.Generate: %v", err)
	}
	h.Receipt = s

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerReceipt); got != "" {
		t.Errorf("%s = %q, want absent (streaming settles after the headers go out)", headerReceipt, got)
	}
}

// Without a receipt signer no receipt is ever emitted, even on a metered
// call: receipts are opt-in via SLOTFARE_RECEIPT_KEY.
func TestReceiptAbsentWhenSignerNil(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(headerReceipt); got != "" {
		t.Errorf("%s = %q, want absent (no signer attached)", headerReceipt, got)
	}
}

// An upstream-error rejection records an error row but omits the receipt:
// only served responses whose settled usage is recorded carry one.
func TestReceiptAbsentOnUpstreamError(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())
	s, err := receipt.Generate()
	if err != nil {
		t.Fatalf("receipt.Generate: %v", err)
	}
	h.Receipt = s

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Header().Get(headerReceipt); got != "" {
		t.Errorf("%s = %q, want absent on a rejection", headerReceipt, got)
	}
}

// Free discovery paths reserve nothing and carry no receipt either.
func TestReceiptAbsentOnFreePaths(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"data":[]}`)
	h, _, _ := newInferenceTestHandler(t, secret, 100, stub.url())
	s, err := receipt.Generate()
	if err != nil {
		t.Fatalf("receipt.Generate: %v", err)
	}
	h.Receipt = s

	for _, target := range []string{"/infer/v1/models", "/infer/health"} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body: %s", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(headerReceipt); got != "" {
			t.Errorf("%s: %s = %q, want absent (free path)", target, headerReceipt, got)
		}
	}
}
