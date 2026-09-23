package meter

import "testing"

// TestRaiseMintedOnlyEverRaises pins the deposit rail's guard at the storage
// layer. Minted is derived from the treasury's transfer history, so a scanned
// total below what is already recorded can only be an incomplete read, and
// accepting it would destroy credits the chain still holds.
func TestRaiseMintedOnlyEverRaises(t *testing.T) {
	cases := []struct {
		name          string
		first, second uint64
		want          uint64
	}{
		{"a lower scan is refused", 2500, 1000, 2500},
		{"an equal scan is a no-op", 2500, 2500, 2500},
		{"a higher scan raises the total", 2500, 3500, 3500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLedger()
			l.RaiseMinted("acct", tc.first)
			l.RaiseMinted("acct", tc.second)
			if got := l.Minted("acct"); got != tc.want {
				t.Errorf("minted = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRaiseMintedCannotLowerAcrossRestore is the audit's restart scenario at
// the storage layer: a balance restored from the checkpoint, then a first pass
// that saw only a truncated RPC history.
func TestRaiseMintedCannotLowerAcrossRestore(t *testing.T) {
	before := NewLedger()
	before.RaiseMinted("acct", 3500)

	after := NewLedger()
	after.Restore(before.Snapshot())
	after.RaiseMinted("acct", 1000)

	if got := after.Minted("acct"); got != 3500 {
		t.Errorf("minted = %d after an incomplete rescan, want the restored 3500", got)
	}
}

// TestSetMintedAdoptsTheChainFigure pins the other half of the split: the
// settler's refresh must still be able to lower Minted, because the chain
// program is the authority and a local figure above it is a figure the chain
// will not let this account settle. Without this, the monotonic guard would
// have silently removed the settler's ability to correct an over-credit.
func TestSetMintedAdoptsTheChainFigure(t *testing.T) {
	l := NewLedger()
	l.SetMinted("acct", 1000*MicroCreditsPerCredit)
	l.SetMinted("acct", 500*MicroCreditsPerCredit)

	if got := l.Minted("acct"); got != 500*MicroCreditsPerCredit {
		t.Errorf("minted = %d, want the chain figure %d", got, 500*MicroCreditsPerCredit)
	}
}

// TestMintedUnknownAccountIsZero: a wallet that has never deposited reads as
// zero rather than as a missing entry.
func TestMintedUnknownAccountIsZero(t *testing.T) {
	if got := NewLedger().Minted("nobody"); got != 0 {
		t.Errorf("minted = %d, want 0 for an unknown account", got)
	}
}

// TestRaiseMintedLeavesTheMarketAdjustAlone: a deposit rescan must not clobber
// credits the account acquired by buying a listing, which is why the market
// movement rides in Adjust rather than in Minted.
func TestRaiseMintedLeavesTheMarketAdjustAlone(t *testing.T) {
	l := NewLedger()
	l.RaiseMinted("acct", 2500)
	l.AdjustMinted("acct", 500) // 500 credits bought from a seller
	l.RaiseMinted("acct", 3000) // a later deposit rescan, absolute

	a := l.Snapshot()["acct"]
	if a.Minted != 3000 || a.Adjust != 500 {
		t.Errorf("minted/adjust = %d/%d, want 3000/500", a.Minted, a.Adjust)
	}
	if got := a.TotalMinted(); got != 3500 {
		t.Errorf("total minted = %d, want 3500 (3000 deposited + 500 received)", got)
	}
}
