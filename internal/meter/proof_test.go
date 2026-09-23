package meter

import (
	"encoding/hex"
	"testing"
	"time"
)

// pinnedRoots holds the fixture roots TestUsageRootPinned pins, so the proof
// roundtrip checks against the exact committed values.
var (
	pinnedThreeRowRoot, _ = hex.DecodeString("8e24b900c8ab33591b96e7a80a01e48feeebcd7e0e48b5813bfdb7d5fc278d75")
	pinnedFourRowRoot, _  = hex.DecodeString("801fe5716d10c73024cd720e27c506c6ab1fb04a1181b9193dda53f0c173b26b")
)

// fourFixtureRows is the three-row fixture plus the fourth row
// TestUsageRootPinned appends.
func fourFixtureRows() []UsageRow {
	four := append([]UsageRow{}, fixtureRows...)
	return append(four, UsageRow{RowID: 4, AccountID: fixtureAccount, KeyPrefix: "attk_abcdefg", Timestamp: time.Unix(1003, 0).UTC(),
		Service: "inference", Model: "fireworks-embeddings", Kind: "embeddings",
		PromptTokens: 2000000, CachedTokens: 1000, CompletionTokens: 0, Credits: 200 * MicroCreditsPerCredit, Status: "ok"})
}

// roundtrip builds a proof for each target row id and verifies it against the
// given root; it also confirms BuildBatchProof's recomputed root matches.
func roundtrip(t *testing.T, rows []UsageRow, wantRoot [32]byte, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		root, proof, err := BuildBatchProof(rows, id)
		if err != nil {
			t.Fatalf("BuildBatchProof(%d): %v", id, err)
		}
		if root != wantRoot {
			t.Errorf("BuildBatchProof(%d) root = %x, want %x", id, root[:], wantRoot[:])
		}
		leaf, err := UsageLeaf(rowByID(rows, id))
		if err != nil {
			t.Fatalf("UsageLeaf(%d): %v", id, err)
		}
		if !VerifyBatchProof(leaf, proof, wantRoot) {
			t.Errorf("proof for row %d does not verify against the batch root", id)
		}
	}
}

func rowByID(rows []UsageRow, id int64) UsageRow {
	for _, r := range rows {
		if r.RowID == id {
			return r
		}
	}
	panic("row not in fixture")
}

// TestProofRoundtripThreeRows proves first, middle and last rows of the
// three-row fixture batch against the pinned root.
func TestProofRoundtripThreeRows(t *testing.T) {
	roundtrip(t, fixtureRows, [32]byte(pinnedThreeRowRoot), 1, 2, 3)
}

// TestProofRoundtripFourRows does the same for the full-pair four-row shape.
func TestProofRoundtripFourRows(t *testing.T) {
	roundtrip(t, fourFixtureRows(), [32]byte(pinnedFourRowRoot), 1, 2, 3, 4)
}

// TestProofSingleRow checks the degenerate tree: one row gives an empty proof
// whose leaf is the root.
func TestProofSingleRow(t *testing.T) {
	root, proof, err := BuildBatchProof(fixtureRows[:1], 1)
	if err != nil {
		t.Fatalf("BuildBatchProof(single): %v", err)
	}
	leaf, _ := UsageLeaf(fixtureRows[0])
	if len(proof) != 0 {
		t.Errorf("single-row proof has %d steps, want 0", len(proof))
	}
	if root != leaf {
		t.Errorf("single-row root = %x, want the leaf %x", root[:], leaf[:])
	}
	if !VerifyBatchProof(leaf, proof, root) {
		t.Error("single-row empty proof should verify leaf == root")
	}
}

// TestProofWrongLeafFails confirms a tampered leaf no longer verifies.
func TestProofWrongLeafFails(t *testing.T) {
	root, proof, err := BuildBatchProof(fixtureRows, 2)
	if err != nil {
		t.Fatalf("BuildBatchProof: %v", err)
	}
	bad := [32]byte{}
	for i := range bad {
		bad[i] = byte(i)
	}
	if VerifyBatchProof(bad, proof, root) {
		t.Error("a wrong leaf verified against the real root")
	}
}

// TestProofWrongRootFails confirms the real leaf does not verify against a
// different root.
func TestProofWrongRootFails(t *testing.T) {
	_, proof, err := BuildBatchProof(fixtureRows, 2)
	if err != nil {
		t.Fatalf("BuildBatchProof: %v", err)
	}
	leaf, _ := UsageLeaf(fixtureRows[1])
	wrong := [32]byte{0xff}
	if VerifyBatchProof(leaf, proof, wrong) {
		t.Error("the leaf verified against a wrong root")
	}
}

// TestBuildBatchProofMissingTarget confirms an absent target errors.
func TestBuildBatchProofMissingTarget(t *testing.T) {
	if _, _, err := BuildBatchProof(fixtureRows, 99); err == nil {
		t.Error("BuildBatchProof for an absent row: got nil error, want error")
	}
	if _, _, err := BuildBatchProof(nil, 1); err == nil {
		t.Error("BuildBatchProof on no rows: got nil error, want error")
	}
}
