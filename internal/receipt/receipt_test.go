package receipt

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"slotfare/internal/meter"
)

// samplePayload is a fixed payload the round-trip tests sign and verify.
func samplePayload() Payload {
	return Payload{
		AccountID:        "0xaccount",
		TS:               1750000000,
		Service:          "inference",
		Model:            "mistral-nemotron",
		PromptTokens:     400_000,
		CachedTokens:     12_000,
		CompletionTokens: 300_000,
		CreditsMicro:     7 * meter.MicroCreditsPerCredit,
		Status:           "ok",
		Nonce:            NewNonce(),
	}
}

// A signed receipt verifies against the signer's own address and returns the
// exact payload that was signed.
func TestSignVerifyRoundtrip(t *testing.T) {
	s, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	p := samplePayload()
	value, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, ok, err := Verify(value, s.PublicKey())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify reports mismatch for the signer's own address")
	}
	if got != p {
		t.Errorf("verified payload = %+v, want %+v", got, p)
	}
}

// The signature covers the canonical bytes: tampering with any field after
// signing breaks verification.
func TestVerifyRejectsTamperedPayload(t *testing.T) {
	s, _ := Generate()
	p := samplePayload()
	value, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	raw, err := decodeEnvelope(value)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	tampered := p
	tampered.CreditsMicro++
	tb, err := tampered.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	raw.Payload = base64.StdEncoding.EncodeToString(tb)
	tamperedValue, err := encodeEnvelope(raw)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if _, ok, err := Verify(tamperedValue, s.PublicKey()); err != nil {
		t.Fatalf("Verify on tampered receipt must not error, got %v", err)
	} else if ok {
		t.Error("tampered payload verified: the signature does not pin the fields")
	}
}

// A receipt from one keypair does not verify against another address.
func TestVerifyRejectsWrongAddress(t *testing.T) {
	s, _ := Generate()
	other, _ := Generate()
	p := samplePayload()
	value, err := s.Sign(p)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, ok, err := Verify(value, other.PublicKey()); err != nil {
		t.Fatalf("Verify: %v", err)
	} else if ok {
		t.Error("receipt verified against a foreign address")
	}
}

// Malformed envelopes are errors, not silent mismatches.
func TestVerifyMalformed(t *testing.T) {
	s, _ := Generate()
	for name, value := range map[string]string{
		"not base64":  "!!!",
		"not json":    "aGVsbG8=",
		"bad version": `{"v":9,"alg":"secp256k1","payload":"e30=","sig":"0x"}`,
		"bad alg":     `{"v":1,"alg":"ed25519","payload":"e30=","sig":"0x"}`,
		"short sig":   `{"v":1,"alg":"secp256k1","payload":"e30=","sig":"0xabcd"}`,
		"bad sig hex": `{"v":1,"alg":"secp256k1","payload":"e30=","sig":"0zzz"}`,
		"bad payload": `{"v":1,"alg":"secp256k1","payload":"!!","sig":"0x` + strings.Repeat("ab", 32) + `"}`,
	} {
		if _, _, err := Verify(value, s.PublicKey()); err == nil {
			t.Errorf("%s: Verify returned (ok, nil), want an error", name)
		}
	}
}

// LoadHex accepts bare and 0x-prefixed hex of the same key; both load to the
// same address.
func TestLoadHexRoundtrip(t *testing.T) {
	s, _ := Generate()
	keyHex := hex.EncodeToString(s.key)
	for _, form := range []string{keyHex, "0x" + keyHex} {
		loaded, err := LoadHex(form)
		if err != nil {
			t.Fatalf("LoadHex(%s...): %v", form[:8], err)
		}
		if loaded.Address() != s.Address() {
			t.Errorf("loaded address = %s, want %s", loaded.Address(), s.Address())
		}
	}
	if _, err := LoadHex("nope"); err == nil {
		t.Error("LoadHex on garbage returned no error")
	}
	if _, err := LoadHex(""); err == nil {
		t.Error("LoadHex on empty returned no error")
	}
}

// NewNonce is 32 lowercase hex characters and does not repeat in practice.
func TestNewNonce(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		n := NewNonce()
		if len(n) != 32 || strings.ToLower(n) != n {
			t.Fatalf("nonce %q is not 32 lowercase hex chars", n)
		}
		if _, err := hex.DecodeString(n); err != nil {
			t.Fatalf("nonce %q is not hex: %v", n, err)
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated within 1000 draws", n)
		}
		seen[n] = true
	}
}

// PayloadFromRow renders exactly the recorded row's fields, so a verifier
// comparing receipt fields to the stored row sees equality.
func TestPayloadFromRow(t *testing.T) {
	row := meter.UsageRow{
		AccountID:        "acct",
		Timestamp:        time.Unix(1750000000, 0).UTC(),
		Service:          "rpc",
		Model:            "m",
		PromptTokens:     1,
		CachedTokens:     2,
		CompletionTokens: 3,
		Credits:          4,
		Status:           "ok",
		Nonce:            "n",
	}
	p := PayloadFromRow(row)
	if p.AccountID != row.AccountID || p.TS != row.Timestamp.Unix() ||
		p.Service != row.Service || p.Model != row.Model ||
		p.PromptTokens != row.PromptTokens || p.CachedTokens != row.CachedTokens ||
		p.CompletionTokens != row.CompletionTokens || p.CreditsMicro != row.Credits ||
		p.Status != row.Status || p.Nonce != row.Nonce {
		t.Errorf("PayloadFromRow = %+v, want the row's fields", p)
	}
}

// CanonicalBytes is deterministic and in declaration order.
func TestCanonicalBytesOrdering(t *testing.T) {
	p := Payload{AccountID: "a", TS: 1, Nonce: "n"}
	b1, err := p.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	want := `{"accountId":"a","ts":1,"service":"","model":"","promptTokens":0,"cachedTokens":0,"completionTokens":0,"creditsMicro":0,"status":"","nonce":"n"}`
	if string(b1) != want {
		t.Errorf("canonical bytes = %s, want %s", b1, want)
	}
}

// decodeEnvelope / encodeEnvelope are test helpers around the wire shape so
// the tamper test can rewrite one field without re-signing.
type testEnvelope struct {
	V       int    `json:"v"`
	Alg     string `json:"alg"`
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
}

func decodeEnvelope(value string) (*testEnvelope, error) {
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	var out testEnvelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func encodeEnvelope(e *testEnvelope) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
