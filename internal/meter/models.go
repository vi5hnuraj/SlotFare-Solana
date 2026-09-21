package meter

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Pricing is one registry entry's schedule, in credits per million tokens,
// at the fixed 1000 credits to the dollar rate. Cached input is conventionally
// one tenth of uncached input. Both explicit entries (pricing written
// directly) and derived entries (provider cost plus margin, computed once at
// load) end up in this same schedule; the wire only ever sees credits.
type Pricing struct {
	UncachedInput uint64 `json:"uncachedInput"`
	CachedInput   uint64 `json:"cachedInput"`
	Output        uint64 `json:"output"`
}

// Model is one entry of the SLOTFARE_MODELS registry: a stable console-facing
// id, the upstream id the proxy forwards under, a human name, the pricing
// schedule and an optional service tier. Registry ids are what clients
// request and the console shows; upstream ids are operator configuration
// and never leak out. The provider cost and margin that a derived entry was
// parsed from are consumed at load time and never stored here, so they
// cannot reach the wire. ServiceTier is likewise server-side: when set the
// proxy injects it into the forwarded body as service_tier, and it never
// appears on the console's wire payload.
//
// The metadata fields (Vendor, ContextLength, ServingPath, Description,
// Parameters, Capabilities, Kind) are console-facing display and filter data,
// unlike ServiceTier they DO reach the console's wire payload. They are all
// optional: absent means absent (empty string, zero, or nil), and the console
// serves each verbatim, never fabricating a value. Vendor falls back to the
// first path segment of the upstream id when that segment is a lowercase
// slug; ContextLength is never guessed.
type Model struct {
	ID            string   `json:"id"`
	UpstreamID    string   `json:"upstreamId"`
	Name          string   `json:"name"`
	Pricing       Pricing  `json:"pricing"`
	ServiceTier   string   `json:"serviceTier,omitempty"`
	Vendor        string   `json:"vendor,omitempty"`
	ContextLength uint64   `json:"contextLength,omitempty"`
	ServingPath   string   `json:"servingPath,omitempty"`
	Description   string   `json:"description,omitempty"`
	Parameters    string   `json:"parameters,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Moe           *bool    `json:"moe,omitempty"`
}

// vendorSlugRE is the shape of a vendor slug: lowercase, starting with a
// letter, hyphen-separated runs of letters and digits.
var vendorSlugRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// The closed sets a metadata value is validated against when present.
var (
	validServingPaths = map[string]bool{"standard": true, "priority": true, "fast": true}
	validKinds        = map[string]bool{"chat": true, "embeddings": true, "reranker": true}
	validCapabilities = map[string]bool{
		"vision": true, "function-calling": true,
		"text": true, "image": true, "video": true, "audio": true,
	}
)

// vendorFallback derives a vendor from the upstream id when the entry does
// not set one explicitly: exactly one slash with a non-empty first segment
// that is itself a lowercase slug ("mistralai/mistral-nemotron" ->
// "mistralai"). Multi-segment ids such as Fireworks'
// "accounts/fireworks/models/x" do not derive a vendor - the first segment
// is a namespace word, so the operator must set vendor explicitly there.
func vendorFallback(upstreamID string) string {
	parts := strings.Split(upstreamID, "/")
	if len(parts) != 2 || parts[0] == "" {
		return ""
	}
	if !vendorSlugRE.MatchString(parts[0]) {
		return ""
	}
	return parts[0]
}

// Models is the parsed registry. An empty slice means today's single-model
// behavior: no validation, no mapping, default prices.
type Models []Model

// ParseModels reads SLOTFARE_MODELS: a JSON array of model objects with the
// keys id, upstreamId and name plus exactly one pricing form - either
// pricing (an object with uncachedInput, cachedInput and output credits per
// million) or cost (an object with uncachedInputUsd, cachedInputUsd and
// outputUsd per million) together with margin (a fraction), from which the
// credit prices are derived at load as ceil(usd * (1 + margin) * 1000).
// An entry whose kind is embeddings or reranker is input-only: the cached
// and output components are optional (absent means zero), and a present
// nonzero value for either is a startup config error naming the field.
// Empty or whitespace raw returns an empty registry with no error; every
// other failure names the offending entry index and field, so an operator
// who mistypes one field in a long list sees exactly which one.
func ParseModels(raw string) (Models, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var items []struct {
		ID            *string         `json:"id"`
		UpstreamID    *string         `json:"upstreamId"`
		Name          *string         `json:"name"`
		ServiceTier   *string         `json:"serviceTier"`
		Vendor        *string         `json:"vendor"`
		ContextLength json.RawMessage `json:"contextLength"`
		ServingPath   *string         `json:"servingPath"`
		Description   *string         `json:"description"`
		Parameters    *string         `json:"parameters"`
		Capabilities  []string        `json:"capabilities"`
		Kind          *string         `json:"kind"`
		Moe           *bool           `json:"moe"`
		Pricing       json.RawMessage `json:"pricing"`
		Cost          json.RawMessage `json:"cost"`
		Margin        json.RawMessage `json:"margin"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("SLOTFARE_MODELS: not JSON: %w", err)
	}
	out := make(Models, 0, len(items))
	seen := make(map[string]int, len(items))
	for i, it := range items {
		m := Model{}
		if it.ID == nil || *it.ID == "" {
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].id: empty", i)
		}
		m.ID = *it.ID
		if prev, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].id: duplicate of models[%d]", i, prev)
		}
		seen[m.ID] = i
		if it.UpstreamID == nil || *it.UpstreamID == "" {
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].upstreamId: empty", i)
		}
		m.UpstreamID = *it.UpstreamID
		if it.Name == nil || *it.Name == "" {
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].name: empty", i)
		}
		m.Name = *it.Name
		// serviceTier is optional: absent or empty leaves it unset, which
		// means the proxy sends no service_tier field for this entry.
		if it.ServiceTier != nil {
			m.ServiceTier = *it.ServiceTier
		}
		// The metadata fields are optional console display/filter data. Each
		// is validated when present; absent means absent (never fabricated).
		if it.Vendor != nil && *it.Vendor != "" {
			if !vendorSlugRE.MatchString(*it.Vendor) {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].vendor: not a lowercase slug", i)
			}
			m.Vendor = *it.Vendor
		}
		if len(it.ContextLength) > 0 {
			n, err := nonNegativeNumber(i, "contextLength", it.ContextLength)
			if err != nil {
				return nil, err
			}
			v, err := n.Int64()
			if err != nil || v <= 0 {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].contextLength: not a positive integer", i)
			}
			m.ContextLength = uint64(v)
		}
		if it.ServingPath != nil && *it.ServingPath != "" {
			if !validServingPaths[*it.ServingPath] {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].servingPath: must be one of standard, priority, fast", i)
			}
			m.ServingPath = *it.ServingPath
		}
		if it.Description != nil {
			m.Description = *it.Description
		}
		if it.Parameters != nil {
			m.Parameters = *it.Parameters
		}
		for _, cap := range it.Capabilities {
			if !validCapabilities[cap] {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].capabilities: unknown capability %q", i, cap)
			}
		}
		if len(it.Capabilities) > 0 {
			m.Capabilities = it.Capabilities
		}
		if it.Kind != nil && *it.Kind != "" {
			if !validKinds[*it.Kind] {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].kind: must be one of chat, embeddings, reranker", i)
			}
			m.Kind = *it.Kind
		}
		// moe is an optional boolean: nil means unknown and stays absent on
		// the wire, a non-nil pointer is served verbatim, never defaulted.
		if it.Moe != nil {
			m.Moe = it.Moe
		}
		// Non-chat kinds have no completion and no separate cached rate: in
		// their pricing form the cached and output components are optional
		// (absent means zero), and a present nonzero value is rejected
		// after the form is parsed, naming the field that carries it.
		nonChat := m.Kind == "embeddings" || m.Kind == "reranker"
		// Vendor fallback: when no explicit vendor was set, derive it from the
		// upstream id's first path segment when that segment is a slug.
		if m.Vendor == "" {
			m.Vendor = vendorFallback(m.UpstreamID)
		}
		switch {
		case len(it.Pricing) > 0 && (len(it.Cost) > 0 || len(it.Margin) > 0):
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d]: both pricing and cost+margin present; exactly one form is allowed", i)
		case len(it.Pricing) > 0:
			var pmap map[string]json.RawMessage
			if err := json.Unmarshal(it.Pricing, &pmap); err != nil {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].pricing: not an object: %w", i, err)
			}
			var prices [3]uint64
			for j, key := range []string{"uncachedInput", "cachedInput", "output"} {
				raw, ok := pmap[key]
				if !ok {
					if nonChat && j > 0 {
						continue // absent cached/output defaults to zero
					}
					return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].pricing.%s: missing", i, key)
				}
				n, err := nonNegativeNumber(i, "pricing."+key, raw)
				if err != nil {
					return nil, err
				}
				v, err := n.Int64()
				if err != nil {
					return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].pricing.%s: not a non-negative number", i, key)
				}
				prices[j] = uint64(v)
			}
			m.Pricing = Pricing{
				UncachedInput: prices[0],
				CachedInput:   prices[1],
				Output:        prices[2],
			}
		case len(it.Cost) > 0 && len(it.Margin) > 0:
			var cmap map[string]json.RawMessage
			if err := json.Unmarshal(it.Cost, &cmap); err != nil {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].cost: not an object: %w", i, err)
			}
			margin, err := nonNegativeRat(i, "margin", it.Margin)
			if err != nil {
				return nil, err
			}
			var prices [3]uint64
			for j, key := range []string{"uncachedInputUsd", "cachedInputUsd", "outputUsd"} {
				raw, ok := cmap[key]
				if !ok {
					if nonChat && j > 0 {
						continue // absent cached/output defaults to zero
					}
					return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].cost.%s: missing", i, key)
				}
				usd, err := nonNegativeRat(i, "cost."+key, raw)
				if err != nil {
					return nil, err
				}
				prices[j], err = deriveCredits(usd, margin)
				if err != nil {
					return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].cost.%s: %w", i, key, err)
				}
			}
			m.Pricing = Pricing{
				UncachedInput: prices[0],
				CachedInput:   prices[1],
				Output:        prices[2],
			}
		case len(it.Cost) > 0:
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].margin: missing", i)
		case len(it.Margin) > 0:
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].cost: missing", i)
		default:
			return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d]: missing pricing or cost+margin", i)
		}
		if nonChat {
			// Settlement prices every prompt token at the input rate, so a
			// cached or output rate would be dead configuration: a present
			// nonzero value is a startup config error naming the form's
			// own field.
			cachedField, outputField := "pricing.cachedInput", "pricing.output"
			if len(it.Cost) > 0 {
				cachedField, outputField = "cost.cachedInputUsd", "cost.outputUsd"
			}
			if m.Pricing.CachedInput > 0 {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].%s: must be 0 for kind %q", i, cachedField, m.Kind)
			}
			if m.Pricing.Output > 0 {
				return nil, fmt.Errorf("SLOTFARE_MODELS: models[%d].%s: must be 0 for kind %q", i, outputField, m.Kind)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// nonNegativeNumber parses a registry field as a non-negative integer number;
// label is the dotted field path after the entry index, e.g. pricing.output.
func nonNegativeNumber(i int, label string, raw json.RawMessage) (json.Number, error) {
	prefix := fmt.Sprintf("SLOTFARE_MODELS: models[%d].%s", i, label)
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fmt.Errorf("%s: not a number: %w", prefix, err)
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return "", fmt.Errorf("%s: not a non-negative number", prefix)
	}
	return n, nil
}

// nonNegativeRat parses a registry field as a non-negative decimal number in
// exact rational form; label is the dotted field path after the entry index,
// e.g. cost.outputUsd.
func nonNegativeRat(i int, label string, raw json.RawMessage) (*big.Rat, error) {
	prefix := fmt.Sprintf("SLOTFARE_MODELS: models[%d].%s", i, label)
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("%s: not a number: %w", prefix, err)
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return nil, fmt.Errorf("%s: not a number: %s", prefix, n)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("%s: not a non-negative number", prefix)
	}
	return r, nil
}

// deriveCredits computes ceil(usd * (1 + margin) * 1000): the credit price
// of one component, at the fixed 1000 credits to the dollar rate, rounded up
// so a derived price never sits below its provider cost. Zero cost derives
// to zero; a result that overflows uint64 is a config error.
func deriveCredits(usd, margin *big.Rat) (uint64, error) {
	r := new(big.Rat).Set(usd)
	factor := new(big.Rat).SetInt64(1)
	factor.Add(factor, margin)
	r.Mul(r, factor)
	r.Mul(r, big.NewRat(1000, 1))
	if r.Sign() == 0 {
		return 0, nil
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if new(big.Int).Rem(r.Num(), r.Denom()).Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsUint64() {
		return 0, fmt.Errorf("derived price %s does not fit in a uint64", r.String())
	}
	return q.Uint64(), nil
}

// Prices maps the entry's schedule onto the token pricer arithmetic:
// uncached input is the prompt rate, cached input the cached-prompt rate,
// output the completion rate. DefaultMaxTokens keeps the compiled-in
// reservation cap, which the registry does not configure. InputOnly marks
// the non-chat kinds: no completion exists to bound, and the cached rate
// does not apply.
func (m Model) Prices() TokenPrices {
	return TokenPrices{
		PromptPerMillion:       m.Pricing.UncachedInput,
		CompletionPerMillion:   m.Pricing.Output,
		CachedPromptPerMillion: m.Pricing.CachedInput,
		DefaultMaxTokens:       DefaultTokenPrices().DefaultMaxTokens,
		InputOnly:              m.Kind == "embeddings" || m.Kind == "reranker",
	}
}

// Lookup finds the entry by console-facing id.
func (ms Models) Lookup(id string) (Model, bool) {
	for _, m := range ms {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// IDs lists the console-facing ids in configuration order.
func (ms Models) IDs() []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
