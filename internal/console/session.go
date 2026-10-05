package console

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/base58"

	"slotfare/internal/session"
)

// nonceTTL is how long a signed-in nonce stays valid.
const nonceTTL = 5 * time.Minute

// statement is the constant statement every sign-in message carries.
const statement = "Sign this nonce to prove you control this account to the Slotfare console."

// nonceRecord is one outstanding nonce for an address. A new nonce request
// for an address replaces its outstanding record.
type nonceRecord struct {
	nonce     string
	statement string
	issuedAt  time.Time
	expiresAt time.Time
	// domain and uri are the origin the message names, decided when the
	// nonce was issued and re-derived when it is verified. They are part
	// of the signed bytes, so both sides must agree on them.
	domain string
	uri    string
}

// nonceKey is the store key for a Solana public key: base58 as-is, which is
// already unique and case-sensitive.
func nonceKey(pub solana.PublicKey) string {
	return pub.String()
}

// newNonce builds a fresh record for now. Timing is truncated to whole
// seconds so the record the store hands back rebuilds the exact message the
// client signed.
func newNonce(now time.Time) nonceRecord {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("console: generate nonce: %v", err))
	}
	issued := now.UTC().Truncate(time.Second)
	return nonceRecord{
		nonce:     hex.EncodeToString(raw[:]),
		statement: statement,
		issuedAt:  issued,
		expiresAt: issued.Add(nonceTTL),
	}
}

// parseAddress validates a base58 Solana public key.
func parseAddress(s string) (solana.PublicKey, bool) {
	pub, err := solana.PublicKeyFromBase58(s)
	if err != nil {
		return solana.PublicKey{}, false
	}
	return pub, true
}

// AccountIDFromPublicKey is the console account id for a Solana wallet:
// 0x-hex of the raw 32-byte public key, identical to the deposit watcher's
// account id, so the wallet that signs in IS the wallet that deposits.
func AccountIDFromPublicKey(pub solana.PublicKey) string {
	return "0x" + hex.EncodeToString(pub[:])
}

// handleNonce issues a fresh nonce for the claimed public key.
func (h *Handler) handleNonce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	pub, ok := parseAddress(req.Address)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}
	if h.Sessions == nil {
		writeError(w, http.StatusNotImplemented, "session store not configured")
		return
	}
	rec := newNonce(h.now())
	rec.domain, rec.uri = h.signInDomainURI(r)
	if err := h.Sessions.SaveNonce(r.Context(), session.Nonce{
		Address:   nonceKey(pub),
		Nonce:     rec.nonce,
		IssuedAt:  rec.issuedAt.Unix(),
		ExpiresAt: rec.expiresAt.Unix(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"nonce":     rec.nonce,
		"statement": rec.statement,
		"domain":    rec.domain,
		"uri":       rec.uri,
		"issuedAt":  rec.issuedAt.Format(time.RFC3339),
		"expiresAt": rec.expiresAt.Format(time.RFC3339),
	})
}

// handleVerify checks an ed25519 signature over the sign-in message against
// the stored nonce and, on success, issues a bearer token for the account.
func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address   string `json:"address"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	pub, ok := parseAddress(req.Address)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}
	sig, err := decodeSignature(req.Signature)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid signature")
		return
	}

	// Consume the nonce immediately after lookup, before any other check, so
	// it is single use even on a failed attempt and a replay always fails.
	// The store takes it in one statement, so two replicas cannot both
	// accept the same nonce.
	if h.Sessions == nil {
		writeError(w, http.StatusNotImplemented, "session store not configured")
		return
	}
	stored, found, err := h.Sessions.TakeNonce(r.Context(), nonceKey(pub))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found || stored.Nonce != req.Nonce {
		writeError(w, http.StatusBadRequest, "unknown nonce")
		return
	}
	rec := nonceRecord{
		nonce:     stored.Nonce,
		statement: statement,
		issuedAt:  time.Unix(stored.IssuedAt, 0).UTC(),
		expiresAt: time.Unix(stored.ExpiresAt, 0).UTC(),
	}
	// Re-derive the origin from this request. The message is rebuilt from
	// these fields to check the signature, so they must be the same ones
	// the nonce response carried - which they are whenever both requests
	// come from the same page.
	rec.domain, rec.uri = h.signInDomainURI(r)
	if !h.now().Before(rec.expiresAt) {
		writeError(w, http.StatusBadRequest, "nonce expired")
		return
	}

	msg := []byte(h.buildMessage(pub, rec))
	if !ed25519.Verify(pub[:], msg, sig) {
		writeError(w, http.StatusUnauthorized, "signature verification failed")
		return
	}

	accountID := AccountIDFromPublicKey(pub)
	if err := h.sweepSessions(r); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	token, expiresAt, err := h.tokens.issue(r.Context(), accountID, h.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"accountId": accountID,
		"token":     token,
		"expiresAt": expiresAt.Format(time.RFC3339),
	})
}

// handleSecret derives an account from a raw secret and issues a bearer token
// for it. This is weaker than the wallet path: the caller hands over a value
// that also spends the balance, and the console itself does not use it. It
// exists only for scripts with no wallet.
func (h *Handler) handleSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Secret == "" {
		writeError(w, http.StatusBadRequest, "invalid secret")
		return
	}
	// keys.Issue semantics: keccak256 of the raw secret string bytes, so a
	// script's spending secret maps to the account it funds.
	accountID := AccountIDFromSecret(req.Secret)
	if err := h.sweepSessions(r); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	token, expiresAt, err := h.tokens.issue(r.Context(), accountID, h.now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"accountId": accountID,
		"token":     token,
		"expiresAt": expiresAt.Format(time.RFC3339),
	})
}

// sweepSessions opportunistically removes expired session rows on the sign-in
// path; it is a no-op when no session store was attached.
func (h *Handler) sweepSessions(r *http.Request) error {
	if h.Sessions == nil {
		return nil
	}
	_, err := h.Sessions.SweepExpired(r.Context(), h.now().Unix())
	return err
}

// handleSignout revokes the caller's session server-side: the bearer token's
// row is deleted and its next use fails like any unknown token. The 401
// shape matches every other authenticated route.
func (h *Handler) handleSignout(w http.ResponseWriter, r *http.Request) {
	accountID, ok, reason, err := h.resolveBearer(r)
	_ = accountID
	if err != nil || !ok {
		failBearer(w, reason, err)
		return
	}
	// resolveBearer already validated the header; sign-out needs the token
	// itself to re-derive the hash it revokes.
	token, _ := bearerToken(r.Header.Get("Authorization"))
	var hash [32]byte
	copy(hash[:], keccak256([]byte(token)))
	if _, err := h.Sessions.Revoke(r.Context(), hash); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// signInDomainURI decides the domain and URI that go into the sign-in
// message.
//
// They must name the page the user is actually on. Phantom recognises a
// Sign-In with Solana message and validates it before showing a prompt: the
// domain has to be the requesting page's exact host, port included, and the
// URI's origin has to match the page's origin. Anything else is refused
// without a prompt ("The app's signature request cannot be shown due to
// invalid formatting"), so a placeholder domain is a sign-in that can never
// complete.
//
// The browser sends Origin on both the nonce and verify requests, so that is
// the page's own answer. Host is the fallback for a request without one - a
// script, or a proxy that stripped the header - and behind the dev-server
// proxy it is the console's host anyway. A configured Domain pins both.
func (h *Handler) signInDomainURI(r *http.Request) (string, string) {
	if h.Domain != "" {
		return h.Domain, h.URI
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
		if u, err := url.Parse(origin); err == nil && u.Host != "" {
			return u.Host, u.Scheme + "://" + u.Host
		}
	}
	if host := strings.TrimSpace(r.Host); host != "" {
		scheme := "http"
		if r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		return host, scheme + "://" + host
	}
	return "localhost", "http://localhost"
}

// buildMessage renders the exact sign-in message; one newline between lines,
// a blank line where shown. A wallet's signMessage signs these bytes
// verbatim with ed25519, and the verifier rebuilds the same string from the
// stored nonce, so client and server can never disagree on framing.
//
// The header names follow the Sign-In with Solana standard exactly
// ("Expiration Time", not "Expiration"): Phantom parses any message that
// looks like a sign-in request against that grammar and refuses to show a
// prompt for one that deviates, so a nonstandard header is a sign-in that
// can never complete.
func (h *Handler) buildMessage(pub solana.PublicKey, rec nonceRecord) string {
	return fmt.Sprintf(
		"%s wants you to sign in with your Solana account:\n%s\n\n%s\n\nURI: %s\nVersion: 1\nNonce: %s\nIssued At: %s\nExpiration Time: %s",
		rec.domain,
		pub.String(),
		rec.statement,
		rec.uri,
		rec.nonce,
		rec.issuedAt.Format(time.RFC3339),
		rec.expiresAt.Format(time.RFC3339),
	)
}

// decodeSignature parses a 64-byte ed25519 signature presented as hex (with
// or without a 0x prefix) or base58.
func decodeSignature(s string) ([]byte, error) {
	if b, err := hex.DecodeString(strings.TrimPrefix(s, "0x")); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	if b, err := base58.Decode(s); err == nil && len(b) == ed25519.SignatureSize {
		return b, nil
	}
	return nil, fmt.Errorf("console: signature must be %d bytes", ed25519.SignatureSize)
}
