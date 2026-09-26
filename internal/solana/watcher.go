package solana

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// USDC mints. The devnet mint is Circle's faucet USDC; mainnet is the native
// USDC mint.
const (
	DevnetUSDCMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	MainnetUSDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

// MicroCreditsPerUSD is the fixed mint rate: 1,000 credits per USD, and one
// credit is meter.MicroCreditsPerCredit (10,000) micro-credits, so one
// USDC base unit (1e-6 USD) is exactly 10 micro-credits.
const MicroCreditsPerUSDCBase = 10

// MintFunc writes an account's absolute minted total (micro-credits) to the
// balance authority. It is absolute, not incremental: the watcher re-derives
// totals from chain history, so a restart or a replay converges to the same
// number instead of double-minting.
type MintFunc func(ctx context.Context, accountID string, mintedMicro uint64) error

// Watcher polls Solana for USDC transfers to the treasury and mints credits
// to the sending wallet's account. The chain is the only minter: there is
// no admin top-up path, and the balance anyone can verify is reconstructable
// from the treasury's transfer history.
type Watcher struct {
	client   *rpc.Client
	cfg      Config
	mint     solana.PublicKey
	treasury solana.PublicKey
	minted   MintFunc

	// interval is the poll period; cursor is the newest signature applied.
	interval time.Duration
	cursor   string
	// totals is the running per-account minted total (micro-credits),
	// rebuilt from full history on boot and extended incrementally after.
	totals map[string]uint64

	// mu guards the health snapshot below, which Health() serves to any
	// goroutine (the console's attestation card polls it) while rescan
	// updates it. lastScan is the last COMPLETED pass; head is the slot
	// observed at that pass; scanErr is the most recent pass's failure,
	// cleared by the next success.
	mu       sync.Mutex
	lastScan time.Time
	head     uint64
	scanErr  string

	// OnDeposit, when set, is called for every treasury deposit on every
	// rescan pass, with the containing slot and the transaction's block
	// time. It fires repeatedly for the same deposit across passes, so the
	// implementation must be idempotent per signature (the funding store's
	// insert-or-ignore primary key is what makes that cheap).
	OnDeposit func(ctx context.Context, d Deposit, slot uint64, blockTime time.Time)
}

// NewWatcher wires a watcher against the configured cluster. It performs no
// network I/O; Run does.
func NewWatcher(cfg Config, minted MintFunc) (*Watcher, error) {
	treasury, err := solana.PublicKeyFromBase58(cfg.Treasury)
	if err != nil {
		return nil, fmt.Errorf("parse treasury: %w", err)
	}
	mint, err := solana.PublicKeyFromBase58(cfg.Mint)
	if err != nil {
		return nil, fmt.Errorf("parse mint: %w", err)
	}
	poll := cfg.Poll
	if poll <= 0 {
		poll = 15 * time.Second
	}
	return &Watcher{
		client:   rpc.New(cfg.RPC),
		cfg:      cfg,
		mint:     mint,
		treasury: treasury,
		minted:   minted,
		interval: poll,
		totals:   make(map[string]uint64),
	}, nil
}

// Run rescans the treasury's full transfer history once (so totals are
// correct even after a restart), then polls for new transfers until ctx is
// done. Errors are reported through onErr and never stop the loop.
func (w *Watcher) Run(ctx context.Context, onErr func(error)) error {
	if err := w.rescan(ctx); err != nil {
		if onErr != nil {
			onErr(fmt.Errorf("solana: watcher initial scan: %w", err))
		}
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.rescan(ctx); err != nil && onErr != nil {
				onErr(fmt.Errorf("solana: watcher scan: %w", err))
			}
		}
	}
}

// Health is the watcher's self-reported frontier: when the last pass
// completed, the slot it observed, and the most recent failure (empty when
// healthy). Zero LastScan means no pass has completed yet - the chain
// surface is off, or the RPC has never answered.
type Health struct {
	LastScan time.Time
	Head     uint64
	Err      string
}

// Health returns the current snapshot. It is safe to call from any
// goroutine while the watcher runs.
func (w *Watcher) Health() Health {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Health{LastScan: w.lastScan, Head: w.head, Err: w.scanErr}
}

// Head returns the last observed chain slot, 0 before the first scan.
func (w *Watcher) Head() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.head
}

// noteScan records a completed pass.
func (w *Watcher) noteScan(head uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastScan = time.Now().UTC()
	w.scanErr = ""
	if head > 0 {
		w.head = head
	}
}

// noteScanErr records a failed pass: the error is remembered, lastScan and
// head stay where the last success left them, so the lag the console shows
// keeps growing until the watcher recovers.
func (w *Watcher) noteScanErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.scanErr = err.Error()
}

// rescan rebuilds the deposit totals from the treasury's signature history
// and pushes each changed account's absolute total to the authority. Full
// rescan rather than cursor-only polling is deliberate: correctness after a
// crash costs one indexed scan, and a demo treasury has few transactions.
// Every pass records the health snapshot the console's attestation card
// serves: a success stamps lastScan (lag then grows only as the watcher
// goes quiet), a failure keeps the previous stamp and remembers the error.
func (w *Watcher) rescan(ctx context.Context) (err error) {
	var head uint64
	defer func() {
		if err != nil {
			w.noteScanErr(err)
		} else {
			w.noteScan(head)
		}
	}()
	sigs, err := w.allSignatures(ctx)
	if err != nil {
		return err
	}
	// The chain head for the health row. A treasury with no recent
	// transactions still has a head, and the signature history alone
	// cannot show one; a failed head read does not fail the pass, it just
	// reports the previous head.
	if h, hErr := w.client.GetSlot(ctx, rpc.CommitmentConfirmed); hErr == nil {
		head = h
	} else {
		head = w.Head()
	}
	totals := make(map[string]uint64)
	for _, info := range sigs {
		if info.Err != nil {
			continue // failed transactions moved nothing
		}
		deposit, err := w.depositOf(ctx, info.Signature)
		if err != nil {
			return err
		}
		if deposit != nil {
			totals[deposit.AccountID] += deposit.microCredits()
			if w.OnDeposit != nil {
				var blockTime time.Time
				if info.BlockTime != nil {
					blockTime = time.Unix(int64(*info.BlockTime), 0).UTC()
				} else {
					blockTime = time.Now().UTC()
				}
				w.OnDeposit(ctx, *deposit, info.Slot, blockTime)
			}
		}
	}
	for id, micro := range totals {
		if prev, ok := w.totals[id]; ok && prev == micro {
			continue
		}
		if err := w.minted(ctx, id, micro); err != nil {
			return fmt.Errorf("mint %s: %w", id, err)
		}
		w.totals[id] = micro
	}
	w.cursor = cursorOf(sigs)
	return nil
}

// allSignatures pages through the treasury's confirmed signature history,
// newest first.
func (w *Watcher) allSignatures(ctx context.Context) ([]rpc.TransactionSignature, error) {
	var out []rpc.TransactionSignature
	before := solana.Signature{}
	limit := 1000
	for {
		page, err := w.client.GetSignaturesForAddressWithOpts(ctx, w.treasury, &rpc.GetSignaturesForAddressOpts{
			Limit:  &limit,
			Before: before,
		})
		if err != nil {
			return nil, fmt.Errorf("signatures: %w", err)
		}
		for _, p := range page {
			out = append(out, *p)
		}
		if len(page) < 1000 {
			return out, nil
		}
		before = page[len(page)-1].Signature
	}
}

// Deposit is one USDC transfer to the treasury.
type Deposit struct {
	Signature string
	// From is the base58 public key of the wallet that sent the USDC.
	From string
	// AccountID is the Slotfare account the deposit mints to: 0x-hex of the
	// sender's 32-byte public key, so a wallet address IS the account.
	AccountID string
	// USDCBase is the transfer amount in USDC base units (1e-6 USD).
	USDCBase uint64
}

func (d Deposit) microCredits() uint64 { return d.USDCBase * MicroCreditsPerUSDCBase }

// AccountIDFor returns the Slotfare account id for a Solana wallet: the raw
// 32-byte public key in lowercase hex, 0x-prefixed. Nothing is hashed, so
// the mapping is visible and reversible in one step for anyone verifying a
// deposit against the ledger.
func AccountIDFor(pub solana.PublicKey) string {
	return "0x" + hex.EncodeToString(pub[:])
}

// depositOf fetches one transaction and extracts the USDC transfer to the
// treasury from its token-balance diff, or nil when the transaction did not
// move USDC to the treasury.
func (w *Watcher) depositOf(ctx context.Context, sig solana.Signature) (*Deposit, error) {
	maxVer := uint64(0)
	tx, err := w.client.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Encoding:                       solana.EncodingJSON,
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &maxVer,
	})
	if err != nil {
		return nil, fmt.Errorf("tx %s: %w", sig, err)
	}
	if tx.Meta == nil {
		return nil, nil
	}
	var parsed *solana.Transaction
	if tx.Transaction != nil {
		var err error
		if parsed, err = tx.Transaction.GetTransaction(); err != nil {
			return nil, fmt.Errorf("tx %s: decode: %w", sig, err)
		}
	}
	if parsed == nil {
		return nil, nil
	}
	// Account-index resolution for pre/post token balances: static keys
	// first, then the addresses loaded from lookup tables (writable, then
	// readonly) — the transaction's account array in index order.
	keys := append(solana.PublicKeySlice{}, parsed.Message.AccountKeys...)
	keys = append(keys, tx.Meta.LoadedAddresses.Writable...)
	keys = append(keys, tx.Meta.LoadedAddresses.ReadOnly...)
	return w.parseDeposit(sig.String(), keys, tx.Meta.PreTokenBalances, tx.Meta.PostTokenBalances)
}

// parseDeposit is the pure token-balance diff: given one transaction's
// account keys and pre/post token balances for this mint, it returns the
// deposit the treasury received, or nil when the transaction did not credit
// the treasury. The treasury is identified by its token account (keys[idx]),
// not by owner: the owner wallet is not part of the transaction's account
// array, and matching on owner would credit a deposit that merely passed
// through a wallet the treasury happens to be related to. The sender's
// identity comes from the balance's owner field — that is the wallet whose
// Solana public key IS the account. Split from depositOf so it is testable
// offline.
func (w *Watcher) parseDeposit(sig string, keys solana.PublicKeySlice, preBal, postBal []rpc.TokenBalance) (*Deposit, error) {
	type leg struct {
		acct  string
		owner string
		delta int64
	}
	keyAt := func(idx uint16) (string, error) {
		if int(idx) >= len(keys) {
			return "", fmt.Errorf("tx %s: account index %d out of range (%d keys)", sig, idx, len(keys))
		}
		return keys[idx].String(), nil
	}
	// Diff pre/post token balances per account index, this mint only.
	pre := map[uint16]int64{}
	preOwner := map[uint16]string{}
	for _, b := range preBal {
		if b.Mint != w.mint || b.UiTokenAmount == nil {
			continue
		}
		n, err := strconv.ParseInt(b.UiTokenAmount.Amount, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tx %s: pre balance %q: %w", sig, b.UiTokenAmount.Amount, err)
		}
		pre[b.AccountIndex] = n
		if b.Owner != nil {
			preOwner[b.AccountIndex] = b.Owner.String()
		}
	}
	post := map[uint16]int64{}
	postOwner := map[uint16]string{}
	for _, b := range postBal {
		if b.Mint != w.mint || b.UiTokenAmount == nil {
			continue
		}
		n, err := strconv.ParseInt(b.UiTokenAmount.Amount, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tx %s: post balance %q: %w", sig, b.UiTokenAmount.Amount, err)
		}
		post[b.AccountIndex] = n
		if b.Owner != nil {
			postOwner[b.AccountIndex] = b.Owner.String()
		}
	}
	var legs []leg
	seen := map[uint16]bool{}
	for idx, before := range pre {
		seen[idx] = true
		after := post[idx]
		if d := after - before; d != 0 {
			acct, err := keyAt(idx)
			if err != nil {
				return nil, err
			}
			owner := postOwner[idx]
			if owner == "" {
				owner = preOwner[idx]
			}
			legs = append(legs, leg{acct: acct, owner: owner, delta: d})
		}
	}
	for idx, after := range post {
		if seen[idx] {
			continue
		}
		if after != 0 {
			acct, err := keyAt(idx)
			if err != nil {
				return nil, err
			}
			owner := postOwner[idx]
			if owner == "" {
				owner = preOwner[idx]
			}
			legs = append(legs, leg{acct: acct, owner: owner, delta: after})
		}
	}
	var creditTo uint64
	for _, l := range legs {
		if l.acct == w.cfg.Treasury && l.delta > 0 {
			creditTo += uint64(l.delta)
		}
	}
	if creditTo == 0 {
		return nil, nil
	}
	// The sender is the non-treasury leg that lost exactly what the
	// treasury gained; fall back to the largest debit when routing split
	// the send. Its balance owner is the wallet that IS the account.
	var from string
	var debit int64
	for _, l := range legs {
		if l.acct == w.cfg.Treasury {
			continue
		}
		if l.delta < 0 && uint64(-l.delta) == creditTo {
			from = l.owner
			break
		}
		if l.delta < 0 && l.delta < debit {
			from, debit = l.owner, l.delta
		}
	}
	if from == "" {
		return nil, nil
	}
	fromKey, err := solana.PublicKeyFromBase58(from)
	if err != nil {
		return nil, fmt.Errorf("tx %s: sender %q: %w", sig, from, err)
	}
	return &Deposit{
		Signature: sig,
		From:      from,
		AccountID: AccountIDFor(fromKey),
		USDCBase:  creditTo,
	}, nil
}

func cursorOf(sigs []rpc.TransactionSignature) string {
	if len(sigs) == 0 {
		return ""
	}
	return sigs[0].Signature.String()
}
