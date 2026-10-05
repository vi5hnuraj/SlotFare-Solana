package console

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"slotfare/internal/meter"
	"slotfare/internal/solana"
)

// The accounting-separation scenario from the audit: Alice deposits 10,000
// credits, lists 5,000, Bob buys 5,000 - and nothing about Bob's purchase
// may claim Bob deposited 5,000 USDC.
//
// In ledger terms deposits ride the Minted axis and market activity rides
// the signed Adjust axis, so the audit's invariant is:
//
//	Alice: Minted up 10,000 (deposit), Adjust down 5,000 (sale).
//	Bob:   Minted unchanged, Adjust up 5,000 (acquired).
//
// If the pay route ever wrote Bob's gain into Minted - the bug class the
// audit flagged, where "minted" conflates deposit and receive - a later
// deposit rescan would either adopt the inflated figure as "deposits" or
// clash with RaiseMinted's monotonic guard. This test pins the split at the
// pay route, so it fails the moment accounting moves through the deposit
// rail again.
//
// On chain the same distinction holds structurally: state.rs `receive()` is
// `credit()`, so `minted` there counts credits acquired, while the deposit
// rail is identified by the treasury signer authority at the processor
// (programs/slotfare-ledger/src/processor.rs). The console never reads the
// on-chain minted figure into the local Minted axis for acquired credits -
// the indexer reads them into Adjust instead (cmd/slotfared/indexer).
func TestPayListingKeepsDepositedAndReceivedCreditsSeparate(t *testing.T) {
	env := newMarketWriteEnv(t)
	fp := &fakePayment{info: solana.PaymentInfo{
		Signature: "5AliceBobSig",
		Slot:      7,
		BlockTime: marketWriteClock.Add(-time.Minute),
		Amount:    1,
	}}
	h := marketWriteHandler(t, env, fp)

	micro := uint64(5_000) * meter.MicroCreditsPerCredit
	alice := testSolanaAccount(t)
	bob := testSolanaAccount(t)

	// Alice deposits 10,000 credits (the deposit rail's Minted write).
	env.ledger.RaiseMinted(alice, 10_000*meter.MicroCreditsPerCredit)

	// Alice lists 5,000.
	l := createListing(t, h, alice, 5_000, 1)

	// Bob pays and buys 5,000.
	rec := doRequest(t, h, http.MethodPost, fmt.Sprintf("/listings/%d/pay", l.ListingID),
		consoleTokenFor(t, h, bob), []byte(`{"signature":"5AliceBobSig"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("pay status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// The core check: Bob's Minted is untouched - he did not deposit 5,000
	// USDC. His gained credits are Adjust, not Minted.
	if got := env.ledger.Minted(bob); got != 0 {
		t.Errorf("bob Minted = %d, want 0 (a purchase is not a deposit)", got)
	}
	if got := env.ledger.Balance(bob); got != micro {
		t.Errorf("bob spendable = %d, want %d (the purchased credits)", got, micro)
	}

	// Alice: her deposit figure survives the sale exactly as minted, and
	// the sale is a negative Adjust on top.
	if got := env.ledger.Minted(alice); got != 10_000*meter.MicroCreditsPerCredit {
		t.Errorf("alice Minted = %d, want the untouched deposit total", got)
	}
	snap := env.ledger.Snapshot()[alice]
	if snap.Adjust != -int64(micro) {
		t.Errorf("alice Adjust = %d, want %d (the sale released 5,000 credits)", snap.Adjust, -int64(micro))
	}

	// Bob's snapshot: a positive Adjust is the only trace of the purchase.
	bsnap := env.ledger.Snapshot()[bob]
	if bsnap.Adjust != int64(micro) {
		t.Errorf("bob Adjust = %d, want %d (market acquisition, not deposit)", bsnap.Adjust, int64(micro))
	}
	if bsnap.Minted != 0 || bsnap.Settled != 0 || bsnap.Pending != 0 {
		t.Errorf("bob axes = %+v, want only Adjust nonzero", bsnap)
	}

	// Conservation: total owned credits are the same before and after -
	// a transfer, not a mint.
	aliceOwned := env.ledger.Snapshot()[alice].TotalMinted()
	bobOwned := env.ledger.Snapshot()[bob].TotalMinted()
	if aliceOwned+bobOwned != 10_000*meter.MicroCreditsPerCredit {
		t.Errorf("conservation: alice %d + bob %d != the 10,000 deposited", aliceOwned, bobOwned)
	}
}
