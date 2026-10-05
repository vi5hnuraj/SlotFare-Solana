package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"slotfare/internal/meter"
)

// catalogModels is the two-model test catalog.
var catalogModels = meter.Models{
	{ID: "mistral-nemotron", UpstreamID: "upstream-mistral", Name: "Mistral Nemotron",
		Pricing: meter.Pricing{UncachedInput: 20, CachedInput: 2, Output: 30}},
	{ID: "diffusiongemma-26b", UpstreamID: "upstream-gemma", Name: "Google DiffusionGemma 26B A4B",
		Pricing: meter.Pricing{UncachedInput: 50, CachedInput: 5, Output: 250}},
}

// GET /models serves the configured catalog with pricing, omits upstream
// ids, requires auth like its peers and answers 501 without WithModels.
func TestModelsRoute(t *testing.T) {
	h := New(WithSessions(testSessionStore(t)), WithModels(catalogModels))
	token := consoleTokenFor(t, h, "0xtest")

	rec := doRequest(t, h, http.MethodGet, "/models", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			UpstreamID string `json:"upstreamId"`
			Pricing    struct {
				UncachedInput uint64 `json:"uncachedInput"`
				CachedInput   uint64 `json:"cachedInput"`
				Output        uint64 `json:"output"`
			} `json:"pricing"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /models body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Models) != 2 {
		t.Fatalf("models = %+v, want 2 entries", out.Models)
	}
	want := []struct {
		id      string
		name    string
		unc     uint64
		cached  uint64
		outside uint64
	}{
		{"mistral-nemotron", "Mistral Nemotron", 20, 2, 30},
		{"diffusiongemma-26b", "Google DiffusionGemma 26B A4B", 50, 5, 250},
	}
	for i, w := range want {
		m := out.Models[i]
		if m.ID != w.id || m.Name != w.name {
			t.Errorf("model %d id/name = %q/%q, want %q/%q", i, m.ID, m.Name, w.id, w.name)
		}
		if m.Pricing.UncachedInput != w.unc || m.Pricing.CachedInput != w.cached || m.Pricing.Output != w.outside {
			t.Errorf("model %d pricing = %+v, want %d/%d/%d", i, m.Pricing, w.unc, w.cached, w.outside)
		}
		if m.UpstreamID != "" {
			t.Errorf("model %d leaked its upstream id %q; upstream ids are operator configuration", i, m.UpstreamID)
		}
	}

	// Auth and method rules match the other console routes.
	if rec := doRequest(t, h, http.MethodGet, "/models", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodPost, "/models", token, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /models: status = %d, want 405", rec.Code)
	}

	// Without a registry the route is unavailable like its peers.
	hNoModels := New(WithSessions(testSessionStore(t)))
	if rec := doRequest(t, hNoModels, http.MethodGet, "/models", consoleTokenFor(t, hNoModels, "0xtest"), nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /models without WithModels: status = %d, want 501", rec.Code)
	}

	// An empty registry serves an empty list, not an error.
	hEmpty := New(WithSessions(testSessionStore(t)), WithModels(meter.Models{}))
	rec = doRequest(t, hEmpty, http.MethodGet, "/models", consoleTokenFor(t, hEmpty, "0xtest"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models with empty registry: status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"models\":[]}\n" {
		t.Errorf("empty registry body = %q, want {\"models\":[]}", got)
	}
}

// GET /models serves the optional metadata fields verbatim when present and
// omits them entirely when absent: absent stays absent, never fabricated.
func TestModelsRouteMetadata(t *testing.T) {
	ms := meter.Models{
		{ID: "rich", UpstreamID: "upstream-rich", Name: "Rich", Vendor: "deepseek",
			ContextLength: 128000, ServingPath: "priority", Description: "A paragraph",
			Parameters: "428B MoE", Capabilities: []string{"vision", "function-calling"}, Kind: "chat",
			Moe:     func() *bool { b := true; return &b }(),
			Pricing: meter.Pricing{UncachedInput: 10, CachedInput: 1, Output: 20}},
		{ID: "bare", UpstreamID: "upstream-bare", Name: "Bare",
			Pricing: meter.Pricing{UncachedInput: 5, CachedInput: 1, Output: 10}},
	}
	h := New(WithSessions(testSessionStore(t)), WithModels(ms))
	token := consoleTokenFor(t, h, "0xtest")

	rec := doRequest(t, h, http.MethodGet, "/models", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []struct {
			ID            string   `json:"id"`
			Vendor        string   `json:"vendor"`
			ContextLength uint64   `json:"contextLength"`
			ServingPath   string   `json:"servingPath"`
			Description   string   `json:"description"`
			Parameters    string   `json:"parameters"`
			Capabilities  []string `json:"capabilities"`
			Kind          string   `json:"kind"`
			Moe           *bool    `json:"moe"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /models body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Models) != 2 {
		t.Fatalf("models = %+v, want 2 entries", out.Models)
	}
	rich := out.Models[0]
	if rich.Vendor != "deepseek" || rich.ContextLength != 128000 || rich.ServingPath != "priority" ||
		rich.Description != "A paragraph" || rich.Parameters != "428B MoE" || rich.Kind != "chat" {
		t.Errorf("rich metadata = %+v, want the verbatim values", rich)
	}
	if len(rich.Capabilities) != 2 || rich.Capabilities[0] != "vision" || rich.Capabilities[1] != "function-calling" {
		t.Errorf("rich capabilities = %v, want [vision function-calling]", rich.Capabilities)
	}
	if rich.Moe == nil || !*rich.Moe {
		t.Errorf("rich moe = %v, want true (served verbatim)", rich.Moe)
	}

	// The bare entry carries none of the metadata keys on the wire.
	body := rec.Body.String()
	// Split the two entries by their id so the bare entry's absence is
	// checked against its own object, not the whole body.
	idx := strings.Index(body, `"id":"bare"`)
	if idx < 0 {
		t.Fatalf("body does not carry the bare entry: %s", body)
	}
	bareBody := body[idx:]
	for _, key := range []string{"vendor", "contextLength", "servingPath", "description", "parameters", "capabilities", "kind", "moe"} {
		if strings.Contains(bareBody, `"`+key+`"`) {
			t.Errorf("bare entry body %q carries the key %q; absent metadata must be omitted", bareBody, key)
		}
	}
}

// GET /models serves a derived entry with exactly the explicit wire shape:
// the pricing object carries the credit prices slotfared derived from cost and
// margin at load, and the server-side facts (cost, margin) never appear in
// the body.
func TestModelsRouteDerivedPricing(t *testing.T) {
	ms, err := meter.ParseModels(`[
	 {"id":"fireworks-mistral","upstreamId":"accounts/fireworks/models/mistral-nemotron","name":"Fireworks Mistral Nemotron",
	  "cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":0.30}
	]`)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	h := New(WithSessions(testSessionStore(t)), WithModels(ms))
	token := consoleTokenFor(t, h, "0xtest")

	rec := doRequest(t, h, http.MethodGet, "/models", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []struct {
			ID      string `json:"id"`
			Pricing struct {
				UncachedInput uint64 `json:"uncachedInput"`
				CachedInput   uint64 `json:"cachedInput"`
				Output        uint64 `json:"output"`
			} `json:"pricing"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /models body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Models) != 1 {
		t.Fatalf("models = %+v, want 1 entry", out.Models)
	}
	p := out.Models[0].Pricing
	if p.UncachedInput != 26 || p.CachedInput != 3 || p.Output != 39 {
		t.Errorf("derived pricing = %+v, want 26/3/39 credits per million", p)
	}
	body := rec.Body.String()
	for _, key := range []string{`"cost"`, `"margin"`} {
		if strings.Contains(body, key) {
			t.Errorf("body %q carries the server-side key %s; cost and margin never reach the wire", body, key)
		}
	}
}

// GET /models advertises the configured base URL when one was attached, and
// omits the field entirely otherwise: a slotfared without inference keeps its
// payload shape unchanged.
func TestModelsRouteAdvertisesBaseURL(t *testing.T) {
	const base = "http://127.0.0.1:8443/infer/v1"
	h := New(WithSessions(testSessionStore(t)), WithModels(catalogModels), WithBaseURL(base))
	token := consoleTokenFor(t, h, "0xtest")

	rec := doRequest(t, h, http.MethodGet, "/models", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		BaseURL string `json:"baseUrl"`
		Models  []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /models body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if out.BaseURL != base {
		t.Errorf("baseUrl = %q, want %q (the endpoint that actually serves)", out.BaseURL, base)
	}
	if len(out.Models) != 2 {
		t.Errorf("models = %+v, want the 2 catalog entries alongside baseUrl", out.Models)
	}

	// Without WithBaseURL the key is absent, not empty.
	hNoBase := New(WithSessions(testSessionStore(t)), WithModels(catalogModels))
	rec = doRequest(t, hNoBase, http.MethodGet, "/models", consoleTokenFor(t, hNoBase, "0xtest"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /models without base URL: status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "baseUrl") {
		t.Errorf("body %q carries a baseUrl key; it must be omitted when unset", rec.Body.String())
	}
}
