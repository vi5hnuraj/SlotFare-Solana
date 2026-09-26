package solana

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// commitPayload is the memo body of a settlement commit. It is deliberately
// JSON: anyone can read the exact batch commitment from the transaction in
// any explorer without knowing this codebase, and the root it names is the
// Merkle root of the usage rows whose spend the same store transaction
// burned.
type commitPayload struct {
	Scheme  string `json:"scheme"`
	Root    string `json:"root"`
	Account string `json:"account"`
	First   uint64 `json:"firstUsageId"`
	Last    uint64 `json:"lastUsageId"`
	Credits uint64 `json:"credits"`
}

// Settle is unreachable in this configuration: the settler always runs the
// committed path (Attestor set), which burns inside CommitRecord together
// with the root commit. It exists to satisfy meter.ChainClient and fails
// loudly rather than silently skipping the burn.
func (c *Client) Settle(_ context.Context, accountID string, credits uint64) (string, error) {
	return "", fmt.Errorf("solana: direct settle of %d credits to %s is disabled; the committed root path is required", credits, accountID)
}

// CommitAndSettle submits the batch's usage Merkle root to Solana and returns
// its signature. It decides nothing: the caller has already selected the batch
// and computed the root, and the local burn happens only after this lands, in
// CommitRecord's store transaction. A failed commit defers the settle; the
// next pass retries the same window.
//
// Two rails, one signature: when a ledger program is configured the root is
// committed *inside that program*, which also burns the credits it covers in
// the same atomic transition, so "no burn without a committed root and one
// root per burn" is enforced by chain state rather than by the daemon's own
// bookkeeping. Without a program the root goes out as a Memo transaction,
// which proves authorship and ordering to any reader but cannot reject a
// duplicate.
func (c *Client) CommitAndSettle(ctx context.Context, root [32]byte, accountID string, firstUsageId, lastUsageId uint64, credits uint64) (string, error) {
	if c.ledger != nil {
		owner, err := PubkeyForAccountID(accountID)
		if err != nil {
			return "", fmt.Errorf("solana: ledger commit account: %w", err)
		}
		sig, err := c.LedgerCommitAndSettle(ctx, owner, root, firstUsageId, lastUsageId, credits)
		if err != nil {
			return "", err
		}
		return sig.String(), nil
	}
	if c.payer == nil {
		return "", fmt.Errorf("solana: no fee payer configured")
	}
	payload, err := json.Marshal(commitPayload{
		Scheme:  "slotfare-usage-root/v1",
		Root:    hex.EncodeToString(root[:]),
		Account: accountID,
		First:   firstUsageId,
		Last:    lastUsageId,
		Credits: credits,
	})
	if err != nil {
		return "", fmt.Errorf("solana: encode commit payload: %w", err)
	}
	if len(payload) > 566 {
		// The memo instruction's data cap; a payload this large means a
		// schema change, not a big batch (the fields are fixed-width).
		return "", fmt.Errorf("solana: commit payload is %d bytes, over the 566-byte memo limit", len(payload))
	}
	bh, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return "", fmt.Errorf("solana: latest blockhash: %w", err)
	}
	ix := solana.NewInstruction(solana.MemoProgramID, nil, payload)
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		bh.Value.Blockhash,
		solana.TransactionPayer(c.payer.PublicKey()),
	)
	if err != nil {
		return "", fmt.Errorf("solana: build commit tx: %w", err)
	}
	if _, err := tx.Sign(func(solana.PublicKey) *solana.PrivateKey { return &c.payer }); err != nil {
		return "", fmt.Errorf("solana: sign commit tx: %w", err)
	}
	// Preflight at the same commitment the blockhash was fetched at:
	// solana-go's plain SendTransaction leaves preflight at the node's
	// default (finalized), which rejects a fresh confirmed blockhash with
	// BlockhashNotFound.
	sig, err := c.rpc.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return "", fmt.Errorf("solana: send commit tx: %w", err)
	}
	return sig.String(), nil
}
