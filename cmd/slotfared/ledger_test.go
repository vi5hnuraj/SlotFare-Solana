package main

import (
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"

	"slotfare/internal/solana"
)

// The ledger-init subcommand is the deployment path, so its refusals matter
// more than its happy path: it runs once, against a live cluster, and a silent
// mistake here binds the wrong treasury for the life of the program.

func TestLedgerInitRefusesToRunUnconfigured(t *testing.T) {
	t.Setenv("SLOTFARE_SOLANA_RPC", "")
	t.Setenv("SLOTFARE_LEDGER_PROGRAM", "")
	err := runLedgerInit(nil)
	if err == nil || !strings.Contains(err.Error(), "SLOTFARE_SOLANA_RPC") {
		t.Fatalf("without an RPC the subcommand must name it: %v", err)
	}

	t.Setenv("SLOTFARE_SOLANA_RPC", "http://127.0.0.1:8899")
	t.Setenv("SLOTFARE_SOLANA_FEE_PAYER", "")
	err = runLedgerInit(nil)
	if err == nil {
		t.Fatal("a configured RPC without a fee payer must fail")
	}

	key, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SLOTFARE_SOLANA_FEE_PAYER", key.String())
	t.Setenv("SLOTFARE_LEDGER_PROGRAM", "")
	err = runLedgerInit(nil)
	if err == nil || !strings.Contains(err.Error(), "SLOTFARE_LEDGER_PROGRAM") {
		t.Fatalf("without a program id the subcommand must name it: %v", err)
	}

	// A malformed program id must be a parse error, not a transaction.
	t.Setenv("SLOTFARE_LEDGER_PROGRAM", "not-a-key")
	if err := runLedgerInit(nil); err == nil {
		t.Fatal("a malformed program id must be rejected")
	}
}

func TestLedgerInitRejectsStrayArguments(t *testing.T) {
	if err := runLedgerInit([]string{"unexpected"}); err == nil {
		t.Fatal("a positional argument must be rejected")
	}
}

func TestCheckBoundConfigComparesTheEnvironment(t *testing.T) {
	mint, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	treasury, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := solana.Config{Mint: mint.PublicKey().String(), Treasury: treasury.PublicKey().String()}
	bound := solana.LedgerConfig{USDC: mint.PublicKey(), Treasury: treasury.PublicKey()}
	if err := checkBoundConfig(cfg, bound); err != nil {
		t.Fatalf("a matching binding must pass: %v", err)
	}

	// The treasury is the one that decides which transfers mint, so a swap of
	// the two must not pass as a match.
	swapped := solana.LedgerConfig{USDC: treasury.PublicKey(), Treasury: mint.PublicKey()}
	if err := checkBoundConfig(cfg, swapped); err == nil {
		t.Fatal("a swapped mint and treasury must be reported")
	}

	other, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkBoundConfig(cfg, solana.LedgerConfig{USDC: other.PublicKey(), Treasury: treasury.PublicKey()}); err == nil {
		t.Fatal("a different mint must be reported")
	}

	// Nothing to compare against is itself a refusal: an operator who does not
	// name the treasury cannot be told they confirmed the right one.
	if err := checkBoundConfig(solana.Config{Mint: cfg.Mint}, bound); err == nil {
		t.Fatal("an unnamed treasury must be a refusal, not a pass")
	}
	if err := checkBoundConfig(solana.Config{Treasury: cfg.Treasury}, bound); err == nil {
		t.Fatal("an unnamed mint must be a refusal, not a pass")
	}
}
