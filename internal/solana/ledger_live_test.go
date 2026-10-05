package solana

import (
	"context"
	"crypto/rand"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// TestLiveLedgerProgram drives the on-chain ledger through its whole lifecycle
// against a real cluster, and asserts on the rejections - because the
// rejections are the point of the program.
//
// Without a program deployed, a burn's rules ("one root per burn", "never spend
// past what you prepaid", "locked credits are not settleable") are promises our
// Go code keeps. With it, they are the chain's rules, and the only way to know
// that is to ask the chain. So this test replays a committed root, overspends,
// and rewinds the usage frontier, and requires the cluster to say no each time.
//
// Opt-in, and it needs a deployed and initialised program:
//
//	SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
//	    SLOTFARE_LEDGER_PROGRAM=<program id> SLOTFARE_SMOKE_KEY=<funded key> \
//	    go test ./internal/solana -run LiveLedger -v
//
// `slotfared ledger-init` performs the deploy-side half (docs/onchain-ledger.md).
// The key must be the program's config authority, because minting and settling
// are restricted to it; the test skips rather than fails when it is not.
func TestLiveLedgerProgram(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	payer := livePayer(t, ctx, rpcURL)
	client, err := NewClient(Config{RPC: rpcURL, FeePayer: payer.String(), Cluster: "localnet", LedgerProgram: programID}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !client.LedgerEnabled() {
		t.Fatal("a program id must enable the ledger surface")
	}

	config, ok, err := client.LedgerConfigRead(ctx)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !ok {
		t.Skip("the program has no config account; run `slotfared ledger-init` first")
	}
	owner := payer.PublicKey()
	if config.Authority != owner {
		t.Skipf("the test key %s is not the config authority %s", owner, config.Authority)
	}
	t.Logf("program %s, authority %s, treasury %s", programID, config.Authority, config.Treasury)

	// The owner's own account, so one key can sign as both authority and owner
	// and the lock, unlock and transfer rules can be exercised too.
	if _, created, err := client.LedgerEnsureCredit(ctx, owner); err != nil {
		t.Fatalf("open credit account: %v", err)
	} else if created {
		t.Logf("opened a credit account for %s", owner)
	}
	// Every step below reads the account's live state and asserts a delta or
	// an absolute it just derived, so a rerun against a cluster that already
	// holds this account's history still tests the same rules.
	start := readLedger(t, ctx, client, owner)

	// --- mint is authority-gated and absolute -------------------------------
	target := start.Minted + 2500
	sig, submitted, err := client.LedgerReconcileMint(ctx, owner, target)
	if err != nil {
		t.Fatalf("reconcile mint: %v", err)
	}
	if !submitted {
		t.Fatal("a reconcile to a higher total must submit")
	}
	t.Logf("mint tx: https://explorer.solana.com/tx/%s", sig)
	acct := waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Minted >= target }, "the mint to land")
	if acct.Minted != target {
		t.Fatalf("minted = %d, want exactly %d", acct.Minted, target)
	}
	if _, submitted, err := client.LedgerReconcileMint(ctx, owner, target); err != nil || submitted {
		t.Fatalf("a reconcile to the same total must be a no-op: submitted=%v err=%v", submitted, err)
	}

	// --- lock and unlock ----------------------------------------------------
	// Clear anything a previous run left locked, so the assertions below are
	// about this run.
	if acct.Locked > 0 {
		if _, err := client.LedgerUnlock(ctx, owner, acct.Locked); err != nil {
			t.Fatalf("unlock leftover: %v", err)
		}
		waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Locked == 0 }, "the leftover unlock to land")
	}
	unlocked := readLedger(t, ctx, client, owner)
	if sig, err := client.LedgerLock(ctx, owner, 500); err != nil {
		t.Fatalf("lock: %v", err)
	} else {
		t.Logf("lock tx: %s", sig)
	}
	acct = waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Locked == 500 }, "the lock to land")
	// A lock must remove exactly its own amount from spendable, and nothing
	// else: locked credits sit outside the settleable figure.
	if want := unlocked.Spendable() - 500; acct.Spendable() != want {
		t.Fatalf("spendable with 500 locked = %d, want %d", acct.Spendable(), want)
	}
	if _, err := client.LedgerUnlock(ctx, owner, 500); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if a := waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Locked == 0 }, "the unlock to land"); a.Spendable() != unlocked.Spendable() {
		t.Fatalf("cancel returned %d spendable, want %d back", a.Spendable(), unlocked.Spendable())
	}

	// --- commit and settle --------------------------------------------------
	// A random root per run: the program rejects a root it has seen, so a fixed
	// one would make the second run of this test fail for the wrong reason.
	var root [32]byte
	if _, err := rand.Read(root[:]); err != nil {
		t.Fatalf("random root: %v", err)
	}
	before := readLedger(t, ctx, client, owner)
	first := before.LastUsageID + 1
	if sig, err := client.LedgerCommitAndSettle(ctx, owner, root, first, first+9, 100); err != nil {
		t.Fatalf("commit and settle: %v", err)
	} else {
		t.Logf("settle tx: https://explorer.solana.com/tx/%s", sig)
	}
	acct = waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Spent >= before.Spent+100 }, "the settle to land")
	if acct.Spent != before.Spent+100 || acct.BatchCount != before.BatchCount+1 || acct.LastUsageID != first+9 {
		t.Fatalf("after settle: spent=%d batch=%d lastUsage=%d, want %d/%d/%d",
			acct.Spent, acct.BatchCount, acct.LastUsageID, before.Spent+100, before.BatchCount+1, first+9)
	}
	if acct.LastRoot != root {
		t.Fatalf("last_root = %x, want the committed root %x", acct.LastRoot, root)
	}

	// --- the chain refuses what our code used to just avoid -----------------
	next := acct.LastUsageID + 1

	// One root per burn: replaying the same root, over an advancing range so
	// the root rule is the one under test, must be rejected.
	if _, err := client.LedgerCommitAndSettle(ctx, owner, root, next, next+9, 10); err == nil {
		t.Error("a replayed root must be rejected on chain")
	} else if code := programErrorCode(err); code != 3 {
		t.Errorf("replayed root error = %v (custom code %d), want custom code 3", err, code)
	}

	// Never settle past what was prepaid. A fresh root and an advancing range
	// keep the earlier checks from firing first, so the spendable rule is the
	// one under test.
	var over [32]byte
	if _, err := rand.Read(over[:]); err != nil {
		t.Fatalf("random root: %v", err)
	}
	if _, err := client.LedgerCommitAndSettle(ctx, owner, over, next, next+9, acct.Spendable()+1); err == nil {
		t.Error("settling more than the spendable balance must be rejected on chain")
	} else if code := programErrorCode(err); code != 1 {
		t.Errorf("overspend error = %v (custom code %d), want custom code 1", err, code)
	}

	// The usage frontier only advances: a batch whose first id is not above
	// the settled one is rejected rather than silently rewritten.
	var rewind [32]byte
	if _, err := rand.Read(rewind[:]); err != nil {
		t.Fatalf("random root: %v", err)
	}
	if _, err := client.LedgerCommitAndSettle(ctx, owner, rewind, acct.LastUsageID, acct.LastUsageID+9, 10); err == nil {
		t.Error("a non-advancing usage frontier must be rejected on chain")
	} else if code := programErrorCode(err); code != 4 {
		t.Errorf("rewind error = %v (custom code %d), want custom code 4", err, code)
	}

	// None of the rejected batches may have moved the account.
	final := readLedger(t, ctx, client, owner)
	if final.Spent != acct.Spent || final.BatchCount != acct.BatchCount {
		t.Fatalf("rejected batches changed state: spent=%d batch=%d, want %d/%d",
			final.Spent, final.BatchCount, acct.Spent, acct.BatchCount)
	}

	// --- the market move: transfer spendable credits to another owner -------
	to, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate recipient: %v", err)
	}
	if _, created, err := client.LedgerEnsureCredit(ctx, to.PublicKey()); err != nil {
		t.Fatalf("open recipient account: %v", err)
	} else if !created {
		t.Fatal("the recipient account must have been created by this call")
	}
	// A transfer preflights against the cluster's current state, so the
	// recipient's account must be readable before the transfer names it -
	// otherwise the transfer fails as "account not owned by the program",
	// which is the check doing its job on an account that does not exist yet.
	mustWaitFor(t, 30*time.Second, func() bool {
		_, ok, err := client.LedgerAccount(ctx, to.PublicKey())
		return err == nil && ok
	}, "the recipient account to become readable")

	sender := readLedger(t, ctx, client, owner)
	if _, err := client.LedgerTransfer(ctx, owner, to.PublicKey(), 200); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	waitForLedger(t, ctx, client, owner, func(a LedgerAccount) bool { return a.Spent >= sender.Spent+200 }, "the transfer to land")
	got := waitForLedger(t, ctx, client, to.PublicKey(), func(a LedgerAccount) bool { return a.Minted >= 200 }, "the recipient to be credited")
	if got.Minted != 200 || got.Spendable() != 200 {
		t.Fatalf("recipient = minted %d spendable %d, want 200/200", got.Minted, got.Spendable())
	}

	// Transferring more than the sender has must be rejected, not clamped.
	if _, err := client.LedgerTransfer(ctx, owner, to.PublicKey(), 1<<62); err == nil {
		t.Error("a transfer beyond the spendable balance must be rejected on chain")
	} else if code := programErrorCode(err); code != 1 {
		t.Errorf("over-transfer error = %v (custom code %d), want custom code 1", err, code)
	}
}

// livePayer resolves the funded key the live tests sign with, mirroring the
// memo smoke test so both opt-in suites share one configuration story.
func livePayer(t *testing.T, ctx context.Context, rpcURL string) solana.PrivateKey {
	t.Helper()
	rpcc := rpc.New(rpcURL)
	if wif := os.Getenv("SLOTFARE_SMOKE_KEY"); wif != "" {
		k, err := solana.PrivateKeyFromBase58(wif)
		if err != nil {
			t.Fatalf("SLOTFARE_SMOKE_KEY: %v", err)
		}
		return k
	}
	k, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate fee payer: %v", err)
	}
	if _, err := rpcc.RequestAirdrop(ctx, k.PublicKey(), solana.LAMPORTS_PER_SOL, rpc.CommitmentConfirmed); err != nil {
		t.Skipf("airdrop unavailable (%v); set SLOTFARE_SMOKE_KEY to a funded key", err)
	}
	mustWaitFor(t, 45*time.Second, func() bool {
		bal, err := rpcc.GetBalance(ctx, k.PublicKey(), rpc.CommitmentConfirmed)
		return err == nil && bal.Value > 0
	}, "the airdrop to confirm")
	return k
}

// readLedger reads one credit account, failing the test if it is unreadable:
// every assertion in the live test starts from a real on-chain figure.
func readLedger(t *testing.T, ctx context.Context, c *Client, owner solana.PublicKey) LedgerAccount {
	t.Helper()
	acct, ok, err := c.LedgerAccount(ctx, owner)
	if err != nil || !ok {
		t.Fatalf("read credit account %s: ok=%v err=%v", owner, ok, err)
	}
	return acct
}

// waitForLedger polls one credit account until `done` holds, and returns the
// account it settled on. A settle is only preflighted, not awaited, so every
// read-back in the live test has to tolerate the landing delay.
func waitForLedger(t *testing.T, ctx context.Context, c *Client, owner solana.PublicKey, done func(LedgerAccount) bool, what string) LedgerAccount {
	t.Helper()
	var last LedgerAccount
	mustWaitFor(t, 30*time.Second, func() bool {
		acct, ok, err := c.LedgerAccount(ctx, owner)
		if err != nil || !ok {
			return false
		}
		last = acct
		return done(acct)
	}, what)
	return last
}

// mustWaitFor polls `ready` until it holds or the deadline passes.
func mustWaitFor(t *testing.T, timeout time.Duration, ready func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// programErrorCode extracts the ProgramError::Custom code from an RPC failure.
// A rejected instruction surfaces as "custom program error: 0x3" in the
// simulation log, which is the only place the chain states the reason.
var customCodeRe = regexp.MustCompile(`custom program error: 0x([0-9a-fA-F]+)`)

func programErrorCode(err error) int {
	if err == nil {
		return 0
	}
	m := customCodeRe.FindStringSubmatch(err.Error())
	if m == nil {
		return -1
	}
	n, perr := strconv.ParseInt(m[1], 16, 32)
	if perr != nil {
		return -1
	}
	return int(n)
}
