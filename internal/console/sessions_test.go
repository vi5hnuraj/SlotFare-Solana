package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slotfare/internal/session"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// sessionHash is the keccak256 of a bearer token's bytes, the only form the
// store ever sees.
func sessionHash(t *testing.T, token string) [32]byte {
	t.Helper()
	var h [32]byte
	copy(h[:], keccak256([]byte(token)))
	return h
}

// secretToken signs in through /session/secret and returns the token.
func secretToken(t *testing.T, h *Handler, secret string) string {
	t.Helper()
	rec := doRequest(t, h, http.MethodPost, "/session/secret", "", []byte(`{"secret":`+quote(secret)+`}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /session/secret status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("secret body not JSON: %v", err)
	}
	return out.Token
}

// 1. A token survives a store roundtrip and the store holds its hash, not
// the plaintext.
func TestSessionRoundtripStoresHashOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(path)
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	h := New(WithSessions(store))
	token := secretToken(t, h, "roundtrip-secret")

	// The row's hash is exactly keccak256 of the token.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open db read-only: %v", err)
	}
	defer db.Close()
	var stored []byte
	if err := db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("read sessions row: %v", err)
	}
	if want := sessionHash(t, token); !bytes.Equal(stored, want[:]) {
		t.Errorf("stored hash = %x, want keccak256([]byte(token)) = %x", stored, want[:])
	}

	// Close every writer, then the plaintext must be nowhere in the file.
	store.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Error("plaintext token found in the sessions database")
	}
}

// 2. A token outlives a restart: a fresh store on the same file resolves it
// to the same account.
func TestSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	accountID := "0x" + hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
	token, _, err := newTokenStore(mustSessions(t, path)).issue(context.Background(), accountID, time.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Simulate the restart: close the first store, open a second on the
	// same file, build a fresh handler around it.
	store2, err := session.NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	h := New(WithSessions(store2))
	rec := doRequest(t, h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(AccountIDFromContext(r.Context())))
	})), http.MethodGet, "/", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("middleware status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != accountID {
		t.Errorf("restarted handler resolved account %q, want %q", got, accountID)
	}
}

// mustSessions opens a temp-file session store and registers its close.
func mustSessions(t *testing.T, path string) *session.SQLite {
	t.Helper()
	s, err := session.NewStore(path)
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// 3. Use slides the expiry forward at most once a minute.
func TestSessionSlidesOnUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := New(WithSessions(store), withNow(clock.Now))
	accountID := "0x" + hex.EncodeToString(bytes.Repeat([]byte{2}, 32))
	t0 := clock.Now()
	token, _, err := h.tokens.issue(context.Background(), accountID, t0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	hash := sessionHash(t, token)

	mw := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	do := func() int {
		rec := doRequest(t, mw, http.MethodGet, "/", token, nil)
		return rec.Code
	}

	// Five days of use slides the expiry to now + 14 days.
	clock.Advance(5 * 24 * time.Hour)
	if code := do(); code != http.StatusOK {
		t.Fatalf("request after 5 days status = %d, want 200", code)
	}
	wantExpires := t0.Add(5*24*time.Hour + session.TTL).Unix()
	got, ok, err := store.Lookup(context.Background(), hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("session row vanished after use")
	}
	if got.LastSeen != t0.Add(5*24*time.Hour).Unix() || got.ExpiresAt != wantExpires {
		t.Errorf("after slide last_seen/expires_at = (%d, %d), want (%d, %d)",
			got.LastSeen, got.ExpiresAt, t0.Add(5*24*time.Hour).Unix(), wantExpires)
	}

	// Thirty seconds later the request succeeds but writes nothing: the
	// once-a-minute throttle holds.
	clock.Advance(30 * time.Second)
	if code := do(); code != http.StatusOK {
		t.Fatalf("throttled request status = %d, want 200", code)
	}
	got, _, err = store.Lookup(context.Background(), hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.LastSeen != t0.Add(5*24*time.Hour).Unix() || got.ExpiresAt != wantExpires {
		t.Errorf("throttle window wrote anyway: last_seen/expires_at = (%d, %d)", got.LastSeen, got.ExpiresAt)
	}

	// Two minutes after the last write it slides again.
	clock.Advance(2 * time.Minute)
	if code := do(); code != http.StatusOK {
		t.Fatalf("second slide request status = %d, want 200", code)
	}
	now := clock.Now()
	got, _, err = store.Lookup(context.Background(), hash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.LastSeen != now.Unix() || got.ExpiresAt != now.Add(session.TTL).Unix() {
		t.Errorf("second slide last_seen/expires_at = (%d, %d), want (%d, %d)",
			got.LastSeen, got.ExpiresAt, now.Unix(), now.Add(session.TTL).Unix())
	}
}

// 4. An idle session past its TTL is rejected with today's 401 shape.
func TestIdleSessionExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := New(WithSessions(store), withNow(clock.Now))
	accountID := "0x" + hex.EncodeToString(bytes.Repeat([]byte{3}, 32))
	token, _, err := h.tokens.issue(context.Background(), accountID, clock.Now())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	mw := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	clock.Advance(session.TTL + time.Second)
	rec := doRequest(t, mw, http.MethodGet, "/", token, nil)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "token expired" {
		t.Errorf("idle past TTL: status=%d reason=%q, want 401/\"token expired\"", rec.Code, errorReason(t, rec))
	}
}

// 5. Sign-out revokes server-side; afterwards the token fails like any
// unknown one.
func TestSignoutRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	h := New(WithSessions(store))
	token := secretToken(t, h, "signout-secret")
	hash := sessionHash(t, token)
	if _, ok, err := store.Lookup(context.Background(), hash); err != nil || !ok {
		t.Fatalf("session row missing before sign-out: ok=%v err=%v", ok, err)
	}

	rec := doRequest(t, h, http.MethodPost, "/session/signout", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("signout status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if _, ok, err := store.Lookup(context.Background(), hash); err != nil || ok {
		t.Errorf("session row still present after sign-out: ok=%v err=%v", ok, err)
	}

	// The revoked token now fails like any unknown one.
	mw := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	rec = doRequest(t, mw, http.MethodGet, "/", token, nil)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "malformed token" {
		t.Errorf("post-signout: status=%d reason=%q, want 401/\"malformed token\"", rec.Code, errorReason(t, rec))
	}

	// Missing header keeps today's 401 shape; GET is not a method here.
	rec = doRequest(t, h, http.MethodPost, "/session/signout", "", nil)
	if rec.Code != http.StatusUnauthorized || errorReason(t, rec) != "missing token" {
		t.Errorf("signout without header: status=%d reason=%q, want 401/\"missing token\"", rec.Code, errorReason(t, rec))
	}
	rec = doRequest(t, h, http.MethodGet, "/session/signout", token, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET signout status = %d, want 405", rec.Code)
	}
}

// 6. The sweep removes expired rows and leaves live ones alone; the sign-in
// path sweeps opportunistically too.
func TestSweepRemovesExpiredOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	live := session.Session{Hash: [32]byte{0x01}, AccountID: "acct-live", IssuedAt: now - 10, ExpiresAt: now + 100, LastSeen: now - 10}
	expired := session.Session{Hash: [32]byte{0x02}, AccountID: "acct-dead", IssuedAt: now - 200, ExpiresAt: now - 10, LastSeen: now - 200}
	for _, s := range []session.Session{live, expired} {
		if err := store.Save(ctx, s); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	n, err := store.SweepExpired(ctx, now)
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("sweep removed %d rows, want 1", n)
	}
	if _, ok, err := store.Lookup(ctx, live.Hash); err != nil || !ok {
		t.Errorf("live session swept: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.Lookup(ctx, expired.Hash); err != nil || ok {
		t.Errorf("expired session survived the sweep: ok=%v err=%v", ok, err)
	}

	// The sign-in path sweeps before issuing: a stale row is gone after a
	// fresh /session/secret.
	h := New(WithSessions(store))
	secretToken(t, h, "sweep-on-signin-secret")
	if _, ok, err := store.Lookup(ctx, expired.Hash); err != nil || ok {
		t.Errorf("stale row still present after a sign-in sweep: ok=%v err=%v", ok, err)
	}
}

// 7. A canceled request context - a client hangup mid-flight - comes back
// from Lookup as a wrapped error, never a panic.
func TestSessionLookupCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	rec := session.Session{Hash: [32]byte{0x77}, AccountID: "acct-live", IssuedAt: now, ExpiresAt: now + 100, LastSeen: now}
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, ok, err := store.Lookup(canceled, rec.Hash)
	if ok {
		t.Error("Lookup on a canceled context returned true")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want context.Canceled", err)
	}
}

// 8. A genuine database failure is returned as an error the caller can
// distinguish from a canceled context.
func TestSessionLookupGenuineErrorSurfaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store := mustSessions(t, path)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, ok, err := store.Lookup(context.Background(), [32]byte{0x78})
	if ok {
		t.Error("Lookup after Close returned true")
	}
	if err == nil || errors.Is(err, context.Canceled) {
		t.Errorf("Lookup error = %v, want a genuine (non-canceled) error", err)
	}
}

// failSessions is a session.Store whose Lookup returns a configurable error;
// every other method is a no-op. It pins the middleware's failure policy for
// the session lookup without touching a database.
type failSessions struct {
	lookupErr error
}

func (s *failSessions) Save(ctx context.Context, rec session.Session) error { return nil }

func (s *failSessions) Lookup(ctx context.Context, h [32]byte) (session.Session, bool, error) {
	return session.Session{}, false, s.lookupErr
}

func (s *failSessions) Touch(ctx context.Context, h [32]byte, now int64) error { return nil }

func (s *failSessions) Revoke(ctx context.Context, h [32]byte) (bool, error) {
	return false, nil
}

func (s *failSessions) SweepExpired(ctx context.Context, now int64) (int64, error) {
	return 0, nil
}

func (s *failSessions) SaveNonce(ctx context.Context, n session.Nonce) error { return nil }

func (s *failSessions) TakeNonce(ctx context.Context, address string) (session.Nonce, bool, error) {
	return session.Nonce{}, false, nil
}

func (s *failSessions) Close() error { return nil }

// 9. A canceled request context - the browser-hangup case from the live log
// - must not panic and not answer: the connection is already gone.
func TestMiddlewareCanceledContextIsSilent(t *testing.T) {
	h := New(WithSessions(&failSessions{lookupErr: context.Canceled}))
	mw := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	canceled, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(canceled)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("canceled request got a response of %d bytes, want none: %s", rec.Body.Len(), rec.Body.String())
	}
}

// 10. A genuine store failure stays visible: one 500 in today's shape, never
// a panic and never a 401.
func TestMiddlewareGenuineStoreErrorAnswers500(t *testing.T) {
	h := New(WithSessions(&failSessions{lookupErr: errors.New("disk on fire")}))
	mw := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError || errorReason(t, rec) != "internal error" {
		t.Errorf("store failure: status=%d reason=%q, want 500/\"internal error\"", rec.Code, errorReason(t, rec))
	}
}
