package meter

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// fixtureAccount is the account id every usage-root fixture row belongs to.
const fixtureAccount = "0x0000000000000000000000000000000000000000000000000000000000000001"

// fixtureRows are the pinned usage-root fixture rows; the leaf and root
// hex below are the scheme's fixtures - any change to the encoding or the
// combine is a breaking change for outside verifiers and must update them.
var fixtureRows = []UsageRow{
	{RowID: 1, AccountID: fixtureAccount, KeyPrefix: "attk_abcdefg", Timestamp: time.Unix(1000, 0).UTC(),
		Service: "rpc", Model: "m", PromptTokens: 5, CachedTokens: 0, CompletionTokens: 13, Credits: 7, Status: "ok"},
	{RowID: 2, AccountID: fixtureAccount, KeyPrefix: "attk_abcdefg", Timestamp: time.Unix(1001, 0).UTC(),
		Service: "rpc", Model: "m", PromptTokens: 10, CachedTokens: 2, CompletionTokens: 20, Credits: 14, Status: "error"},
	{RowID: 3, AccountID: fixtureAccount, KeyPrefix: "attk_abcdefg", Timestamp: time.Unix(1002, 0).UTC(),
		Service: "inference", Model: "fireworks-embeddings", Kind: "embeddings",
		PromptTokens: 1000000, CachedTokens: 0, CompletionTokens: 0, Credits: 100 * MicroCreditsPerCredit, Status: "ok"},
}

// TestUsageLeafPinned pins one leaf against its literal hex: the keccak256
// of the ten fixed words in the documented order.
func TestUsageLeafPinned(t *testing.T) {
	got, err := UsageLeaf(fixtureRows[0])
	if err != nil {
		t.Fatalf("UsageLeaf: %v", err)
	}
	want, _ := hex.DecodeString("125200cca680eab7cae233bbe9846fb1cbf20b00a104a1dbfa91df99fb88a95b")
	if hex.EncodeToString(got[:]) != hex.EncodeToString(want) {
		t.Errorf("leaf of row 1 = %s, want %s", hex.EncodeToString(got[:]), hex.EncodeToString(want))
	}
	if got2, err := UsageLeaf(fixtureRows[1]); err != nil {
		t.Fatalf("UsageLeaf(row 2): %v", err)
	} else if want2, _ := hex.DecodeString("3a9dce9529ddf49666820c6e4e34b187da20c406b7666000123dc74b5ba7e1f7"); hex.EncodeToString(got2[:]) != hex.EncodeToString(want2) {
		t.Errorf("leaf of row 2 = %s, want %s", hex.EncodeToString(got2[:]), hex.EncodeToString(want2))
	}
}

// TestUsageRootPinned pins the three-row root (odd last node promotes
// unchanged at level two) and the four-row root (full-pair shape).
func TestUsageRootPinned(t *testing.T) {
	root, err := UsageRoot(fixtureRows)
	if err != nil {
		t.Fatalf("UsageRoot: %v", err)
	}
	want, _ := hex.DecodeString("8e24b900c8ab33591b96e7a80a01e48feeebcd7e0e48b5813bfdb7d5fc278d75")
	if hex.EncodeToString(root[:]) != hex.EncodeToString(want) {
		t.Errorf("root of rows 1..3 = %s, want %s", hex.EncodeToString(root[:]), hex.EncodeToString(want))
	}
	four := append([]UsageRow{}, fixtureRows...)
	four = append(four, UsageRow{RowID: 4, AccountID: fixtureAccount, KeyPrefix: "attk_abcdefg", Timestamp: time.Unix(1003, 0).UTC(),
		Service: "inference", Model: "fireworks-embeddings", Kind: "embeddings",
		PromptTokens: 2000000, CachedTokens: 1000, CompletionTokens: 0, Credits: 200 * MicroCreditsPerCredit, Status: "ok"})
	root4, err := UsageRoot(four)
	if err != nil {
		t.Fatalf("UsageRoot(4): %v", err)
	}
	want4, _ := hex.DecodeString("801fe5716d10c73024cd720e27c506c6ab1fb04a1181b9193dda53f0c173b26b")
	if hex.EncodeToString(root4[:]) != hex.EncodeToString(want4) {
		t.Errorf("root of rows 1..4 = %s, want %s", hex.EncodeToString(root4[:]), hex.EncodeToString(want4))
	}
}

// TestUsageRootUnsortedInputSameRoot checks the function sorts its input:
// a shuffled order gives the same root as the ascending order.
func TestUsageRootUnsortedInputSameRoot(t *testing.T) {
	sorted, err := UsageRoot(fixtureRows)
	if err != nil {
		t.Fatalf("UsageRoot(sorted): %v", err)
	}
	shuffled := []UsageRow{fixtureRows[2], fixtureRows[0], fixtureRows[1]}
	got, err := UsageRoot(shuffled)
	if err != nil {
		t.Fatalf("UsageRoot(shuffled): %v", err)
	}
	if got != sorted {
		t.Errorf("shuffled input root = %x, want the sorted root %x", got[:], sorted[:])
	}
}

// TestUsageRootSingleRowIsLeaf pins the degenerate tree: with one row the
// root is that row's leaf.
func TestUsageRootSingleRowIsLeaf(t *testing.T) {
	root, err := UsageRoot(fixtureRows[:1])
	if err != nil {
		t.Fatalf("UsageRoot(1): %v", err)
	}
	leaf, err := UsageLeaf(fixtureRows[0])
	if err != nil {
		t.Fatalf("UsageLeaf: %v", err)
	}
	if root != leaf {
		t.Errorf("single-row root = %x, want the leaf %x", root[:], leaf[:])
	}
}

// TestUsageRootEmptyErrors checks zero rows has no root.
func TestUsageRootEmptyErrors(t *testing.T) {
	if _, err := UsageRoot(nil); err == nil {
		t.Error("UsageRoot of no rows: got nil error, want error")
	}
}

// TestUsageLeafMalformedAccountID checks ids that are not the ledger's
// off-chain form fail instead of being zero-filled into the leaf word.
func TestUsageLeafMalformedAccountID(t *testing.T) {
	row := fixtureRows[0]
	cases := []string{
		"0x" + "00000000000000000000000000000000000000000000000000000000000000",         // 63 chars
		"0x" + "0000000000000000000000000000000000000000000000000000000000000001" + "0", // 65 chars
		"alice",
		"",
	}
	for i, id := range cases {
		row.AccountID = id
		if _, err := UsageLeaf(row); err == nil {
			t.Errorf("case %d: UsageLeaf accepted account id %q, want error", i, id)
		}
	}
}

// TestUsageRootPropagatesLeafError checks a bad id in one row fails the
// whole root.
func TestUsageRootPropagatesLeafError(t *testing.T) {
	bad := append([]UsageRow{}, fixtureRows...)
	bad[1].AccountID = "not-an-id"
	if _, err := UsageRoot(bad); !errors.Is(err, ErrUsageAccountID) {
		t.Errorf("UsageRoot with a malformed account id: got %v, want ErrUsageAccountID", err)
	}
}
