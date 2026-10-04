package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"slotfare/internal/meter"
)

// fireworksModels is a registry in the Fireworks shape: slash-bearing model
// ids, a routers/ fast-tier id and a priority service tier.
var fireworksModels = meter.Models{
	{ID: "mistral-nemotron", UpstreamID: "accounts/fireworks/models/mistral-nemotron", Name: "Mistral Nemotron",
		Pricing: meter.Pricing{UncachedInput: 26, CachedInput: 3, Output: 39}, ServiceTier: "priority"},
	{ID: "mistral-nemotron-fast", UpstreamID: "accounts/fireworks/routers/mistral-nemotron-fast", Name: "Mistral Nemotron Fast",
		Pricing: meter.Pricing{UncachedInput: 52, CachedInput: 4, Output: 78}},
}

// Slash-bearing upstream ids - the models/ and routers/ path forms Fireworks
// uses - must survive the registry rewrite byte-for-byte: slashes intact,
// length intact, no escaping surprises in the forwarded body.
func TestInferenceSlashBearingUpstreamIdsSurviveRewrite(t *testing.T) {
	const secret = "slotfare-secret-001"
	for _, id := range []string{"mistral-nemotron", "mistral-nemotron-fast"} {
		want, _ := fireworksModels.Lookup(id)
		stub := newStubUpstream(t, http.StatusOK, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		usage := &recUsage{}
		h, _, _ := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, stub.url(), "", fireworksModels, usage)

		body := `{"model":"` + id + `","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body: %s", id, rec.Code, rec.Body.String())
		}
		gotBody := <-stub.bodies
		var sent struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(gotBody, &sent); err != nil {
			t.Fatalf("%s: upstream body not JSON: %v; body: %s", id, err, gotBody)
		}
		if sent.Model != want.UpstreamID {
			t.Errorf("%s: upstream received model %q, want the upstream id %q byte-for-byte", id, sent.Model, want.UpstreamID)
		}
		// The ids also round-trip through ParseModels/Lookup unchanged.
		parsed, err := meter.ParseModels(`[{"id":"` + id + `","upstreamId":"` + want.UpstreamID + `","name":"n","pricing":{"uncachedInput":1,"cachedInput":1,"output":1}}]`)
		if err != nil {
			t.Fatalf("%s: ParseModels: %v", id, err)
		}
		if got, ok := parsed.Lookup(id); !ok || got.UpstreamID != want.UpstreamID {
			t.Errorf("%s: round-trip Lookup = (%+v, %v), want (%s, true)", id, got, ok, want.UpstreamID)
		}
	}
}

// A registry entry's serviceTier rides into the rewritten body as
// service_tier; an entry without one sends no such key at all.
func TestInferenceServiceTierInjectedWhenSet(t *testing.T) {
	const secret = "slotfare-secret-001"
	respBody := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

	withTier := newStubUpstream(t, http.StatusOK, respBody)
	h, _, _ := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, withTier.url(), "", fireworksModels, &recUsage{})
	req := httptest.NewRequest("POST", "/infer/v1/chat/completions",
		strings.NewReader(`{"model":"mistral-nemotron","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with tier: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var withSent map[string]any
	if err := json.Unmarshal(<-withTier.bodies, &withSent); err != nil {
		t.Fatalf("with tier: upstream body not JSON: %v", err)
	}
	if withSent["model"] != "accounts/fireworks/models/mistral-nemotron" {
		t.Errorf("with tier: model = %v, want the registry-mapped upstream id", withSent["model"])
	}
	if withSent["service_tier"] != "priority" {
		t.Errorf("with tier: service_tier = %v, want %q", withSent["service_tier"], "priority")
	}

	// The Fast tier is just a different upstream id: no tier field on the
	// entry, no service_tier key in the forwarded body.
	noTier := newStubUpstream(t, http.StatusOK, respBody)
	h2, _, _ := newInferenceModelsHandler(t, secret, 100*meter.MicroCreditsPerCredit, noTier.url(), "", fireworksModels, &recUsage{})
	req = httptest.NewRequest("POST", "/infer/v1/chat/completions",
		strings.NewReader(`{"model":"mistral-nemotron-fast","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("without tier: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var noSent map[string]any
	if err := json.Unmarshal(<-noTier.bodies, &noSent); err != nil {
		t.Fatalf("without tier: upstream body not JSON: %v", err)
	}
	if noSent["model"] != "accounts/fireworks/routers/mistral-nemotron-fast" {
		t.Errorf("without tier: model = %v, want the routers/ upstream id", noSent["model"])
	}
	if _, present := noSent["service_tier"]; present {
		t.Errorf("without tier: service_tier = %v, want the key absent", noSent["service_tier"])
	}
}

// newSSEStub serves a fixed SSE body as text/event-stream and records the
// raw request bodies it receives, so the outbound stream_options can be
// asserted.
func newSSEStub(t *testing.T, sse string) (*httptest.Server, chan []byte) {
	t.Helper()
	bodies := make(chan []byte, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// StreamUsage opts the outbound request into stream usage reporting: a
// streaming JSON body gains stream_options.include_usage; non-streaming
// bodies and services without the flag are untouched.
func TestStreamUsageOptInInjectsStreamOptions(t *testing.T) {
	const secret = "slotfare-secret-001"
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\ndata: [DONE]\n\n"

	srvOptIn, optInBodies := newSSEStub(t, sse)
	optInURL, err := url.Parse(srvOptIn.URL)
	if err != nil {
		t.Fatal(err)
	}
	optIn, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, []Service{
		{Name: "inference", Prefix: "/infer", Upstream: optInURL, Pricer: NewInferencePricer(meter.DefaultTokenPrices()), Allow: inferenceAllowlist, StreamUsage: true},
	})
	req := httptest.NewRequest("POST", "/infer/v1/chat/completions",
		strings.NewReader(`{"stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	optIn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-in streaming: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var optSent map[string]any
	if err := json.Unmarshal(<-optInBodies, &optSent); err != nil {
		t.Fatalf("opt-in: upstream body not JSON: %v", err)
	}
	so, ok := optSent["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("opt-in: stream_options = %v, want {include_usage:true}", optSent["stream_options"])
	}

	// With the flag on but a non-streaming request: no injection either.
	req = httptest.NewRequest("POST", "/infer/v1/chat/completions",
		strings.NewReader(`{"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	optIn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-in non-streaming: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var nonStreamSent map[string]any
	if err := json.Unmarshal(<-optInBodies, &nonStreamSent); err != nil {
		t.Fatalf("opt-in non-streaming: upstream body not JSON: %v", err)
	}
	if _, present := nonStreamSent["stream_options"]; present {
		t.Errorf("opt-in non-streaming: stream_options = %v, want the key absent", nonStreamSent["stream_options"])
	}

	// Without the flag the streaming body goes out byte-for-byte: no
	// stream_options key at all, so an upstream that does not support it
	// (the NVIDIA free tier) sees the request it always saw.
	srvPlain, plainBodies := newSSEStub(t, sse)
	plainURL, err := url.Parse(srvPlain.URL)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, []Service{
		{Name: "inference", Prefix: "/infer", Upstream: plainURL, Pricer: NewInferencePricer(meter.DefaultTokenPrices()), Allow: inferenceAllowlist},
	})
	req = httptest.NewRequest("POST", "/infer/v1/chat/completions",
		strings.NewReader(`{"stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-flag streaming: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var plainSent map[string]any
	rawPlain := <-plainBodies
	if err := json.Unmarshal(rawPlain, &plainSent); err != nil {
		t.Fatalf("no-flag: upstream body not JSON: %v", err)
	}
	if _, present := plainSent["stream_options"]; present {
		t.Errorf("no-flag: stream_options = %v, want the key absent (NVIDIA stays untouched)", plainSent["stream_options"])
	}
	if !bytes.Contains(rawPlain, []byte(`"stream":true`)) {
		t.Errorf("no-flag: upstream body lost its stream flag: %s", rawPlain)
	}
}
