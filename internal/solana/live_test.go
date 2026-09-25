package solana

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// TestLiveMemoCommit drives a real usage-root commit against Solana devnet:
// it creates an ephemeral fee payer, airdrops devnet SOL, commits a batch
// root through Client.CommitAndSettle, then fetches the transaction back
// and checks the memo body decodes to exactly what was committed.
//
// Opt-in: SLOTFARE_LIVE=1 go test ./internal/solana -run Live -v.
// The default `go test ./...` gate stays fully offline and skips it.
func TestLiveMemoCommit(t *testing.T) {
	if os.Getenv("SLOTFARE_LIVE") == "" {
		t.Skip("set SLOTFARE_LIVE=1 to run against devnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// SLOTFARE_SMOKE_RPC points the smoke test at any cluster; a local
	// solana-test-validator works and never hits a faucet limit.
	rpcURL := os.Getenv("SLOTFARE_SMOKE_RPC")
	if rpcURL == "" {
		rpcURL = "https://api.devnet.solana.com"
	}
	rpcc := rpc.New(rpcURL)

	// Prefer a pre-funded key (SLOTFARE_SMOKE_KEY, base58): the public
	// devnet faucet is frequently dry or rate-limited. Fall back to an
	// ephemeral key plus airdrop; skip when the faucet refuses so the
	// opt-in suite stays usable.
	var payer solana.PrivateKey
	if wif := os.Getenv("SLOTFARE_SMOKE_KEY"); wif != "" {
		k, err := solana.PrivateKeyFromBase58(wif)
		if err != nil {
			t.Fatalf("SLOTFARE_SMOKE_KEY: %v", err)
		}
		payer = k
	} else {
		k, err := solana.NewRandomPrivateKey()
		if err != nil {
			t.Fatalf("generate fee payer: %v", err)
		}
		payer = k
		if _, err := rpcc.RequestAirdrop(ctx, payer.PublicKey(), solana.LAMPORTS_PER_SOL, rpc.CommitmentConfirmed); err != nil {
			t.Skipf("devnet airdrop unavailable (%v); set SLOTFARE_SMOKE_KEY to a funded devnet key", err)
		}
		deadline := time.Now().Add(45 * time.Second)
		for {
			bal, err := rpcc.GetBalance(ctx, payer.PublicKey(), rpc.CommitmentConfirmed)
			if err == nil && bal.Value > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Skipf("airdrop not confirmed (balance err=%v); set SLOTFARE_SMOKE_KEY", err)
			}
			time.Sleep(2 * time.Second)
		}
	}

	client, err := NewClient(Config{RPC: rpcURL, FeePayer: payer.String(), Cluster: "localnet"}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var root [32]byte
	copy(root[:], []byte("slotfare-smoke-root"))
	account := AccountIDFor(payer.PublicKey())

	sig, err := client.CommitAndSettle(ctx, root, account, 10, 20, 3)
	if err != nil {
		t.Fatalf("CommitAndSettle: %v", err)
	}
	t.Logf("commit tx: https://explorer.solana.com/tx/%s", sig)

	sigParsed, err := solana.SignatureFromBase58(sig)
	if err != nil {
		t.Fatalf("parse signature: %v", err)
	}
	var maxVer uint64
	var tx *rpc.GetTransactionResult
	// The tx is confirmed when Send returns, but GetTransaction can 404
	// for a moment while the RPC catches up; poll briefly.
	deadline := time.Now().Add(20 * time.Second)
	for {
		tx, err = rpcc.GetTransaction(ctx, sigParsed, &rpc.GetTransactionOpts{
			Encoding:                       solana.EncodingJSON,
			Commitment:                     rpc.CommitmentConfirmed,
			MaxSupportedTransactionVersion: &maxVer,
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetTransaction: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if tx.Meta == nil || tx.Meta.Err != nil {
		t.Fatalf("transaction failed on chain: %+v", tx.Meta)
	}

	parsed, err := tx.Transaction.GetTransaction()
	if err != nil || parsed == nil {
		t.Fatalf("decode transaction: %v", err)
	}
	if len(parsed.Message.Instructions) != 1 {
		t.Fatalf("expected 1 instruction, got %d", len(parsed.Message.Instructions))
	}
	ix := parsed.Message.Instructions[0]
	progID, err := parsed.ResolveProgramIDIndex(ix.ProgramIDIndex)
	if err != nil {
		t.Fatalf("resolve program id: %v", err)
	}
	if progID != solana.MemoProgramID {
		t.Fatalf("instruction program = %s, want memo program", progID)
	}

	// The memo program's instruction data IS the payload we sent.
	var got commitPayload
	if err := json.Unmarshal([]byte(ix.Data), &got); err != nil {
		t.Fatalf("memo payload is not our JSON: %v (%q)", err, string(ix.Data))
	}
	want := hex.EncodeToString(root[:])
	if got.Root != want {
		t.Errorf("memo root = %s, want %s", got.Root, want)
	}
	if got.Scheme != "slotfare-usage-root/v1" {
		t.Errorf("memo scheme = %q", got.Scheme)
	}
	if got.Account != account || got.First != 10 || got.Last != 20 || got.Credits != 3 {
		t.Errorf("memo fields = %+v", got)
	}
}
