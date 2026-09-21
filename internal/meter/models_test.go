package meter

import (
	"os"
	"strings"
	"testing"
)

// demoModelsJSON is the 2026-08-25 demo catalog, used here only as parse
// input: the values ship as the SLOTFARE_MODELS environment value, not in
// code.
const demoModelsJSON = `[
 {"id":"mistral-nemotron","upstreamId":"mistralai/mistral-nemotron","name":"Mistral Nemotron",
  "pricing":{"uncachedInput":20,"cachedInput":2,"output":30}},
 {"id":"diffusiongemma-26b","upstreamId":"google/diffusiongemma-26b-a4b-it","name":"Google DiffusionGemma 26B A4B",
  "pricing":{"uncachedInput":50,"cachedInput":5,"output":250}},
 {"id":"nv-embedqa-e5-v5","upstreamId":"nvidia/nv-embedqa-e5-v5","name":"NV EmbedQA E5 v5",
  "vendor":"nvidia","kind":"embeddings",
  "pricing":{"uncachedInput":10}}
]`

func TestParseModelsValid(t *testing.T) {
	ms, err := ParseModels(demoModelsJSON)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("len = %d, want 3; models: %+v", len(ms), ms)
	}
	want := []Model{
		{ID: "mistral-nemotron", UpstreamID: "mistralai/mistral-nemotron", Name: "Mistral Nemotron",
			Pricing: Pricing{UncachedInput: 20, CachedInput: 2, Output: 30}, Vendor: "mistralai"},
		{ID: "diffusiongemma-26b", UpstreamID: "google/diffusiongemma-26b-a4b-it", Name: "Google DiffusionGemma 26B A4B",
			Pricing: Pricing{UncachedInput: 50, CachedInput: 5, Output: 250}, Vendor: "google"},
		{ID: "nv-embedqa-e5-v5", UpstreamID: "nvidia/nv-embedqa-e5-v5", Name: "NV EmbedQA E5 v5",
			Pricing: Pricing{UncachedInput: 10}, Vendor: "nvidia", Kind: "embeddings"},
	}
	for i := range want {
		if ms[i].ID != want[i].ID || ms[i].UpstreamID != want[i].UpstreamID ||
			ms[i].Name != want[i].Name || ms[i].Pricing != want[i].Pricing ||
			ms[i].ServiceTier != want[i].ServiceTier || ms[i].Vendor != want[i].Vendor ||
			ms[i].ContextLength != want[i].ContextLength || ms[i].ServingPath != want[i].ServingPath ||
			ms[i].Description != want[i].Description || ms[i].Parameters != want[i].Parameters ||
			ms[i].Kind != want[i].Kind || len(ms[i].Capabilities) != len(want[i].Capabilities) {
			t.Errorf("model %d = %+v, want %+v", i, ms[i], want[i])
		}
	}
	if got := ms.IDs(); len(got) != 3 || got[0] != "mistral-nemotron" || got[1] != "diffusiongemma-26b" || got[2] != "nv-embedqa-e5-v5" {
		t.Errorf("IDs() = %v, want the configuration order", got)
	}
	m, ok := ms.Lookup("diffusiongemma-26b")
	if !ok || m.UpstreamID != "google/diffusiongemma-26b-a4b-it" {
		t.Errorf("Lookup(diffusiongemma-26b) = (%+v, %v)", m, ok)
	}
	if _, ok := ms.Lookup("nope"); ok {
		t.Error("Lookup(nope) found an entry, want none")
	}
	p := ms[1].Prices()
	if p.PromptPerMillion != 50 || p.CompletionPerMillion != 250 || p.CachedPromptPerMillion != 5 {
		t.Errorf("Prices() = %+v, want prompt 50 completion 250 cached 5", p)
	}
	if p.DefaultMaxTokens != DefaultTokenPrices().DefaultMaxTokens {
		t.Errorf("Prices().DefaultMaxTokens = %d, want the compiled-in default %d",
			p.DefaultMaxTokens, DefaultTokenPrices().DefaultMaxTokens)
	}
}

func TestParseModelsEmpty(t *testing.T) {
	for _, raw := range []string{"", " \n\t"} {
		ms, err := ParseModels(raw)
		if err != nil {
			t.Errorf("ParseModels(%q): unexpected error %v", raw, err)
		}
		if len(ms) != 0 {
			t.Errorf("ParseModels(%q) = %+v, want an empty registry", raw, ms)
		}
	}
}

// marginModelsJSON is a derived catalog: the first entry prices itself from
// Fireworks provider cost at a 30 percent margin, the second from provider
// cost at zero margin (price equals cost).
const marginModelsJSON = `[
 {"id":"fireworks-mistral","upstreamId":"accounts/fireworks/models/mistral-nemotron","name":"Fireworks Mistral Nemotron",
  "cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":0.30},
 {"id":"exact","upstreamId":"some/provider","name":"Zero Margin",
  "cost":{"uncachedInputUsd":1.234,"cachedInputUsd":0.123,"outputUsd":3.0},"margin":0}
]`

func TestParseModelsMarginDerived(t *testing.T) {
	ms, err := ParseModels(marginModelsJSON)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d, want 2; models: %+v", len(ms), ms)
	}
	// 0.02*1.3*1000 = 26, 0.002*1.3*1000 = 2.6 rounds UP to 3,
	// 0.03*1.3*1000 = 39.
	if got := ms[0].Pricing; got != (Pricing{UncachedInput: 26, CachedInput: 3, Output: 39}) {
		t.Errorf("model 0 pricing = %+v, want 26/3/39 credits per million", got)
	}
	// Zero margin prices exactly at cost: 1.234 -> 1234, 0.123 -> 123, 3.0 -> 3000.
	if got := ms[1].Pricing; got != (Pricing{UncachedInput: 1234, CachedInput: 123, Output: 3000}) {
		t.Errorf("model 1 pricing = %+v, want 1234/123/3000 credits per million", got)
	}
	if got := ms.IDs(); len(got) != 2 || got[0] != "fireworks-mistral" || got[1] != "exact" {
		t.Errorf("IDs() = %v, want the configuration order", got)
	}
	if m, ok := ms.Lookup("fireworks-mistral"); !ok || m.UpstreamID != "accounts/fireworks/models/mistral-nemotron" {
		t.Errorf("Lookup(fireworks-mistral) = (%+v, %v)", m, ok)
	}
	// The derived schedule settles in micro-credits at the same unit as an
	// explicit one: one million prompt tokens at 26 credits per million is
	// exactly 26 * MicroCreditsPerCredit.
	settled := ms[0].Prices().Settle(Usage{PromptTokens: 1_000_000})
	if want := uint64(26) * MicroCreditsPerCredit; settled != want {
		t.Errorf("Settle(1M prompt) = %d micro-credits, want %d", settled, want)
	}
}

// serviceTier is an optional operator field: entries that carry it parse to
// the tier, entries without it stay empty, and it composes with both
// pricing forms.
func TestParseModelsServiceTier(t *testing.T) {
	raw := `[
  {"id":"prio","upstreamId":"accounts/fireworks/models/mistral-nemotron","name":"Priority",
   "serviceTier":"priority",
   "cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":0.30},
  {"id":"plain","upstreamId":"accounts/fireworks/routers/mistral-nemotron-fast","name":"Fast",
   "pricing":{"uncachedInput":52,"cachedInput":4,"output":78}}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if ms[0].ServiceTier != "priority" {
		t.Errorf("model 0 ServiceTier = %q, want %q", ms[0].ServiceTier, "priority")
	}
	if ms[1].ServiceTier != "" {
		t.Errorf("model 1 ServiceTier = %q, want empty (no tier configured)", ms[1].ServiceTier)
	}
	// The tier composes with the derived pricing: prices are unchanged by
	// the extra field.
	if got := ms[0].Pricing; got != (Pricing{UncachedInput: 26, CachedInput: 3, Output: 39}) {
		t.Errorf("model 0 pricing = %+v, want 26/3/39 (tier must not touch pricing)", got)
	}
}

// The mainnet runbook's SLOTFARE_MODELS sample, verbatim: it must parse and
// carry the ids, tiers and derived prices the doc states, so the document
// cannot drift from ParseModels.
func TestParseModelsMainnetRunbookSample(t *testing.T) {
	raw := `[
 {"id":"mistral-nemotron","upstreamId":"accounts/fireworks/models/mistral-nemotron",
  "name":"Mistral Nemotron","serviceTier":"priority",
  "cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},
  "margin":0.30},
 {"id":"mistral-nemotron-fast","upstreamId":"accounts/fireworks/routers/mistral-nemotron-fast",
  "name":"Mistral Nemotron Fast",
  "cost":{"uncachedInputUsd":0.04,"cachedInputUsd":0.004,"outputUsd":0.06},
  "margin":0.30}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels(mainnet sample): %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d, want 2; models: %+v", len(ms), ms)
	}
	m0, m1 := ms[0], ms[1]
	if m0.ID != "mistral-nemotron" || m0.UpstreamID != "accounts/fireworks/models/mistral-nemotron" ||
		m0.ServiceTier != "priority" {
		t.Errorf("entry 0 = %+v, want the priority-tier models/ entry", m0)
	}
	// The doc states 26 / 3 / 39 explicitly.
	if got := m0.Pricing; got != (Pricing{UncachedInput: 26, CachedInput: 3, Output: 39}) {
		t.Errorf("entry 0 pricing = %+v, want 26/3/39 as the doc states", got)
	}
	if m1.ID != "mistral-nemotron-fast" || m1.UpstreamID != "accounts/fireworks/routers/mistral-nemotron-fast" ||
		m1.ServiceTier != "" {
		t.Errorf("entry 1 = %+v, want the tierless routers/ fast entry", m1)
	}
	// 0.04*1.3*1000 = 52, 0.004*1.3*1000 = 5.2 rounds up to 6,
	// 0.06*1.3*1000 = 78.
	if got := m1.Pricing; got != (Pricing{UncachedInput: 52, CachedInput: 6, Output: 78}) {
		t.Errorf("entry 1 pricing = %+v, want the derived form of 0.04/0.004/0.06 at 0.30 margin", got)
	}
}

// Non-chat kinds price input-only: the cached and output components are
// optional (absent means zero) in both pricing forms, and a present
// nonzero value is a startup config error naming the form's own field.
func TestParseModelsNonChatPricing(t *testing.T) {
	// An embeddings entry with only uncachedInput parses to 100/0/0.
	ms, err := ParseModels(`[
		{"id":"e","upstreamId":"fw/embed-8b","name":"E","kind":"embeddings",
		 "pricing":{"uncachedInput":100}}
	]`)
	if err != nil {
		t.Fatalf("ParseModels(input-only): %v", err)
	}
	if got := ms[0].Pricing; got != (Pricing{UncachedInput: 100}) {
		t.Errorf("embeddings pricing = %+v, want 100/0/0", got)
	}
	if !ms[0].Prices().InputOnly {
		t.Error("embeddings Prices().InputOnly = false, want true")
	}
	// A reranker entry derives input-only from the cost form: 0.20 at zero
	// margin is exactly 200 credits per million.
	ms, err = ParseModels(`[
		{"id":"r","upstreamId":"fw/rerank-8b","name":"R","kind":"reranker",
		 "cost":{"uncachedInputUsd":0.20},"margin":0}
	]`)
	if err != nil {
		t.Fatalf("ParseModels(cost-form reranker): %v", err)
	}
	if got := ms[0].Pricing; got != (Pricing{UncachedInput: 200}) {
		t.Errorf("reranker pricing = %+v, want 200/0/0", got)
	}
	if !ms[0].Prices().InputOnly {
		t.Error("reranker Prices().InputOnly = false, want true")
	}
	// A chat entry keeps today's all-three-keys-required behavior.
	cases := []struct {
		raw   string
		field string
	}{
		{`{"id":"e","upstreamId":"u","name":"n","kind":"embeddings","pricing":{"uncachedInput":10,"output":50}}`, "pricing.output"},
		{`{"id":"e","upstreamId":"u","name":"n","kind":"embeddings","pricing":{"uncachedInput":10,"cachedInput":5}}`, "pricing.cachedInput"},
		{`{"id":"r","upstreamId":"u","name":"n","kind":"reranker","cost":{"uncachedInputUsd":0.20,"outputUsd":0.01},"margin":0}`, "cost.outputUsd"},
		{`{"id":"r","upstreamId":"u","name":"n","kind":"reranker","cost":{"uncachedInputUsd":0.20,"cachedInputUsd":0.01},"margin":0}`, "cost.cachedInputUsd"},
		{`{"id":"c","upstreamId":"u","name":"n","pricing":{"uncachedInput":10}}`, "cachedInput"},
	}
	for _, c := range cases {
		_, err := ParseModels(`[` + c.raw + `]`)
		if err == nil {
			t.Errorf("raw %s: error = nil, want one naming %q", c.raw, c.field)
			continue
		}
		if !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), "models[0]") {
			t.Errorf("raw %s: error %q does not name models[0] and %q", c.raw, err, c.field)
		}
	}
}

// The mainnet runbook's non-chat sample, verbatim: Fireworks' Qwen3
// Embedding 8B and Qwen3 Reranker 8B serverless entries, priced from
// provider cost at zero margin - $0.10/M derives to 100 credits/M and
// $0.20/M to 200 credits/M. It must parse so the document cannot drift
// from ParseModels.
func TestParseModelsMainnetRunbookNonChatSample(t *testing.T) {
	raw := `[
 {"id":"qwen3-embedding-8b","upstreamId":"accounts/fireworks/models/qwen3-embedding-8b",
  "name":"Qwen3 Embedding 8B","vendor":"fireworks","kind":"embeddings",
  "cost":{"uncachedInputUsd":0.10,"cachedInputUsd":0.0,"outputUsd":0.0},
  "margin":0},
 {"id":"qwen3-reranker-8b","upstreamId":"accounts/fireworks/models/qwen3-reranker-8b",
  "name":"Qwen3 Reranker 8B","vendor":"fireworks","kind":"reranker",
  "cost":{"uncachedInputUsd":0.20,"cachedInputUsd":0.0,"outputUsd":0.0},
  "margin":0}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels(mainnet non-chat sample): %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("len = %d, want 2; models: %+v", len(ms), ms)
	}
	m0, m1 := ms[0], ms[1]
	if m0.ID != "qwen3-embedding-8b" || m0.Kind != "embeddings" || m0.Vendor != "fireworks" {
		t.Errorf("entry 0 = %+v, want the embeddings entry with vendor fireworks", m0)
	}
	if got := m0.Pricing; got != (Pricing{UncachedInput: 100}) {
		t.Errorf("entry 0 pricing = %+v, want 100/0/0 as the doc states", got)
	}
	if m1.ID != "qwen3-reranker-8b" || m1.Kind != "reranker" || m1.Vendor != "fireworks" {
		t.Errorf("entry 1 = %+v, want the reranker entry with vendor fireworks", m1)
	}
	if got := m1.Pricing; got != (Pricing{UncachedInput: 200}) {
		t.Errorf("entry 1 pricing = %+v, want 200/0/0 as the doc states", got)
	}
	// Both kinds settle input-only: one million prompt tokens prices at
	// the input rate, with no completion or cached component anywhere.
	if got := m0.Prices().Settle(Usage{PromptTokens: 1_000_000}); got != uint64(100)*MicroCreditsPerCredit {
		t.Errorf("embeddings Settle(1M prompt) = %d, want %d micro-credits", got, uint64(100)*MicroCreditsPerCredit)
	}
	if got := m1.Prices().Settle(Usage{PromptTokens: 1_000_000}); got != uint64(200)*MicroCreditsPerCredit {
		t.Errorf("reranker Settle(1M prompt) = %d, want %d micro-credits", got, uint64(200)*MicroCreditsPerCredit)
	}
}

// Every malformed pricing-form case must name the offending entry and field.
func TestParseModelsPricingFormsErrors(t *testing.T) {
	const base = `{"id":"a","upstreamId":"u","name":"n"`
	cases := []struct {
		raw   string
		field string
	}{
		{base + `,"pricing":{"uncachedInput":1,"cachedInput":1,"output":1},"cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03}}`, "both pricing and cost"},
		{base + `,"pricing":{"uncachedInput":1,"cachedInput":1,"output":1},"margin":0.30}`, "both pricing and cost"},
		{base + `,"cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03}}`, "margin"},
		{base + `,"margin":0.30}`, "cost"},
		{base + `,"cost":{"cachedInputUsd":0.002,"outputUsd":0.03},"margin":0.30}`, "uncachedInputUsd"},
		{base + `,"cost":{"uncachedInputUsd":-0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":0.30}`, "uncachedInputUsd"},
		{base + `,"cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":-0.1}`, "margin"},
		{base + `,"cost":{"uncachedInputUsd":0.02,"cachedInputUsd":0.002,"outputUsd":0.03},"margin":"x"}`, "margin"},
		{base + `,"cost":"nope","margin":0.30}`, "cost"},
		{base + `}`, "pricing"},
	}
	for _, c := range cases {
		_, err := ParseModels(`[` + c.raw + `]`)
		if err == nil {
			t.Errorf("raw %s: error = nil, want one naming %q", c.raw, c.field)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("raw %s: error %q does not name %q", c.raw, err, c.field)
		}
		if !strings.Contains(err.Error(), "models[0]") {
			t.Errorf("raw %s: error %q does not name models[0]", c.raw, err)
		}
	}
}

// The metadata fields are optional console display/filter data: an entry
// carrying all seven parses to the exact stored values, and the fields do
// not touch pricing or id handling.
func TestParseModelsMetadataPresent(t *testing.T) {
	raw := `[
  {"id":"m","upstreamId":"some/provider","name":"N",
   "vendor":"deepseek","contextLength":128000,"servingPath":"priority",
   "description":"A short paragraph","parameters":"428B MoE",
   "capabilities":["vision","function-calling"],"kind":"embeddings",
   "pricing":{"uncachedInput":10}}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("len = %d, want 1; models: %+v", len(ms), ms)
	}
	m := ms[0]
	if m.Vendor != "deepseek" || m.ContextLength != 128000 || m.ServingPath != "priority" ||
		m.Description != "A short paragraph" || m.Parameters != "428B MoE" || m.Kind != "embeddings" {
		t.Errorf("metadata = vendor %q ctx %d path %q desc %q params %q kind %q, want the exact stored values",
			m.Vendor, m.ContextLength, m.ServingPath, m.Description, m.Parameters, m.Kind)
	}
	if len(m.Capabilities) != 2 || m.Capabilities[0] != "vision" || m.Capabilities[1] != "function-calling" {
		t.Errorf("capabilities = %v, want [vision function-calling]", m.Capabilities)
	}
	// The metadata must not touch pricing; an embeddings entry prices
	// input-only, so absent cached/output components default to zero.
	if got := m.Pricing; got != (Pricing{UncachedInput: 10}) {
		t.Errorf("pricing = %+v, want 10/0/0 (metadata must not touch pricing)", got)
	}
}

// Absent metadata stays absent: empty strings, zero context length, nil
// capabilities. Vendor may still fall back per the derivation rule.
func TestParseModelsMetadataAbsent(t *testing.T) {
	raw := `[
  {"id":"m","upstreamId":"accounts/fireworks/models/x","name":"N",
   "pricing":{"uncachedInput":10,"cachedInput":1,"output":20}}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	m := ms[0]
	if m.Vendor != "" || m.ContextLength != 0 || m.ServingPath != "" ||
		m.Description != "" || m.Parameters != "" || m.Kind != "" {
		t.Errorf("absent metadata = %+v, want all empty/zero", m)
	}
	if m.Capabilities != nil {
		t.Errorf("absent capabilities = %v, want nil", m.Capabilities)
	}
}

// The modality slugs and the optional moe boolean parse: all six slugs are
// accepted, moe true/false round-trip verbatim, and an absent moe stays nil
// (unknown), never defaulted.
func TestParseModelsModalitiesAndMoe(t *testing.T) {
	raw := `[
	  {"id":"a","upstreamId":"u/a","name":"A",
	   "capabilities":["text","image","video","audio","vision","function-calling"],
	   "moe":true,
	   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}},
	  {"id":"b","upstreamId":"u/b","name":"B",
	   "moe":false,
	   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}},
	  {"id":"c","upstreamId":"u/c","name":"C",
	   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}
	]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("len = %d, want 3; models: %+v", len(ms), ms)
	}
	// Entry 0: all six slugs parse; moe is explicitly true.
	if len(ms[0].Capabilities) != 6 {
		t.Errorf("entry 0 capabilities = %v, want 6 slugs", ms[0].Capabilities)
	}
	if ms[0].Moe == nil || !*ms[0].Moe {
		t.Errorf("entry 0 moe = %v, want true", ms[0].Moe)
	}
	// Entry 1: moe is explicitly false.
	if ms[1].Moe == nil || *ms[1].Moe {
		t.Errorf("entry 1 moe = %v, want false", ms[1].Moe)
	}
	// Entry 2: moe absent means nil, never defaulted.
	if ms[2].Moe != nil {
		t.Errorf("entry 2 moe = %v, want nil (absent)", ms[2].Moe)
	}
}

// Vendor fallback derives from the upstream id's first path segment when it
// is a lowercase slug; multi-segment ids derive nothing; explicit always wins.
func TestParseModelsVendorFallback(t *testing.T) {
	raw := `[
  {"id":"a","upstreamId":"mistralai/mistral-nemotron","name":"A",
   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}},
  {"id":"b","upstreamId":"accounts/fireworks/models/x","name":"B",
   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}},
  {"id":"c","upstreamId":"mistralai/mistral-nemotron","name":"C","vendor":"nvidia",
   "pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}
]`
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels: %v", err)
	}
	if ms[0].Vendor != "mistralai" {
		t.Errorf("model 0 Vendor = %q, want fallback to %q", ms[0].Vendor, "mistralai")
	}
	if ms[1].Vendor != "" {
		t.Errorf("model 1 Vendor = %q, want empty (multi-segment id does not derive a vendor)", ms[1].Vendor)
	}
	if ms[2].Vendor != "nvidia" {
		t.Errorf("model 2 Vendor = %q, want explicit %q to win over fallback", ms[2].Vendor, "nvidia")
	}
}

// Each bad metadata value is a startup config error naming models[i] and the
// dotted field.
func TestParseModelsMetadataInvalid(t *testing.T) {
	const base = `{"id":"a","upstreamId":"u","name":"n","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}`
	cases := []struct {
		extra string
		field string
	}{
		{`"vendor":"DeepSeek"`, "vendor"},
		{`"vendor":"9bad"`, "vendor"},
		{`"contextLength":0`, "contextLength"},
		{`"contextLength":-5`, "contextLength"},
		{`"contextLength":"x"`, "contextLength"},
		{`"servingPath":"turbo"`, "servingPath"},
		{`"capabilities":["vision","quantized"]`, "capabilities"},
		{`"kind":"image"`, "kind"},
	}
	for _, c := range cases {
		_, err := ParseModels(`[` + base + `,` + c.extra + `}]`)
		if err == nil {
			t.Errorf("extra %s: error = nil, want one naming %q", c.extra, c.field)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("extra %s: error %q does not name %q", c.extra, err, c.field)
		}
		if !strings.Contains(err.Error(), "models[0]") {
			t.Errorf("extra %s: error %q does not name models[0]", c.extra, err)
		}
	}
}

// The env example's SLOTFARE_MODELS must parse and carry the vendors
// and context lengths the doc annotates, so the example file and the parser
// cannot drift.
func TestEnvExampleMeterdModels(t *testing.T) {
	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	var raw string
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "SLOTFARE_MODELS="); idx >= 0 {
			raw = line[idx+len("SLOTFARE_MODELS="):]
			break
		}
	}
	if raw == "" {
		t.Fatal("SLOTFARE_MODELS= line not found in .env.example")
	}
	ms, err := ParseModels(raw)
	if err != nil {
		t.Fatalf("ParseModels(env example): %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("len = %d, want 3; models: %+v", len(ms), ms)
	}
	if ms[0].ID != "mistral-nemotron" || ms[0].Vendor != "mistralai" || ms[0].ContextLength != 131072 {
		t.Errorf("entry 0 = %+v, want vendor mistralai with context length 131072", ms[0])
	}
	if ms[1].ID != "diffusiongemma-26b" || ms[1].Vendor != "google" || ms[1].ContextLength != 32768 {
		t.Errorf("entry 1 = %+v, want vendor google with context length 32768", ms[1])
	}
	if ms[2].ID != "nv-embedqa-e5-v5" || ms[2].Vendor != "nvidia" || ms[2].Kind != "embeddings" {
		t.Errorf("entry 2 = %+v, want the embeddings entry with kind and vendor nvidia", ms[2])
	}
	if got := ms[2].Pricing; got != (Pricing{UncachedInput: 10}) {
		t.Errorf("entry 2 pricing = %+v, want input-only 10/0/0", got)
	}
	// The enriched chat entries carry their modality slugs and moe value;
	// the embeddings entry carries neither.
	if len(ms[0].Capabilities) != 2 || ms[0].Capabilities[0] != "text" || ms[0].Capabilities[1] != "image" {
		t.Errorf("entry 0 capabilities = %v, want [text image]", ms[0].Capabilities)
	}
	if ms[0].Moe == nil || *ms[0].Moe {
		t.Errorf("entry 0 moe = %v, want false", ms[0].Moe)
	}
	if len(ms[1].Capabilities) != 3 || ms[1].Capabilities[0] != "text" ||
		ms[1].Capabilities[1] != "image" || ms[1].Capabilities[2] != "video" {
		t.Errorf("entry 1 capabilities = %v, want [text image video]", ms[1].Capabilities)
	}
	if ms[1].Moe == nil || !*ms[1].Moe {
		t.Errorf("entry 1 moe = %v, want true", ms[1].Moe)
	}
	if ms[2].Capabilities != nil || ms[2].Moe != nil {
		t.Errorf("entry 2 = %+v, want no capabilities and moe absent", ms[2])
	}
}

// Every malformed case must name the offending field.
func TestParseModelsMalformed(t *testing.T) {
	cases := []struct {
		raw   string
		field string
	}{
		{`{}`, "not JSON"},
		{`"x"`, "not JSON"},
		{`{"id":"a"}`, "not JSON"},
		{`[{"id":"a"}]`, "upstreamId"},
		{`[{"id":"","upstreamId":"u","name":"n","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}]`, "id"},
		{`[{"id":"a","upstreamId":"u","name":"n","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}},{"id":"a","upstreamId":"u2","name":"n2","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}]`, "duplicate"},
		{`[{"id":"a","upstreamId":"","name":"n","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}]`, "upstreamId"},
		{`[{"id":"a","upstreamId":"u","name":"","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}]`, "name"},
		{`[{"id":"a","upstreamId":"u","name":"n"}]`, "pricing"},
		{`[{"id":"a","upstreamId":"u","name":"n","pricing":{"uncachedInput":1,"cachedInput":1}}]`, "output"},
		{`[{"id":"a","upstreamId":"u","name":"n","pricing":{"uncachedInput":"x","cachedInput":1,"output":1}}]`, "uncachedInput"},
		{`[{"id":"a","upstreamId":"u","name":"n","pricing":{"uncachedInput":-1,"cachedInput":1,"output":1}}]`, "uncachedInput"},
	}
	for _, c := range cases {
		_, err := ParseModels(c.raw)
		if err == nil {
			t.Errorf("raw %s: error = nil, want one naming %q", c.raw, c.field)
			continue
		}
		if !strings.Contains(err.Error(), c.field) {
			t.Errorf("raw %s: error %q does not name %q", c.raw, err, c.field)
		}
	}
}
