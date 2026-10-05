package console

import (
	"net/http"

	"slotfare/internal/meter"
)

// WithModels attaches the inference model registry served by /models: the
// console's endpoint screen shows the catalog and its prices without
// hardcoding either.
func WithModels(m meter.Models) Option {
	return func(h *Handler) { h.Models = m }
}

// WithBaseURL attaches the advertised OpenAI-compatible base URL that /models
// carries on the wire, so the endpoint screen can show a curl a key holder
// can actually run. Empty omits the field and keeps the payload unchanged.
func WithBaseURL(u string) Option {
	return func(h *Handler) { h.BaseURL = u }
}

// modelView is one entry of GET /console/models: the console-facing id, the
// human name, the price schedule and the optional metadata fields. The
// upstream id is deliberately omitted - registry ids are the stable
// console-facing names and upstream ids are operator configuration. The
// metadata fields (vendor, contextLength, servingPath, description,
// parameters, capabilities, kind, moe) are served verbatim alongside id/name/
// pricing; an absent value is omitted from the wire (omitempty), never a
// fabricated value.
type modelView struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Pricing       meter.Pricing `json:"pricing"`
	Vendor        string        `json:"vendor,omitempty"`
	ContextLength uint64        `json:"contextLength,omitempty"`
	ServingPath   string        `json:"servingPath,omitempty"`
	Description   string        `json:"description,omitempty"`
	Parameters    string        `json:"parameters,omitempty"`
	Capabilities  []string      `json:"capabilities,omitempty"`
	Kind          string        `json:"kind,omitempty"`
	Moe           *bool         `json:"moe,omitempty"`
}

// handleModels lists the configured model catalog with pricing. Like its
// read peers it requires an authenticated account and answers 501 when no
// registry was attached. When a base URL was attached (WithBaseURL) the
// payload also advertises it as baseUrl: the OpenAI-compatible root a key
// holder curls for inference.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAccount(w, r); !ok {
		return
	}
	if h.Models == nil {
		writeError(w, http.StatusNotImplemented, "not available")
		return
	}
	out := struct {
		BaseURL *string     `json:"baseUrl,omitempty"`
		Models  []modelView `json:"models"`
	}{Models: []modelView{}}
	if h.BaseURL != "" {
		out.BaseURL = &h.BaseURL
	}
	for _, m := range h.Models {
		out.Models = append(out.Models, modelView{
			ID:            m.ID,
			Name:          m.Name,
			Pricing:       m.Pricing,
			Vendor:        m.Vendor,
			ContextLength: m.ContextLength,
			ServingPath:   m.ServingPath,
			Description:   m.Description,
			Parameters:    m.Parameters,
			Capabilities:  m.Capabilities,
			Kind:          m.Kind,
			Moe:           m.Moe,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
