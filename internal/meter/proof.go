package meter

import (
	"errors"
	"sort"

	"golang.org/x/crypto/sha3"
)

// ProofStep is one combine on the path from a leaf to a batch root: Hash is
// the sibling node, Direction says where that sibling sat - "right" when the
// target was the left child (the sibling is combined to the target's right),
// "left" when the target was the right child. Steps are ordered leaf to root.
// An odd last node promotes unchanged and contributes no step.
type ProofStep struct {
	Hash      [32]byte
	Direction string // "left" or "right"
}

// errProofTargetMissing is returned when the target row id is not among the
// rows a proof is requested for.
var errProofTargetMissing = errors.New("meter: proof target row not in batch range")

// BuildBatchProof computes, for the row with id targetRowID, the Merkle proof
// path that links its leaf to the batch root per the UsageRoot scheme. The
// input is the batch's stored row range, in any order - it is sorted the same
// way UsageRoot sorts before it combines. It returns the recomputed root, the
// proof steps (empty for a single-row batch, where the root is the leaf), and
// an error when targetRowID is absent from rows.
//
// The proof is built on demand from the range: nothing beyond the rows
// already stored by T02 is kept, and the walk re-derives the exact tree the
// committed root was computed over.
func BuildBatchProof(rows []UsageRow, targetRowID int64) ([32]byte, []ProofStep, error) {
	if len(rows) == 0 {
		return [32]byte{}, nil, errProofTargetMissing
	}
	sorted := make([]UsageRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RowID < sorted[j].RowID })

	target := -1
	for i, r := range sorted {
		if r.RowID == targetRowID {
			target = i
			break
		}
	}
	if target < 0 {
		return [32]byte{}, nil, errProofTargetMissing
	}

	// Build the level arrays exactly as UsageRoot does, tracing the
	// target's index up the tree.
	level := make([][32]byte, len(sorted))
	for i, r := range sorted {
		leaf, err := UsageLeaf(r)
		if err != nil {
			return [32]byte{}, nil, err
		}
		level[i] = leaf
	}
	proof := []ProofStep{}
	idx := target
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
		// Record the sibling on the path that holds idx this level.
		if idx%2 == 0 {
			if idx+1 < len(level) { // even and has a right sibling
				proof = append(proof, ProofStep{Hash: level[idx+1], Direction: "right"})
			}
			// even and odd-last (no sibling): no step, node promotes
		} else {
			proof = append(proof, ProofStep{Hash: level[idx-1], Direction: "left"})
		}
		idx = idx / 2
		level = next
	}
	return level[0], proof, nil
}

// VerifyBatchProof walks the proof from the leaf to the root per the
// ProofStep ordering and reports whether the result equals root. An empty
// proof is valid only when the leaf itself is the root (single-row batch).
func VerifyBatchProof(leaf [32]byte, proof []ProofStep, root [32]byte) bool {
	cur := leaf
	for _, s := range proof {
		h := sha3.NewLegacyKeccak256()
		switch s.Direction {
		case "left":
			h.Write(s.Hash[:])
			h.Write(cur[:])
		case "right":
			h.Write(cur[:])
			h.Write(s.Hash[:])
		default:
			return false
		}
		copy(cur[:], h.Sum(nil))
	}
	return cur == root
}
