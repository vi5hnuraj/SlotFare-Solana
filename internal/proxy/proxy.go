// Package proxy is the metering reverse proxy: it authenticates an API key,
// routes the request to one of the configured upstream services by path
// prefix, prices it, reserves the credits, forwards the original body
// byte-for-byte with the service prefix stripped, and returns the upstream
// response unchanged. On upstream failure the reservation is released and a
// 502 is returned; on success the true cost is left pending - or the full
// reservation when the true cost cannot be determined. Every service draws
// from the same credit balance.
//
// Metered, non-streaming responses carry three headers: x-slotfare-credits-
// charged, the amount settled for this request, and x-slotfare-balance, the
// account's spendable balance after settlement - both in integer micro-
// credits, so a client can meter without a second call - and x-slotfare-
// receipt, a compact base64 JSON envelope of the canonical usage fields the
// recorded row will hold, signed by a dedicated receipt keypair. Streaming
// responses send headers before settlement is knowable: they carry
// x-slotfare-balance as of reservation time and omit x-slotfare-credits-
// charged and x-slotfare-receipt rather than lie. Free discovery paths and
// error rejections carry no charge header; the insufficient-credits
// rejection may carry x-slotfare-balance. When CORS is active the headers
// are exposed to browsers (internal/cors).
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
	"slotfare/internal/receipt"
)

// JSON-RPC error codes used by the proxy. The -32xxx range is reserved for
// server errors in the JSON-RPC 2.0 spec.
const (
	codeUnauthorized     = -32001
	codeInsufficientCred = -32002
	codeUpstreamError    = -32003
	codeNotFound         = -32004
	codeRateLimited      = -32005
	codeInvalidRequest   = -32600
)

// Slotfare response headers, in integer micro-credits: the amount settled
// for this request and the account's spendable balance after settlement.
const (
	headerCreditsCharged = "x-slotfare-credits-charged"
	headerBalance        = "x-slotfare-balance"
	// headerReceipt carries the signed usage receipt: a base64 JSON
	// envelope of the canonical fields the usage row will be recorded
	// with, signed by the dedicated receipt keypair. Metered, non-
	// streaming responses only; streaming responses and rejections omit
	// it (docs/usage-receipts.md).
	headerReceipt = "x-slotfare-receipt"
)

// Pricer prices a request body for one service. Reserve returns the credits
// to hold before the request is forwarded to the upstream.
type Pricer interface {
	Reserve(body []byte) (uint64, error)
}

// PathPricer is an optional extension of Pricer for services that price by
// the post-prefix path as well as the body. A pricer that does not
// implement it ignores the path and prices the body alone.
type PathPricer interface {
	ReservePath(path string, body []byte) (uint64, error)
}

// ResponseSettler is an optional extension of Pricer for services whose
// true cost is readable from the served response. SettleResponse returns
// that cost and whether usage was reported; when it reports false the
// handler settles the full reservation instead, since serving happened and
// the cost is unknown.
type ResponseSettler interface {
	SettleResponse(respBody []byte) (uint64, bool)
}

// ModelPricer is an optional extension of Pricer for services with a model
// registry: Resolve validates the request's model against it and rewrites
// the body to the upstream id; ReserveModel and SettleModel price at that
// model's own schedule. Free discovery paths resolve to an empty model id
// with the body unchanged, exactly like today's single-model behavior.
type ModelPricer interface {
	Resolve(path string, body []byte) (modelID string, newBody []byte, err error)
	ReserveModel(modelID string, path string, body []byte) (uint64, error)
	SettleModel(modelID string, respBody []byte) (uint64, bool)
}

// StreamSettler is an optional extension of ModelPricer for services whose
// streaming responses carry a usage report in a late chunk (OpenAI-style
// SSE with stream_options.include_usage). SettleStream returns the true
// cost of the buffered stream body and whether usage was found; when it
// reports false the handler settles the full reservation instead.
type StreamSettler interface {
	SettleStream(modelID string, respBody []byte) (uint64, bool)
}

// KindProvider is an optional extension of ModelPricer for services whose
// registry entries carry a service kind: ModelKind returns it for the
// resolved id so the recorded usage rows can label the service. It
// reports empty for an empty id or one that does not resolve.
type KindProvider interface {
	ModelKind(modelID string) string
}

// Service is one metered upstream behind the proxy: requests whose path
// starts with Prefix are priced by Pricer, charged to the shared balance,
// and forwarded to Upstream with the Prefix stripped. Allow lists the path
// suffixes (after the prefix is stripped) the service accepts; empty means
// any path. UpstreamKey, when set, is injected as the outbound bearer in
// place of the client's key; empty means no Authorization header, which is
// today's behavior since forward drops all inbound headers anyway.
// StreamUsage opts the outbound request into stream usage reporting by
// adding stream_options.include_usage to streaming bodies; it is off by
// default so upstreams that do not support stream_options (the NVIDIA free
// tier among them) see byte-identical requests.
type Service struct {
	Name        string
	Prefix      string
	Upstream    *url.URL
	Pricer      Pricer
	Allow       []string
	UpstreamKey string
	StreamUsage bool
}

// Handler authenticates, routes, prices and forwards requests against a set
// of upstream services that share one credit balance.
type Handler struct {
	Keys keys.Store
	// Authority owns credit reservation: the Postgres store when
	// SLOTFARE_DATABASE_URL is set (atomic across replicas), or
	// meter.LocalAuthority wrapping the in-memory ledger in sqlite mode.
	Authority meter.Authority
	// Usage     meter.UsageLogger // nil disables metered-call logging
	Usage meter.UsageLogger // nil disables metered-call logging
	// Receipt signs the x-slotfare-receipt header on metered, non-streaming
	// responses; nil disables receipts entirely (no header, and the
	// well-known key path answers 404). It is a dedicated Ed25519
	// keypair, never the chain spender key.
	Receipt  *receipt.Signer
	Services []Service
	Client   *http.Client // may be nil; New installs a sane default

	// Rate limits, requests per minute: per key (every authenticated
	// request on every service) and per registry model (the global cap on
	// the free inference upstream). Zero disables a layer; main.go sets
	// both from SLOTFARE_KEY_RATE_LIMIT and SLOTFARE_MODEL_RATE_LIMIT.
	KeyRateLimit   int
	ModelRateLimit int

	rateLimiter *rateLimiter // built by New; tests may replace its clock
}

// New wires a Handler. Client is defaulted when nil. Rate limiting stays
// off until KeyRateLimit or ModelRateLimit is set; main.go sets both from
// the environment so production always runs with them on.
func New(keysStore keys.Store, authority meter.Authority, services []Service) *Handler {
	h := &Handler{
		Keys:      keysStore,
		Authority: authority,
		Services:  services,
		Client:    &http.Client{},
	}
	h.rateLimiter = newRateLimiter(time.Now)
	h.setRateLimits()
	return h
}

// setRateLimits registers the current limit settings with the limiter;
// call it again after changing KeyRateLimit or ModelRateLimit.
func (h *Handler) setRateLimits() {
	h.rateLimiter.setLimit("key", h.KeyRateLimit)
	h.rateLimiter.setLimit("model", h.ModelRateLimit)
}

// rpcPricer prices a JSON-RPC request body with the method weight table: a
// batch is priced as the sum of its members, everything else as a single
// call with method and params parsed out of the object.
type rpcPricer struct {
	weights meter.Weights
}

// NewRPCPricer returns the JSON-RPC method-weight Pricer.
func NewRPCPricer(w meter.Weights) Pricer {
	return rpcPricer{weights: w}
}

func (p rpcPricer) Reserve(body []byte) (uint64, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return p.weights.BatchCost(trimmed)
	}
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(trimmed, &req); err != nil {
		// Not a parseable object: charge the unknown-method standard
		// weight rather than refusing, matching Cost's contract.
		cost, _, _ := p.weights.Cost("", nil)
		return cost, nil
	}
	cost, _, ok := p.weights.Cost(req.Method, req.Params)
	if !ok {
		log.Printf("slotfared: unrecognized method %q charged standard weight", req.Method)
	}
	return cost, nil
}

// freeInferencePaths are the discovery paths that cost nothing: a client
// must not pay to find out what the service offers or whether it is up.
var freeInferencePaths = []string{"/v1/models", "/health"}

// inferencePricer prices OpenAI-compatible inference requests by tokens:
// it reserves an upper bound before forwarding and settles the true usage
// after serving. It implements PathPricer (free discovery paths) and
// ResponseSettler (non-streaming settlement); rpcPricer implements neither,
// so the RPC service keeps its T01 behavior untouched. With a non-empty
// registry it also implements ModelPricer: requests are validated against
// it, mapped to their upstream id and priced at their own schedule.
type inferencePricer struct {
	prices meter.TokenPrices // the fallback schedule (SLOTFARE_TOKEN_PRICES)
	models meter.Models      // empty means today's single-model behavior
}

// NewInferencePricer returns the token-priced Pricer for the inference
// service with no model registry: every request prices at one schedule.
func NewInferencePricer(p meter.TokenPrices) Pricer {
	return inferencePricer{prices: p}
}

// NewInferencePricerWithModels returns the registry-aware Pricer: the
// request's model must be a registry id, is mapped to its upstream id and
// priced at that entry's schedule; p remains the fallback schedule for
// free discovery paths and registry-less arithmetic.
func NewInferencePricerWithModels(p meter.TokenPrices, m meter.Models) Pricer {
	return inferencePricer{prices: p, models: m}
}

func (p inferencePricer) Reserve(body []byte) (uint64, error) {
	return p.ReservePath("", body)
}

// ReservePath prices by the post-prefix path: the free discovery paths cost
// zero, everything else is priced from the body alone.
func (p inferencePricer) ReservePath(path string, body []byte) (uint64, error) {
	for _, free := range freeInferencePaths {
		if path == free {
			return 0, nil
		}
	}
	return p.prices.Reserve(body)
}

// SettleResponse is the true cost of a served non-streaming response, read
// from the upstream's own usage report. It reports false when no usage was
// found; the handler then settles the full reservation.
func (p inferencePricer) SettleResponse(respBody []byte) (uint64, bool) {
	usage, ok := meter.ParseUsage(respBody)
	if !ok {
		return 0, false
	}
	return p.prices.Settle(usage), true
}

// Resolve validates the request's model against the registry and rewrites
// the body to the upstream id. With an empty registry, or on a free
// discovery path, it returns an empty model id and the body unchanged -
// today's behavior, byte-for-byte.
func (p inferencePricer) Resolve(path string, body []byte) (string, []byte, error) {
	for _, free := range freeInferencePaths {
		if path == free {
			return "", body, nil
		}
	}
	if len(p.models) == 0 {
		return "", body, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", nil, fmt.Errorf("inference body is not JSON")
	}
	raw, present := obj["model"]
	id, isID := raw.(string)
	if !present || !isID || id == "" {
		return "", nil, fmt.Errorf("missing model; valid model ids: %s", strings.Join(p.models.IDs(), ", "))
	}
	m, ok := p.models.Lookup(id)
	if !ok {
		return "", nil, fmt.Errorf("unknown model %q; valid model ids: %s", id, strings.Join(p.models.IDs(), ", "))
	}
	obj["model"] = m.UpstreamID
	// A registry entry's service tier rides into the forwarded body as
	// service_tier; entries without one send no such field at all.
	if m.ServiceTier != "" {
		obj["service_tier"] = m.ServiceTier
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return "", nil, err
	}
	return m.ID, rewritten, nil
}

// ReserveModel holds the upper bound at the resolved model's own schedule;
// an empty model id falls back to the path-based reservation, which is the
// free discovery paths and the empty-registry case.
func (p inferencePricer) ReserveModel(modelID string, path string, body []byte) (uint64, error) {
	if m, ok := p.models.Lookup(modelID); ok {
		return m.Prices().Reserve(body)
	}
	return p.ReservePath(path, body)
}

// SettleModel settles the true usage at the resolved model's own schedule;
// an empty model id settles at the fallback schedule, as today.
func (p inferencePricer) SettleModel(modelID string, respBody []byte) (uint64, bool) {
	usage, ok := meter.ParseUsage(respBody)
	if !ok {
		return 0, false
	}
	if m, found := p.models.Lookup(modelID); found {
		return m.Prices().Settle(usage), true
	}
	return p.prices.Settle(usage), true
}

// SettleStream settles a buffered SSE body from its final-chunk usage report
// at the resolved model's own schedule; an empty model id settles at the
// fallback schedule. Bodies without any usage report - including streams
// from upstreams that do not support stream_options - report false so the
// handler keeps the full reservation pending.
func (p inferencePricer) SettleStream(modelID string, respBody []byte) (uint64, bool) {
	usage, ok := meter.ParseSSEUsage(respBody)
	if !ok {
		return 0, false
	}
	if m, found := p.models.Lookup(modelID); found {
		return m.Prices().Settle(usage), true
	}
	return p.prices.Settle(usage), true
}

// ModelKind returns the registry entry's service kind so the recorded
// usage row can label the service; empty for an empty id, an empty
// registry, an unknown id and chat entries - chat is the unlabelled
// default, so its rows stay byte-identical to the pre-label shape.
func (p inferencePricer) ModelKind(modelID string) string {
	if modelID == "" || len(p.models) == 0 {
		return ""
	}
	m, ok := p.models.Lookup(modelID)
	if !ok {
		return ""
	}
	if m.Kind == "embeddings" || m.Kind == "reranker" {
		return m.Kind
	}
	return ""
}

// ServeHTTP implements http.Handler: authenticate, route by prefix, price,
// reserve, forward, and either release (upstream failure) or leave the
// reservation pending (success). Every rejection is JSON-RPC shaped so
// client libraries surface it properly.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	presented, fromPath := extractKey(r)
	if presented == "" {
		writeJSONRPCError(w, http.StatusUnauthorized, codeUnauthorized, "unauthorized")
		return
	}
	k, ok, err := h.Keys.Lookup(ctx, keys.HashKey(presented))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// A client hangup: the connection is already gone, so at most
			// one debug-level line and no response.
			log.Printf("slotfared: debug: key lookup canceled: client disconnected")
			return
		}
		log.Printf("slotfared: key lookup failed: %v", err)
		writeJSONRPCError(w, http.StatusInternalServerError, codeInvalidRequest, "internal error")
		return
	}
	if !ok {
		writeJSONRPCError(w, http.StatusUnauthorized, codeUnauthorized, "unauthorized")
		return
	}
	if k.Revoked {
		// A revoked key answers exactly like an unknown one: no
		// distinction, no leak.
		writeJSONRPCError(w, http.StatusUnauthorized, codeUnauthorized, "unauthorized")
		return
	}
	if err := h.Keys.Touch(ctx, k.Hash); err != nil {
		// Usage counting is best-effort: a transient database error logs
		// and the request still proceeds.
		log.Printf("slotfared: touch key: %v", err)
	}

	// Wall-clock start of the metered exchange: the recorded duration is
	// measured from here to the moment the usage row is written - after
	// the upstream response has fully settled, or at time-to-failure for
	// error rows. Rate-limited and rejected requests are never metered,
	// so the start only matters for the rows below.
	start := time.Now()

	// The per-key rate limit runs before the body is even read: it caps
	// how fast one key may call, on every service. A rejection is never
	// metered and never reaches the upstream.
	if ok, wait := h.rateLimiter.allow("key", string(k.Hash[:])); !ok {
		writeRateLimited(w, wait, "rate limit exceeded for key")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, codeInvalidRequest, "invalid request")
		return
	}

	// When the key came from the path, its segment is not part of the
	// service path: strip it before routing so /<key>/rpc/v1/x routes the
	// same as /rpc/v1/x.
	routePath := r.URL.Path
	if fromPath {
		rest := strings.TrimPrefix(r.URL.Path, "/")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			routePath = rest[i:]
		} else {
			routePath = ""
		}
	}

	svc, rest, ok := h.match(routePath)
	if !ok {
		writeJSONRPCError(w, http.StatusNotFound, codeNotFound, "no service for path "+routePath+"; available prefixes: "+h.prefixList())
		return
	}
	if !h.allowed(svc, rest) {
		writeJSONRPCError(w, http.StatusNotFound, codeNotFound, "path not allowed for service "+svc.Name)
		return
	}

	// Registry-aware pricers resolve the requested model before anything
	// else: an unknown or missing model is a clean 400 naming the valid
	// ids - never a passthrough to the upstream.
	modeler, hasModels := svc.Pricer.(ModelPricer)
	modelID := ""
	if hasModels {
		var resolveErr error
		modelID, body, resolveErr = modeler.Resolve(rest, body)
		if resolveErr != nil {
			writeJSONRPCError(w, http.StatusBadRequest, codeInvalidRequest, resolveErr.Error())
			return
		}
		// The global per-model cap: no matter how many keys exist, the
		// free upstream sees at most ModelRateLimit requests per minute
		// per model. Free discovery paths and rejected models do not
		// consume the budget.
		if modelID != "" {
			if ok, wait := h.rateLimiter.allow("model", modelID); !ok {
				writeRateLimited(w, wait, "rate limit exceeded for model "+modelID)
				return
			}
		}
	}

	// Services that price by path use the post-prefix remainder; others
	// price the body alone. Registry-aware pricers reserve at the
	// resolved model's own schedule.
	// The resolved entry's service kind labels the usage rows this
	// exchange records: empty for chat, the free discovery paths and
	// pricers without a registry.
	kind := ""
	if kp, ok := svc.Pricer.(KindProvider); ok && modelID != "" {
		kind = kp.ModelKind(modelID)
	}
	var reserved uint64
	if hasModels {
		reserved, err = modeler.ReserveModel(modelID, rest, body)
	} else if pp, ok := svc.Pricer.(PathPricer); ok {
		// Services that price by path use the post-prefix remainder; others
		// price the body alone.
		reserved, err = pp.ReservePath(rest, body)
	} else {
		reserved, err = svc.Pricer.Reserve(body)
	}
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}

	if err := h.Authority.Reserve(ctx, k.AccountID, reserved); err != nil {
		if errors.Is(err, meter.ErrInsufficientCredits) {
			// Nothing settled, so no charge header; the balance is carried
			// so the client sees how little it holds without a second call.
			if bal := h.readBalance(ctx, k.AccountID); bal != nil {
				w.Header().Set(headerBalance, fmt.Sprintf("%d", *bal))
			}
			writeJSONRPCError(w, http.StatusPaymentRequired, codeInsufficientCred, "insufficient credits")
			return
		}
		writeJSONRPCError(w, http.StatusInternalServerError, codeInvalidRequest, "internal error")
		return
	}

	// The spendable balance as of reservation time: streaming responses
	// send it before settlement is knowable, and non-streaming responses
	// re-read after the release. Free paths (reserved == 0) carry no
	// slotfare headers at all, so it stays nil there.
	var balanceAtReserve *uint64
	if reserved > 0 {
		balanceAtReserve = h.readBalance(ctx, k.AccountID)
	}

	upstreamResp, err := h.forward(svc.Upstream, svc.UpstreamKey, r, rest, body, svc.StreamUsage)
	if err != nil {
		h.Authority.Release(ctx, k.AccountID, reserved)
		dur := uint64(time.Since(start).Milliseconds())
		h.recordUsage(meter.UsageRow{
			AccountID:  k.AccountID,
			KeyPrefix:  k.Prefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Status:     "error",
			DurationMS: &dur,
		})
		writeJSONRPCError(w, http.StatusBadGateway, codeUpstreamError, "upstream error")
		return
	}
	defer upstreamResp.Body.Close()
	if upstreamResp.StatusCode >= 400 {
		h.Authority.Release(ctx, k.AccountID, reserved)
		io.Copy(io.Discard, upstreamResp.Body)
		dur := uint64(time.Since(start).Milliseconds())
		h.recordUsage(meter.UsageRow{
			AccountID:  k.AccountID,
			KeyPrefix:  k.Prefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Status:     "error",
			DurationMS: &dur,
		})
		writeJSONRPCError(w, http.StatusBadGateway, codeUpstreamError, "upstream error")
		return
	}

	h.serveResponse(ctx, w, k.AccountID, k.Prefix, svc, modelID, kind, reserved, balanceAtReserve, body, upstreamResp, start)
}

// balancer reports the authority's balance reader when it implements
// meter.BalanceProvider, nil otherwise - in which case no slotfare headers
// are sent at all, exactly today's behavior.
func (h *Handler) balancer() meter.BalanceProvider {
	bp, _ := h.Authority.(meter.BalanceProvider)
	return bp
}

// readBalance is a best-effort read of the account's spendable micro-
// credits: on error or when the authority provides no balances it logs and
// reports nil, mirroring the Keys.Touch and recordUsage handling - a
// transient database error must not fail a served request.
func (h *Handler) readBalance(ctx context.Context, account string) *uint64 {
	balancer := h.balancer()
	if balancer == nil {
		return nil
	}
	bal, err := balancer.Balance(ctx, account)
	if err != nil {
		log.Printf("slotfared: read balance: %v", err)
		return nil
	}
	return &bal
}

// writeSlotfareHeaders sets the slotfare response headers from their values:
// each non-nil pointer sets its header, each nil pointer omits it, so a
// caller can express "settled but balance unknown" or "streaming, charge
// not knowable yet" without lying. Call before WriteHeader.
func writeSlotfareHeaders(w http.ResponseWriter, charged, balance *uint64) {
	if charged != nil {
		w.Header().Set(headerCreditsCharged, fmt.Sprintf("%d", *charged))
	}
	if balance != nil {
		w.Header().Set(headerBalance, fmt.Sprintf("%d", *balance))
	}
}

// serveResponse writes the upstream response back to the client and settles
// the reservation. A pricer that implements ResponseSettler settles the
// true cost out of the non-streaming response body and releases the
// difference between the hold and the truth; a pricer that implements
// StreamSettler settles streaming responses from their final-chunk usage
// report the same way. Streaming responses whose pricer cannot settle them
// keep the full reservation pending. When a registry resolved the model,
// settlement runs at that model's schedule and the recorded row carries the
// registry id.
func (h *Handler) serveResponse(ctx context.Context, w http.ResponseWriter, account, keyPrefix string, svc Service, modelID, kind string, reserved uint64, balanceAtReserve *uint64, reqBody []byte, upstreamResp *http.Response, start time.Time) {
	// The server-assigned request nonce for this exchange: drawn once when
	// receipts are enabled and recorded on the "ok" usage row(s) it signs.
	// With no receipt signer the rows record empty nonces, like pre-
	// receipt rows.
	nonce := ""
	if h.Receipt != nil {
		nonce = receipt.NewNonce()
	}
	settler, _ := svc.Pricer.(ResponseSettler)
	if isStreaming(reqBody, upstreamResp) {
		if streamer, ok := svc.Pricer.(StreamSettler); ok {
			h.serveStream(ctx, w, account, keyPrefix, svc, modelID, kind, reserved, balanceAtReserve, upstreamResp, streamer, start)
			return
		}
		// No stream settlement for this service: copy through and keep the
		// full reservation pending.
		w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
		// A streaming response sends headers before settlement is knowable:
		// the balance as of reservation time, no charge header.
		writeSlotfareHeaders(w, nil, balanceAtReserve)
		w.WriteHeader(upstreamResp.StatusCode)
		io.Copy(w, upstreamResp.Body)
		dur := uint64(time.Since(start).Milliseconds())
		h.recordUsage(meter.UsageRow{
			AccountID:  account,
			KeyPrefix:  keyPrefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Credits:    reserved,
			Status:     "ok",
			DurationMS: &dur,
			Nonce:      nonce,
		})
		return
	}
	if settler == nil {
		// No settlement at all for this service: copy through and keep the
		// full reservation pending.
		w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
		// The recorded row charges the full reservation; the balance is read
		// after settlement - which releases nothing here, so it equals the
		// reservation-time figure. Free paths carry no slotfare headers.
		if reserved > 0 {
			writeSlotfareHeaders(w, &reserved, h.readBalance(ctx, account))
		}
		row := meter.UsageRow{
			AccountID:  account,
			KeyPrefix:  keyPrefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Credits:    reserved,
			Status:     "ok",
			DurationMS: ptrUint64(uint64(time.Since(start).Milliseconds())),
			Nonce:      nonce,
		}
		if reserved > 0 {
			h.setReceiptHeader(w, row)
		}
		w.WriteHeader(upstreamResp.StatusCode)
		io.Copy(w, upstreamResp.Body)
		h.recordUsage(row)
		return
	}

	respBody, err := io.ReadAll(upstreamResp.Body)
	if err != nil {
		// The body could not be read in full: settle the whole hold and
		// write what arrived rather than losing either.
		w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
		if reserved > 0 {
			writeSlotfareHeaders(w, &reserved, h.readBalance(ctx, account))
		}
		row := meter.UsageRow{
			AccountID:  account,
			KeyPrefix:  keyPrefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Credits:    reserved,
			Status:     "ok",
			DurationMS: ptrUint64(uint64(time.Since(start).Milliseconds())),
			Nonce:      nonce,
		}
		if reserved > 0 {
			h.setReceiptHeader(w, row)
		}
		w.WriteHeader(upstreamResp.StatusCode)
		w.Write(respBody)
		h.recordUsage(row)
		return
	}
	var trueCost uint64
	var hasUsage bool
	if mp, ok := svc.Pricer.(ModelPricer); ok && modelID != "" {
		trueCost, hasUsage = mp.SettleModel(modelID, respBody)
	} else {
		trueCost, hasUsage = settler.SettleResponse(respBody)
	}
	if !hasUsage {
		// Serving happened but the upstream reported no usage: settle
		// the full reservation, since the cost is unknown.
		trueCost = reserved
	}
	if reserved > trueCost {
		h.Authority.Release(ctx, account, reserved-trueCost)
	}
	// When trueCost >= reserved nothing is released: the estimate was low
	// and the higher figure stays pending.
	row := meter.UsageRow{
		AccountID:  account,
		KeyPrefix:  keyPrefix,
		Timestamp:  time.Now().UTC(),
		Service:    svc.Name,
		Kind:       kind,
		Credits:    trueCost,
		Status:     "ok",
		DurationMS: ptrUint64(uint64(time.Since(start).Milliseconds())),
		Nonce:      nonce,
	}
	if usage, ok := meter.ParseUsage(respBody); ok {
		// Only the upstream's own report fills the row's token fields;
		// a missing usage object leaves them zero.
		row.Model = usage.Model
		row.PromptTokens = usage.PromptTokens
		row.CachedTokens = usage.CachedPromptTokens
		row.CompletionTokens = usage.CompletionTokens
	}
	if modelID != "" {
		// The row records the console-facing registry id, not whatever
		// the upstream echoes back.
		row.Model = modelID
	}
	w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
	// Exact settled values: the charge is what the usage row records and
	// the balance is read after the release. Free paths carry no slotfare
	// headers.
	if reserved > 0 {
		writeSlotfareHeaders(w, &trueCost, h.readBalance(ctx, account))
		h.setReceiptHeader(w, row)
	}
	w.WriteHeader(upstreamResp.StatusCode)
	w.Write(respBody)
	h.recordUsage(row)
}

// setReceiptHeader signs the usage row's canonical fields and sets the
// x-slotfare-receipt header before WriteHeader. It is a no-op when receipts
// are disabled (nil signer) or signing fails - mirroring recordUsage, a
// failure logs and the response still goes out. Callers gate it on
// reserved > 0: free paths record nothing paid and carry no receipt, only
// served, non-streaming responses whose settled usage is recorded do.
func (h *Handler) setReceiptHeader(w http.ResponseWriter, row meter.UsageRow) {
	if h.Receipt == nil {
		return
	}
	value, err := h.Receipt.Sign(receipt.PayloadFromRow(row))
	if err != nil {
		log.Printf("slotfared: sign receipt: %v", err)
		return
	}
	w.Header().Set(headerReceipt, value)
}

// serveStream buffers a streaming (SSE) response, settles it from the final
// chunk's usage report when one is present, and writes the buffered body
// back byte-for-byte. A stream without any usage report - or one whose body
// cannot be read in full - keeps the full reservation pending, exactly like
// the pre-settlement behavior.
func (h *Handler) serveStream(ctx context.Context, w http.ResponseWriter, account, keyPrefix string, svc Service, modelID, kind string, reserved uint64, balanceAtReserve *uint64, upstreamResp *http.Response, streamer StreamSettler, start time.Time) {
	sseBody, err := io.ReadAll(upstreamResp.Body)
	if err != nil {
		// The body could not be read in full: settle the whole hold and
		// write what arrived rather than losing either.
		w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
		// Streaming sends headers before settlement is knowable: the balance
		// as of reservation time, no charge header.
		writeSlotfareHeaders(w, nil, balanceAtReserve)
		w.WriteHeader(upstreamResp.StatusCode)
		w.Write(sseBody)
		dur := uint64(time.Since(start).Milliseconds())
		h.recordUsage(meter.UsageRow{
			AccountID:  account,
			KeyPrefix:  keyPrefix,
			Timestamp:  time.Now().UTC(),
			Service:    svc.Name,
			Model:      modelID,
			Kind:       kind,
			Credits:    reserved,
			Status:     "ok",
			DurationMS: &dur,
		})
		return
	}
	trueCost, hasUsage := streamer.SettleStream(modelID, sseBody)
	if !hasUsage {
		// Serving happened but the stream reported no usage: settle the
		// full reservation, since the cost is unknown.
		trueCost = reserved
	}
	if reserved > trueCost {
		h.Authority.Release(ctx, account, reserved-trueCost)
	}
	row := meter.UsageRow{
		AccountID:  account,
		KeyPrefix:  keyPrefix,
		Timestamp:  time.Now().UTC(),
		Service:    svc.Name,
		Kind:       kind,
		Credits:    trueCost,
		Status:     "ok",
		DurationMS: ptrUint64(uint64(time.Since(start).Milliseconds())),
	}
	if hasUsage {
		if usage, ok := meter.ParseSSEUsage(sseBody); ok {
			// Only the upstream's own report fills the row's token fields;
			// a missing usage object leaves them zero.
			row.Model = usage.Model
			row.PromptTokens = usage.PromptTokens
			row.CachedTokens = usage.CachedPromptTokens
			row.CompletionTokens = usage.CompletionTokens
		}
	}
	if modelID != "" {
		// The row records the console-facing registry id, not whatever
		// the upstream echoes back.
		row.Model = modelID
	}
	w.Header().Set("Content-Type", upstreamResp.Header.Get("Content-Type"))
	// Streaming sends headers before settlement is knowable: the balance as
	// of reservation time, and no charge header rather than lie.
	writeSlotfareHeaders(w, nil, balanceAtReserve)
	w.WriteHeader(upstreamResp.StatusCode)
	w.Write(sseBody)
	h.recordUsage(row)
}

// recordUsage best-effort writes one metered-call row: on failure it logs
// and moves on, mirroring the Keys.Touch handling - a transient database
// error must not fail a served request.
func (h *Handler) recordUsage(row meter.UsageRow) {
	if h.Usage == nil {
		return
	}
	if err := h.Usage.RecordUsage(context.Background(), row); err != nil {
		log.Printf("slotfared: record usage: %v", err)
	}
}

// ptrUint64 returns a pointer to v, for the usage row's nullable DurationMS
// field.
func ptrUint64(v uint64) *uint64 {
	return &v
}

// isStreaming reports whether this exchange is a server-sent-events
// stream: the client explicitly asked for "stream": true, or the upstream
// is already answering with text/event-stream. Only an explicit opt-in
// counts; a request body that fails to parse is treated as non-streaming.
func isStreaming(reqBody []byte, resp *http.Response) bool {
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return true
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	if len(bytes.TrimSpace(reqBody)) == 0 {
		return false
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		return false
	}
	return req.Stream
}

// match picks the service with the longest Prefix that the path starts with
// and returns it with the path remainder after the prefix is stripped. On a
// tie the first declared service wins.
func (h *Handler) match(path string) (Service, string, bool) {
	var best Service
	bestLen := -1
	for _, s := range h.Services {
		if strings.HasPrefix(path, s.Prefix) && len(s.Prefix) > bestLen {
			best, bestLen = s, len(s.Prefix)
		}
	}
	if bestLen < 0 {
		return Service{}, "", false
	}
	return best, strings.TrimPrefix(path, best.Prefix), true
}

// allowed reports whether rest (the path after the service prefix) is
// accepted by the service: any path when Allow is empty, otherwise one of
// the listed suffixes.
func (h *Handler) allowed(svc Service, rest string) bool {
	if len(svc.Allow) == 0 {
		return true
	}
	for _, a := range svc.Allow {
		if rest == a {
			return true
		}
	}
	return false
}

// prefixList joins the configured service prefixes for the no-match error.
func (h *Handler) prefixList() string {
	prefixes := make([]string, 0, len(h.Services))
	for _, s := range h.Services {
		prefixes = append(prefixes, s.Prefix)
	}
	return strings.Join(prefixes, ", ")
}

// extractKey pulls the API key from the Authorization header (Bearer form,
// which wins when present) or, failing that, from the first path segment.
// The second return value reports whether the key came from the path.
func extractKey(r *http.Request) (string, bool) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		key := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if key != "" {
			return key, false
		}
	}
	seg := strings.TrimPrefix(r.URL.Path, "/")
	if i := strings.IndexByte(seg, '/'); i > 0 {
		return seg[:i], true
	}
	if seg != "" {
		return seg, true
	}
	return "", false
}

// forward builds and sends the upstream request per the forwarding rules:
// original method, original query string, original body byte-for-byte, and
// the path with the service prefix already stripped onto the upstream's own
// path component. All inbound headers are dropped except Content-Type; when
// the service carries an UpstreamKey it is injected as the outbound bearer,
// replacing any client key. When the service opts into stream usage
// reporting (StreamUsage) and the request is a JSON body asking for a
// stream, stream_options.include_usage is injected so the upstream reports
// usage in the final SSE chunk; without the flag no upstream sees any
// change.
func (h *Handler) forward(upstream *url.URL, upstreamKey string, r *http.Request, path string, body []byte, streamUsage bool) (*http.Response, error) {
	u := *upstream
	u.Path = joinUpstreamPath(upstream.Path, path)
	if r.URL.RawQuery != "" {
		u.RawQuery = r.URL.RawQuery
	}
	if streamUsage && wantsStream(body) {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			obj["stream_options"] = map[string]any{"include_usage": true}
			rewritten, err := json.Marshal(obj)
			if err == nil {
				body = rewritten
			}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if upstreamKey != "" {
		req.Header.Set("Authorization", "Bearer "+upstreamKey)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// joinUpstreamPath joins the upstream's own path component with the routed
// path: the upstream's trailing slash is trimmed and the routed path's
// leading slash is kept, so /inference + /v1/x becomes /inference/v1/x and
// a bare-host upstream (empty or root path) keeps today's stripped path
// unchanged.
func joinUpstreamPath(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// wantsStream reports whether a request body is valid JSON asking for a
// stream: "stream": true on an object body. Non-JSON bodies and bodies
// without the flag report false, so the opt-in never rewrites anything it
// cannot parse.
func wantsStream(body []byte) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	v, ok := obj["stream"].(bool)
	return ok && v
}

// writeRateLimited answers a rate-limited request: 429 with Retry-After in
// whole seconds until the current window ends, JSON-RPC shaped so client
// libraries surface it like every other proxy rejection.
func writeRateLimited(w http.ResponseWriter, wait time.Duration, message string) {
	seconds := int(wait.Seconds()) + 1
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	writeJSONRPCError(w, http.StatusTooManyRequests, codeRateLimited, message)
}

// writeJSONRPCError writes a JSON-RPC shaped error body with the given HTTP
// status, so a client library surfaces the failure properly instead of
// seeing bare HTTP text.
func writeJSONRPCError(w http.ResponseWriter, status int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}
