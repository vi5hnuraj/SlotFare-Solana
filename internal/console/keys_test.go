package console

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/keys"
	"slotfare/internal/meter"
	"slotfare/internal/proxy"
	"slotfare/internal/session"
)

// testWalletID mints a fresh Solana-native account id: 0x-hex of a random
// 32-byte public key, the shape AccountIDFromPublicKey produces.
func testWalletID(t *testing.T) string {
	t.Helper()
	pub := gsolana.NewWallet().PublicKey()
	return "0x" + hex.EncodeToString(pub[:])
}

// newKeysTestHandler builds a Handler wired to a temp-file key store and a
// fresh ledger, on an injected clock.
func newKeysTestHandler(t *testing.T) (*Handler, *testClock, *keys.SQLite, *meter.Ledger) {
	t.Helper()
	store, err := keys.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("keys.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ledger := meter.NewLedger()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	t.Cleanup(func() { sessions.Close() })
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := New(WithKeys(store), WithSessions(sessions), WithLedger(ledger), withNow(clock.Now))
	return h, clock, store, ledger
}

// consoleTokenFor issues a console bearer token for accountID on the
// handler's own clock. The handler must carry a session store (attach
// testSessionStore when building one bare).
func consoleTokenFor(t *testing.T, h *Handler, accountID string) string {
	t.Helper()
	token, _, err := h.tokens.issue(context.Background(), accountID, h.now())
	if err != nil {
		t.Fatalf("tokens.issue: %v", err)
	}
	return token
}

// testSessionStore opens a temp-file session store for handlers the tests
// build without WithSessions.
func testSessionStore(t *testing.T) *session.SQLite {
	t.Helper()
	ss, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}

// newTestProxy stands up the RPC service under /rpc against an httptest
// upstream, sharing the handler's key store and ledger.
func newTestProxy(t *testing.T, store keys.Store, ledger *meter.Ledger) *proxy.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x102031"}`))
	}))
	t.Cleanup(upstream.Close)
	upURL, _ := url.Parse(upstream.URL)
	return proxy.New(store, meter.NewLocalAuthority(ledger), []proxy.Service{
		{Name: "rpc", Prefix: "/rpc", Upstream: upURL, Pricer: proxy.NewRPCPricer(meter.DefaultWeights())},
	})
}

// doRequest sends one request to h with an optional Bearer auth value.
func doRequest(t *testing.T, h http.Handler, method, path string, auth string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// issueKey mints a key for the token's account through POST /keys and
// returns the response fields.
func issueKey(t *testing.T, h *Handler, token, label string) (plaintext, prefix string) {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/keys", token, []byte(`{"label":`+quote(label)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /keys status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Key       string `json:"key"`
		Prefix    string `json:"prefix"`
		Label     string `json:"label"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST /keys body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if out.CreatedAt == "" {
		t.Error("POST /keys returned no createdAt")
	}
	return out.Key, out.Prefix
}

// listKeys GETs /keys for the token's account and returns the parsed entries.
func listKeys(t *testing.T, h *Handler, token string) []keyView {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, "/keys", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /keys status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /keys body not JSON: %v; body: %s", err, rec.Body.String())
	}
	return out.Keys
}

// findKeyView returns the entry with the given prefix, or nil.
func findKeyView(list []keyView, prefix string) *keyView {
	for i := range list {
		if list[i].Prefix == prefix {
			return &list[i]
		}
	}
	return nil
}

// rpcCall posts eth_chainId to the proxy with the given Bearer key.
func rpcCall(t *testing.T, p *proxy.Handler, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, p, http.MethodPost, "/rpc", apiKey, []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
}

// 1. Issuing returns the key once, it authenticates against the proxy, and
// no later call returns it.
func TestIssueKeyReturnedOnceAndAuthenticates(t *testing.T) {
	accountID := testWalletID(t)
	h, _, store, ledger := newKeysTestHandler(t)
	token := consoleTokenFor(t, h, accountID)
	ledger.SetMinted(accountID, 100*meter.MicroCreditsPerCredit)

	plaintext, prefix := issueKey(t, h, token, "test key")
	if !strings.HasPrefix(plaintext, "attk_") || len(plaintext) != 37 {
		t.Fatalf("key = %q, want attk_ plus 32 hex chars (37 total)", plaintext)
	}
	if len(prefix) != 13 || prefix != plaintext[:13] {
		t.Errorf("prefix = %q, want the first 13 chars of the key", prefix)
	}

	p := newTestProxy(t, store, ledger)
	if rec := rpcCall(t, p, plaintext); rec.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Follow-up calls must never return the plaintext again.
	followUps := map[string]*httptest.ResponseRecorder{
		"GET /keys":    doRequest(t, h, http.MethodGet, "/keys", token, nil),
		"GET /account": doRequest(t, h, http.MethodGet, "/account", token, nil),
		"POST /keys":   doRequest(t, h, http.MethodPost, "/keys", token, []byte(`{}`)),
	}
	for name, rec := range followUps {
		if strings.Contains(rec.Body.String(), plaintext) {
			t.Errorf("%s leaked the key: %s", name, rec.Body.String())
		}
	}

	list := listKeys(t, h, token)
	v := findKeyView(list, prefix)
	if v == nil {
		t.Fatalf("GET /keys has no entry for prefix %q; entries: %+v", prefix, list)
	}
	if v.Label != "test key" {
		t.Errorf("label = %q, want \"test key\"", v.Label)
	}
	if v.RequestCount < 1 {
		t.Errorf("requestCount = %d, want >= 1 after one authenticated proxy call", v.RequestCount)
	}
	if v.Revoked {
		t.Error("revoked = true, want false")
	}
}

// 2. Revoking with the owner's token works and the key stops authenticating.
func TestRevokeOwnKeyStopsAuthenticating(t *testing.T) {
	accountID := testWalletID(t)
	h, _, store, ledger := newKeysTestHandler(t)
	token := consoleTokenFor(t, h, accountID)
	ledger.SetMinted(accountID, 100*meter.MicroCreditsPerCredit)

	plaintext, prefix := issueKey(t, h, token, "")
	p := newTestProxy(t, store, ledger)
	if rec := rpcCall(t, p, plaintext); rec.Code != http.StatusOK {
		t.Fatalf("before revoke: proxy status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	rec := doRequest(t, h, http.MethodDelete, "/keys/"+prefix, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /keys/%s status = %d, want 200; body: %s", prefix, rec.Code, rec.Body.String())
	}
	var out struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.Revoked {
		t.Fatalf("DELETE body = %s, want {\"revoked\":true}", rec.Body.String())
	}

	if rec := rpcCall(t, p, plaintext); rec.Code != http.StatusUnauthorized {
		t.Errorf("after revoke: proxy status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
	v := findKeyView(listKeys(t, h, token), prefix)
	if v == nil || !v.Revoked {
		t.Errorf("GET /keys after revoke: entry = %+v, want revoked true", v)
	}
}

// 3. Revoking a prefix owned by another account returns 404 and leaves it
// working.
func TestRevokeForeignPrefix404(t *testing.T) {
	idA := testWalletID(t)
	idB := testWalletID(t)
	h, _, store, ledger := newKeysTestHandler(t)
	tokenA := consoleTokenFor(t, h, idA)
	tokenB := consoleTokenFor(t, h, idB)
	ledger.SetMinted(idB, 100*meter.MicroCreditsPerCredit)

	keyB, prefixB := issueKey(t, h, tokenB, "")

	rec := doRequest(t, h, http.MethodDelete, "/keys/"+prefixB, tokenA, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign DELETE status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}

	p := newTestProxy(t, store, ledger)
	if rec := rpcCall(t, p, keyB); rec.Code != http.StatusOK {
		t.Errorf("B's key after A's failed revoke: proxy status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	v := findKeyView(listKeys(t, h, tokenB), prefixB)
	if v == nil || v.Revoked {
		t.Errorf("B's key entry = %+v, want revoked false", v)
	}
}

// 4. A token for account A cannot read or mutate account B's keys.
func TestTokensCannotCrossAccounts(t *testing.T) {
	idA := testWalletID(t)
	idB := testWalletID(t)
	h, _, store, ledger := newKeysTestHandler(t)
	tokenA := consoleTokenFor(t, h, idA)
	tokenB := consoleTokenFor(t, h, idB)
	ledger.SetMinted(idA, 100*meter.MicroCreditsPerCredit)
	ledger.SetMinted(idB, 100*meter.MicroCreditsPerCredit)

	keyA, prefixA := issueKey(t, h, tokenA, "a")
	keyB, _ := issueKey(t, h, tokenB, "b")

	// B cannot read A's keys.
	for _, v := range listKeys(t, h, tokenB) {
		if v.Prefix == prefixA {
			t.Errorf("B's GET /keys lists A's prefix %q", prefixA)
		}
	}

	// B cannot revoke A's key; it keeps working.
	rec := doRequest(t, h, http.MethodDelete, "/keys/"+prefixA, tokenB, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("B revoking A's key: status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	p := newTestProxy(t, store, ledger)
	if rec := rpcCall(t, p, keyA); rec.Code != http.StatusOK {
		t.Errorf("A's key after B's failed revoke: proxy status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// B's new key appears in B's list and not in A's.
	_, prefixB := issueKey(t, h, tokenB, "b2")
	if findKeyView(listKeys(t, h, tokenB), prefixB) == nil {
		t.Error("B's new key missing from B's list")
	}
	if findKeyView(listKeys(t, h, tokenA), prefixB) != nil {
		t.Error("B's new key appears in A's list")
	}
	_ = keyB
}

// 5. Account arithmetic: minted minus settled minus pending minus locked
// equals spendable.
func TestAccountArithmetic(t *testing.T) {
	idA := testWalletID(t)
	h, _, _, ledger := newKeysTestHandler(t)
	token := consoleTokenFor(t, h, idA)

	ledger.SetMinted(idA, 100)
	if err := ledger.Reserve(idA, 30); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	ledger.MarkSettled(idA, 10)
	ledger.SetLocked(idA, 20)

	rec := doRequest(t, h, http.MethodGet, "/account", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		AccountID string `json:"accountId"`
		Minted    uint64 `json:"mintedMicro"`
		Settled   uint64 `json:"settledMicro"`
		Pending   uint64 `json:"pendingMicro"`
		Locked    uint64 `json:"lockedMicro"`
		Spendable uint64 `json:"spendableMicro"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /account body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if out.AccountID != idA {
		t.Errorf("accountId = %q, want %q", out.AccountID, idA)
	}
	if out.Minted != 100 || out.Settled != 10 || out.Pending != 20 || out.Locked != 20 {
		t.Errorf("minted/settled/pending/locked = %d/%d/%d/%d, want 100/10/20/20",
			out.Minted, out.Settled, out.Pending, out.Locked)
	}
	if out.Spendable != out.Minted-out.Settled-out.Pending-out.Locked {
		t.Errorf("spendable = %d, want minted-settled-pending-locked = %d",
			out.Spendable, out.Minted-out.Settled-out.Pending-out.Locked)
	}
	if out.Spendable != 50 {
		t.Errorf("spendable = %d, want 50", out.Spendable)
	}

	// An unknown-but-authorized account reports zeros.
	idZ := testWalletID(t)
	rec = doRequest(t, h, http.MethodGet, "/account", consoleTokenFor(t, h, idZ), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account for unknown account status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Minted != 0 || out.Spendable != 0 {
		t.Errorf("unknown account minted/spendable = %d/%d, want 0/0", out.Minted, out.Spendable)
	}
}

// 6. Auth and path edge cases on the key routes.
func TestKeyRoutesAuthAndBadPrefixes(t *testing.T) {
	idA := testWalletID(t)
	h, clock, _, _ := newKeysTestHandler(t)

	// No Authorization header at all.
	rec := doRequest(t, h, http.MethodGet, "/keys", "", nil)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "missing token" {
		t.Errorf("no header: status=%d reason=%q, want 401/\"missing token\"", rec.Code, errorReason(t, rec))
	}

	// Expired token: past the sliding 14-day TTL.
	token := consoleTokenFor(t, h, idA)
	clock.Advance(14*24*time.Hour + time.Second)
	rec = doRequest(t, h, http.MethodGet, "/keys", token, nil)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "token expired" {
		t.Errorf("expired: status=%d reason=%q, want 401/\"token expired\"", rec.Code, errorReason(t, rec))
	}

	// Bad prefixes are 404, not 405 or 500.
	live := consoleTokenFor(t, h, idA) // re-issued after the clock advanced
	for _, path := range []string{"/keys/", "/keys/a/b"} {
		rec = doRequest(t, h, http.MethodDelete, path, live, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("DELETE %s: status = %d, want 404; body: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// sharedAccounts is a stand-in authority: what the Postgres accounts table
// says, independent of this handler's in-memory ledger.
type sharedAccounts map[string]meter.Account

func (s sharedAccounts) Account(_ context.Context, id string) (meter.Account, error) {
	return s[id], nil
}

// 6. With an AccountReader attached the account route reports the shared
// row, not the local ledger: a lock pushed by another replica is visible
// here even though this ledger never saw it.
func TestAccountReadsFromSharedAuthority(t *testing.T) {
	idA := testWalletID(t)
	h, _, _, ledger := newKeysTestHandler(t)
	token := consoleTokenFor(t, h, idA)

	ledger.SetMinted(idA, 100) // this replica's stale view: nothing locked
	h.Accounts = sharedAccounts{idA: {ID: idA, Minted: 100, Settled: 10, Pending: 5, Locked: 20}}

	rec := doRequest(t, h, http.MethodGet, "/account", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /account status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Settled   uint64 `json:"settledMicro"`
		Pending   uint64 `json:"pendingMicro"`
		Locked    uint64 `json:"lockedMicro"`
		Spendable uint64 `json:"spendableMicro"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Locked != 20 || out.Settled != 10 || out.Pending != 5 || out.Spendable != 65 {
		t.Errorf("shared position not reported: %+v (want locked 20, settled 10, pending 5, spendable 65)", out)
	}
}
