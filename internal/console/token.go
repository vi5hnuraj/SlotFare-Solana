package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"

	"slotfare/internal/session"
)

// tokenStore issues and resolves persisted bearer tokens through the
// handler's session.Store. Only the token's keccak256 hash ever reaches the
// store, exactly like API keys.
type tokenStore struct {
	store session.Store
}

func newTokenStore(s session.Store) *tokenStore {
	return &tokenStore{store: s}
}

// issue stores and returns a fresh bearer token for accountID, valid for
// session.TTL from now. The store holds only its hash; the plaintext token
// exists only in the response body.
func (s *tokenStore) issue(ctx context.Context, accountID string, now time.Time) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("console: generate token: %v", err))
	}
	token := hex.EncodeToString(raw[:])
	var hash [32]byte
	copy(hash[:], keccak256([]byte(token)))
	expiresAt := now.Add(session.TTL).UTC()
	rec := session.Session{
		Hash:      hash,
		AccountID: accountID,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
		LastSeen:  now.Unix(),
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return "", time.Time{}, fmt.Errorf("console: issue session for account %s: %w", accountID, err)
	}
	return token, expiresAt, nil
}

// resolve returns the account id for a token and whether it is currently
// valid, sliding its expiry on the way. reason distinguishes the failure
// modes when ok is false; err is non-nil only when the store itself failed
// (a 500, never a 401).
func (s *tokenStore) resolve(ctx context.Context, token string, now time.Time) (string, bool, string, error) {
	var hash [32]byte
	copy(hash[:], keccak256([]byte(token)))
	rec, found, err := s.store.Lookup(ctx, hash)
	if err != nil {
		return "", false, "", err
	}
	if !found {
		return "", false, "unknown", nil
	}
	if !now.Before(time.Unix(rec.ExpiresAt, 0)) {
		return "", false, "expired", nil
	}
	if err := s.store.Touch(ctx, hash, now.Unix()); err != nil {
		return "", false, "", err
	}
	return rec.AccountID, true, "", nil
}

// AccountIDFromSecret derives an account id from a raw secret string using
// keys.Issue semantics: keccak256 of the secret's bytes. The keccak framing
// is inherited from the project's EVM origins; every live identity path
// (SIWS sign-in and the deposit watcher) uses AccountIDFromPublicKey
// instead - 0x-hex of the raw 32-byte Solana public key. This derives ids
// only for script keys with no wallet.
func AccountIDFromSecret(secret string) string {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(secret))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return "0x" + hex.EncodeToString(out[:])
}

// keccak256 hashes b with the legacy Keccak-256 used throughout the repo.
func keccak256(b []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(b)
	return h.Sum(nil)
}

// Middleware wraps next so it can resolve a bearer token to an account id. A
// valid token puts the account id in the request context (read it back with
// AccountIDFromContext); anything else is answered by failBearer.
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID, ok, reason, err := h.resolveBearer(r)
		if err != nil || !ok {
			failBearer(w, reason, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountIDKey{}, accountID)))
	})
}

// failBearer owns the failure policy for every resolveBearer caller. A
// canceled request context is a client hangup: at most one debug-level line
// and no response, since the connection is already gone. Any other store
// failure is one clear log line plus a 500 - never a panic and never a 401.
// A clean miss or expiry is a 401 naming the reason.
func failBearer(w http.ResponseWriter, reason string, err error) {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			log.Printf("slotfared: debug: session lookup canceled: client disconnected")
			return
		}
		log.Printf("slotfared: session store lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeError(w, http.StatusUnauthorized, reason)
}

// resolveBearer resolves the request's bearer token to an account id. reason
// names the failure mode when ok is false, in the same language as
// Middleware: "missing token", "malformed token" or "token expired". err is
// non-nil only when the session store failed; callers write a 500 for it.
func (h *Handler) resolveBearer(r *http.Request) (string, bool, string, error) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return "", false, "missing token", nil
	}
	if !isHexToken(token) {
		return "", false, "malformed token", nil
	}
	accountID, valid, reason, err := h.tokens.resolve(r.Context(), token, h.now())
	if err != nil {
		return "", false, "", err
	}
	switch {
	case valid:
		return accountID, true, "", nil
	case reason == "expired":
		return "", false, "token expired", nil
	default:
		return "", false, "malformed token", nil
	}
}

// AccountIDFromContext returns the account id set by Middleware, or "" when
// absent.
func AccountIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(accountIDKey{}).(string)
	return v
}

// bearerToken extracts the non-empty token from a "Bearer <token>" header.
func bearerToken(auth string) (string, bool) {
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(auth[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// isHexToken reports whether t is a well-formed 64-hex-char token.
func isHexToken(t string) bool {
	if len(t) != 64 {
		return false
	}
	for _, c := range t {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// accountIDKey is the context key under which Middleware stores the account
// id. An unexported type keeps it distinct from any other key.
type accountIDKey struct{}
