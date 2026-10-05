package solana

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	system "github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
)

// TestLiveLedgerMarket drives the escrow market through a whole trade on a real
// cluster: a seller escrows credits, a buyer pays USDC and takes them in one
// atomic transaction, and the program refuses every way of moving the escrow
// twice.
//
// This is the test that makes "the chain enforces the market" more than a
// claim. The seller never signs the sale - the escrow the program holds stands
// in for that - so the assertions here are the ones that matter: the listed
// credits leave the seller's spendable balance, the buyer's USDC reaches the
// seller in the same transaction that moves the credits, and an already-sold
// listing cannot be bought or cancelled again.
//
// Opt-in, and it needs a deployed, initialised program whose authority is the
// funded key:
//
//	SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
//	    SLOTFARE_LEDGER_PROGRAM=<program id> SLOTFARE_SMOKE_KEY=<funded key> \
//	    go test ./internal/solana -run LiveLedgerMarket -v
func TestLiveLedgerMarket(t *testing.T) {
	if os.Getenv("SLOTFARE_LIVE") == "" {
		t.Skip("set SLOTFARE_LIVE=1 to run against a cluster")
	}
	programID := os.Getenv("SLOTFARE_LEDGER_PROGRAM")
	if programID == "" {
		t.Skip("set SLOTFARE_LEDGER_PROGRAM to a deployed, initialised slotfare-ledger")
	}
	rpcURL := os.Getenv("SLOTFARE_SMOKE_RPC")
	if rpcURL == "" {
		rpcURL = "https://api.devnet.solana.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	seller := livePayer(t, ctx, rpcURL)
	client, err := NewClient(Config{RPC: rpcURL, FeePayer: seller.String(), Cluster: "localnet", LedgerProgram: programID}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !client.LedgerEnabled() {
		t.Fatal("a program id must enable the ledger surface")
	}
	config, ok, err := client.LedgerConfigRead(ctx)
	if err != nil || !ok {
		t.Fatalf("read config: ok=%v err=%v", ok, err)
	}
	if config.Authority != seller.PublicKey() {
		t.Skipf("the test key %s is not the config authority %s", seller.PublicKey(), config.Authority)
	}

	// The buyer is a fresh key, funded so it can pay fees and hold a token
	// account. The seller is the authority, so one key can mint the credits
	// and a second can be the buyer the program requires to differ.
	buyer, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("buyer key: %v", err)
	}
	if _, err := client.rpc.RequestAirdrop(ctx, buyer.PublicKey(), 5*solana.LAMPORTS_PER_SOL, rpc.CommitmentConfirmed); err != nil {
		t.Skipf("airdrop unavailable (%v); the market test needs a fundable buyer", err)
	}
	mustWaitFor(t, 45*time.Second, func() bool {
		bal, err := client.rpc.GetBalance(ctx, buyer.PublicKey(), rpc.CommitmentConfirmed)
		return err == nil && bal.Value > 0
	}, "the buyer's airdrop to confirm")

	// A fresh mint, so the test never depends on the deployment's own USDC.
	const decimals = 6
	mint := createTestMint(t, ctx, client, seller, decimals)
	buyerATA := mustATASync(t, buyer.PublicKey(), mint)
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller, buyer}, []solana.Instruction{
		associatedtokenaccount.NewCreateIdempotentInstruction(seller.PublicKey(), buyer.PublicKey(), mint).Build(),
		token.NewMintToInstruction(1_000_000_000, mint, buyerATA, seller.PublicKey(), nil).Build(),
	}); err != nil {
		t.Fatalf("fund the buyer's token account: %v", err)
	}
	mustWaitFor(t, 20*time.Second, func() bool {
		return tokenAmount(ctx, client, buyerATA) >= 1_000_000_000
	}, "the buyer's USDC to land")

	// Credit accounts for both sides, and a spendable balance the seller can
	// escrow. The credits stay whole on chain, exactly as the settler spends.
	for _, owner := range []solana.PublicKey{seller.PublicKey(), buyer.PublicKey()} {
		if _, _, err := client.LedgerEnsureCredit(ctx, owner); err != nil {
			t.Fatalf("open credit account for %s: %v", owner, err)
		}
		// A just-created account is not readable at `confirmed` immediately, so
		// wait before any read that assumes it exists - the same landing delay
		// the ledger live test documents.
		mustWaitFor(t, 30*time.Second, func() bool {
			_, ok, err := client.LedgerAccount(ctx, owner)
			return err == nil && ok
		}, "the credit account to become readable")
	}
	start := readLedger(t, ctx, client, seller.PublicKey())
	target := start.Minted + 100
	if _, _, err := client.LedgerReconcileMint(ctx, seller.PublicKey(), target); err != nil {
		t.Fatalf("mint credits to the seller: %v", err)
	}
	// Wait for the mint itself, not merely for "spendable >= 100": a balance
	// left over from an earlier run would satisfy the looser check before this
	// mint lands, and then the mint would land mid-assertion and break the
	// exact delta the list is supposed to produce.
	mustWaitFor(t, 30*time.Second, func() bool {
		a, ok, err := client.LedgerAccount(ctx, seller.PublicKey())
		return err == nil && ok && a.Minted >= target
	}, "the seller's mint to land")
	before := readLedger(t, ctx, client, seller.PublicKey())
	buyerBefore := readLedger(t, ctx, client, buyer.PublicKey())

	const credits = 5
	const price = 1_000_000 // 1 USDC, six decimals
	nonce := randomNonce(t)

	// --- list: the credits leave the seller's spendable balance ------------
	listIx, err := client.LedgerListIx(seller.PublicKey(), credits, price, nonce, mint)
	if err != nil {
		t.Fatalf("list ix: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, []solana.Instruction{listIx}); err != nil {
		t.Fatalf("list: %v", err)
	}
	var listing LedgerListing
	mustWaitFor(t, 30*time.Second, func() bool {
		l, ok, err := client.LedgerListingRead(ctx, seller.PublicKey(), nonce)
		if err != nil || !ok {
			return false
		}
		listing = l
		return true
	}, "the listing to appear on chain")
	if !listing.Open() {
		t.Fatalf("a fresh listing must be open, got state %d", listing.State)
	}
	if listing.Seller != seller.PublicKey() || listing.Credits != credits || listing.Price != price || listing.PayMint != mint {
		t.Fatalf("listing terms are wrong: %+v", listing)
	}
	afterList := waitForLedger(t, ctx, client, seller.PublicKey(), func(a LedgerAccount) bool {
		return a.Spendable() == before.Spendable()-credits
	}, "the escrow to remove exactly the listed credits from spendable")
	if afterList.Spent != before.Spent+credits {
		t.Fatalf("seller spent after list = %d, want %d", afterList.Spent, before.Spent+credits)
	}

	// --- buy: one transaction pays the seller and takes the credits --------
	buyIxs, err := client.LedgerBuyIx(buyer.PublicKey(), seller.PublicKey(), nonce, mint)
	if err != nil {
		t.Fatalf("buy ix: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{buyer}, buyIxs); err != nil {
		t.Fatalf("buy: %v", err)
	}
	waitForListing(t, ctx, client, seller.PublicKey(), nonce, func(l LedgerListing) bool {
		return l.State == LedgerListingSold && l.Buyer == buyer.PublicKey()
	}, "the sale to land")
	got := waitForLedger(t, ctx, client, buyer.PublicKey(), func(a LedgerAccount) bool {
		return a.Spendable() >= buyerBefore.Spendable()+credits
	}, "the buyer to receive the credits")
	if got.Spendable() != buyerBefore.Spendable()+credits {
		t.Fatalf("buyer spendable after buy = %d, want %d", got.Spendable(), buyerBefore.Spendable()+credits)
	}
	mustWaitFor(t, 30*time.Second, func() bool {
		return tokenAmount(ctx, client, buyerATA) == 1_000_000_000-price
	}, "the buyer's USDC to reach the seller")
	// The seller's spendable must not have moved again: the credits were
	// already out, and the sale paid in USDC, not credits.
	afterSale := readLedger(t, ctx, client, seller.PublicKey())
	if afterSale.Spendable() != afterList.Spendable() {
		t.Fatalf("seller spendable changed across the sale: %d -> %d", afterList.Spendable(), afterSale.Spendable())
	}

	// --- the escrow moves once --------------------------------------------
	// Buying a sold listing has no escrow behind it.
	if _, err := trySend(ctx, client, []solana.PrivateKey{buyer}, buyIxs); err == nil {
		t.Error("a sold listing must not be buyable twice")
	} else if code := programErrorCode(err); code != 106 {
		t.Errorf("second buy error = %v (custom %d), want custom 106", err, code)
	}
	// And a sold listing cannot be withdrawn either.
	cancelIx, err := client.LedgerCancelListIx(seller.PublicKey(), nonce)
	if err != nil {
		t.Fatalf("cancel ix: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, []solana.Instruction{cancelIx}); err == nil {
		t.Error("a sold listing must not be cancellable")
	} else if code := programErrorCode(err); code != 106 {
		t.Errorf("cancel-after-sale error = %v (custom %d), want custom 106", err, code)
	}

	// --- cancel: a fresh listing returns its escrow -------------------------
	cancelNonce := nonce + 1
	listIx2, err := client.LedgerListIx(seller.PublicKey(), credits, price, cancelNonce, mint)
	if err != nil {
		t.Fatalf("list ix 2: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, []solana.Instruction{listIx2}); err != nil {
		t.Fatalf("list 2: %v", err)
	}
	waitForListing(t, ctx, client, seller.PublicKey(), cancelNonce, func(l LedgerListing) bool { return l.Open() }, "the second listing to open")
	// Anchor on what this listing's own escrow took, not on the pre-first-list
	// figure: opening the second listing deducts its credits too, so the cancel
	// returns the seller to this listing's starting point, not the first one's.
	escrowed := waitForLedger(t, ctx, client, seller.PublicKey(), func(a LedgerAccount) bool {
		return a.Spendable() == afterSale.Spendable()-credits
	}, "the second escrow to take its credits")
	cancelIx2, err := client.LedgerCancelListIx(seller.PublicKey(), cancelNonce)
	if err != nil {
		t.Fatalf("cancel ix 2: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, []solana.Instruction{cancelIx2}); err != nil {
		t.Fatalf("cancel 2: %v", err)
	}
	waitForListing(t, ctx, client, seller.PublicKey(), cancelNonce, func(l LedgerListing) bool {
		return l.State == LedgerListingCancelled
	}, "the cancellation to land")
	restored := waitForLedger(t, ctx, client, seller.PublicKey(), func(a LedgerAccount) bool {
		return a.Spendable() == escrowed.Spendable()+credits
	}, "the escrow to return to the seller")
	if restored.Spendable() != escrowed.Spendable()+credits {
		t.Fatalf("seller spendable after cancel = %d, want %d", restored.Spendable(), escrowed.Spendable()+credits)
	}

	// --- a seller cannot buy its own listing -------------------------------
	selfNonce := nonce + 2
	listIx3, err := client.LedgerListIx(seller.PublicKey(), credits, price, selfNonce, mint)
	if err != nil {
		t.Fatalf("list ix 3: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, []solana.Instruction{listIx3}); err != nil {
		t.Fatalf("list 3: %v", err)
	}
	waitForListing(t, ctx, client, seller.PublicKey(), selfNonce, func(l LedgerListing) bool { return l.Open() }, "the third listing to open")
	selfIxs, err := client.LedgerBuyIx(seller.PublicKey(), seller.PublicKey(), selfNonce, mint)
	if err != nil {
		t.Fatalf("self-buy ix: %v", err)
	}
	if _, err := trySend(ctx, client, []solana.PrivateKey{seller}, selfIxs); err == nil {
		t.Error("a seller must not buy its own listing")
	} else if code := programErrorCode(err); code != 108 {
		t.Errorf("self-buy error = %v (custom %d), want custom 108", err, code)
	}
}

// --- helpers ------------------------------------------------------------

// trySend builds, signs with every signer and submits one transaction. It
// returns the RPC error rather than failing, because the rejection tests need
// the program's own error code.
func trySend(ctx context.Context, c *Client, signers []solana.PrivateKey, ixs []solana.Instruction) (solana.Signature, error) {
	bh, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return solana.Signature{}, err
	}
	payer := signers[0].PublicKey()
	tx, err := solana.NewTransaction(ixs, bh.Value.Blockhash, solana.TransactionPayer(payer))
	if err != nil {
		return solana.Signature{}, err
	}
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		for i := range signers {
			if signers[i].PublicKey().Equals(k) {
				return &signers[i]
			}
		}
		return nil
	}); err != nil {
		return solana.Signature{}, err
	}
	return c.rpc.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
}

// createTestMint creates and initialises a fresh SPL mint owned by `authority`.
func createTestMint(t *testing.T, ctx context.Context, c *Client, authority solana.PrivateKey, decimals uint8) solana.PublicKey {
	t.Helper()
	mintWallet, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	rent, err := c.rpc.GetMinimumBalanceForRentExemption(ctx, token.MINT_SIZE, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatalf("mint rent: %v", err)
	}
	_, err = trySend(ctx, c, []solana.PrivateKey{authority, mintWallet}, []solana.Instruction{
		system.NewCreateAccountInstruction(rent, token.MINT_SIZE, solana.TokenProgramID, authority.PublicKey(), mintWallet.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(decimals, authority.PublicKey(), authority.PublicKey(), mintWallet.PublicKey()).Build(),
	})
	if err != nil {
		t.Fatalf("create mint: %v", err)
	}
	mustWaitFor(t, 20*time.Second, func() bool {
		_, err := c.rpc.GetAccountInfo(ctx, mintWallet.PublicKey())
		return err == nil
	}, "the mint to appear")
	return mintWallet.PublicKey()
}

// mustATASync derives an owner's associated token account for a mint.
func mustATASync(t *testing.T, owner, mint solana.PublicKey) solana.PublicKey {
	t.Helper()
	ata, _, err := solana.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		t.Fatalf("derive ATA: %v", err)
	}
	return ata
}

// tokenAmount reads an SPL token account's amount (offset 64, little-endian),
// or 0 when the account does not exist yet.
func tokenAmount(ctx context.Context, c *Client, ata solana.PublicKey) uint64 {
	res, err := c.rpc.GetAccountInfo(ctx, ata)
	if err != nil || res == nil || res.Value == nil {
		return 0
	}
	data := res.Value.Data.GetBinary()
	if len(data) < 72 {
		return 0
	}
	return binary.LittleEndian.Uint64(data[64:72])
}

// waitForListing polls a listing until `done` holds.
func waitForListing(t *testing.T, ctx context.Context, c *Client, seller solana.PublicKey, nonce uint64, done func(LedgerListing) bool, what string) {
	t.Helper()
	mustWaitFor(t, 30*time.Second, func() bool {
		l, ok, err := c.LedgerListingRead(ctx, seller, nonce)
		return err == nil && ok && done(l)
	}, what)
}

// randomNonce picks a listing nonce so a rerun against a cluster that already
// holds this key's listings cannot collide with an earlier one.
func randomNonce(t *testing.T) uint64 {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	return binary.LittleEndian.Uint64(b[:])
}
