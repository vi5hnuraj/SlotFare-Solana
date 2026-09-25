package solana

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// PaymentInfo is a verified token payment: what the market's pay route
// reads back after the on-chain check passes.
type PaymentInfo struct {
	Signature string
	Slot      uint64
	// BlockTime is the transaction's estimated processing time; the pay
	// route rejects a payment whose block time falls outside the listing's
	// window, so it must be present.
	BlockTime time.Time
	// Amount is what the payee's wallets net-gained of the mint, in mint
	// base units.
	Amount uint64
}

// VerifyPayment checks that sig is a successful transfer of at least
// minAmount of mint whose net effect moves that much from payer to payee.
// Owners (not token accounts) are matched against the transaction's
// pre/post token-balance diff, so a payment routed through an intermediate
// account still counts as payer -> payee, while a transfer to anyone else,
// in the wrong mint, or for less than the price fails. Every failure is a
// plain error describing the first check that did not hold: the pay route
// maps them all to one client-visible rejection, since a wrong signature
// and an RPC hiccup are both "not verified" from the buyer's seat.
func (c *Client) VerifyPayment(ctx context.Context, sig string, mint, payer, payee string, minAmount uint64) (PaymentInfo, error) {
	parsed, err := solana.SignatureFromBase58(sig)
	if err != nil {
		return PaymentInfo{}, fmt.Errorf("solana: payment signature %q: %w", sig, err)
	}
	mintKey, err := solana.PublicKeyFromBase58(mint)
	if err != nil {
		return PaymentInfo{}, fmt.Errorf("solana: mint %q: %w", mint, err)
	}
	maxVersion := uint64(0)
	tx, err := c.rpc.GetTransaction(ctx, parsed, &rpc.GetTransactionOpts{
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &maxVersion,
	})
	if err != nil {
		return PaymentInfo{}, fmt.Errorf("solana: fetch payment %s: %w", sig, err)
	}
	if tx == nil {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s not found", sig)
	}
	if tx.Meta == nil {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s has no metadata", sig)
	}
	if tx.Meta.Err != nil {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s failed on chain: %v", sig, tx.Meta.Err)
	}
	if tx.BlockTime == nil {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s has no block time", sig)
	}

	// Net per-owner movement of the mint, from the balance diffs. An owner
	// absent from one side contributes zero there (account drained or
	// created within the transaction).
	delta := map[string]int64{}
	for _, b := range tx.Meta.PreTokenBalances {
		if b.Mint != mintKey || b.Owner == nil || b.UiTokenAmount == nil {
			continue
		}
		if amt, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64); err == nil {
			delta[b.Owner.String()] -= int64(amt)
		}
	}
	for _, b := range tx.Meta.PostTokenBalances {
		if b.Mint != mintKey || b.Owner == nil || b.UiTokenAmount == nil {
			continue
		}
		if amt, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64); err == nil {
			delta[b.Owner.String()] += int64(amt)
		}
	}

	got := delta[payee]
	paid := -delta[payer]
	want := int64(minAmount)
	if minAmount > uint64(want) { // minAmount above int64 cannot occur for USDC
		return PaymentInfo{}, fmt.Errorf("solana: payment %s amount out of range", sig)
	}
	if paid < want {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s moved %d of %s from payer, want at least %d", sig, paid, mint, minAmount)
	}
	if got < want {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s delivered %d of %s to payee, want at least %d", sig, got, mint, minAmount)
	}
	if payer == payee {
		return PaymentInfo{}, fmt.Errorf("solana: payment %s is payer and payee alike", sig)
	}

	return PaymentInfo{
		Signature: sig,
		Slot:      tx.Slot,
		BlockTime: time.Unix(int64(*tx.BlockTime), 0).UTC(),
		Amount:    uint64(got),
	}, nil
}
