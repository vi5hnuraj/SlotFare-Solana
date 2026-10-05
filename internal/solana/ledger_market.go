package solana

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/rpc"
)

// This file is the daemon's half of the on-chain market: the escrow listing
// the program holds, and the three instructions that open, take and withdraw
// one. It is the Solana port of the original CreditMarket - `list` moved a
// seller's credits into the contract, `buy` paid the seller and released them
// in one transaction, and `cancel` returned them. Here the escrow is a
// program-owned PDA and the payment is an SPL `TransferChecked` the program
// makes itself, so the same three facts hold: a listed credit is unspendable
// while the listing is open, a sale is atomic, and only the seller may
// withdraw.
//
// The wire format is a deployed contract, mirrored from
// programs/slotfare-ledger/src/{instruction,state}.rs and pinned by golden-byte
// tests on both sides.

// Market instruction tags, from instruction.rs. Tags are stable on the wire:
// they are appended to, never renumbered.
const (
	ixLedgerList       byte = 7
	ixLedgerBuy        byte = 8
	ixLedgerCancelList byte = 9
)

// LedgerListingSize is a listing account's on-disk length: magic(4) seller(32)
// buyer(32) pay_mint(32) credits(8) price(8) nonce(8) state(1).
const LedgerListingSize = 125

const (
	ledgerListingMagic = "SFLS"
	ledgerListingSeed  = "listing"
	ledgerEscrowSeed   = "escrow"
)

// A listing's state byte, from state.rs.
const (
	LedgerListingOpen      uint8 = 0
	LedgerListingSold      uint8 = 1
	LedgerListingCancelled uint8 = 2
)

// LedgerListing is one market listing as the program holds it: the seller and
// terms, the buyer once sold, and the escrow's credit count. Reading it is how
// the console confirms a listing is really on chain before it records a row.
type LedgerListing struct {
	Seller  solana.PublicKey
	Buyer   solana.PublicKey
	PayMint solana.PublicKey
	Credits uint64
	Price   uint64
	Nonce   uint64
	State   uint8
}

// Open reports whether a buyer may still take the listing.
func (l LedgerListing) Open() bool { return l.State == LedgerListingOpen }

// DecodeLedgerListing parses the program's fixed listing layout. A short
// buffer, a foreign magic or an unknown state byte is an error: a listing
// read as a credit account (or the reverse) is the mistake the magic exists
// to catch.
func DecodeLedgerListing(data []byte) (LedgerListing, error) {
	if len(data) < LedgerListingSize {
		return LedgerListing{}, fmt.Errorf("solana: listing account is %d bytes, want %d", len(data), LedgerListingSize)
	}
	if string(data[0:4]) != ledgerListingMagic {
		return LedgerListing{}, fmt.Errorf("solana: listing magic %q is not %q", data[0:4], ledgerListingMagic)
	}
	if data[124] > LedgerListingCancelled {
		return LedgerListing{}, fmt.Errorf("solana: listing state byte %d is unknown", data[124])
	}
	var l LedgerListing
	copy(l.Seller[:], data[4:36])
	copy(l.Buyer[:], data[36:68])
	copy(l.PayMint[:], data[68:100])
	l.Credits = binary.LittleEndian.Uint64(data[100:108])
	l.Price = binary.LittleEndian.Uint64(data[108:116])
	l.Nonce = binary.LittleEndian.Uint64(data[116:124])
	l.State = data[124]
	return l, nil
}

// LedgerListingAddress is a listing's PDA: [b"listing", seller, nonce]. The
// nonce is the caller's, so one seller may hold several listings.
func LedgerListingAddress(programID, seller solana.PublicKey, nonce uint64) (solana.PublicKey, error) {
	var nonceBytes [8]byte
	binary.LittleEndian.PutUint64(nonceBytes[:], nonce)
	addr, _, err := solana.FindProgramAddress(
		[][]byte{[]byte(ledgerListingSeed), seller.Bytes(), nonceBytes[:]},
		programID,
	)
	return addr, err
}

// LedgerEscrowAddress is a listing's escrow credit account: [b"escrow", listing].
func LedgerEscrowAddress(programID, listing solana.PublicKey) (solana.PublicKey, error) {
	addr, _, err := solana.FindProgramAddress(
		[][]byte{[]byte(ledgerEscrowSeed), listing.Bytes()},
		programID,
	)
	return addr, err
}

// ledgerListData is the List payload: tag(1) credits(8) price(8) nonce(8)
// pay_mint(32). Pinned by TestLedgerMarketWireFormatMatchesTheProgram.
func ledgerListData(credits, price, nonce uint64, payMint solana.PublicKey) []byte {
	out := make([]byte, 1+8+8+8+32)
	out[0] = ixLedgerList
	binary.LittleEndian.PutUint64(out[1:9], credits)
	binary.LittleEndian.PutUint64(out[9:17], price)
	binary.LittleEndian.PutUint64(out[17:25], nonce)
	copy(out[25:57], payMint[:])
	return out
}

// LedgerListIx builds the seller-signed instruction that opens a listing:
// accounts `[seller (signer, writable), seller_credit, listing, escrow,
// system]`. The credits move from the seller's spendable balance into the
// escrow, so a listed credit is unspendable on chain.
func (c *Client) LedgerListIx(seller solana.PublicKey, credits, price, nonce uint64, payMint solana.PublicKey) (solana.Instruction, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return nil, err
	}
	sellerCredit, err := LedgerCreditAddress(programID, seller)
	if err != nil {
		return nil, err
	}
	listing, err := LedgerListingAddress(programID, seller, nonce)
	if err != nil {
		return nil, err
	}
	escrow, err := LedgerEscrowAddress(programID, listing)
	if err != nil {
		return nil, err
	}
	return solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(seller, true, true),
			solana.NewAccountMeta(sellerCredit, true, false),
			solana.NewAccountMeta(listing, true, false),
			solana.NewAccountMeta(escrow, true, false),
			solana.NewAccountMeta(solana.SystemProgramID, false, false),
		},
		ledgerListData(credits, price, nonce, payMint),
	), nil
}

// LedgerCancelListIx builds the seller-signed instruction that withdraws a
// listing: accounts `[seller (signer), listing, escrow, seller_credit]`. The
// escrow returns to the seller's spendable balance.
func (c *Client) LedgerCancelListIx(seller solana.PublicKey, nonce uint64) (solana.Instruction, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return nil, err
	}
	listing, err := LedgerListingAddress(programID, seller, nonce)
	if err != nil {
		return nil, err
	}
	escrow, err := LedgerEscrowAddress(programID, listing)
	if err != nil {
		return nil, err
	}
	sellerCredit, err := LedgerCreditAddress(programID, seller)
	if err != nil {
		return nil, err
	}
	return solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(seller, true, false),
			solana.NewAccountMeta(listing, true, false),
			solana.NewAccountMeta(escrow, true, false),
			solana.NewAccountMeta(sellerCredit, true, false),
		},
		[]byte{ixLedgerCancelList},
	), nil
}

// LedgerBuyIx builds the buyer-signed instructions that take a listing: an
// idempotent associated-token-account create for the seller (a seller who has
// never held USDC needs one), then the program's Buy. accounts
// `[buyer (signer, writable), buyer_credit, seller_credit, listing, escrow,
// buyer_token, seller_token, mint, token_program]`. The program pays the
// seller and releases the escrow in one atomic transition.
//
// It returns a slice, not one instruction, because the buyer's transaction
// must carry the account create first.
func (c *Client) LedgerBuyIx(buyer, seller solana.PublicKey, nonce uint64, payMint solana.PublicKey) ([]solana.Instruction, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return nil, err
	}
	buyerCredit, err := LedgerCreditAddress(programID, buyer)
	if err != nil {
		return nil, err
	}
	sellerCredit, err := LedgerCreditAddress(programID, seller)
	if err != nil {
		return nil, err
	}
	listing, err := LedgerListingAddress(programID, seller, nonce)
	if err != nil {
		return nil, err
	}
	escrow, err := LedgerEscrowAddress(programID, listing)
	if err != nil {
		return nil, err
	}
	buyerToken, _, err := solana.FindAssociatedTokenAddress(buyer, payMint)
	if err != nil {
		return nil, err
	}
	sellerToken, _, err := solana.FindAssociatedTokenAddress(seller, payMint)
	if err != nil {
		return nil, err
	}
	// The seller may never have held the mint, so their token account may not
	// exist yet. Creating it idempotently is the buyer's cost, and the program
	// then has a real account to pay into.
	create := associatedtokenaccount.NewCreateIdempotentInstruction(buyer, seller, payMint).Build()
	buy := solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(buyer, true, true),
			solana.NewAccountMeta(buyerCredit, true, false),
			solana.NewAccountMeta(sellerCredit, true, false),
			solana.NewAccountMeta(listing, true, false),
			solana.NewAccountMeta(escrow, true, false),
			solana.NewAccountMeta(buyerToken, true, false),
			solana.NewAccountMeta(sellerToken, true, false),
			solana.NewAccountMeta(payMint, false, false),
			solana.NewAccountMeta(solana.TokenProgramID, false, false),
		},
		[]byte{ixLedgerBuy},
	)
	return []solana.Instruction{create, buy}, nil
}

// LedgerListingRead reads one listing by seller and nonce. The bool reports
// whether the account exists; a listing that was never opened is not an error.
func (c *Client) LedgerListingRead(ctx context.Context, seller solana.PublicKey, nonce uint64) (LedgerListing, bool, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return LedgerListing{}, false, err
	}
	addr, err := LedgerListingAddress(programID, seller, nonce)
	if err != nil {
		return LedgerListing{}, false, err
	}
	raw, ok, err := c.readAccount(ctx, addr)
	if err != nil || !ok {
		return LedgerListing{}, false, err
	}
	l, err := DecodeLedgerListing(raw)
	if err != nil {
		return LedgerListing{}, false, err
	}
	return l, true, nil
}

// PrepareLedgerTx serializes an unsigned transaction for a user's wallet to
// sign and submit. The user is the fee payer, exactly as the browser's own
// transfer path sets it, and the blockhash is fetched at `confirmed` so the
// transaction does not expire at the node's default commitment before the
// wallet answers. Nothing here signs: the daemon never holds a user's key.
//
// The returned bytes are the standard Solana wire form, so any wallet's
// `Transaction.from(bytes)` reads them back with the same accounts and data.
func (c *Client) PrepareLedgerTx(ctx context.Context, payer solana.PublicKey, ixs []solana.Instruction) ([]byte, error) {
	if len(ixs) == 0 {
		return nil, fmt.Errorf("solana: no instructions to prepare")
	}
	bh, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return nil, fmt.Errorf("solana: latest blockhash: %w", err)
	}
	tx, err := solana.NewTransaction(ixs, bh.Value.Blockhash, solana.TransactionPayer(payer))
	if err != nil {
		return nil, fmt.Errorf("solana: build prepared tx: %w", err)
	}
	return tx.MarshalBinary()
}

// RPC returns the client's JSON-RPC connection. The console's market is
// wallet-signed, so a caller that has to build, sign and submit a transaction
// the daemon does not own - a live test standing in for the browser, most of
// all - needs the same endpoint the daemon talks to.
func (c *Client) RPC() *rpc.Client { return c.rpc }

// LedgerListingID is the store's listing_id for a program listing: the
// last 8 bytes of the listing PDA, little-endian. The console's confirm
// writes this id and the indexer derives the same value on replay, so one
// on-chain listing maps to exactly one row.
func (c *Client) LedgerListingID(seller solana.PublicKey, nonce uint64) (uint64, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return 0, err
	}
	addr, err := LedgerListingAddress(programID, seller, nonce)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(addr[24:32]), nil
}

// LedgerTxSlot returns the slot a submitted transaction landed in: the
// console's listing rows date themselves with it, exactly as the
// local-mirror path recorded an event's block. A transaction the cluster
// does not know at confirmed commitment is an error, not a zero - a row
// dated slot 0 would be a row dated nothing.
func (c *Client) LedgerTxSlot(ctx context.Context, signature solana.Signature) (uint64, error) {
	res, err := c.rpc.GetTransaction(ctx, signature, &rpc.GetTransactionOpts{
		Commitment: rpc.CommitmentConfirmed,
		Encoding:   solana.EncodingBase64,
	})
	if err != nil {
		return 0, fmt.Errorf("solana: read transaction %s: %w", signature, err)
	}
	if res == nil {
		return 0, fmt.Errorf("solana: transaction %s is not known to the cluster", signature)
	}
	return res.Slot, nil
}

// LedgerMarketSubmit signs the given instructions with the daemon's payer and
// submits them. It is the server-side half: the market routes hand the user a
// prepared transaction instead, but a deployment that has the owner's key -
// a test, or an operator acting for themselves - can submit directly.
func (c *Client) LedgerMarketSubmit(ctx context.Context, ixs []solana.Instruction) (solana.Signature, error) {
	return c.sendLedger(ctx, ixs)
}
