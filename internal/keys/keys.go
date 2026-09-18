// Package keys issues and verifies the API keys that gate the metering
// proxy. A key is the secret itself; only its keccak256 hash is stored, so
// the store holds nothing that identifies a person (D05).
package keys

import (
	"encoding/hex"

	"golang.org/x/crypto/sha3"
)

// Key is one issued key: the stored hash plus the account id it maps to in
// the credit ledger. The secret behind the key is never held here.
type Key struct {
	Hash         [32]byte
	AccountID    string
	Prefix       string
	Label        string
	CreatedAt    int64
	LastUsed     int64
	RequestCount int64
	Revoked      bool
}

// HashKey returns keccak256(secret).
func HashKey(secret string) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(secret))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Issue derives the key material from a secret: Hash is keccak256(secret)
// and AccountID is "0x" plus the lowercase hex of the same digest, matching
// the ledger's off-chain accountId form so settlement can look the same
// account up on chain.
func Issue(secret string) Key {
	hash := HashKey(secret)
	return Key{Hash: hash, AccountID: "0x" + hex.EncodeToString(hash[:])}
}

// Verify reports whether presented hashes to the stored key hash. It is an
// exact byte match on the digest; no normalization of the secret happens.
func Verify(presented string, stored [32]byte) bool {
	return HashKey(presented) == stored
}
