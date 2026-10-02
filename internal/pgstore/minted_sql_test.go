package pgstore

import (
	"context"
	"path/filepath"
	"testing"

	"slotfare/internal/meter"
)

// TestRaiseMintedCannotLowerTheTotal is the Postgres half of the audit's
// critical finding, and it is behavioral rather than a string check:
// SQLRaiseMinted is what the shared authority uses for a deposit rescan, and
// it is exercised here through the stand-in store, which runs the same text
// this package runs against Postgres. A statement that plainly assigned minted
// would pass every other test in this file and fail these.
func TestRaiseMintedCannotLowerTheTotal(t *testing.T) {
	cases := []struct {
		name            string
		stored, scanned uint64
		want            uint64
	}{
		{"a lower scan keeps the stored total", 2500, 1000, 2500},
		{"an equal scan changes nothing", 2500, 2500, 2500},
		{"a higher scan raises the stored total", 2500, 3500, 3500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "slotfared.db")
			seedAccounts(t, path, map[string]meter.Account{
				"a": {ID: "a", Minted: tc.stored, Settled: 40, Pending: 30, Locked: 5},
			})
			s := standinStore(t, path)

			if err := s.RaiseMinted(ctx, "a", tc.scanned); err != nil {
				t.Fatalf("RaiseMinted: %v", err)
			}
			accts, err := s.Load(ctx)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			a := accts["a"]
			if a.Minted != tc.want {
				t.Errorf("minted = %d, want %d", a.Minted, tc.want)
			}
			// The scan must not disturb the counters a concurrent reserve or
			// settle is holding.
			if a.Settled != 40 || a.Pending != 30 || a.Locked != 5 {
				t.Errorf("settled/pending/locked = %d/%d/%d, want 40/30/5", a.Settled, a.Pending, a.Locked)
			}
		})
	}
}

// TestSetMintedStillAdoptsALowerChainFigure is the counterweight the previous
// test needs: the settler's write must keep being able to lower the row, since
// the chain program is the authority there. Splitting the two intents (rather
// than making every minted write monotonic) is what keeps that behavior, and
// this pins it so a later "just make it all monotonic" edit fails loudly.
func TestSetMintedStillAdoptsALowerChainFigure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	seedAccounts(t, path, map[string]meter.Account{
		"a": {ID: "a", Minted: 1000},
	})
	s := standinStore(t, path)

	if err := s.SetMinted(ctx, "a", 400); err != nil {
		t.Fatalf("SetMinted: %v", err)
	}
	accts, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := accts["a"].Minted; got != 400 {
		t.Errorf("minted = %d, want the adopted chain figure 400", got)
	}
}
