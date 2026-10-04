package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"slotfare/internal/meter"
)

// nonChatRegistry is the parsed registry these tests run against: one
// embeddings entry, one reranker entry and one chat entry, so a regression
// that drops the completion component anywhere is visible.
func nonChatRegistry(t *testing.T) meter.Models {
	t.Helper()
	models, err := meter.ParseModels(`[
 {"id":"embed","upstreamId":"fw/embed-8b","name":"E","kind":"embeddings",
  "pricing":{"uncachedInput":100}},
 {"id":"rerank","upstreamId":"fw/rerank-8b","name":"R","kind":"reranker",
  "pricing":{"uncachedInput":200}},
 {"id":"chat","upstreamId":"fw/chat","name":"C","kind":"chat",
  "pricing":{"uncachedInput":1000,"cachedInput":100,"output":3000}}
]`)
	if err != nil {
		t.Fatalf("ParseModels(non-chat registry): %v", err)
	}
	return models
}

// An embeddings call settles from the upstream's prompt_tokens/total_tokens
// usage report at the entry's input rate, rewrites the model id on the way
// out, and records a usage row labelled with the service kind.
func TestInferenceEmbeddingsSettleFromFixture(t *testing.T) {
	const secret = "slotfare-secret-001"
	respBody := `{"model":"fw/embed-8b","object":"list","data":[],
 "usage":{"prompt_tokens":120,"total_tokens":120}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	models := nonChatRegistry(t)
	h, k, ledger := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url(), "", models, usage)

	// A batch large enough that the input-only hold covers the true cost,
	// so the settle releases the difference and pending lands on it.
	body := `{"model":"embed","input":["` + strings.Repeat("a", 2000) + `","` + strings.Repeat("b", 2000) + `"]}`
	req := httptest.NewRequest("POST", "/infer/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// True cost: 120 prompt tokens at 100 credits per million - every
	// prompt token at the input rate, no completion component.
	wantTrue := uint64(120 * 100 / 100) // 120 micro-credits
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d micro-credits", pending, wantTrue)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(<-stub.bodies, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if sent.Model != "fw/embed-8b" {
		t.Errorf("upstream received model %q, want the registry-mapped upstream id %q", sent.Model, "fw/embed-8b")
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Kind != "embeddings" || row.Model != "embed" || row.PromptTokens != 120 || row.Credits != wantTrue || row.Status != "ok" {
		t.Errorf("row = %+v, want kind embeddings model embed 120 prompt tokens %d credits ok", row, wantTrue)
	}
}

// /v1/rerank is an allowed path and settles from the total_tokens-only
// usage report at the reranker entry's input rate.
func TestInferenceRerankAllowedAndSettlesFromTotalTokens(t *testing.T) {
	const secret = "slotfare-secret-001"
	respBody := `{"id":"1","model":"fw/rerank-8b","results":[],
 "usage":{"total_tokens":349}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url(), "", nonChatRegistry(t), usage)

	// A documents list large enough that the input-only hold covers the
	// true cost, so the settle releases the difference and pending lands
	// on it.
	body := `{"model":"rerank","query":"q","documents":["` + strings.Repeat("d", 3000) + `","` + strings.Repeat("e", 3000) + `"]}`
	req := httptest.NewRequest("POST", "/infer/v1/rerank", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// True cost: 349 prompt tokens (backfilled from total_tokens) at
	// 200 credits per million - 698 micro-credits.
	wantTrue := uint64(349 * 200 / 100)
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d micro-credits", pending, wantTrue)
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Kind != "reranker" || row.Model != "rerank" || row.PromptTokens != 349 || row.Credits != wantTrue {
		t.Errorf("row = %+v, want kind reranker model rerank 349 prompt tokens %d credits", row, wantTrue)
	}
}

// An embeddings response without any usage report settles the full
// reservation, and that reservation carries no completion component: it is
// strictly below the chat-shape reservation of the same body.
func TestInferenceEmbeddingsNoUsageReserveHasNoCompletionComponent(t *testing.T) {
	const secret = "slotfare-secret-001"
	respBody := `{"model":"fw/embed-8b","object":"list","data":[]}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	models := nonChatRegistry(t)
	h, k, ledger := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url(), "", models, usage)

	body := []byte(`{"model":"embed","input":["a","b"]}`)
	req := httptest.NewRequest("POST", "/infer/v1/embeddings", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// The hold: the raw input field ["a","b"] is 10 bytes, 10/4+1 = 3
	// tokens at 100 credits per million - 3 micro-credits, and nothing else.
	embed, _ := models.Lookup("embed")
	reserved, err := embed.Prices().Reserve(body)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if reserved != 3 {
		t.Fatalf("sanity: input-only reserve = %d, want 3 micro-credits", reserved)
	}
	chat, _ := models.Lookup("chat")
	chatReserved, err := chat.Prices().Reserve(body)
	if err != nil {
		t.Fatalf("chat Reserve: %v", err)
	}
	if reserved >= chatReserved {
		t.Errorf("input-only reserve %d must be strictly below the chat-shape reserve %d (no default-cap component)", reserved, chatReserved)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != reserved {
		t.Errorf("pending after no-usage response = %d, want the full input-only reservation %d", pending, reserved)
	}
	if len(usage.rows) != 1 || usage.rows[0].Kind != "embeddings" {
		t.Errorf("usage rows = %+v, want one row labelled embeddings", usage.rows)
	}
}

// An unknown model on /v1/embeddings gets the same honest error as on chat:
// a clean 400 naming the valid ids, nothing forwarded, nothing reserved.
func TestInferenceEmbeddingsUnknownModel400(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, 100, stub.url(), "", nonChatRegistry(t), usage)

	req := httptest.NewRequest("POST", "/infer/v1/embeddings", strings.NewReader(`{"model":"nope","input":["a"]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInvalidRequest {
		t.Errorf("error code = %d, want %d", code, codeInvalidRequest)
	}
	if !strings.Contains(rec.Body.String(), `unknown model`) || !strings.Contains(rec.Body.String(), "nope") || !strings.Contains(rec.Body.String(), "embed") {
		t.Errorf("error message %q does not name the unknown model and the valid ids", rec.Body.String())
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
		t.Errorf("pending = %d, want 0 (nothing reserved)", pending)
	}
	if len(usage.rows) != 0 {
		t.Errorf("usage rows = %+v, want none", usage.rows)
	}
}

// Chat regression in the same registry: a chat entry still reserves and
// settles with the completion component, so dropping it anywhere is caught
// by one exact assertion.
func TestInferenceChatStillSettlesWithCompletionComponent(t *testing.T) {
	const secret = "slotfare-secret-001"
	respBody := `{"id":"x","model":"fw/chat","choices":[],
 "usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	models := nonChatRegistry(t)
	h, k, ledger := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url(), "", models, usage)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"model":"chat","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// True cost: 100 prompt at 1000/M (1000) plus 50 completion at
	// 3000/M (1500) - 2500 micro-credits; the reservation's excess released.
	chat, _ := models.Lookup("chat")
	wantTrue := chat.Prices().Settle(meter.Usage{PromptTokens: 100, CompletionTokens: 50})
	if wantTrue != 2500 {
		t.Fatalf("sanity: true cost = %d, want 2500 micro-credits", wantTrue)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (the completion component must still exist)", pending, wantTrue)
	}
	if len(usage.rows) != 1 || usage.rows[0].Kind != "" {
		t.Errorf("usage rows = %+v, want one row with empty kind for chat", usage.rows)
	}
}
