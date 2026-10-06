package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"slotfare/internal/solana"
)

// runLedgerInit initialises the on-chain ledger program and optionally opens
// credit accounts, so the deployment procedure in docs/onchain-ledger.md is one
// command instead of a throwaway program.
//
// It is idempotent by inspection. InitConfig is first-caller-wins on chain, so
// rather than submitting a call that would fail once the config account exists,
// it reads the config first. Re-running after a successful run is therefore a
// safe no-op that prints the deployment's authority, mint and treasury - which
// is exactly the check an operator wants - and it refuses to proceed silently
// if what is bound does not match the environment it was given.
func runLedgerInit(args []string) error {
	fs := flag.NewFlagSet("slotfared ledger-init", flag.ContinueOnError)
	var accounts multiFlag
	fs.Var(&accounts, "account", "account id (0x-prefixed 32-byte hex) to open a credit account for; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: slotfared ledger-init [--account 0x…]…")
	}

	cfg, err := solana.LoadConfig()
	if err != nil {
		return err
	}
	if !cfg.Enabled() {
		return errors.New("SLOTFARE_SOLANA_RPC must be set: ledger-init talks to a cluster")
	}
	if cfg.LedgerProgram == "" {
		return errors.New("SLOTFARE_LEDGER_PROGRAM must be set to the deployed slotfare-ledger program id")
	}
	client, err := solana.NewClient(cfg, nil)
	if err != nil {
		return err
	}
	ctx := context.Background()

	_, exists, err := client.LedgerConfigRead(ctx)
	if err != nil {
		return err
	}
	var bound *solana.LedgerConfig
	if exists {
		log.Printf("slotfared: ledger program already initialised")
		cfgNow, _, err := client.LedgerConfigRead(ctx)
		if err != nil {
			return err
		}
		bound = &cfgNow
	} else {
		log.Printf("slotfared: initialising ledger program %s", client.LedgerProgram())
		sig, ierr := client.LedgerInitConfig(ctx)
		if ierr != nil {
			// InitConfig is first-caller-wins, and a config we just read as
			// missing may have landed in between - a re-run, or a race with
			// another operator. The program answers that with its own code
			// rather than a bare system error, so re-read and continue with
			// what is bound instead of failing on a config that plainly is.
			cfgNow, nowExists, rerr := client.LedgerConfigRead(ctx)
			if rerr != nil || !nowExists {
				return fmt.Errorf("init config: %w", ierr)
			}
			log.Printf("slotfared: config was already initialised")
			bound = &cfgNow
		} else {
			log.Printf("slotfared: config initialised in %s", sig)
			// The submission is only preflighted, not awaited, so the account
			// may not be readable the instant this returns. Poll for it so the
			// binding printed below is read back from the chain rather than
			// asserted from the input, and give up honestly rather than hang.
			cfgNow, nowExists := waitForConfig(ctx, client)
			if nowExists {
				bound = &cfgNow
			} else {
				log.Print("slotfared: config is submitted but not yet readable at `confirmed`; re-run to confirm the binding")
			}
		}
	}
	if bound != nil {
		log.Printf("slotfared:   authority %s", bound.Authority)
		log.Printf("slotfared:   mint      %s", bound.USDC)
		log.Printf("slotfared:   treasury  %s", bound.Treasury)
		if err := checkBoundConfig(cfg, *bound); err != nil {
			return err
		}
	}

	for _, accountID := range accounts {
		owner, err := solana.PubkeyForAccountID(accountID)
		if err != nil {
			return err
		}
		sig, created, err := client.LedgerEnsureCredit(ctx, owner)
		if err != nil {
			return fmt.Errorf("open credit account for %s: %w", accountID, err)
		}
		if created {
			log.Printf("slotfared: credit account opened for %s in %s", accountID, sig)
		} else {
			log.Printf("slotfared: credit account for %s already exists", accountID)
		}
	}
	return nil
}

// waitForConfig polls until the program's config account is readable, bounded
// so a cluster that never confirms reports a fact instead of hanging. Errors
// are swallowed into "not readable yet": the caller re-runs, which is safe
// precisely because InitConfig is first-caller-wins.
func waitForConfig(ctx context.Context, client *solana.Client) (solana.LedgerConfig, bool) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		if cfg, ok, err := client.LedgerConfigRead(ctx); err == nil && ok {
			return cfg, true
		}
		if time.Now().After(deadline) {
			return solana.LedgerConfig{}, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// checkBoundConfig compares the on-chain config against the environment it was
// asked to match. A config bound to the wrong mint mints from the wrong
// transfers, and finding that out here is cheaper than finding it out from a
// settlement that will not reconcile.
func checkBoundConfig(cfg solana.Config, bound solana.LedgerConfig) error {
	// The environment must name both, or there is nothing to check the
	// deployment against and a mismatch would go unreported.
	if cfg.Mint == "" || cfg.Treasury == "" {
		return errors.New("SLOTFARE_SOLANA_MINT and SLOTFARE_SOLANA_TREASURY must both be set to confirm what the program is bound to")
	}
	// Base58 is canonical, so comparing the rendered keys compares the keys.
	if bound.USDC.String() != cfg.Mint || bound.Treasury.String() != cfg.Treasury {
		return fmt.Errorf("the bound config (mint %s, treasury %s) does not match the environment (mint %s, treasury %s)",
			bound.USDC, bound.Treasury, cfg.Mint, cfg.Treasury)
	}
	return nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
