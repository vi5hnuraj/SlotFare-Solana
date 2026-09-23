package meter

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Usage root scheme - pinned here so an outside verifier can reimplement
// it from this comment alone. The hash is keccak256 (the legacy Keccak
// variant the proxy's existing commitment format already used,
// golang.org/x/crypto/sha3.NewLegacyKeccak256), and every value is written
// into fixed 32-byte words before hashing.
//
// Leaf of one usage row: keccak256 of the concatenation of these ten
// words, in this exact order:
//  1. id            - uint64, big-endian, right-aligned in 32 bytes
//  2. account_id    - the 32 raw bytes of the account id: the 64 hex chars
//     of the stored "0x..." string decoded; an id that is
//     not exactly that form is an error, never zero-filled
//  3. ts            - uint64 unix seconds (UsageRow.Timestamp.Unix()),
//     big-endian, right-aligned
//  4. service       - ABI string word: 32-byte big-endian byte length,
//     then the UTF-8 bytes zero-padded up to a multiple
//     of 32
//  5. model         - ABI string word, same rule
//  6. prompt_tokens   - uint64 big-endian right-aligned
//  7. cached_tokens   - uint64 big-endian right-aligned
//  8. completion_tokens - uint64 big-endian right-aligned
//  9. credits       - uint64 micro-credits, big-endian right-aligned
//
// 10. status        - ABI string word, same rule
//
// Root of a batch: the rows' leaves ordered by row id ascending, combined
// with the standard sorted-pair Merkle tree: at each level adjacent pairs
// (n[i], n[i+1]) combine to keccak256(n[i] || n[i+1]) - left first - and an
// odd last node promotes unchanged to the next level; the root is the
// final single node. With one row the root is that row's leaf. An empty
// batch has no root: a batch always covers at least one row.
const usageLeafWords = 10

// ErrUsageAccountID is returned for an account id that is not the ledger's
// off-chain form ("0x" plus 64 lowercase hex chars).
var ErrUsageAccountID = errors.New("meter: usage leaf: account id is not 0x plus 64 hex chars")

// accountIDLeafWord returns word 2 of the leaf: the 32 raw bytes of the
// account id. Any other length, case, or non-hex input is an error.
func accountIDLeafWord(id string) ([32]byte, error) {
	var out [32]byte
	if len(id) != 2+64 || !strings.HasPrefix(id, "0x") {
		return out, ErrUsageAccountID
	}
	b, err := hex.DecodeString(id[2:]) // rejects uppercase and non-hex
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrUsageAccountID, err)
	}
	copy(out[:], b)
	return out, nil
}

// abiStringWord encodes s as one or more 32-byte ABI string words: each
// word starts with its big-endian byte length, then the UTF-8 bytes
// zero-padded to a multiple of 32. Empty strings encode as one all-zero
// word.
func abiStringWord(s string) []byte {
	b := []byte(s)
	n := len(b) / 32
	if n*32 < len(b) {
		n++
	}
	if n == 0 {
		n = 1
	}
	out := make([]byte, n*32)
	for i := 0; i < n; i++ {
		seg := b[i*32:]
		if len(seg) > 32 {
			seg = seg[:32]
		}
		binary.BigEndian.PutUint32(out[i*32:], uint32(len(seg)))
		copy(out[i*32+4:], seg)
	}
	return out
}

// UsageLeaf computes the Merkle leaf of one usage row per the scheme in
// the package comment above.
func UsageLeaf(row UsageRow) ([32]byte, error) {
	acc, err := accountIDLeafWord(row.AccountID)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha3.NewLegacyKeccak256()
	writeUint64 := func(v uint64) {
		var w [32]byte
		binary.BigEndian.PutUint64(w[24:], v)
		h.Write(w[:])
	}
	writeUint64(uint64(row.RowID))
	h.Write(acc[:])
	writeUint64(uint64(row.Timestamp.Unix()))
	h.Write(abiStringWord(row.Service))
	h.Write(abiStringWord(row.Model))
	writeUint64(row.PromptTokens)
	writeUint64(row.CachedTokens)
	writeUint64(row.CompletionTokens)
	writeUint64(row.Credits)
	h.Write(abiStringWord(row.Status))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// UsageRoot computes the Merkle root of a settle batch's usage rows per
// the scheme in the package comment above: leaves ordered by row id
// ascending (the input may arrive in any order), standard sorted-pair
// combine. Zero rows is an error.
func UsageRoot(rows []UsageRow) ([32]byte, error) {
	if len(rows) == 0 {
		return [32]byte{}, errors.New("meter: usage root of no rows")
	}
	sorted := make([]UsageRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RowID < sorted[j].RowID })
	level := make([][32]byte, len(sorted))
	for i, r := range sorted {
		leaf, err := UsageLeaf(r)
		if err != nil {
			return [32]byte{}, err
		}
		level[i] = leaf
	}
	for len(level) > 1 {
		next := make([][32]byte, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				k := sha3.NewLegacyKeccak256()
				k.Write(level[i][:])
				k.Write(level[i+1][:])
				copy(next[i/2][:], k.Sum(nil))
			} else {
				next[i/2] = level[i] // odd last node promotes unchanged
			}
		}
		level = next
	}
	return level[0], nil
}
