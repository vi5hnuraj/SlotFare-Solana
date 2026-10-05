package console

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	solana "github.com/gagliardetto/solana-go"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
	"slotfare/internal/proxy"
	"slotfare/internal/session"
	fbsolana "slotfare/internal/solana"
)

// testClock is a mutable clock injected into the handler for expiry control.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestHandler builds a Handler with an injected clock and a temp-file
// session store.
func newTestHandler(t *testing.T) (*Handler, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	t.Cleanup(func() { sessions.Close() })
	h := New(WithSessions(sessions), withNow(clock.Now))
	return h, clock
}

// newWallet generates an ed25519 keypair and returns the public key, the
// private key and the base58 address a wallet request carries.
func newWallet(t *testing.T) (solana.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key := solana.PublicKeyFromBytes(pub)
	return key, priv, key.String()
}

// signMessage signs the exact server-built message bytes with priv and
// returns the hex form the browser client posts (no 0x prefix).
func signMessage(t *testing.T, msg string, priv ed25519.PrivateKey) string {
	t.Helper()
	return hex.EncodeToString(ed25519.Sign(priv, []byte(msg)))
}

// fetchNonce issues a nonce for addr and returns the record fields.
func fetchNonce(t *testing.T, h *Handler, addr string) map[string]string {
	t.Helper()
	body := `{"address":` + quote(addr) + `}`
	req := httptest.NewRequest("POST", "/session/nonce", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nonce status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("nonce body not JSON: %v", err)
	}
	return out
}

// verify posts a verify request and returns the recorder.
func verify(h *Handler, addr, nonce, sig string) *httptest.ResponseRecorder {
	body := `{"address":` + quote(addr) + `,"nonce":` + quote(nonce) + `,"signature":` + quote(sig) + `}`
	req := httptest.NewRequest("POST", "/session/verify", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// quote wraps s in JSON double quotes (inputs are all simple ASCII).
func quote(s string) string { return `"` + s + `"` }

// errorReason extracts the "error" field from a JSON response body.
func errorReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return out.Error
}

// recFrom builds the nonce record fields the client echoes into the message
// it signs.
func recFrom(t *testing.T, n map[string]string) nonceRecord {
	t.Helper()
	return nonceRecord{
		nonce:     n["nonce"],
		statement: n["statement"],
		issuedAt:  mustParse(t, n["issuedAt"]),
		expiresAt: mustParse(t, n["expiresAt"]),
		domain:    n["domain"],
		uri:       n["uri"],
	}
}

// 1. A nonce verifies once; replaying it fails.
func TestNonceVerifiesOnceReplayFails(t *testing.T) {
	_, priv, addr := newWallet(t)
	h, _ := newTestHandler(t)

	n := fetchNonce(t, h, addr)
	sig := signMessage(t, h.buildMessage(mustPub(t, addr), recFrom(t, n)), priv)

	rec := verify(h, addr, n["nonce"], sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("first verify status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var first struct {
		AccountID string `json:"accountId"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("verify body not JSON: %v", err)
	}
	if first.AccountID == "" || first.Token == "" {
		t.Fatalf("verify returned empty accountId/token: %+v", first)
	}

	// Replay the same nonce: it was consumed, so it is now unknown.
	rec = verify(h, addr, n["nonce"], sig)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", rec.Code)
	}
	if got := errorReason(t, rec); got != "unknown nonce" {
		t.Errorf("replay reason = %q, want \"unknown nonce\"", got)
	}
}

// 2. An expired nonce fails.
func TestExpiredNonceFails(t *testing.T) {
	_, priv, addr := newWallet(t)
	h, clock := newTestHandler(t)

	n := fetchNonce(t, h, addr)
	clock.Advance(5*time.Minute + time.Second)

	sig := signMessage(t, h.buildMessage(mustPub(t, addr), recFrom(t, n)), priv)
	rec := verify(h, addr, n["nonce"], sig)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expired status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if got := errorReason(t, rec); got != "nonce expired" {
		t.Errorf("reason = %q, want \"nonce expired\"", got)
	}
}

// 3. A signature from a different key than the claimed address fails.
func TestDifferentSignerFails(t *testing.T) {
	_, _, claimed := newWallet(t)
	_, otherPriv, _ := newWallet(t)
	h, _ := newTestHandler(t)

	n := fetchNonce(t, h, claimed)
	sig := signMessage(t, h.buildMessage(mustPub(t, claimed), recFrom(t, n)), otherPriv)

	rec := verify(h, claimed, n["nonce"], sig)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	if got := errorReason(t, rec); got != "signature verification failed" {
		t.Errorf("reason = %q, want \"signature verification failed\"", got)
	}
}

// 4. accountId is 0x-hex of the raw 32-byte public key and equals the
// deposit watcher's account id, so signing in and depositing meet in the
// same ledger row.
func TestAccountIDMatchesDepositWatcher(t *testing.T) {
	pub, priv, addr := newWallet(t)
	h, _ := newTestHandler(t)

	n := fetchNonce(t, h, addr)
	rec := verify(h, addr, n["nonce"], signMessage(t, h.buildMessage(mustPub(t, addr), recFrom(t, n)), priv))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if want := fbsolana.AccountIDFor(pub); out.AccountID != want {
		t.Errorf("accountId = %q, want deposit watcher id %q", out.AccountID, want)
	}
	if want := AccountIDFromPublicKey(pub); out.AccountID != want {
		t.Errorf("accountId = %q, want %q", out.AccountID, want)
	}
	if len(out.AccountID) != 66 {
		t.Errorf("accountId length = %d, want 66", len(out.AccountID))
	}
}

// 5. A fixed keypair maps to the fixture 0x-hex of its bytes.
func TestAccountIDFixture(t *testing.T) {
	var b [32]byte
	for i := range b {
		b[i] = byte(0xa0 - i)
	}
	pub := solana.PublicKeyFromBytes(b[:])
	want := "0x" + hex.EncodeToString(b[:])
	if got := AccountIDFromPublicKey(pub); got != want {
		t.Errorf("AccountIDFromPublicKey = %q, want fixture %q", got, want)
	}
}

// 6. The middleware rejects a missing, malformed and expired token distinctly.
func TestMiddlewareDistinctRejections(t *testing.T) {
	h, clock := newTestHandler(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok:" + AccountIDFromContext(r.Context())))
	})
	mw := h.Middleware(next)

	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		return rec
	}

	// Missing token.
	rec := do("")
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "missing token" {
		t.Errorf("missing: status=%d reason=%q, want 401/\"missing token\"", rec.Code, errorReason(t, rec))
	}

	// Malformed: wrong length.
	rec = do("Bearer " + strings.Repeat("a", 10))
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "malformed token" {
		t.Errorf("short: status=%d reason=%q, want 401/\"malformed token\"", rec.Code, errorReason(t, rec))
	}

	// Malformed: well-formed 64-hex but unknown.
	rec = do("Bearer " + strings.Repeat("b", 64))
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "malformed token" {
		t.Errorf("unknown: status=%d reason=%q, want 401/\"malformed token\"", rec.Code, errorReason(t, rec))
	}

	// Valid token reaches next with the account id set.
	accountID := "0x" + strings.Repeat("c", 64)
	token, _, err := h.tokens.issue(context.Background(), accountID, clock.Now())
	if err != nil {
		t.Fatalf("tokens.issue: %v", err)
	}
	rec = do("Bearer " + token)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid: status=%d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "ok:"+accountID {
		t.Errorf("next saw %q, want account id in context", got)
	}

	// Expired token: issued far enough back that even the sliding 14-day
	// TTL has passed.
	expiredToken, _, err := h.tokens.issue(context.Background(), accountID, clock.Now().Add(-14*24*time.Hour-time.Second))
	if err != nil {
		t.Fatalf("tokens.issue (expired): %v", err)
	}
	clock.Advance(time.Second)
	rec = do("Bearer " + expiredToken)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "token expired" {
		t.Errorf("expired: status=%d reason=%q, want 401/\"token expired\"", rec.Code, errorReason(t, rec))
	}
}

// 7. A console token is rejected by the proxy's inference path: the proxy only
// knows keys.Store hashes, never console bearer tokens.
func TestConsoleTokenRejectedByProxyInference(t *testing.T) {
	ch, _ := newTestHandler(t)
	accountID := "0x" + strings.Repeat("d", 64)
	consoleToken, _, err := ch.tokens.issue(context.Background(), accountID, time.Now())
	if err != nil {
		t.Fatalf("tokens.issue: %v", err)
	}

	// A proxy with one issued API key and one inference service.
	store, err := keys.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("keys.Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	k := keys.Issue("spending-secret")
	if err := store.Add(ctx, k); err != nil {
		t.Fatalf("keys.Add: %v", err)
	}
	ledger := meter.NewLedger()
	ledger.SetMinted(k.AccountID, 1000)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	h := proxy.New(store, meter.NewLocalAuthority(ledger), []proxy.Service{
		{Name: "inference", Prefix: "/infer", Upstream: upURL, Pricer: proxy.NewInferencePricer(meter.DefaultTokenPrices()), Allow: []string{"/v1/models"}},
	})

	req := httptest.NewRequest("POST", "/infer/v1/models", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+consoleToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

// A happy-path check for /session/secret: it returns accountId equal to
// keys.Issue(secret).AccountID.
func TestSecretEndpointDerivesAccount(t *testing.T) {
	const secret = "script-spending-secret"
	h, _ := newTestHandler(t)

	req := httptest.NewRequest("POST", "/session/secret", strings.NewReader(`{"secret":`+quote(secret)+`}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AccountID string `json:"accountId"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if want := keys.Issue(secret).AccountID; out.AccountID != want {
		t.Errorf("accountId = %q, want %q", out.AccountID, want)
	}
	if out.Token == "" {
		t.Error("token empty")
	}
}

// mustParse parses an RFC3339 timestamp.
func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// mustPub decodes a base58 address.
func mustPub(t *testing.T, s string) solana.PublicKey {
	t.Helper()
	pub, err := solana.PublicKeyFromBase58(s)
	if err != nil {
		t.Fatalf("pubkey %q: %v", s, err)
	}
	return pub
}

// The wallet posts the signature as hex (with or without 0x) or base58;
// all three encodings of the same 64 bytes verify. Each encoding gets a
// fresh nonce: verification consumes it.
func TestVerifyAcceptsSignatureEncodings(t *testing.T) {
	pub, priv, addr := newWallet(t)
	h, _ := newTestHandler(t)

	for _, enc := range func() []string {
		sig := ed25519.Sign(priv, []byte("placeholder"))
		return []string{hex.EncodeToString(sig), "0x" + hex.EncodeToString(sig), solana.SignatureFromBytes(sig).String()}
	}() {
		n := fetchNonce(t, h, addr)
		msg := h.buildMessage(pub, recFrom(t, n))
		sig := ed25519.Sign(priv, []byte(msg))
		// Re-encode the real signature the same way this case encodes.
		var presented string
		switch {
		case strings.HasPrefix(enc, "0x"):
			presented = "0x" + hex.EncodeToString(sig)
		case len(enc) == hex.EncodedLen(ed25519.SignatureSize):
			presented = hex.EncodeToString(sig)
		default:
			presented = solana.SignatureFromBytes(sig).String()
		}
		if got := verify(h, addr, n["nonce"], presented); got.Code != http.StatusOK {
			t.Errorf("encoding %q: status = %d, body = %s", presented, got.Code, got.Body.String())
		}
	}
}

// A signature over anything but the rebuilt message fails: framing is
// pinned by the server, not by the client.
func TestVerifyRejectsTamperedMessage(t *testing.T) {
	_, priv, addr := newWallet(t)
	h, _ := newTestHandler(t)

	n := fetchNonce(t, h, addr)
	// Sign a message with a different statement than the stored one.
	rec := recFrom(t, n)
	rec.statement = "something else"
	sig := signMessage(t, h.buildMessage(mustPub(t, addr), rec), priv)
	if got := verify(h, addr, n["nonce"], sig); got.Code != http.StatusUnauthorized {
		t.Errorf("tampered statement: status = %d, want 401", got.Code)
	}
}

// TestNonceIssuedOnOneReplicaVerifiesOnAnother pins the multi-replica
// contract: two handlers sharing one session store stand in for two
// slotfared pods behind a gateway. A nonce issued by one must verify on the
// other, and the store's single use must still hold across both.
func TestNonceIssuedOnOneReplicaVerifiesOnAnother(t *testing.T) {
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	t.Cleanup(func() { sessions.Close() })
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	issuer := New(WithSessions(sessions), withNow(clock.Now))
	verifier := New(WithSessions(sessions), withNow(clock.Now))

	_, priv, addr := newWallet(t)
	pub := mustPub(t, addr)
	n := fetchNonce(t, issuer, addr)
	sig := signMessage(t, verifier.buildMessage(pub, recFrom(t, n)), priv)

	if got := verify(verifier, addr, n["nonce"], sig); got.Code != http.StatusOK {
		t.Fatalf("verify on the other replica: status = %d, body = %s", got.Code, got.Body.String())
	}
	if got := verify(issuer, addr, n["nonce"], sig); got.Code != http.StatusBadRequest {
		t.Fatalf("replay on the issuing replica: status = %d, want 400; body = %s", got.Code, got.Body.String())
	}
}

// TestSignInDomainComesFromTheRequestingPage pins the reason sign-in works at
// all with a real wallet. Phantom validates a message it recognises as
// Sign-In with Solana against the page that asked for it: the domain must be
// that page's exact host, port included, and the URI's origin must match. A
// placeholder domain is refused before the user is ever prompted (observed:
// "The app's signature request cannot be shown due to invalid formatting"),
// and no server-side test can catch that on its own, because the wallet is
// what rejects it.
func TestSignInDomainComesFromTheRequestingPage(t *testing.T) {
	_, priv, addr := newWallet(t)
	h, _ := newTestHandler(t)
	const page = "http://127.0.0.1:4200"

	// The nonce request carries the page's Origin, as a browser sends it.
	req := httptest.NewRequest("POST", "/session/nonce", strings.NewReader(`{"address":`+quote(addr)+`}`))
	req.Header.Set("Origin", page)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nonce status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var n map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatalf("nonce body not JSON: %v", err)
	}
	if n["domain"] != "127.0.0.1:4200" {
		t.Errorf("domain = %q, want the page host %q", n["domain"], "127.0.0.1:4200")
	}
	if n["uri"] != page {
		t.Errorf("uri = %q, want the page origin %q", n["uri"], page)
	}

	// The message the wallet is asked to sign names that host and origin.
	msg := h.buildMessage(mustPub(t, addr), recFrom(t, n))
	if !strings.HasPrefix(msg, "127.0.0.1:4200 wants you to sign in with your Solana account:\n") {
		t.Errorf("message does not name the page host:\n%s", msg)
	}
	if !strings.Contains(msg, "URI: "+page+"\n") {
		t.Errorf("message does not name the page origin:\n%s", msg)
	}

	// And a verify from the same page, over that message, is accepted.
	vreq := httptest.NewRequest("POST", "/session/verify", strings.NewReader(
		`{"address":`+quote(addr)+`,"nonce":`+quote(n["nonce"])+`,"signature":`+quote(signMessage(t, msg, priv))+`}`))
	vreq.Header.Set("Origin", page)
	vrec := httptest.NewRecorder()
	h.ServeHTTP(vrec, vreq)
	if vrec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200; body: %s", vrec.Code, vrec.Body.String())
	}
}
