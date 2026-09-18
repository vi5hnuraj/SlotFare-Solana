package keys

import "testing"

// Fixture precomputed against sha3.NewLegacyKeccak256:
// keccak256("slotfare-secret-001") =
// b062d0bcb0d2680d69f94aca31c00ca633c29ac35198c9718dbadbaccdd1a436
const fixtureAccountID = "0xb062d0bcb0d2680d69f94aca31c00ca633c29ac35198c9718dbadbaccdd1a436"

func TestIssueDerivesAccountID(t *testing.T) {
	k := Issue("slotfare-secret-001")
	if k.AccountID != fixtureAccountID {
		t.Errorf("AccountID = %s, want %s", k.AccountID, fixtureAccountID)
	}
	if got, want := HashKey("slotfare-secret-001"), [32]byte{}; got == want {
		t.Error("HashKey returned all zeros for a non-empty secret")
	}
}

func TestVerifyOwnHashNotOther(t *testing.T) {
	self := Issue("slotfare-secret-001")
	other := Issue("slotfare-secret-002")
	if !Verify("slotfare-secret-001", self.Hash) {
		t.Error("a key does not verify against its own hash")
	}
	if Verify("slotfare-secret-002", self.Hash) {
		t.Error("a different secret verifies against another key's hash")
	}
	if Verify("slotfare-secret-001", other.Hash) {
		t.Error("a secret verifies against a key it was not issued under")
	}
}

func TestVerifyIsCaseSensitive(t *testing.T) {
	k := Issue("SlotFare-Secret-001")
	if !Verify("SlotFare-Secret-001", k.Hash) {
		t.Error("the exact secret does not verify")
	}
	if Verify("slotfare-secret-001", k.Hash) {
		t.Error("a case-mutated secret verifies; Verify must be an exact byte match")
	}
}
