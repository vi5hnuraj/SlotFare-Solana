package meter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultTokenPricesValues(t *testing.T) {
	p := DefaultTokenPrices()
	if p.PromptPerMillion != 1000 || p.CompletionPerMillion != 3000 ||
		p.CachedPromptPerMillion != 100 || p.DefaultMaxTokens != 2048 {
		t.Fatalf("DefaultTokenPrices() = %+v, want {1000 3000 100 2048}", p)
	}
}

// Reserve uses max_tokens when present, priced at the completion rate on
// top of the prompt estimate.
func TestReserveUsesMaxTokensWhenPresent(t *testing.T) {
	p := DefaultTokenPrices()
	raw := []byte(`[{"role":"user","content":"` + strings.Repeat("x", 4000) + `"}]`)
	body := append(append([]byte(`{"model":"qwen38-27b","max_tokens":1000,"messages":`), raw...), '}')
	want := tokenCost(uint64(len(raw))/4+1, p.PromptPerMillion) +
		tokenCost(1000, p.CompletionPerMillion)
	got, err := p.Reserve(body)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got != want {
		t.Errorf("Reserve = %d, want %d micro-credits (prompt estimate plus max_tokens at the completion rate)", got, want)
	}
}

// Absent, explicit zero and non-numeric max_tokens all reserve against the
// default cap.
func TestReserveFallsBackToDefaultCap(t *testing.T) {
	p := DefaultTokenPrices()
	bodies := []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`,
		`{"max_tokens":"lots","messages":[{"role":"user","content":"hi"}]}`,
	}
	want := tokenCost(9, p.PromptPerMillion) + tokenCost(2048, p.CompletionPerMillion)
	for i, body := range bodies {
		got, err := p.Reserve([]byte(body))
		if err != nil {
			t.Fatalf("case %d: Reserve: %v", i, err)
		}
		if got != want {
			t.Errorf("case %d (%s): Reserve = %d, want %d micro-credits (default cap)", i, body, got, want)
		}
	}
}

// A body that is not JSON returns an error rather than panicking.
func TestReserveNonJSONBodyErrors(t *testing.T) {
	p := DefaultTokenPrices()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Reserve panicked: %v", r)
		}
	}()
	if _, err := p.Reserve([]byte(`garbage not json{`)); err == nil {
		t.Fatal("want error for non-JSON body")
	}
}

// A vision request with a ~1MB base64 image_url part reserves near the
// fixed per-image allowance plus the text, not ~250k tokens' worth of the
// raw bytes: the part's bytes leave the estimate and ImageTokenAllowance
// takes their place.
func TestReserveVisionRequestReservesProportionately(t *testing.T) {
	p := DefaultTokenPrices()
	image := strings.Repeat("A", 1_000_000) // ~1MB base64 payload
	body := `{"model":"vision","max_tokens":1000,` +
		`"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"},"detail":"auto"}` +
		`]}]}`
	got, err := p.Reserve([]byte(body))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// The same formula the code uses: whole field over four plus one,
	// minus the image part's raw bytes, plus 4 bytes per token of the
	// fixed allowance.
	fieldStart := strings.Index(body, `[`)
	fieldEnd := strings.LastIndex(body, `]`)
	fieldBytes := fieldEnd - fieldStart + 1
	part := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"},"detail":"auto"}`
	want := tokenCost(uint64(fieldBytes-len(part)+4*int(ImageTokenAllowance))/4+1, p.PromptPerMillion) +
		tokenCost(1000, p.CompletionPerMillion)
	if got != want {
		t.Errorf("Reserve(vision) = %d, want %d micro-credits from the allowance formula", got, want)
	}
	// And far below what the old raw-bytes estimate would have held:
	oldStyle := tokenCost(uint64(fieldBytes)/4+1, p.PromptPerMillion) +
		tokenCost(1000, p.CompletionPerMillion)
	if got >= oldStyle/10 {
		t.Errorf("Reserve(vision) = %d is within tenfold of the old estimate %d; the image bytes still dominate", got, oldStyle)
	}
}

// Text-only messages bodies reserve exactly what the plain byte formula
// gives: the image-aware rework must not move text-only arithmetic.
func TestReserveTextOnlyMessagesUnchanged(t *testing.T) {
	p := DefaultTokenPrices()
	bodies := []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,                          // string content
		`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, // array content, no images
	}
	for i, body := range bodies {
		fieldStart := strings.Index(body, `[`)
		fieldEnd := strings.LastIndex(body, `]`)
		fieldBytes := fieldEnd - fieldStart + 1
		want := tokenCost(uint64(fieldBytes)/4+1, p.PromptPerMillion) +
			tokenCost(p.DefaultMaxTokens, p.CompletionPerMillion)
		got, err := p.Reserve([]byte(body))
		if err != nil {
			t.Fatalf("case %d: Reserve: %v", i, err)
		}
		if got != want {
			t.Errorf("case %d (%s): Reserve = %d, want %d (the old byte formula)", i, body, got, want)
		}
	}
}

// Multiple images each count one fixed allowance; malformed messages fall
// back to the whole-field estimate rather than erroring.
func TestReserveMultipleImagesAndMalformedFallback(t *testing.T) {
	p := DefaultTokenPrices()
	img := strings.Repeat("B", 4096)
	body := `{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"two"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + img + `"}},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + img + `"}}` +
		`]}]}`
	got, err := p.Reserve([]byte(body))
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	fieldStart := strings.Index(body, `[`)
	fieldEnd := strings.LastIndex(body, `]`)
	fieldBytes := fieldEnd - fieldStart + 1
	part := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` + img + `"}}`
	want := tokenCost(uint64(fieldBytes-2*len(part)+2*4*int(ImageTokenAllowance))/4+1, p.PromptPerMillion) +
		tokenCost(p.DefaultMaxTokens, p.CompletionPerMillion)
	if got != want {
		t.Errorf("Reserve(2 images) = %d, want %d (one allowance per image)", got, want)
	}
	// Malformed messages: an object instead of an array - fall back to
	// the whole-field estimate, no error.
	malformed := `{"messages":{"role":"user","content":"hi"}}`
	gotM, err := p.Reserve([]byte(malformed))
	if err != nil {
		t.Fatalf("Reserve(malformed messages): %v", err)
	}
	fieldStart = strings.Index(malformed, `{"role"`)
	fieldEnd = strings.LastIndex(malformed, `"hi"}`)
	wantM := tokenCost(uint64(fieldEnd-fieldStart+len(`"hi"}`))/4+1, p.PromptPerMillion) +
		tokenCost(p.DefaultMaxTokens, p.CompletionPerMillion)
	if gotM != wantM {
		t.Errorf("Reserve(malformed) = %d, want %d (whole-field fallback)", gotM, wantM)
	}
}

// An input-only schedule (embeddings, reranker) reserves the prompt
// estimate at the prompt rate and nothing else: no max_tokens component,
// so a typical embedding batch does not wildly overshoot.
func TestReserveInputOnlyHasNoCompletionComponent(t *testing.T) {
	inputOnly := TokenPrices{PromptPerMillion: 100, CompletionPerMillion: 300, DefaultMaxTokens: 2048, InputOnly: true}
	chat := TokenPrices{PromptPerMillion: 100, CompletionPerMillion: 300, DefaultMaxTokens: 2048}
	body := []byte(`{"model":"x","input":["a","b","c"]}`)
	// The raw input field is ["a","b","c"] - 13 bytes, 13/4+1 = 4 tokens.
	want := tokenCost(4, 100)
	got, err := inputOnly.Reserve(body)
	if err != nil {
		t.Fatalf("Reserve(input-only): %v", err)
	}
	if got != want {
		t.Errorf("Reserve(input-only) = %d, want %d micro-credits (prompt estimate only)", got, want)
	}
	chatGot, err := chat.Reserve(body)
	if err != nil {
		t.Fatalf("Reserve(chat): %v", err)
	}
	if got >= chatGot {
		t.Errorf("input-only reserve %d must be strictly below the chat-shape reserve %d (no default-cap component)", got, chatGot)
	}
	// The same body with an explicit max_tokens still reserves no
	// completion component: there is no completion to bound.
	withMax := []byte(`{"model":"x","max_tokens":1000000,"input":["a","b","c"]}`)
	gotMax, err := inputOnly.Reserve(withMax)
	if err != nil {
		t.Fatalf("Reserve(input-only, max_tokens): %v", err)
	}
	if gotMax != want {
		t.Errorf("Reserve(input-only, max_tokens) = %d, want %d (max_tokens must not enter the hold)", gotMax, want)
	}
}

// A rerank body whose only input source is the documents list estimates
// from it: messages > input > prompt > documents.
func TestReserveInputOnlyPicksDocuments(t *testing.T) {
	p := TokenPrices{PromptPerMillion: 100, DefaultMaxTokens: 2048, InputOnly: true}
	body := []byte(`{"model":"x","query":"q","documents":["d1","d2"]}`)
	// The raw documents field is ["d1","d2"] - 11 bytes, 11/4+1 = 3 tokens.
	got, err := p.Reserve(body)
	if err != nil {
		t.Fatalf("Reserve(rerank): %v", err)
	}
	if want := tokenCost(3, 100); got != want {
		t.Errorf("Reserve(rerank) = %d, want %d micro-credits from the documents field", got, want)
	}
}

// An input-only schedule settles every prompt token at the prompt rate:
// a provider-reported cached breakdown is ignored deliberately, since
// billing it at a zero cached rate would undercharge, and completion is
// zero by construction.
func TestSettleInputOnlyPricesAllPromptAtInputRate(t *testing.T) {
	p := TokenPrices{PromptPerMillion: 100, CachedPromptPerMillion: 10, InputOnly: true}
	u := Usage{PromptTokens: 1000, CachedPromptTokens: 500}
	if got, want := p.Settle(u), tokenCost(1000, 100); got != want {
		t.Errorf("Settle(input-only, 1000 prompt / 500 cached) = %d, want %d (the full 1000 at the input rate)", got, want)
	}
	// A completion count in the usage report is never billed.
	if got, want := p.Settle(Usage{PromptTokens: 1000, CompletionTokens: 999}), tokenCost(1000, 100); got != want {
		t.Errorf("Settle(input-only, with completion) = %d, want %d", got, want)
	}
}

// Settle prices prompt and completion separately at their own rates, at a
// 3:1 completion-to-prompt ratio.
func TestSettlePricesPromptAndCompletionSeparately(t *testing.T) {
	p := DefaultTokenPrices()
	cases := []struct {
		usage Usage
		want  uint64
	}{
		{Usage{PromptTokens: 1_000_000}, 1000 * MicroCreditsPerCredit},
		{Usage{CompletionTokens: 1_000_000}, 3000 * MicroCreditsPerCredit},
		{Usage{PromptTokens: 400_000, CompletionTokens: 300_000}, 1300 * MicroCreditsPerCredit},
	}
	for _, c := range cases {
		if got := p.Settle(c.usage); got != c.want {
			t.Errorf("Settle(%+v) = %d, want %d", c.usage, got, c.want)
		}
	}
	const n = uint64(1_000_000)
	if got, want := p.Settle(Usage{CompletionTokens: n}), 3*p.Settle(Usage{PromptTokens: n}); got != want {
		t.Errorf("3:1 ratio: Settle(completion %d) = %d, want 3 x prompt cost %d", n, got, want/3)
	}
}

// Small counts must order correctly without integer-division surprises.
func TestSettleNoIntegerDivisionSurprisesOnSmallCounts(t *testing.T) {
	p := DefaultTokenPrices()
	if a, b := p.Settle(Usage{CompletionTokens: 1500}), p.Settle(Usage{CompletionTokens: 1000}); a <= b {
		t.Errorf("1500 completion tokens cost %d, not more than 1000 tokens at %d", a, b)
	}
	if a, b := p.Settle(Usage{PromptTokens: 2000}), p.Settle(Usage{PromptTokens: 100}); a <= b {
		t.Errorf("2000 prompt tokens cost %d, not more than 100 tokens at %d", a, b)
	}
}

// The live-inference bug this unit exists to fix: a real call at the
// catalog's competitor-anchored rates (20-250 credits per million) settled
// at 0 credits under whole-credit integer math. In micro-credits the same
// call settles at its exact, nonzero price.
func TestSettleSmallCallIsNonzero(t *testing.T) {
	p := TokenPrices{
		PromptPerMillion:       250,
		CompletionPerMillion:   250,
		CachedPromptPerMillion: 25,
		DefaultMaxTokens:       2048,
	}
	got := p.Settle(Usage{PromptTokens: 5, CompletionTokens: 13})
	want := uint64(5*250/MicroPerMillion + 13*250/MicroPerMillion) // 12 + 32 = 44
	if got == 0 {
		t.Fatalf("small call settled at 0 micro-credits; want %d", want)
	}
	if got != want {
		t.Errorf("Settle(5 prompt + 13 completion) = %d, want %d micro-credits", got, want)
	}
	// At the catalog's cheapest rate the same completion count still
	// settles nonzero: 13 * 20 / 100 = 2 micro-credits.
	cheap := TokenPrices{PromptPerMillion: 20, CompletionPerMillion: 20, CachedPromptPerMillion: 2, DefaultMaxTokens: 2048}
	if c := cheap.Settle(Usage{CompletionTokens: 13}); c != 2 || c == 0 {
		t.Errorf("cheapest-rate Settle(13 completion) = %d, want 2 micro-credits, nonzero", c)
	}
}

// Cached prompt tokens are billed at the cached rate; only the remainder
// pays the full prompt rate.
func TestSettleBillsCachedAtCachedRate(t *testing.T) {
	p := DefaultTokenPrices()
	u := Usage{PromptTokens: 1_000_000, CachedPromptTokens: 400_000}
	want := tokenCost(400_000, p.CachedPromptPerMillion) +
		tokenCost(600_000, p.PromptPerMillion) // 40 + 600 credits in micro-credits
	if got := p.Settle(u); got != want {
		t.Errorf("Settle(cached) = %d, want %d", got, want)
	}
	if allUncached := p.Settle(Usage{PromptTokens: 1_000_000}); p.Settle(u) >= allUncached {
		t.Errorf("cached price %d must be below all-uncached %d", p.Settle(u), allUncached)
	}
}

// A malformed response reporting more cached than prompt tokens must not
// underflow; the cached count is capped at the prompt count.
func TestSettleMoreCachedThanPromptDoesNotUnderflow(t *testing.T) {
	p := DefaultTokenPrices()
	u := Usage{PromptTokens: 10, CompletionTokens: 5, CachedPromptTokens: 99999}
	got := p.Settle(u)
	want := tokenCost(10, p.CachedPromptPerMillion) + tokenCost(5, p.CompletionPerMillion)
	if got != want {
		t.Errorf("Settle(malformed) = %d, want capped value %d", got, want)
	}
	if got > 100_000_000 {
		t.Errorf("Settle(malformed) = %d, wrapped to an absurd value", got)
	}
}

// The exact measured SGLang response: null prompt_tokens_details means zero
// cached tokens, and reasoning_tokens is never added on top of
// completion_tokens.
func TestParseUsageRealResponse(t *testing.T) {
	body := []byte(`{"id":"acfb41c5...","object":"chat.completion","model":"qwen38-27b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"The user is simply asking me to say OK"},
   "finish_reason":"length"}],
 "usage":{"prompt_tokens":12,"total_tokens":28,"completion_tokens":16,
   "prompt_tokens_details":null,"reasoning_tokens":16}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(real response) = false, want true")
	}
	if u.PromptTokens != 12 || u.CompletionTokens != 16 || u.CachedPromptTokens != 0 {
		t.Errorf("ParseUsage = %+v, want {12 16 0}", u)
	}
}

// The /v1/embeddings shape: prompt_tokens present alongside total_tokens;
// the backfill is a no-op because prompt_tokens is nonzero.
func TestParseUsageEmbeddingsFixture(t *testing.T) {
	body := []byte(`{"model":"fw/embed-8b","object":"list",
 "data":[{"index":0,"embedding":[0.1,0.2],"object":"embedding"}],
 "usage":{"prompt_tokens":120,"total_tokens":120}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(embeddings fixture) = false, want true")
	}
	if u.Model != "fw/embed-8b" || u.PromptTokens != 120 || u.CompletionTokens != 0 || u.CachedPromptTokens != 0 {
		t.Errorf("ParseUsage = %+v, want {fw/embed-8b 120 0 0}", u)
	}
}

// The /v1/rerank shape (Jina-style, what Fireworks serves): the prompt
// total is reported as total_tokens alone, so it backfills prompt_tokens.
func TestParseUsageRerankFixtureBackfillsFromTotalTokens(t *testing.T) {
	body := []byte(`{"id":"1","model":"fw/rerank-8b",
 "results":[{"index":0,"relevance_score":0.9}],
 "usage":{"total_tokens":349}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(rerank fixture) = false, want true")
	}
	if u.Model != "fw/rerank-8b" || u.PromptTokens != 349 || u.CompletionTokens != 0 {
		t.Errorf("ParseUsage = %+v, want {fw/rerank-8b 349 0} from total_tokens", u)
	}
}

// A non-chat body without any usage object reports false, so the handler
// settles the full reservation as today.
func TestParseUsageNonChatNoUsageReportsFalse(t *testing.T) {
	bodies := []string{
		`{"model":"fw/embed-8b","data":[]}`,              // embeddings without usage
		`{"id":"1","model":"fw/rerank-8b","results":[]}`, // rerank without usage
	}
	for i, body := range bodies {
		if _, ok := ParseUsage([]byte(body)); ok {
			t.Errorf("case %d (%s): ParseUsage = true, want false", i, body)
		}
	}
}

// No usage object, a null usage, and non-JSON bodies all report false.
func TestParseUsageNoUsageReportsFalse(t *testing.T) {
	bodies := []string{
		`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`,
		`{"id":"x","usage":null}`,
		`not json at all`,
	}
	for i, body := range bodies {
		if _, ok := ParseUsage([]byte(body)); ok {
			t.Errorf("case %d (%s): ParseUsage = true, want false", i, body)
		}
	}
}

// A populated prompt_tokens_details.cached_tokens is read through.
func TestParseUsagePopulatedCachedTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":40,
 "prompt_tokens_details":{"cached_tokens":77}}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(populated details) = false, want true")
	}
	if u.CachedPromptTokens != 77 {
		t.Errorf("CachedPromptTokens = %d, want 77", u.CachedPromptTokens)
	}
}

// The exact measured SGLang response shape keeps parsing identically after
// the envelope extraction: the NVIDIA fixture must stay green untouched.
func TestParseUsageNVIDIAFixtureStillParses(t *testing.T) {
	body := []byte(`{"id":"acfb41c5...","object":"chat.completion","model":"qwen38-27b",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],
 "usage":{"prompt_tokens":400000,"total_tokens":700000,"completion_tokens":300000,
   "prompt_tokens_details":null}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(NVIDIA fixture) = false, want true")
	}
	if u.Model != "qwen38-27b" || u.PromptTokens != 400_000 || u.CompletionTokens != 300_000 || u.CachedPromptTokens != 0 {
		t.Errorf("ParseUsage = %+v, want {qwen38-27b 400000 300000 0}", u)
	}
}

// Fireworks' documented non-streaming chat completion response: the usage
// block is OpenAI-compatible and carries prompt_tokens_details.
func TestParseUsageFireworksNonStreaming(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-abc123","object":"chat.completion",
 "created":1767225600,"model":"accounts/fireworks/models/mistral-nemotron",
 "choices":[{"index":0,"message":{"role":"assistant","content":"OK","reasoning_content":null},
   "logprobs":null,"finish_reason":"stop"}],
 "usage":{"prompt_tokens":12,"completion_tokens":16,"total_tokens":28,
   "prompt_tokens_details":{"cached_tokens":8}}}`)
	u, ok := ParseUsage(body)
	if !ok {
		t.Fatal("ParseUsage(Fireworks non-streaming) = false, want true")
	}
	if u.Model != "accounts/fireworks/models/mistral-nemotron" || u.PromptTokens != 12 ||
		u.CompletionTokens != 16 || u.CachedPromptTokens != 8 {
		t.Errorf("ParseUsage = %+v, want {accounts/fireworks/models/mistral-nemotron 12 16 8}", u)
	}
}

// Fireworks' documented streaming shape: content chunks carry no usage,
// the final chunk carries it (stream_options.include_usage), then [DONE].
const fireworksSSEWithUsage = "data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"accounts/fireworks/models/mistral-nemotron\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"O\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"accounts/fireworks/models/mistral-nemotron\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"K\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-abc123\",\"object\":\"chat.completion.chunk\",\"model\":\"accounts/fireworks/models/mistral-nemotron\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":16,\"total_tokens\":28,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n" +
	"data: [DONE]\n\n"

func TestParseSSEUsageFireworksFinalChunk(t *testing.T) {
	u, ok := ParseSSEUsage([]byte(fireworksSSEWithUsage))
	if !ok {
		t.Fatal("ParseSSEUsage(Fireworks stream) = false, want true")
	}
	if u.PromptTokens != 12 || u.CompletionTokens != 16 || u.CachedPromptTokens != 0 {
		t.Errorf("ParseSSEUsage = %+v, want {12 16 0} from the final chunk", u)
	}
}

// Streams without any usage report - no stream_options support, a bare
// [DONE] body, CRLF line endings with no usage, and non-SSE garbage - all
// report false so the caller settles the full reservation instead.
func TestParseSSEUsageNoUsageReportsFalse(t *testing.T) {
	bodies := []string{
		// Content chunks only, no usage chunk: what an upstream without
		// stream_options answers.
		"data: {\"choices\":[{\"delta\":{\"content\":\"O\"}}]}\n\ndata: [DONE]\n\n",
		// Only the terminator.
		"data: [DONE]\n\n",
		// CRLF line endings without usage.
		"data: {\"choices\":[]}\r\n\r\ndata: [DONE]\r\n\r\n",
		// Not SSE at all.
		`{"usage":{"prompt_tokens":5,"completion_tokens":5}}`,
		// Empty body.
		"",
	}
	for i, body := range bodies {
		if _, ok := ParseSSEUsage([]byte(body)); ok {
			t.Errorf("case %d (%s): ParseSSEUsage = true, want false", i, body)
		}
	}
}

// An earlier chunk that carries usage loses to the later one: the last
// data chunk with a usage object wins, matching where providers put the
// final aggregate.
func TestParseSSEUsageLastChunkWins(t *testing.T) {
	body := "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":7}}\n\n"
	u, ok := ParseSSEUsage([]byte(body))
	if !ok {
		t.Fatal("ParseSSEUsage = false, want true")
	}
	if u.PromptTokens != 9 || u.CompletionTokens != 7 {
		t.Errorf("ParseSSEUsage = %+v, want the last chunk's {9 7}", u)
	}
}

func TestLoadTokenPricesMissingFileReturnsDefaults(t *testing.T) {
	p, err := LoadTokenPrices(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error, got %v", err)
	}
	if p != DefaultTokenPrices() {
		t.Fatalf("defaults after missing file: got %+v", p)
	}
}

func TestLoadTokenPricesOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token-prices.json")
	if err := os.WriteFile(path, []byte(`{"prompt_per_million":500,"default_max_tokens":512}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadTokenPrices(path)
	if err != nil {
		t.Fatalf("LoadTokenPrices: %v", err)
	}
	if p.PromptPerMillion != 500 || p.DefaultMaxTokens != 512 {
		t.Errorf("overridden values: got %+v", p)
	}
	if p.CompletionPerMillion != 3000 || p.CachedPromptPerMillion != 100 {
		t.Errorf("untouched values: got %+v", p)
	}
}

func TestLoadTokenPricesRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token-prices.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokenPrices(path); err == nil {
		t.Fatal("want error for malformed JSON")
	}
}
