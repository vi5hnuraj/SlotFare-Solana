package console

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

// TestLiveConsoleSignIn drives the ported ed25519 sign-in flow against a
// running slotfared over HTTP: nonce, byte-exact message, signature, verify,
// then replay and wrong-signer rejections. Opt-in like the other live tests:
//
//	SLOTFARE_CONSOLE_SMOKE=http://127.0.0.1:8088 go test ./internal/console -run TestLiveConsoleSignIn -v
func TestLiveConsoleSignIn(t *testing.T) {
	base := os.Getenv("SLOTFARE_CONSOLE_SMOKE")
	if base == "" {
		t.Skip("set SLOTFARE_CONSOLE_SMOKE=http://host:port against a running slotfared")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	address := solana.PublicKeyFromBytes(pub).String()

	// Round 1: happy path.
	nonce := liveFetchNonce(t, base, address)
	msg := signInMessage(address, nonce)
	sig := ed25519.Sign(priv, []byte(msg))
	body := postJSON(t, base+"/console/session/verify", map[string]string{
		"address":   address,
		"nonce":     nonce["nonce"],
		"signature": hex.EncodeToString(sig),
	}, http.StatusOK)

	wantAccount := AccountIDFromPublicKey(solana.PublicKeyFromBytes(pub))
	if body["accountId"] != wantAccount {
		t.Errorf("accountId = %s, want %s", body["accountId"], wantAccount)
	}
	if body["token"] == "" {
		t.Error("verify returned an empty token")
	}
	t.Logf("sign-in ok: accountId=%s token=%s...", body["accountId"], body["token"][:12])

	// Replaying the same nonce must fail: it was consumed by round 1.
	replay := rawPost(t, base+"/console/session/verify", map[string]string{
		"address":   address,
		"nonce":     nonce["nonce"],
		"signature": hex.EncodeToString(sig),
	})
	if replay.StatusCode == http.StatusOK {
		t.Error("replayed nonce was accepted; nonces must be single use")
	}

	// Round 2: a signature from a different key over a fresh nonce is 401.
	nonce2 := liveFetchNonce(t, base, address)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate second key: %v", err)
	}
	badSig := ed25519.Sign(otherPriv, []byte(signInMessage(address, nonce2)))
	rawPost(t, base+"/console/session/verify", map[string]string{
		"address":   address,
		"nonce":     nonce2["nonce"],
		"signature": hex.EncodeToString(badSig)},
	).expectStatus(t, http.StatusUnauthorized)
}

// signInMessage rebuilds the server's sign-in message from the nonce
// response, byte for byte - the same framing the web console signs.
func signInMessage(address string, n map[string]string) string {
	return fmt.Sprintf(
		"%s wants you to sign in with your Solana account:\n%s\n\n%s\n\nURI: %s\nVersion: 1\nNonce: %s\nIssued At: %s\nExpiration Time: %s",
		n["domain"], address, n["statement"], n["uri"],
		n["nonce"], n["issuedAt"], n["expiresAt"],
	)
}

func liveFetchNonce(t *testing.T, base, address string) map[string]string {
	t.Helper()
	body := postJSON(t, base+"/console/session/nonce", map[string]string{"address": address}, http.StatusOK)
	for _, key := range []string{"nonce", "statement", "domain", "uri", "issuedAt", "expiresAt"} {
		if body[key] == "" {
			t.Fatalf("nonce response missing %q: %v", key, body)
		}
	}
	return body
}

func postJSON(t *testing.T, url string, payload any, wantStatus int) map[string]string {
	t.Helper()
	res := rawPost(t, url, payload)
	if res.StatusCode != wantStatus {
		t.Fatalf("POST %s: status %d, want %d (body %s)", url, res.StatusCode, wantStatus, res.body)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(res.body), &out); err != nil {
		t.Fatalf("decode %s response %q: %v", url, res.body, err)
	}
	return out
}

type httpResult struct {
	StatusCode int
	body       string
}

func (r httpResult) expectStatus(t *testing.T, want int) {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("status %d, want %d (body %s)", r.StatusCode, want, r.body)
	}
}

func rawPost(t *testing.T, url string, payload any) httpResult {
	t.Helper()
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(res.Body); err != nil {
		t.Fatalf("read %s response: %v", url, err)
	}
	return httpResult{StatusCode: res.StatusCode, body: out.String()}
}
