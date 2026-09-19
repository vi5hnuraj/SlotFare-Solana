// Package receipt signs the canonical usage fields of one metered response
// so the provider cannot author them alone after the fact: every metered,
// non-streaming response carries an x-slotfare-receipt header, a compact
// base64 JSON envelope signed by a dedicated Ed25519 keypair - never a chain
// spender key. The signer's Solana public key is the served public identity
// (/.well-known/slotfare-receipt-key); verification is an Ed25519 check
// against that published key, the same scheme as the console's SIWS
// sign-in. The package is HTTP-free so it stays unit-testable offline.
//
// Format history: the original envelope was secp256k1/ecrecover over
// an Ethereum address. No external
// verifier had shipped, so envelope v2 drops the recovery dance: `alg` is
// `ed25519`, `address` is base58, and the signature is a plain 64-byte
// Ed25519 signature over the canonical payload bytes - no digest, no v byte.
// A v1 secp256k1 envelope now fails verification explicitly.
package receipt

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	gsolana "github.com/gagliardetto/solana-go"

	"slotfare/internal/meter"
)

// EnvelopeVersion is the receipt envelope's format tag. v1 was the
// secp256k1/ecrecover envelope; v2 is the Ed25519 envelope below.
const EnvelopeVersion = 2

// Alg is the envelope's signature algorithm tag.
const Alg = "ed25519"

// Signer holds the dedicated receipt keypair. It is distinct from any chain
// key on purpose: signing receipts is a read-only act, and a leaked receipt
// key must never expose spendable funds.
type Signer struct {
	key ed25519.PrivateKey
}

// Generate returns a Signer over a freshly generated Ed25519 key, for
// operators standing up receipts for the first time.
func Generate() (*Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("receipt: generate key: %w", err)
	}
	return &Signer{key: key}, nil
}

// LoadHex returns a Signer over a hex-encoded Ed25519 private key: either
// the full 64-byte key (hex of a Solana keypair file's secret-key field) or
// the 32-byte seed, which is expanded. 0x prefix optional. A malformed key
// is an error, never a silent key.
func LoadHex(keyHex string) (*Signer, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(keyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("receipt: decode hex key: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return &Signer{key: ed25519.PrivateKey(raw)}, nil
	case ed25519.SeedSize:
		return &Signer{key: ed25519.NewKeyFromSeed(raw)}, nil
	default:
		return nil, fmt.Errorf("receipt: key is %d bytes, want 32 (seed) or 64 (private key)", len(raw))
	}
}

// PublicKey is the signer's Solana public key - the served public identity a
// verifier fetches from /.well-known/slotfare-receipt-key and compares
// against.
func (s *Signer) PublicKey() gsolana.PublicKey {
	var pub gsolana.PublicKey
	copy(pub[:], s.key[ed25519.SeedSize:])
	return pub
}

// Address is the wallet-address form of the public key (base58), for logs
// and the well-known body.
func (s *Signer) Address() string {
	return s.PublicKey().String()
}

// PublicKeyHex is the Ed25519 public key in hex, for tooling that prefers
// the raw key over the address. The served identity is the same key.
func (s *Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.key[ed25519.SeedSize:])
}

// NewNonce returns a server-assigned request nonce: 16 random bytes,
// hex-encoded to 32 lowercase characters. It ties a receipt to the exact
// usage row recorded for that exchange without revealing anything about the
// request.
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing means the entropy source is broken; there is no
		// safe fallback, so fail the exchange like any meter error would.
		panic(fmt.Sprintf("receipt: read nonce randomness: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// Payload is the canonical set of usage fields one receipt covers: exactly
// the T02 leaf words the row will be recorded with (minus the not-yet-known
// row id) plus the server-assigned request nonce. The field names mirror
// proofLeafView so "receipt field == leaf field" holds directly, and the
// declaration order IS the canonical order: Go marshals struct fields in
// declaration order, so CanonicalBytes is deterministic and the signature
// pins it.
type Payload struct {
	AccountID        string `json:"accountId"`
	TS               int64  `json:"ts"`
	Service          string `json:"service"`
	Model            string `json:"model"`
	PromptTokens     uint64 `json:"promptTokens"`
	CachedTokens     uint64 `json:"cachedTokens"`
	CompletionTokens uint64 `json:"completionTokens"`
	CreditsMicro     uint64 `json:"creditsMicro"`
	Status           string `json:"status"`
	Nonce            string `json:"nonce"`
}

// PayloadFromRow renders the receipt payload from the exact usage row that
// will be recorded.
func PayloadFromRow(r meter.UsageRow) Payload {
	return Payload{
		AccountID:        r.AccountID,
		TS:               r.Timestamp.Unix(),
		Service:          r.Service,
		Model:            r.Model,
		PromptTokens:     r.PromptTokens,
		CachedTokens:     r.CachedTokens,
		CompletionTokens: r.CompletionTokens,
		CreditsMicro:     r.Credits,
		Status:           r.Status,
		Nonce:            r.Nonce,
	}
}

// CanonicalBytes is the exact byte string the signature covers: the JSON
// encoding of the payload in declaration order, no whitespace.
func (p Payload) CanonicalBytes() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("receipt: marshal canonical payload: %w", err)
	}
	return b, nil
}

// envelope is the signed object: base64 of this JSON is the
// x-slotfare-receipt header value. Payload is the base64 of the canonical
// bytes and Sig is the hex Ed25519 signature over those bytes directly.
type envelope struct {
	V       int    `json:"v"`
	Alg     string `json:"alg"`
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
}

// Sign returns the full x-slotfare-receipt header value for p: the canonical
// bytes signed with the Ed25519 key, wrapped in the base64 JSON envelope.
// The signature covers the canonical bytes alone, so a verifier who
// re-marshals the payload and expects a like-for-like signature gets a
// different digest - the envelope's own base64 payload is the authority.
func (s *Signer) Sign(p Payload) (string, error) {
	canonical, err := p.CanonicalBytes()
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(s.key, canonical)
	env := envelope{
		V:       EnvelopeVersion,
		Alg:     Alg,
		Payload: base64.StdEncoding.EncodeToString(canonical),
		Sig:     hex.EncodeToString(sig),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("receipt: marshal envelope: %w", err)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// Verify decodes the x-slotfare-receipt header value and checks the
// signature against pub. It returns the parsed payload on success so a
// caller can compare the signed fields against a stored row. ok is false -
// with a nil error when nothing is structurally wrong - when the signature
// does not verify, which also covers a v1 secp256k1 envelope: format
// migration is explicit (alg/version rejection), never a silent pass.
func Verify(value string, pub gsolana.PublicKey) (Payload, bool, error) {
	var p Payload
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return p, false, fmt.Errorf("receipt: decode envelope: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return p, false, fmt.Errorf("receipt: parse envelope: %w", err)
	}
	if env.V != EnvelopeVersion {
		return p, false, fmt.Errorf("receipt: unsupported envelope version %d", env.V)
	}
	if env.Alg != Alg {
		return p, false, fmt.Errorf("receipt: unsupported algorithm %q", env.Alg)
	}
	canonical, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return p, false, fmt.Errorf("receipt: decode payload: %w", err)
	}
	sig, err := hex.DecodeString(stringsTrim0x(env.Sig))
	if err != nil {
		return p, false, fmt.Errorf("receipt: decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return p, false, fmt.Errorf("receipt: signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub[:], canonical, sig) {
		return p, false, nil
	}
	if err := json.Unmarshal(canonical, &p); err != nil {
		return p, false, fmt.Errorf("receipt: parse payload: %w", err)
	}
	return p, true, nil
}

// stringsTrim0x drops a leading 0x from a hex string, matching how the
// signer accepts 0x-prefixed or bare hex.
func stringsTrim0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}
