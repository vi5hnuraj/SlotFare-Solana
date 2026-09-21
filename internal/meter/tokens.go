package meter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Usage is one non-streaming upstream response's report of what actually
// happened, in tokens. Only the upstream's own report is trusted: a
// client-supplied token count is never read. Model is the envelope's own
// top-level report of which model served the call.
type Usage struct {
	Model              string
	PromptTokens       uint64
	CompletionTokens   uint64
	CachedPromptTokens uint64
}

// MicroCreditsPerCredit is the internal metering unit: 10,000 micro-credits
// per credit. All reservation, settlement, ledger accounting and usage
// recording in this package are in micro-credits; pricing configuration
// (credits per million tokens, RPC method weights in credits) stays in
// credits and the pricers perform the one conversion. The on-chain ledger
// still mints and burns whole credits: at that boundary the settler
// converts down and carries the sub-credit remainder in the local ledger.
const MicroCreditsPerCredit = 10_000

// TokenPrices prices inference by tokens. The three rates are credits per
// million tokens; DefaultMaxTokens is the cap a request is reserved against
// when it carries no usable max_tokens. Per million, not per thousand,
// because that is how providers quote and because the cached rate is a
// tenth of the prompt rate: per thousand it would be 0.1, and integers are
// the only sane unit for money. Reserve and Settle convert the rates to
// the internal unit and return micro-credits.
type TokenPrices struct {
	PromptPerMillion       uint64
	CompletionPerMillion   uint64
	CachedPromptPerMillion uint64
	DefaultMaxTokens       uint64
	// InputOnly is set for the registry's non-chat kinds (embeddings,
	// reranker): there is no completion to bound, so Reserve carries no
	// completion component, and Settle prices every prompt token at the
	// prompt rate - a cached-prompt breakdown is deliberately ignored, so
	// a provider-reported cache hit is not billed at a zero rate.
	InputOnly bool
}

// DefaultTokenPrices returns the compiled-in rates: 1000 credits per
// million prompt tokens, 3000 per million completion, 100 per million
// cached, and a default cap of 2048 tokens. At 1000 credits to the dollar
// that is $1.00, $3.00 and $0.10 per million respectively.
func DefaultTokenPrices() TokenPrices {
	return TokenPrices{
		PromptPerMillion:       1000,
		CompletionPerMillion:   3000,
		CachedPromptPerMillion: 100,
		DefaultMaxTokens:       2048,
	}
}

// usageFromEnvelope reads the usage object from one non-streaming upstream
// response envelope and reports false when the body is not valid JSON or
// carries no usage. A null prompt_tokens_details (a cache miss in SGLang's
// shape) is treated as zero cached tokens, not as an error.
// reasoning_tokens is deliberately ignored: on a reasoning model it sits
// inside completion_tokens, and pricing it on top would double charge.
// total_tokens backfills prompt_tokens only when prompt_tokens is absent
// or zero: /v1/rerank responses report their prompt total as total_tokens
// alone, while chat reports always carry prompt_tokens, so the backfill
// never reinterprets a chat shape.
func usageFromEnvelope(body []byte) (Usage, bool) {
	var envelope struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens        uint64 `json:"prompt_tokens"`
			CompletionTokens    uint64 `json:"completion_tokens"`
			TotalTokens         uint64 `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Usage == nil {
		return Usage{}, false
	}
	u := Usage{
		Model:            envelope.Model,
		PromptTokens:     envelope.Usage.PromptTokens,
		CompletionTokens: envelope.Usage.CompletionTokens,
	}
	if envelope.Usage.PromptTokensDetails != nil {
		u.CachedPromptTokens = envelope.Usage.PromptTokensDetails.CachedTokens
	}
	if u.PromptTokens == 0 && envelope.Usage.TotalTokens > 0 {
		u.PromptTokens = envelope.Usage.TotalTokens
	}
	return u, true
}

// ParseUsage reads the usage object from a non-streaming upstream response
// and reports false when the body is not valid JSON or carries no usage.
func ParseUsage(body []byte) (Usage, bool) {
	return usageFromEnvelope(body)
}

// ParseSSEUsage reads the usage report out of a server-sent-events body:
// OpenAI-compatible upstreams (Fireworks among them) carry it in the final
// data chunk when stream_options.include_usage was requested. It scans every
// data line, skips [DONE], and returns the usage of the last chunk that
// carries one; bodies without any usage report - including streams served by
// upstreams that do not support stream_options - report false so the caller
// settles the full reservation instead.
func ParseSSEUsage(sse []byte) (Usage, bool) {
	var last Usage
	found := false
	for _, line := range strings.Split(string(sse), "\n") {
		line = strings.TrimRight(line, "\r")
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if u, ok := usageFromEnvelope([]byte(payload)); ok {
			last = u
			found = true
		}
	}
	return last, found
}

// Reserve is the hold to take before the request is forwarded, in
// micro-credits: an estimate of the prompt priced at the prompt rate plus
// max_tokens priced at the completion rate. It is deliberately an upper-ish bound - the estimate
// only has to cover, and Settle corrects it against the true usage. A body
// that is not valid JSON is an error, not a panic; a missing, zero or
// malformed max_tokens falls back to DefaultMaxTokens, because without a
// cap there is no upper bound to reserve against.
func (p TokenPrices) Reserve(body []byte) (uint64, error) {
	var req struct {
		MaxTokens json.RawMessage `json:"max_tokens"`
		Messages  json.RawMessage `json:"messages"`
		Prompt    json.RawMessage `json:"prompt"`
		Input     json.RawMessage `json:"input"`
		Documents json.RawMessage `json:"documents"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, fmt.Errorf("meter: inference body is not JSON: %w", err)
	}
	// One token per four bytes of the input field; messages wins when more
	// than one source is present (image-aware path), then the embeddings
	// input array, the legacy completions prompt, and finally the rerank
	// documents list - rerank batches are dominated by the documents, and
	// the plain bytes/4+1 estimate covers them without wild overshoot.
	var input json.RawMessage
	isMessages := false
	switch {
	case len(req.Messages) > 0:
		input = req.Messages
		isMessages = true
	case len(req.Input) > 0:
		input = req.Input
	case len(req.Prompt) > 0:
		input = req.Prompt
	case len(req.Documents) > 0:
		input = req.Documents
	}
	var promptEstimate uint64
	if n := len(input); n > 0 {
		promptEstimate = estimateInputTokens(input, isMessages)
	}
	if p.InputOnly {
		// No completion exists for this service: the hold is the prompt
		// estimate at the prompt rate, with no max_tokens component.
		return tokenCost(promptEstimate, p.PromptPerMillion), nil
	}
	max := p.DefaultMaxTokens
	if len(req.MaxTokens) > 0 {
		var v uint64
		// Absent, zero or non-numeric all fall back to the default cap.
		if err := json.Unmarshal(req.MaxTokens, &v); err == nil && v > 0 {
			max = v
		}
	}
	return tokenCost(promptEstimate, p.PromptPerMillion) +
		tokenCost(max, p.CompletionPerMillion), nil
}

// ImageTokenAllowance is the fixed per-image prompt estimate that replaces
// the raw bytes of an image_url content part. A base64 data-URI image would
// otherwise inflate the byte-based estimate by orders of magnitude (a 1MB
// image reserves ~250k tokens' worth) and reject callers who could easily
// afford the true cost; settlement still prices the upstream's own usage
// report, which reflects the image's real cost.
const ImageTokenAllowance uint64 = 256

// estimateInputTokens prices the prompt portion of a request body. The base
// math is one token per four bytes of the whole field; for a chat messages
// array the raw bytes of every image_url part are then subtracted and a
// fixed ImageTokenAllowance added per image, so a base64 data-URI image
// reserves proportionately instead of by orders of magnitude. Text-only
// bodies reserve exactly what the plain byte formula gives. Unparseable
// arrays fall back to the whole field's bytes over four plus one, which
// keeps Reserve a pure estimator: it never errors on odd shapes.
func estimateInputTokens(input json.RawMessage, isMessages bool) uint64 {
	if !isMessages {
		return uint64(len(input))/4 + 1
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(input, &msgs); err != nil {
		return uint64(len(input))/4 + 1
	}
	fieldBytes := len(input)
	for _, m := range msgs {
		if len(m.Content) == 0 || m.Content[0] != '[' {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(m.Content))
		if _, err := dec.Token(); err != nil { // not an array after all
			continue
		}
		for dec.More() {
			var part json.RawMessage
			if err := dec.Decode(&part); err != nil {
				break
			}
			var p struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(part, &p); err != nil || p.Type != "image_url" {
				continue
			}
			// The image's raw bytes leave the estimate; a fixed allowance
			// takes their place (4 bytes per token, same ratio as the
			// field-level math).
			fieldBytes -= len(part)
			fieldBytes += 4 * int(ImageTokenAllowance)
		}
	}
	return uint64(fieldBytes)/4 + 1
}

// Settle is the true cost of a served response, in micro-credits. Cached
// prompt tokens are
// billed at the cached rate and the remainder at the full prompt rate; a
// malformed response claiming more cached tokens than prompt tokens is
// capped at the prompt count, never allowed to underflow. An input-only
// schedule (embeddings, reranker) prices every prompt token at the prompt
// rate: there is no completion component by construction, and the cached
// breakdown is ignored deliberately, since billing it at a zero cached
// rate would undercharge.
func (p TokenPrices) Settle(u Usage) uint64 {
	if p.InputOnly {
		return tokenCost(u.PromptTokens, p.PromptPerMillion)
	}
	cached := u.CachedPromptTokens
	if cached > u.PromptTokens {
		cached = u.PromptTokens
	}
	return tokenCost(cached, p.CachedPromptPerMillion) +
		tokenCost(u.PromptTokens-cached, p.PromptPerMillion) +
		tokenCost(u.CompletionTokens, p.CompletionPerMillion)
}

// tokenCost prices tokens at a credits-per-million rate, in 64-bit
// integer math, and returns micro-credits: dividing by 100 (a million over
// MicroCreditsPerCredit, which divides evenly) instead of by a million is
// integer-identical to pricing in micro-credits and truncating, so a small
// call settles at its true sub-credit price instead of 0. Rates and
// realistic token counts keep the product far below 2^64 (the ceiling is
// about 1.8e13 tokens at a 1e6-credit rate). Rounding down is fine -
// Reserve and Settle share this one arithmetic path, so the hold and the
// truth are priced identically.
func tokenCost(tokens, ratePerMillion uint64) uint64 {
	return tokens * ratePerMillion / MicroPerMillion
}

// MicroPerMillion is the division factor that prices a credits-per-million
// schedule directly in micro-credits: 1_000_000 over
// MicroCreditsPerCredit, which divides evenly.
const MicroPerMillion = 1_000_000 / MicroCreditsPerCredit

// LoadTokenPrices reads a JSON file that overrides the default rates, so
// pricing is configuration rather than a redeploy, mirroring LoadWeights.
// A missing file returns the defaults and no error; a present file must be
// valid.
//
// Format (all keys optional, flat):
//
//	{
//	  "prompt_per_million": 1000,
//	  "completion_per_million": 3000,
//	  "cached_prompt_per_million": 100,
//	  "default_max_tokens": 2048
//	}
//
// Only the keys that are present override anything; everything else keeps
// its default.
func LoadTokenPrices(path string) (TokenPrices, error) {
	p := DefaultTokenPrices()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, fmt.Errorf("meter: read token prices file: %w", err)
	}
	var cfg struct {
		PromptPerMillion       *uint64 `json:"prompt_per_million"`
		CompletionPerMillion   *uint64 `json:"completion_per_million"`
		CachedPromptPerMillion *uint64 `json:"cached_prompt_per_million"`
		DefaultMaxTokens       *uint64 `json:"default_max_tokens"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return p, fmt.Errorf("meter: parse token prices file %s: %w", path, err)
	}
	if cfg.PromptPerMillion != nil {
		p.PromptPerMillion = *cfg.PromptPerMillion
	}
	if cfg.CompletionPerMillion != nil {
		p.CompletionPerMillion = *cfg.CompletionPerMillion
	}
	if cfg.CachedPromptPerMillion != nil {
		p.CachedPromptPerMillion = *cfg.CachedPromptPerMillion
	}
	if cfg.DefaultMaxTokens != nil {
		p.DefaultMaxTokens = *cfg.DefaultMaxTokens
	}
	return p, nil
}
