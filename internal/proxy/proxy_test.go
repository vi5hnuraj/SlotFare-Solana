package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
)

// stubUpstream records every request it receives and serves a fixed body.
type stubUpstream struct {
	srv        *httptest.Server
	bodies     chan []byte
	paths      chan string
	methods    chan string
	contentTyp chan string
	auths      chan string
	requests   atomic.Int64
}

func newStubUpstream(t *testing.T, status int, respBody string) *stubUpstream {
	t.Helper()
	s := &stubUpstream{
		bodies:     make(chan []byte, 256),
		paths:      make(chan string, 256),
		methods:    make(chan string, 256),
		contentTyp: make(chan string, 256),
		auths:      make(chan string, 256),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.requests.Add(1)
		s.bodies <- body
		s.paths <- r.URL.Path
		s.methods <- r.Method
		s.contentTyp <- r.Header.Get("Content-Type")
		s.auths <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, respBody)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubUpstream) url() *url.URL {
	u, err := url.Parse(s.srv.URL)
	if err != nil {
		panic(err)
	}
	return u
}

// newTestHandler builds a Handler against a temp-file key store and a fresh
// ledger with one issued key and the given minted balance, serving the RPC
// service under /rpc against the given upstream.
func newTestHandler(t *testing.T, secret string, minted uint64, weights meter.Weights, upstream *url.URL) (*Handler, keys.Key, *meter.Ledger) {
	t.Helper()
	return newMultiTestHandler(t, secret, minted, []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: upstream, Pricer: rpcPricer{weights: weights}},
	})
}

// newMultiTestHandler is newTestHandler for several services.
func newMultiTestHandler(t *testing.T, secret string, minted uint64, services []Service) (*Handler, keys.Key, *meter.Ledger) {
	t.Helper()
	ctx := context.Background()
	store, err := keys.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("keys.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	k := keys.Issue(secret)
	if err := store.Add(ctx, k); err != nil {
		t.Fatalf("keys.Add: %v", err)
	}
	ledger := meter.NewLedger()
	ledger.SetMinted(k.AccountID, minted)
	h := New(store, meter.NewLocalAuthority(ledger), services)
	return h, k, ledger
}

func rpcErrCode(t *testing.T, body []byte) int {
	t.Helper()
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not JSON-RPC shaped: %v\nbody: %s", err, body)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want \"2.0\"", env.JSONRPC)
	}
	if env.Error.Code == 0 {
		t.Error("error.code = 0, want a non-zero JSON-RPC error code")
	}
	return env.Error.Code
}

// 1. Valid Bearer key, covered balance: the upstream body is returned
// byte-for-byte and the reservation is left pending.
func TestForwardValidKey(t *testing.T) {
	const secret = "slotfare-secret-001"
	const cost = 1 * meter.MicroCreditsPerCredit // eth_chainId is trivial, one credit
	upstreamBody := `{"jsonrpc":"2.0","id":1,"result":"0x1"}
`
	stub := newStubUpstream(t, http.StatusOK, upstreamBody)
	h, k, ledger := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != upstreamBody {
		t.Errorf("served body = %q, want the upstream body byte-for-byte %q", got, upstreamBody)
	}
	gotBody := <-stub.bodies
	wantBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	if !bytes.Equal(gotBody, wantBody) {
		t.Errorf("upstream received %q, want %q", gotBody, wantBody)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != cost {
		t.Errorf("pending after success = %d, want %d (reservation left for settlement)", pending, cost)
	}
}

// 2. Unknown key: 401, JSON-RPC shaped, and the upstream is never reached.
func TestUnknownKeyRejected(t *testing.T) {
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h, _, _ := newTestHandler(t, "slotfare-secret-001", 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId"}`))
	req.Header.Set("Authorization", "Bearer not-a-known-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeUnauthorized {
		t.Errorf("error code = %d, want %d", code, codeUnauthorized)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

// A request with no key at all is also a 401.
func TestMissingKeyRejected(t *testing.T) {
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h, _, _ := newTestHandler(t, "slotfare-secret-001", 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

// 3. Insufficient balance: 402, JSON-RPC shaped, and the upstream is never
// reached.
func TestInsufficientBalanceRejected(t *testing.T) {
	const secret = "slotfare-secret-001"
	// eth_getLogs is heavy (20 credits); mint only 10 micro-credits so it
	// cannot cover.
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h, k, _ := newTestHandler(t, secret, 10, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_getLogs","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInsufficientCred {
		t.Errorf("error code = %d, want %d", code, codeInsufficientCred)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
	if pending := len(k.AccountID); pending == 0 {
		t.Fatal("sanity: account id empty")
	}
}

// 4. Upstream 500: the client gets 502 and the reservation is released.
func TestUpstreamFailureReleasesReservation(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	h, k, ledger := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeUpstreamError {
		t.Errorf("error code = %d, want %d", code, codeUpstreamError)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 0 {
		t.Errorf("pending after upstream failure = %d, want 0 (reservation released)", pending)
	}
}

// 5. A batch is priced as the sum of its members and forwarded exactly once.
func TestBatchPricedAsSumForwardedOnce(t *testing.T) {
	const secret = "slotfare-secret-001"
	// two trivial (1+1) + one standard (2) = 4 credits.
	batch := `[{"method":"eth_chainId"},{"method":"net_version"},{"method":"eth_call","params":["0x1","latest"]}]`
	stub := newStubUpstream(t, http.StatusOK, `[{"result":"0x1"},{"result":"0x1"},{"result":"0x"}]`)
	h, k, ledger := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(batch))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if n := stub.requests.Load(); n != 1 {
		t.Fatalf("upstream received %d requests, want exactly 1", n)
	}
	if got, want := <-stub.bodies, []byte(batch); !bytes.Equal(got, want) {
		t.Errorf("upstream received %q, want the batch %q", got, want)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != 4*meter.MicroCreditsPerCredit {
		t.Errorf("pending after batch = %d, want %d (sum of members in micro-credits)", pending, 4*meter.MicroCreditsPerCredit)
	}
}

// 6. Byte-exact forwarding: an odd body arrives at the upstream identical to
// what the client sent, via both key forms.
func TestByteExactForwardingBothKeyForms(t *testing.T) {
	const secret = "slotfare-secret-001"
	oddBody := "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"eth_call\",\"params\":[\"0xabc\",\"latest\"]}\n\t\n"
	// Pad past the default peek buffer so no buffering shortcut can hide a
	// copy bug.
	oddBody += strings.Repeat(" ", 8192) + ",\"extra\":\"padding\""

	for name, mkReq := range map[string]func(*http.Request){
		"bearer": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
			r.URL.Path = "/rpc"
		},
		"path": func(r *http.Request) {
			r.URL.Path = "/" + secret + "/rpc"
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := newStubUpstream(t, http.StatusOK, `{"ok":true}`)
			h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())

			req := httptest.NewRequest("POST", "http://slotfared/rpc", strings.NewReader(oddBody))
			mkReq(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			got := <-stub.bodies
			if !bytes.Equal(got, []byte(oddBody)) {
				t.Errorf("upstream body differs from client body:\n got %d bytes\nwant %d bytes", len(got), len(oddBody))
			}
		})
	}
}

// 7. Concurrency: a balance covering half of N parallel requests serves
// exactly half and rejects the rest.
func TestConcurrentRequestsServeExactlyHalf(t *testing.T) {
	const (
		secret  = "slotfare-secret-001"
		mint    = 500 * meter.MicroCreditsPerCredit
		callers = 100
		perCall = 10 * meter.MicroCreditsPerCredit
		wantOK  = mint / perCall // 50
	)
	// Make the standard class cost 10 so each eth_call reserves exactly
	// perCall credits.
	weightsPath := filepath.Join(t.TempDir(), "weights.json")
	if err := os.WriteFile(weightsPath, []byte(`{"classes": {"standard": 10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	weights, err := meter.LoadWeights(weightsPath)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}

	stub := newStubUpstream(t, http.StatusOK, `{"result":"0x1"}`)
	h, k, ledger := newTestHandler(t, secret, mint, weights, stub.url())

	results := make(chan int, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_call","params":[]}`))
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			results <- rec.Code
		}()
	}
	wg.Wait()
	close(results)
	ok, rejected := 0, 0
	for status := range results {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusPaymentRequired:
			rejected++
		default:
			t.Fatalf("unexpected status %d in concurrent run", status)
		}
	}
	if ok != wantOK {
		t.Errorf("served %d requests, want exactly %d", ok, wantOK)
	}
	if rejected != callers-wantOK {
		t.Errorf("rejected %d requests, want %d", rejected, callers-wantOK)
	}
	if n := stub.requests.Load(); n != int64(wantOK) {
		t.Errorf("upstream received %d requests, want %d", n, wantOK)
	}
	if pending := ledger.PendingTotal()[k.AccountID]; pending != mint {
		t.Errorf("pending after concurrent run = %d, want %d", pending, mint)
	}
}

// 8. A batch over the configured limit is rejected with 400 and never
// reaches the upstream.
func TestOverLimitBatchRejected(t *testing.T) {
	weightsPath := filepath.Join(t.TempDir(), "weights.json")
	// batch_limit 2 forces a three-member batch over the limit.
	if err := os.WriteFile(weightsPath, []byte(`{"batch_limit": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	weights, err := meter.LoadWeights(weightsPath)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}

	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h, _, _ := newTestHandler(t, "slotfare-secret-001", 100, weights, stub.url())

	batch := `[{"method":"eth_chainId"},{"method":"eth_chainId"},{"method":"eth_chainId"}]`
	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(batch))
	req.Header.Set("Authorization", "Bearer slotfare-secret-001")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInvalidRequest {
		t.Errorf("error code = %d, want %d", code, codeInvalidRequest)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

// 9. A request under the second service's prefix reaches that upstream with
// the prefix stripped; the RPC upstream sees nothing.
func TestSecondServiceForwardsStrippedPath(t *testing.T) {
	const secret = "slotfare-secret-001"
	rpcStub := newStubUpstream(t, http.StatusOK, `{"ok":"rpc"}`)
	infStub := newStubUpstream(t, http.StatusOK, `{"ok":"infer"}`)
	services := []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: rpcStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
		{Name: "inference", Prefix: "/infer", Upstream: infStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	}
	h, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, services)

	req := httptest.NewRequest("POST", "/infer/v1/foo", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-infStub.paths; got != "/v1/foo" {
		t.Errorf("inference upstream path = %q, want %q (prefix stripped)", got, "/v1/foo")
	}
	if n := rpcStub.requests.Load(); n != 0 {
		t.Errorf("rpc upstream received %d requests, want 0", n)
	}
}

// joinUpstreamPath joins the upstream's own path component with the routed
// path: a bare-host upstream keeps today's stripped path, an upstream with
// a path component gets it joined without dropping or doubling segments.
func TestJoinUpstreamPath(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"", "/v1/chat/completions", "/v1/chat/completions"}, // bare host (NVIDIA form)
		{"/inference", "/v1/chat/completions", "/inference/v1/chat/completions"},
		{"/inference/", "/v1/chat/completions", "/inference/v1/chat/completions"}, // trailing slash
		{"/", "/v1/chat/completions", "/v1/chat/completions"},                     // root path
	}
	for _, c := range cases {
		if got := joinUpstreamPath(c.base, c.path); got != c.want {
			t.Errorf("joinUpstreamPath(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

// An inference upstream configured with a path component (Fireworks' base
// URL is https://api.fireworks.ai/inference) receives the routed path
// joined onto it: /infer/v1/chat/completions lands on
// /inference/v1/chat/completions, not /v1/chat/completions.
func TestForwardJoinsUpstreamPath(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusOK, `{"ok":true}`)
	upstream, err := url.Parse(stub.url().String() + "/inference")
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, []Service{
		{Name: "inference", Prefix: "/infer", Upstream: upstream, Pricer: NewInferencePricer(meter.DefaultTokenPrices()), Allow: inferenceAllowlist},
	})

	req := httptest.NewRequest("POST", "/infer/v1/chat/completions", strings.NewReader(`{"max_tokens":10,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-stub.paths; got != "/inference/v1/chat/completions" {
		t.Errorf("upstream path = %q, want %q (the upstream's own path kept, not dropped or doubled)", got, "/inference/v1/chat/completions")
	}
}

// 10. When one service's prefix is a prefix of another's, the longer match
// wins.
func TestLongestPrefixWins(t *testing.T) {
	const secret = "slotfare-secret-001"
	shortStub := newStubUpstream(t, http.StatusOK, `{"ok":"short"}`)
	longStub := newStubUpstream(t, http.StatusOK, `{"ok":"long"}`)
	services := []Service{
		{Name: "short", Prefix: "/a", Upstream: shortStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
		{Name: "long", Prefix: "/a/b", Upstream: longStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	}
	h, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, services)

	req := httptest.NewRequest("POST", "/a/b/x", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-longStub.paths; got != "/x" {
		t.Errorf("long-prefix upstream path = %q, want %q", got, "/x")
	}
	if n := shortStub.requests.Load(); n != 0 {
		t.Errorf("short-prefix upstream received %d requests, want 0", n)
	}
}

// 11. A path no service claims is a 404 naming the available prefixes, and
// no upstream is reached.
func TestUnmatchedPath404NoUpstream(t *testing.T) {
	const secret = "slotfare-secret-001"
	rpcStub := newStubUpstream(t, http.StatusOK, `{}`)
	infStub := newStubUpstream(t, http.StatusOK, `{}`)
	services := []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: rpcStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
		{Name: "inference", Prefix: "/infer", Upstream: infStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	}
	h, _, _ := newMultiTestHandler(t, secret, 100, services)

	req := httptest.NewRequest("POST", "/nope", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeNotFound {
		t.Errorf("error code = %d, want %d", code, codeNotFound)
	}
	if body := rec.Body.String(); !strings.Contains(body, "/rpc") || !strings.Contains(body, "/infer") {
		t.Errorf("error message %q does not name the available prefixes", body)
	}
	if n := rpcStub.requests.Load() + infStub.requests.Load(); n != 0 {
		t.Errorf("upstreams received %d requests, want 0", n)
	}
}

// 12. A path not in a service's Allow list is a 404 without reaching the
// upstream; an allowed path forwards normally.
func TestAllowListRejectsUnknownPath(t *testing.T) {
	const secret = "slotfare-secret-001"
	infStub := newStubUpstream(t, http.StatusOK, `{"ok":true}`)
	services := []Service{
		{Name: "inference", Prefix: "/infer", Upstream: infStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}, Allow: []string{"/v1/models"}},
	}
	h, _, _ := newMultiTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, services)

	req := httptest.NewRequest("POST", "/infer/v1/other", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeNotFound {
		t.Errorf("error code = %d, want %d", code, codeNotFound)
	}
	if n := infStub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}

	req = httptest.NewRequest("POST", "/infer/v1/models", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("allowed path status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := <-infStub.paths; got != "/v1/models" {
		t.Errorf("upstream path = %q, want %q", got, "/v1/models")
	}
}

// 13. An insufficient balance is a 402 for either service, and no upstream
// is reached.
func TestInsufficientBalanceEitherService(t *testing.T) {
	const secret = "slotfare-secret-001"
	// eth_getLogs is heavy (20 credits); mint only 10 micro-credits so it
	// cannot cover.
	rpcStub := newStubUpstream(t, http.StatusOK, `{}`)
	infStub := newStubUpstream(t, http.StatusOK, `{}`)
	services := []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: rpcStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
		{Name: "inference", Prefix: "/infer", Upstream: infStub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	}
	h, _, _ := newMultiTestHandler(t, secret, 10, services)

	for _, target := range []string{"/rpc", "/infer/v1/foo"} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{"method":"eth_getLogs","params":[]}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusPaymentRequired {
			t.Fatalf("%s: status = %d, want 402", target, rec.Code)
		}
		if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInsufficientCred {
			t.Errorf("%s: error code = %d, want %d", target, code, codeInsufficientCred)
		}
	}
	if n := rpcStub.requests.Load() + infStub.requests.Load(); n != 0 {
		t.Errorf("upstreams received %d requests, want 0", n)
	}
}

// A metered call whose upstream answers 500 records exactly one error row
// carrying the time-to-failure as its duration.
func TestUpstreamFailureRecordsDuration(t *testing.T) {
	const secret = "slotfare-secret-001"
	stub := newStubUpstream(t, http.StatusInternalServerError, `{"error":"boom"}`)
	h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), stub.url())
	usage := &recUsage{}
	h.Usage = usage

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Status != "error" {
		t.Errorf("row status = %q, want error", row.Status)
	}
	if row.DurationMS == nil {
		t.Error("error row DurationMS is nil, want the time-to-failure recorded")
	}
}

// A metered call against a slow upstream records a plausible duration: the
// upstream sleeps before answering, so the recorded wall time must cover it.
func TestMeteredCallRecordsPlausibleDuration(t *testing.T) {
	const secret = "slotfare-secret-001"
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	t.Cleanup(slow.Close)
	slowURL, _ := url.Parse(slow.URL)
	h, _, _ := newTestHandler(t, secret, 100*meter.MicroCreditsPerCredit, meter.DefaultWeights(), slowURL)
	usage := &recUsage{}
	h.Usage = usage

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId","params":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(usage.rows) != 1 {
		t.Fatalf("usage rows = %+v, want exactly one", usage.rows)
	}
	row := usage.rows[0]
	if row.Status != "ok" {
		t.Errorf("row status = %q, want ok", row.Status)
	}
	if row.DurationMS == nil {
		t.Fatal("ok row DurationMS is nil, want a recorded duration")
	}
	// The upstream slept 25ms; the recorded wall time must cover it with a
	// margin, and must never be a fabricated zero.
	if *row.DurationMS < 10 {
		t.Errorf("recorded duration = %d ms, want at least 10 (upstream slept 25ms)", *row.DurationMS)
	}
}

// failKeys is a keys.Store whose Lookup returns a configurable error; every
// other method is a no-op. It pins the ServeHTTP failure policy for the key
// lookup without touching a database.
type failKeys struct {
	lookupErr error
}

func (k *failKeys) Add(ctx context.Context, key keys.Key) error { return nil }

func (k *failKeys) Lookup(ctx context.Context, h [32]byte) (keys.Key, bool, error) {
	return keys.Key{}, false, k.lookupErr
}

func (k *failKeys) ListByAccount(ctx context.Context, accountID string) ([]keys.Key, error) {
	return nil, nil
}

func (k *failKeys) RevokeByPrefix(ctx context.Context, accountID, prefix string) (bool, error) {
	return false, nil
}

func (k *failKeys) Touch(ctx context.Context, h [32]byte) error { return nil }

func (k *failKeys) Close() error { return nil }

// TestKeyLookupCanceledContextIsSilent proves the browser-hangup case: a
// canceled request context makes the key lookup fail with the cancellation,
// and ServeHTTP answers nothing - no panic, no response write.
func TestKeyLookupCanceledContextIsSilent(t *testing.T) {
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h := New(&failKeys{lookupErr: context.Canceled}, meter.NewLocalAuthority(meter.NewLedger()), []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: stub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	})

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId"}`))
	req.Header.Set("Authorization", "Bearer some-key")
	canceled, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(canceled)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("canceled request got a response of %d bytes, want none: %s", rec.Body.Len(), rec.Body.String())
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

// TestKeyLookupGenuineErrorAnswers500 proves a real store failure stays
// visible: one 500 with the JSON-RPC error shape, never a panic and never a
// 401.
func TestKeyLookupGenuineErrorAnswers500(t *testing.T) {
	stub := newStubUpstream(t, http.StatusOK, `{}`)
	h := New(&failKeys{lookupErr: errors.New("disk on fire")}, meter.NewLocalAuthority(meter.NewLedger()), []Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: stub.url(), Pricer: rpcPricer{weights: meter.DefaultWeights()}},
	})

	req := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"method":"eth_chainId"}`))
	req.Header.Set("Authorization", "Bearer some-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rec.Code, rec.Body.String())
	}
	if code := rpcErrCode(t, rec.Body.Bytes()); code != codeInvalidRequest {
		t.Errorf("error code = %d, want %d", code, codeInvalidRequest)
	}
	if n := stub.requests.Load(); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}
