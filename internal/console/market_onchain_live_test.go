package console

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	gsolana "github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	system "github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"

	"slotfare/internal/solana"
)

// TestLiveOnchainMarketRoutes drives the console's market through the exact
// interface the browser uses: `prepare` hands back an unsigned transaction,
// a wallet signs it, and `confirm` records what the program then holds. No
// handler is called out of turn and no on-chain fact is asserted that the
// console did not read back for itself, so what passes here is the round trip
// the sell and buy screens perform.
//
// The wallet is a local key rather than Phantom; everything else - the
// base64 wire transaction, the signature, the broadcast, the confirm - is the
// real path. The test also proves the negative the UI depends on: a listing
// the program has already moved cannot be confirmed a second time.
//
// Opt-in, and it needs a deployed, initialised program whose authority is the
// funded key:
//
//	SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
//	    SLOTFARE_LEDGER_PROGRAM=<program id> SLOTFARE_SMOKE_KEY=<base58 key> \
//	    go test ./internal/console -run LiveOnchainMarketRoutes -v
func TestLiveOnchainMarketRoutes(t *testing.T) {
	if os.Getenv("SLOTFARE_LIVE") == "" {
		t.Skip("set SLOTFARE_LIVE=1 to run against a cluster")
	}
	programID := os.Getenv("SLOTFARE_LEDGER_PROGRAM")
	if programID == "" {
		t.Skip("set SLOTFARE_LEDGER_PROGRAM to a deployed, initialised slotfare-ledger")
	}
	secret := os.Getenv("SLOTFARE_SMOKE_KEY")
	if secret == "" {
		t.Skip("set SLOTFARE_SMOKE_KEY to a funded base58 private key")
	}
	rpcURL := os.Getenv("SLOTFARE_SMOKE_RPC")
	if rpcURL == "" {
		rpcURL = "https://api.devnet.solana.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	seller, err := gsolana.PrivateKeyFromBase58(secret)
	if err != nil {
		t.Fatalf("parse SLOTFARE_SMOKE_KEY: %v", err)
	}
	client, err := solana.NewClient(solana.Config{
		RPC: rpcURL, FeePayer: secret, Cluster: "localnet", LedgerProgram: programID,
	}, nil)
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

	// The buyer is a fresh key: the program refuses a self-sale, so the two
	// sides of the trade must be different wallets.
	buyer, err := gsolana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("buyer key: %v", err)
	}
	if _, err := client.RPC().RequestAirdrop(ctx, buyer.PublicKey(), 5*gsolana.LAMPORTS_PER_SOL, rpc.CommitmentConfirmed); err != nil {
		t.Skipf("airdrop unavailable (%v); the market test needs a fundable buyer", err)
	}
	waitLive(t, 45*time.Second, func() bool {
		bal, err := client.RPC().GetBalance(ctx, buyer.PublicKey(), rpc.CommitmentConfirmed)
		return err == nil && bal.Value > 0
	}, "the buyer's airdrop to confirm")

	// A fresh mint, so the console's configured USDC is never required to
	// exist on the cluster under test.
	const decimals = 6
	mint := liveMint(t, ctx, client, seller, decimals)
	buyerATA, _, err := gsolana.FindAssociatedTokenAddress(buyer.PublicKey(), mint)
	if err != nil {
		t.Fatalf("buyer ATA: %v", err)
	}
	liveSend(t, ctx, client, []gsolana.PrivateKey{seller, buyer}, []gsolana.Instruction{
		associatedtokenaccount.NewCreateIdempotentInstruction(seller.PublicKey(), buyer.PublicKey(), mint).Build(),
		token.NewMintToInstruction(1_000_000_000, mint, buyerATA, seller.PublicKey(), nil).Build(),
	})
	waitLive(t, 30*time.Second, func() bool { return liveTokenAmount(ctx, client, buyerATA) > 0 },
		"the buyer's USDC to land")

	// Credit accounts for both sides, and a spendable position for the seller.
	for _, owner := range []gsolana.PublicKey{seller.PublicKey(), buyer.PublicKey()} {
		if _, _, err := client.LedgerEnsureCredit(ctx, owner); err != nil {
			t.Fatalf("open credit account for %s: %v", owner, err)
		}
		waitLive(t, 30*time.Second, func() bool {
			_, ok, err := client.LedgerAccount(ctx, owner)
			return err == nil && ok
		}, "the credit account to become readable")
	}
	start, _, err := client.LedgerAccount(ctx, seller.PublicKey())
	if err != nil {
		t.Fatalf("read seller credit: %v", err)
	}
	target := start.Minted + 100
	if _, _, err := client.LedgerReconcileMint(ctx, seller.PublicKey(), target); err != nil {
		t.Fatalf("mint credits to the seller: %v", err)
	}
	waitLive(t, 30*time.Second, func() bool {
		a, ok, err := client.LedgerAccount(ctx, seller.PublicKey())
		return err == nil && ok && a.Minted >= target
	}, "the seller's mint to land")

	// ---- the console, wired exactly as slotfared wires it -----------------
	env := newMarketWriteEnv(t)
	h := onchainMarketHandler(t, env, client, mint.String())
	sellerID := solana.AccountIDFor(seller.PublicKey())
	buyerID := solana.AccountIDFor(buyer.PublicKey())
	sellerToken := consoleTokenFor(t, h, sellerID)
	buyerToken := consoleTokenFor(t, h, buyerID)

	const credits = 7
	const price = 1_000_000 // 1 USDC at six decimals

	// ---- sell: prepare, sign, send, confirm -------------------------------
	prepared := livePrepare(t, h, sellerToken, map[string]any{
		"action": "list", "credits": credits, "price": price,
	})
	listSig := liveSignSend(t, ctx, client, prepared.Transaction, seller)
	code, row, body := liveConfirm(t, h, sellerToken, map[string]any{
		"action": "list", "nonce": prepared.Nonce, "signature": listSig.String(),
	})
	if code != http.StatusCreated {
		t.Fatalf("confirm list = %d, want 201; body: %s", code, body)
	}
	if row.Credits != credits || row.Price != price || row.State != "open" {
		t.Fatalf("row = %+v, want an open row for %d credits at %d", row, credits, price)
	}
	// The row's terms came from the listing account, not from the request.
	if row.Seller != seller.PublicKey().String() {
		t.Errorf("row seller = %q, want %q", row.Seller, seller.PublicKey())
	}

	// ---- confirm is not a way to invent an escrow -------------------------
	code, _, _ = liveConfirm(t, h, sellerToken, map[string]any{
		"action": "list", "nonce": prepared.Nonce ^ 0xdeadbeef, "signature": listSig.String(),
	})
	if code == http.StatusCreated {
		t.Error("a confirm for a nonce the program never opened must not create a row")
	}

	// ---- buy: prepare, sign, send, confirm --------------------------------
	buyPrepared := livePrepare(t, h, buyerToken, map[string]any{
		"action": "buy", "listingId": row.ListingID,
	})
	buySig := liveSignSend(t, ctx, client, buyPrepared.Transaction, buyer)
	code, sold, body := liveConfirm(t, h, buyerToken, map[string]any{
		"action": "buy", "listingId": row.ListingID, "nonce": buyPrepared.Nonce,
		"signature": buySig.String(),
	})
	if code != http.StatusOK {
		t.Fatalf("confirm buy = %d, want 200; body: %s", code, body)
	}
	if sold.State != "sold" || sold.BuyerAccount != buyerID {
		t.Errorf("sold row = %+v, want state sold to %s", sold, buyerID)
	}
	if sold.SoldBlock == 0 {
		t.Error("sold block = 0, want the slot the sale landed in")
	}

	// The seller's payment landed: the buyer's USDC fell by the price.
	waitLive(t, 20*time.Second, func() bool {
		return liveTokenAmount(ctx, client, buyerATA) <= 1_000_000_000-price
	}, "the buyer's payment to leave their token account")

	// ---- the sold escrow cannot be taken again ----------------------------
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/prepare", buyerToken,
		[]byte(`{"action":"buy","listingId":`+itoa(uint64(row.ListingID))+`}`))
	if rec.Code != http.StatusConflict {
		t.Errorf("prepare buy of a sold listing = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
}

// livePrepare posts to prepare and returns the decoded transaction, failing
// the test on anything but a 200. A prepare that is never signed leaves no
// trace, so it is safe to call repeatedly.
func livePrepare(t *testing.T, h *Handler, token string, body map[string]any) preparedTxView {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal prepare body: %v", err)
	}
	rec := doRequest(t, h, http.MethodPost, "/listings/onchain/prepare", token, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare %v = %d; body: %s", body, rec.Code, rec.Body.String())
	}
	var out preparedTxView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("prepare body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if out.Transaction == "" {
		t.Fatalf("prepare %v returned no transaction", body)
	}
	return out
}

// liveConfirm posts to confirm and returns the status, the row and the body.
func liveConfirm(t *testing.T, h *Handler, token string, body map[string]any) (int, listingView, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal confirm body: %v", err)
	}
	return confirmMarket(t, h, token, string(raw))
}

// liveSignSend signs a console-prepared transaction with a wallet key and
// submits it: the browser's half of the round trip, with a local key standing
// in for the wallet popup. The daemon never sees this key.
func liveSignSend(t *testing.T, ctx context.Context, c *solana.Client, prepared string, wallet gsolana.PrivateKey) gsolana.Signature {
	t.Helper()
	tx, err := gsolana.TransactionFromBase64(prepared)
	if err != nil {
		t.Fatalf("decode the console's prepared transaction: %v", err)
	}
	// Account zero is the fee payer, which the console names as the caller.
	if len(tx.Message.AccountKeys) == 0 || !tx.Message.AccountKeys[0].Equals(wallet.PublicKey()) {
		t.Fatalf("prepared fee payer = %v, want the wallet %s", tx.Message.AccountKeys, wallet.PublicKey())
	}
	if _, err := tx.Sign(func(key gsolana.PublicKey) *gsolana.PrivateKey {
		if key.Equals(wallet.PublicKey()) {
			return &wallet
		}
		return nil
	}); err != nil {
		t.Fatalf("sign the prepared transaction: %v", err)
	}
	sig, err := c.RPC().SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		t.Fatalf("send the signed transaction: %v", err)
	}
	liveAwait(t, ctx, c, sig)
	return sig
}

// liveSend builds, signs and submits a transaction with the test's own keys,
// for the chain fixtures the market test needs (a mint and its accounts).
func liveSend(t *testing.T, ctx context.Context, c *solana.Client, signers []gsolana.PrivateKey, ixs []gsolana.Instruction) gsolana.Signature {
	t.Helper()
	bh, err := c.RPC().GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatalf("latest blockhash: %v", err)
	}
	tx, err := gsolana.NewTransaction(ixs, bh.Value.Blockhash, gsolana.TransactionPayer(signers[0].PublicKey()))
	if err != nil {
		t.Fatalf("build transaction: %v", err)
	}
	if _, err := tx.Sign(func(key gsolana.PublicKey) *gsolana.PrivateKey {
		for i := range signers {
			if signers[i].PublicKey().Equals(key) {
				return &signers[i]
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("sign transaction: %v", err)
	}
	sig, err := c.RPC().SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		t.Fatalf("send transaction: %v", err)
	}
	liveAwait(t, ctx, c, sig)
	return sig
}

// liveAwait polls a signature until the cluster confirms it, failing the test
// if the transaction itself failed on chain.
func liveAwait(t *testing.T, ctx context.Context, c *solana.Client, sig gsolana.Signature) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		st, err := c.RPC().GetSignatureStatuses(ctx, true, sig)
		if err == nil && st != nil && len(st.Value) > 0 && st.Value[0] != nil {
			if st.Value[0].Err != nil {
				t.Fatalf("transaction %s failed on chain: %v", sig, st.Value[0].Err)
			}
			s := st.Value[0].ConfirmationStatus
			if s == rpc.ConfirmationStatusConfirmed || s == rpc.ConfirmationStatusFinalized {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to confirm", sig)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// liveMint creates a fresh SPL mint owned by the payer.
func liveMint(t *testing.T, ctx context.Context, c *solana.Client, payer gsolana.PrivateKey, decimals uint8) gsolana.PublicKey {
	t.Helper()
	mintKey, err := gsolana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("mint key: %v", err)
	}
	rent, err := c.RPC().GetMinimumBalanceForRentExemption(ctx, token.MINT_SIZE, rpc.CommitmentConfirmed)
	if err != nil {
		t.Fatalf("rent for a mint: %v", err)
	}
	liveSend(t, ctx, c, []gsolana.PrivateKey{payer, mintKey}, []gsolana.Instruction{
		system.NewCreateAccountInstruction(rent, token.MINT_SIZE, gsolana.TokenProgramID, payer.PublicKey(), mintKey.PublicKey()).Build(),
		token.NewInitializeMint2Instruction(decimals, payer.PublicKey(), payer.PublicKey(), mintKey.PublicKey()).Build(),
	})
	return mintKey.PublicKey()
}

// liveTokenAmount reads an SPL token account's amount, the same offset the
// settlement chain's own tests read.
func liveTokenAmount(ctx context.Context, c *solana.Client, account gsolana.PublicKey) uint64 {
	info, err := c.RPC().GetAccountInfo(ctx, account)
	if err != nil || info == nil || info.Value == nil {
		return 0
	}
	raw := info.Value.Data.GetBinary()
	if len(raw) < 72 {
		return 0
	}
	return binary.LittleEndian.Uint64(raw[64:72])
}

// waitLive polls until cond holds, failing the test at the deadline. The
// predicate runs on the test's own goroutine, so it may fail the test.
func waitLive(t *testing.T, budget time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
