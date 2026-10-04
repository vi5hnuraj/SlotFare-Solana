package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
)

// registryModels is the two-model test registry: modelB's schedule differs
// from the default prices so a settlement at the wrong schedule is visible.
var registryModels = meter.Models{
	{ID: "modelA", UpstreamID: "upstream-a", Name: "Model A", Pricing: meter.Pricing{UncachedInput: 1000, CachedInput: 100, Output: 3000}},
	{ID: "modelB", UpstreamID: "upstream-b", Name: "Model B", Pricing: meter.Pricing{UncachedInput: 2000, CachedInput: 200, Output: 9000}},
}

// recUsage is a UsageLogger that records every row it is asked to write.
type recUsage struct {
	rows []meter.UsageRow
}

func (r *recUsage) RecordUsage(_ context.Context, row meter.UsageRow) error {
	r.rows = append(r.rows, row)
	return nil
}

// newInferenceModelsHandler builds a Handler with one registry-aware
// inference service under /infer, the given upstream key and usage logger.
func newInferenceModelsHandler(t *testing.T, secret string, minted uint64, upstream *url.URL, upstreamKey string, models meter.Models, usage *recUsage) (*Handler, keys.Key, *meter.Ledger) {
	t.Helper()
	h, k, ledger := newMultiTestHandler(t, secret, minted, []Service{
		{Name: "inference", Prefix: "/infer", Upstream: upstream, Pricer: NewInferencePricerWithModels(meter.DefaultTokenPrices(), models), Allow: inferenceAllowlist, UpstreamKey: upstreamKey},
	})
	h.Usage = usage
	return h, k, ledger
}

// With a non-empty registry an unknown model is a clean 400 naming the
// valid ids: nothing is reserved, nothing forwarded, no usage row.
func TestInferenceUnknownModel400(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, 100, stub.url(), "", registryModels, usage)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"model":"nope","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInvalidRequest {
		t.Errorf("error code = %d, want %d", code, codeInvalidRequest)
	}
	if !strings.Contains(rec.Body.String(), "unknown model") || !strings.Contains(rec.Body.String(), "modelA") || !strings.Contains(rec.Body.String(), "modelB") {
		t.Errorf("error message %q does not name the valid ids", rec.Body.String())
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

// A body without a model field gets the same clean 400 when the registry
// is active.
func TestInferenceMissingModel400(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	usage := &recUsage{}
	h, _, _ := newInferenceModelsHandler(t, secret, 100, stub.url(), "", registryModels, usage)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"max_tokens":10,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing model") || !strings.Contains(rec.Body.String(), "modelA") {
		t.Errorf("error message %q does not name the missing model and the valid ids", rec.Body.String())
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if len(usage.rows) != 0 {
		t.Errorf("usage rows = %+v, want none", usage.rows)
	}
}

// A known model maps to its upstream id, settles at its own schedule and
// records the registry id on the usage row.
func TestInferenceKnownModelMapsAndMetersByOwnSchedule(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(20000 * meter.MicroCreditsPerCredit)
	// max_tokens of one million reserves exactly 9000 credits at modelB's
	// completion rate (the prompt estimate prices to 0 at 2000/M).
	body := `{"model":"modelB","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	// The upstream echoes its own id; the row must carry the registry id.
	respBody := `{"id":"x","object":"chat.completion","model":"upstream-b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, minted, stub.url(), "", registryModels, usage)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	gotBody := <-stub.bodies
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v; body: %s", err, gotBody)
	}
	if sent.Model != "upstream-b" {
		t.Errorf("upstream received model %q, want the mapped upstream id %q", sent.Model, "upstream-b")
	}
	// True cost at modelB's schedule: 400000 prompt at 2000/M plus
	// 300000 completion at 9000/M; the default schedule would give 1300.
	mB, _ := registryModels.Lookup("modelB")
	wantTrue := mB.Prices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if wantTrue != 3500*meter.MicroCreditsPerCredit {
		t.Fatalf("sanity: true cost = %d, want %d micro-credits", wantTrue, 3500*meter.MicroCreditsPerCredit)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (settled at modelB's schedule)", pending, wantTrue)
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Model != "modelB" {
		t.Errorf("row model = %q, want the registry id %q", row.Model, "modelB")
	}
	if row.Credits != wantTrue || row.Status != "ok" {
		t.Errorf("row credits/status = %d/%q, want %d/ok", row.Credits, row.Status, wantTrue)
	}
}

// With an empty registry any model string passes through and the default
// schedule applies: today's behavior, unchanged.
func TestInferenceEmptyRegistryPassesThrough(t *testing.T) {
	const secret = "slotfare-secret-001"
	const minted = uint64(5000 * meter.MicroCreditsPerCredit)
	body := `{"model":"anything","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	respBody := `{"id":"x","model":"anything","choices":[],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, minted, stub.url(), "", nil, usage)

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	gotBody := <-stub.bodies
	var sent struct {
		Model string `json:"model"`
	}
	json.Unmarshal(gotBody, &sent)
	if sent.Model != "anything" {
		t.Errorf("upstream received model %q, want the client's string unchanged", sent.Model)
	}
	wantTrue := meter.DefaultTokenPrices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (default schedule)", pending, wantTrue)
	}
	if len(usage.rows) != 1 || usage.rows[0].Model != "anything" {
		t.Errorf("usage rows = %+v, want one row with the upstream-reported model", usage.rows)
	}
}

// Free discovery paths bypass validation even with a non-empty registry:
// no model required, zero cost, forwarded byte-for-byte.
func TestInferenceRegistryFreePathsUnvalidated(t *testing.T) {
	const secret = "slotfare-secret-001"
	body := `{"whatever":true}`
	stub := newStubUpstream(t, http.StatusOK, `{"data":[]}`)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, 100, stub.url(), "", registryModels, usage)

	req := httptest.NewRequest("POST", "/infer/v1/models", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-stub.bodies; !strings.Contains(string(got), `"whatever":true`) {
		t.Errorf("upstream body = %s, want it unchanged", got)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
		t.Errorf("pending = %d, want 0 (free path)", pending)
	}
}

// Upstream key injection: the outbound bearer is the configured upstream
// key, never the client's Slotfare key; without one no Authorization header
// goes out at all.
func TestInferenceUpstreamKeyInjection(t *testing.T) {
	const secret = "slotfare-secret-001"
	const upstreamKey = "nvapi-demo-key"
	respBody := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

	withKey := newStubUpstream(t, http.StatusOK, respBody)
	h, _, _ := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, withKey.url(), upstreamKey, registryModels, &recUsage{})
	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"model":"modelA","max_tokens":10,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with key: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-withKey.auths; got != "Bearer "+upstreamKey {
		t.Errorf("outbound Authorization = %q, want %q (the client's key must not leak)", got, "Bearer "+upstreamKey)
	}

	withoutKey := newStubUpstream(t, http.StatusOK, respBody)
	h2, _, _ := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, withoutKey.url(), "", registryModels, &recUsage{})
	req = httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"model":"modelA","max_tokens":10,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("without key: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-withoutKey.auths; got != "" {
		t.Errorf("outbound Authorization = %q, want absent when no upstream key is configured", got)
	}
}

// The documented inference path, end to end: the advertised base is
// <root>/<prefix>/v1, so a key holder who curls it for chat completions
// reaches the registry-validated upstream. This pins that today's /infer
// prefix and the OpenAI /v1 suffix cannot drift apart silently - the same
// handler that answers <base>/chat/completions is what the proxy routes.
func TestDocumentedInferencePathEndToEnd(t *testing.T) {
	const secret = "slotfare-secret-001"
	const upstreamKey = "nvapi-demo-key"
	const minted = uint64(20000 * meter.MicroCreditsPerCredit)
	// max_tokens of one million reserves 9000 at modelB's completion rate,
	// well above the true cost, so the settle must release the difference.
	body := `{"model":"modelB","max_tokens":1000000,"messages":[{"role":"user","content":"say OK"}]}`
	respBody := `{"id":"x","object":"chat.completion","model":"upstream-b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000}}`
	stub := newStubUpstream(t, http.StatusOK, respBody)
	usage := &recUsage{}
	h, k, ledger := newInferenceModelsHandler(t, secret, minted, stub.url(), upstreamKey, registryModels, usage)

	// The documented path: advertised base http://.../infer/v1 plus
	// /chat/completions.
	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if n := stub.requests.Load(); n != 1 {
		t.Fatalf("upstream received %d requests, want exactly one", n)
	}
	if got := <-stub.paths; got != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want %q (prefix stripped)", got, "/v1/chat/completions")
	}
	gotBody := <-stub.bodies
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v; body: %s", err, gotBody)
	}
	if sent.Model != "upstream-b" {
		t.Errorf("upstream received model %q, want the registry-mapped upstream id %q", sent.Model, "upstream-b")
	}
	if got := <-stub.auths; got != "Bearer "+upstreamKey {
		t.Errorf("outbound Authorization = %q, want %q (the client's key must not leak)", got, "Bearer "+upstreamKey)
	}
	// Metering settled at modelB's own schedule: 400000 prompt at 2000/M
	// plus 300000 completion at 9000/M; the reservation's excess released.
	mB, _ := registryModels.Lookup("modelB")
	wantTrue := mB.Prices().Settle(meter.Usage{PromptTokens: 400_000, CompletionTokens: 300_000})
	if pending := ledger.PendingTotal()[k.AccountID]; pending != wantTrue {
		t.Errorf("pending after settle = %d, want %d (settled at modelB's schedule)", pending, wantTrue)
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Service != "inference" || row.Model != "modelB" || row.Status != "ok" {
		t.Errorf("row service/model/status = %q/%q/%q, want inference/modelB/ok", row.Service, row.Model, row.Status)
	}
	if row.PromptTokens != 400_000 || row.CompletionTokens != 300_000 || row.Credits != wantTrue {
		t.Errorf("row tokens/credits = %d/%d/%d, want 400000/300000/%d", row.PromptTokens, row.CompletionTokens, row.Credits, wantTrue)
	}

	// Drift guard: without the /infer prefix the OpenAI path does not
	// exist on this slotfared - it is a clean 404 naming the available
	// prefixes, so a tester who copies a bare /v1 base gets an error that
	// says where the real base is, not a silent wrong endpoint.
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no-prefix path: status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeNotFound {
		t.Errorf("no-prefix path: error code = %d, want %d", code, codeNotFound)
	}
	if !strings.Contains(rec.Body.String(), "/infer") {
		t.Errorf("no-prefix path: error %q does not name the available prefixes", rec.Body.String())
	}
	if n := stub.requests.Load(); n != 1 {
		t.Errorf("upstream received %d requests after the no-prefix attempt, want still 1", n)
	}

	// An unknown key is refused on the documented path like anywhere else.
	req = httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer not-a-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}
