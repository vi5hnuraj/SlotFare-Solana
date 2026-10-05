package solana

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// This file is the daemon's half of the on-chain ledger: it builds the
// instructions the program in programs/slotfare-ledger executes, derives the
// accounts those instructions name, and reads the balances back. The program
// is what makes "the chain holds the balances" true again - the local ledger
// stays the reservation and display authority, but a burn only happens inside
// the program, gated on a usage root committed in the same transition.
//
// The wire format below is a deployed contract: the tag bytes, the account
// order and the fixed 76-byte layout are all mirrored from
// programs/slotfare-ledger/src/instruction.rs and src/state.rs, and
// TestLedgerWireFormatMatchesTheProgram pins them against golden bytes so a
// change on either side fails a test instead of silently diverging.

// Instruction tags, from instruction.rs.
const (
	ixLedgerInitCredit      byte = 0
	ixLedgerCredit          byte = 1
	ixLedgerLock            byte = 2
	ixLedgerUnlock          byte = 3
	ixLedgerCommitAndSettle byte = 4
	ixLedgerTransfer        byte = 5
	ixLedgerInitConfig      byte = 6
)

// Fixed layouts, from state.rs and processor.rs.
const (
	// LedgerAccountSize is the credit account's on-disk length.
	LedgerAccountSize = 76
	ledgerConfigSize  = 100
)

// Account magics, so an account of the wrong kind is a decode error.
const (
	ledgerAccountMagic = "SFCA"
	ledgerConfigMagic  = "SFCG"
)

// PDA seeds, from processor.rs.
const (
	ledgerCreditSeed = "credit"
	ledgerConfigSeed = "config"
)

// LedgerAccount is one owner's credit position as the program holds it. It is
// the chain-side twin of the local ledger's balance: minted credits in, spent
// credits out (burned by settlement or moved by a transfer), and the locked
// remainder that is listed but unsellable.
type LedgerAccount struct {
	Minted      uint64
	Spent       uint64
	Locked      uint64
	LastRoot    [32]byte
	LastUsageID uint64
	BatchCount  uint64
}

// Spendable is the credits the program will let this account settle, transfer
// or lock right now: minted - spent - locked, clamped at zero so a corrupt
// account cannot underflow into a huge balance.
func (a LedgerAccount) Spendable() uint64 {
	s := a.Minted
	if a.Spent > s {
		return 0
	}
	s -= a.Spent
	if a.Locked > s {
		return 0
	}
	return s - a.Locked
}

// DecodeLedgerAccount parses the program's fixed layout. A short buffer or a
// foreign magic is an error: writing a balance into a look-alike account is
// exactly the mistake the magic exists to catch.
func DecodeLedgerAccount(data []byte) (LedgerAccount, error) {
	if len(data) < LedgerAccountSize {
		return LedgerAccount{}, fmt.Errorf("solana: credit account is %d bytes, want %d", len(data), LedgerAccountSize)
	}
	if string(data[0:4]) != ledgerAccountMagic {
		return LedgerAccount{}, fmt.Errorf("solana: account magic %q is not %q", data[0:4], ledgerAccountMagic)
	}
	var a LedgerAccount
	a.Minted = binary.LittleEndian.Uint64(data[4:12])
	a.Spent = binary.LittleEndian.Uint64(data[12:20])
	a.Locked = binary.LittleEndian.Uint64(data[20:28])
	copy(a.LastRoot[:], data[28:60])
	a.LastUsageID = binary.LittleEndian.Uint64(data[60:68])
	a.BatchCount = binary.LittleEndian.Uint64(data[68:76])
	return a, nil
}

// Encode is the inverse of DecodeLedgerAccount. The daemon never writes an
// account directly - only the program does - but a round-tripped golden byte
// string is how the layout stays pinned to the Rust side.
func (a LedgerAccount) Encode() []byte {
	out := make([]byte, LedgerAccountSize)
	copy(out[0:4], ledgerAccountMagic)
	binary.LittleEndian.PutUint64(out[4:12], a.Minted)
	binary.LittleEndian.PutUint64(out[12:20], a.Spent)
	binary.LittleEndian.PutUint64(out[20:28], a.Locked)
	copy(out[28:60], a.LastRoot[:])
	binary.LittleEndian.PutUint64(out[60:68], a.LastUsageID)
	binary.LittleEndian.PutUint64(out[68:76], a.BatchCount)
	return out
}

// LedgerConfig is the program's single config account: the authority allowed
// to mint and settle, plus the mint and treasury those instructions are meant
// to be about. Reading it is how an operator confirms what a deployment
// actually bound.
type LedgerConfig struct {
	Authority solana.PublicKey
	USDC      solana.PublicKey
	Treasury  solana.PublicKey
}

// DecodeLedgerConfig parses the program's fixed 100-byte config layout. The
// magic matters here more than anywhere else: a config bound to the wrong
// mint is a silent misconfiguration, and this is the check that finds it.
func DecodeLedgerConfig(data []byte) (LedgerConfig, error) {
	if len(data) < ledgerConfigSize {
		return LedgerConfig{}, fmt.Errorf("solana: config account is %d bytes, want %d", len(data), ledgerConfigSize)
	}
	if string(data[0:4]) != ledgerConfigMagic {
		return LedgerConfig{}, fmt.Errorf("solana: config magic %q is not %q", data[0:4], ledgerConfigMagic)
	}
	var c LedgerConfig
	copy(c.Authority[:], data[4:36])
	copy(c.USDC[:], data[36:68])
	copy(c.Treasury[:], data[68:100])
	return c, nil
}

// LedgerCreditAddress is the credit account PDA for one owner:
// [b"credit", owner]. Deriving it is what lets a client find an owner's
// balance from the owner's key alone, and what the program checks against.
func LedgerCreditAddress(programID, owner solana.PublicKey) (solana.PublicKey, error) {
	addr, _, err := solana.FindProgramAddress([][]byte{[]byte(ledgerCreditSeed), owner.Bytes()}, programID)
	return addr, err
}

// LedgerConfigAddress is the program's single config PDA: [b"config"].
func LedgerConfigAddress(programID solana.PublicKey) (solana.PublicKey, error) {
	addr, _, err := solana.FindProgramAddress([][]byte{[]byte(ledgerConfigSeed)}, programID)
	return addr, err
}

// PubkeyForAccountID inverts AccountIDFor. Account ids are the raw public key
// in lowercase hex, so the owner behind any ledger account id is recoverable
// in one step - which is what lets the settler, which speaks in account ids,
// commit a root against the right on-chain account.
func PubkeyForAccountID(accountID string) (solana.PublicKey, error) {
	raw := strings.TrimPrefix(accountID, "0x")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("solana: account id %q is not hex: %w", accountID, err)
	}
	if len(b) != 32 {
		return solana.PublicKey{}, fmt.Errorf("solana: account id %q decodes to %d bytes, want 32", accountID, len(b))
	}
	var pk solana.PublicKey
	copy(pk[:], b)
	return pk, nil
}

// --- instruction data (pure; pinned by TestLedgerWireFormatMatchesTheProgram) ---

func ledgerU64Data(tag byte, amount uint64) []byte {
	out := make([]byte, 9)
	out[0] = tag
	binary.LittleEndian.PutUint64(out[1:9], amount)
	return out
}

func ledgerCommitAndSettleData(root [32]byte, first, last, credits uint64) []byte {
	out := make([]byte, 1+32+8+8+8)
	out[0] = ixLedgerCommitAndSettle
	copy(out[1:33], root[:])
	binary.LittleEndian.PutUint64(out[33:41], first)
	binary.LittleEndian.PutUint64(out[41:49], last)
	binary.LittleEndian.PutUint64(out[49:57], credits)
	return out
}

func ledgerInitConfigData(authority, usdcMint, treasury solana.PublicKey) []byte {
	out := make([]byte, 1+32*3)
	out[0] = ixLedgerInitConfig
	copy(out[1:33], authority[:])
	copy(out[33:65], usdcMint[:])
	copy(out[65:97], treasury[:])
	return out
}

// --- client surface ---

// LedgerEnabled reports whether an on-chain ledger program is configured. When
// it is, settlement commits the usage root and burns inside the program;
// when it is not, the daemon keeps the Memo commit it shipped with.
func (c *Client) LedgerEnabled() bool { return c.ledger != nil }

// LedgerProgram returns the configured program id, or the zero key.
func (c *Client) LedgerProgram() solana.PublicKey {
	if c.ledger == nil {
		return solana.PublicKey{}
	}
	return *c.ledger
}

func (c *Client) requireLedger() (solana.PublicKey, error) {
	if c.ledger == nil {
		return solana.PublicKey{}, fmt.Errorf("solana: no ledger program configured (set SLOTFARE_LEDGER_PROGRAM)")
	}
	return *c.ledger, nil
}

// sendLedger signs and submits one ledger transaction. It mirrors the Memo
// path's commitment handling: the blockhash and the preflight are both at
// `confirmed`, because a plain send leaves preflight at the node's default
// (finalized) and rejects a fresh blockhash.
func (c *Client) sendLedger(ctx context.Context, ixs []solana.Instruction) (solana.Signature, error) {
	if c.payer == nil {
		return solana.Signature{}, fmt.Errorf("solana: no fee payer configured")
	}
	bh, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("solana: latest blockhash: %w", err)
	}
	tx, err := solana.NewTransaction(ixs, bh.Value.Blockhash, solana.TransactionPayer(c.payer.PublicKey()))
	if err != nil {
		return solana.Signature{}, fmt.Errorf("solana: build ledger tx: %w", err)
	}
	if _, err := tx.Sign(func(solana.PublicKey) *solana.PrivateKey { return &c.payer }); err != nil {
		return solana.Signature{}, fmt.Errorf("solana: sign ledger tx: %w", err)
	}
	sig, err := c.rpc.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		return solana.Signature{}, fmt.Errorf("solana: send ledger tx: %w", err)
	}
	return sig, nil
}

// LedgerInitConfig installs the program's authority, USDC mint and treasury.
// The payer must be the authority: the program only lets a signer install
// itself, and the first call wins, so this runs once as part of deployment.
func (c *Client) LedgerInitConfig(ctx context.Context) (solana.Signature, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, err
	}
	mint, err := solana.PublicKeyFromBase58(c.cfg.Mint)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("solana: ledger config mint: %w", err)
	}
	treasury, err := solana.PublicKeyFromBase58(c.cfg.Treasury)
	if err != nil {
		return solana.Signature{}, fmt.Errorf("solana: ledger config treasury: %w", err)
	}
	config, err := LedgerConfigAddress(programID)
	if err != nil {
		return solana.Signature{}, err
	}
	authority := c.payer.PublicKey()
	ix := solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(authority, true, true),
			solana.NewAccountMeta(config, true, false),
			solana.NewAccountMeta(solana.SystemProgramID, false, false),
		},
		ledgerInitConfigData(authority, mint, treasury),
	)
	return c.sendLedger(ctx, []solana.Instruction{ix})
}

// readAccount fetches one program-owned account's bytes at `confirmed`, the
// same commitment `sendLedger` fetches its blockhash and preflights at.
//
// The commitment is not a detail. solana-Go's plain GetAccountInfo leaves
// commitment unset, so the node answers at its own default of `finalized`,
// which trails `confirmed` by 31 slots. Reading there makes an account the
// program has just created look absent - which turns a re-run of the deploy
// flow into a second InitConfig, and a freshly credited balance into a
// re-mint. The bool reports existence; absence is not an error, because an
// owner who has never deposited has no account and that is normal.
func (c *Client) readAccount(ctx context.Context, addr solana.PublicKey) ([]byte, bool, error) {
	res, err := c.rpc.GetAccountInfoWithOpts(ctx, addr, &rpc.GetAccountInfoOpts{
		Encoding:   solana.EncodingBase64,
		Commitment: rpc.CommitmentConfirmed,
	})
	if err != nil {
		if errors.Is(err, rpc.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("solana: read account %s: %w", addr, err)
	}
	return res.Value.Data.GetBinary(), true, nil
}

// LedgerAccount reads one owner's on-chain position. The bool reports whether
// the account exists; a missing account is not an error, it is an owner who
// has never deposited.
func (c *Client) LedgerAccount(ctx context.Context, owner solana.PublicKey) (LedgerAccount, bool, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return LedgerAccount{}, false, err
	}
	addr, err := LedgerCreditAddress(programID, owner)
	if err != nil {
		return LedgerAccount{}, false, err
	}
	raw, ok, err := c.readAccount(ctx, addr)
	if err != nil || !ok {
		return LedgerAccount{}, false, err
	}
	acct, err := DecodeLedgerAccount(raw)
	if err != nil {
		return LedgerAccount{}, false, err
	}
	return acct, true, nil
}

// LedgerConfigRead reads the program's config account. The bool reports
// whether it has been initialised; InitConfig is first-caller-wins, so a
// deployment flow checks this before submitting one.
func (c *Client) LedgerConfigRead(ctx context.Context) (LedgerConfig, bool, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return LedgerConfig{}, false, err
	}
	addr, err := LedgerConfigAddress(programID)
	if err != nil {
		return LedgerConfig{}, false, err
	}
	raw, ok, err := c.readAccount(ctx, addr)
	if err != nil || !ok {
		return LedgerConfig{}, false, err
	}
	cfg, err := DecodeLedgerConfig(raw)
	if err != nil {
		return LedgerConfig{}, false, err
	}
	return cfg, true, nil
}

// initCreditIx is the account-creating instruction for one owner.
func (c *Client) initCreditIx(programID, owner solana.PublicKey) (solana.Instruction, error) {
	credit, err := LedgerCreditAddress(programID, owner)
	if err != nil {
		return nil, err
	}
	return solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(c.payer.PublicKey(), true, true),
			solana.NewAccountMeta(owner, false, false),
			solana.NewAccountMeta(credit, true, false),
			solana.NewAccountMeta(solana.SystemProgramID, false, false),
		},
		[]byte{ixLedgerInitCredit},
	), nil
}

// LedgerEnsureCredit opens the owner's credit account if it is missing. The
// bool reports whether this call created it.
func (c *Client) LedgerEnsureCredit(ctx context.Context, owner solana.PublicKey) (solana.Signature, bool, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, false, err
	}
	if _, ok, err := c.LedgerAccount(ctx, owner); err != nil {
		return solana.Signature{}, false, err
	} else if ok {
		return solana.Signature{}, false, nil
	}
	ix, err := c.initCreditIx(programID, owner)
	if err != nil {
		return solana.Signature{}, false, err
	}
	sig, err := c.sendLedger(ctx, []solana.Instruction{ix})
	return sig, err == nil, err
}

// LedgerLock reserves spendable credits for an open listing, making them
// unspendable on chain. The owner signs: the program refuses to lock someone
// else's credits, which is what stops a listing being fabricated over a
// balance the seller does not hold.
func (c *Client) LedgerLock(ctx context.Context, owner solana.PublicKey, credits uint64) (solana.Signature, error) {
	return c.ledgerOwnerU64(ctx, ixLedgerLock, owner, credits)
}

// LedgerUnlock returns locked credits to spendable, cancelling a listing.
func (c *Client) LedgerUnlock(ctx context.Context, owner solana.PublicKey, credits uint64) (solana.Signature, error) {
	return c.ledgerOwnerU64(ctx, ixLedgerUnlock, owner, credits)
}

// ledgerOwnerU64 is the shared shape of the two owner-signed amount
// instructions: accounts `[owner (signer), credit]`.
func (c *Client) ledgerOwnerU64(ctx context.Context, tag byte, owner solana.PublicKey, credits uint64) (solana.Signature, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, err
	}
	credit, err := LedgerCreditAddress(programID, owner)
	if err != nil {
		return solana.Signature{}, err
	}
	ix := solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(owner, true, false),
			solana.NewAccountMeta(credit, true, false),
		},
		ledgerU64Data(tag, credits),
	)
	return c.sendLedger(ctx, []solana.Instruction{ix})
}

// LedgerReconcileMint brings the on-chain balance up to an absolute credited
// total, creating the account first if needed. It is deliberately absolute
// rather than a delta: the deposit rail re-derives totals from chain history,
// so a restart or a repeated rescan reconciles to the same number instead of
// double-minting. The bool reports whether anything was submitted.
//
// Only whole credits move on chain, which is the same truncation the settler
// already applies to its own batches; the sub-credit remainder stays local.
func (c *Client) LedgerReconcileMint(ctx context.Context, owner solana.PublicKey, targetCredits uint64) (solana.Signature, bool, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, false, err
	}
	acct, exists, err := c.LedgerAccount(ctx, owner)
	if err != nil {
		return solana.Signature{}, false, err
	}
	var ixs []solana.Instruction
	if !exists {
		ix, err := c.initCreditIx(programID, owner)
		if err != nil {
			return solana.Signature{}, false, err
		}
		ixs = append(ixs, ix)
	} else if acct.Minted >= targetCredits {
		// Already at or ahead of the target: nothing to do. This is the
		// path a replayed rescan with no new deposit takes.
		return solana.Signature{}, false, nil
	}
	var minted uint64
	if exists {
		minted = acct.Minted
	}
	if delta := targetCredits - minted; delta > 0 {
		credit, err := LedgerCreditAddress(programID, owner)
		if err != nil {
			return solana.Signature{}, false, err
		}
		config, err := LedgerConfigAddress(programID)
		if err != nil {
			return solana.Signature{}, false, err
		}
		ixs = append(ixs, solana.NewInstruction(
			programID,
			solana.AccountMetaSlice{
				solana.NewAccountMeta(c.payer.PublicKey(), true, true),
				solana.NewAccountMeta(config, false, false),
				solana.NewAccountMeta(owner, false, false),
				solana.NewAccountMeta(credit, true, false),
			},
			ledgerU64Data(ixLedgerCredit, delta),
		))
	}
	if len(ixs) == 0 {
		return solana.Signature{}, false, nil
	}
	sig, err := c.sendLedger(ctx, ixs)
	return sig, err == nil, err
}

// LedgerCommitAndSettle commits a batch's usage root and burns the credits it
// covers, in one instruction the program applies atomically. The root cannot
// be committed twice, the batch must advance the account's usage frontier, and
// the burn cannot touch locked credits - each enforced by the program's own state,
// not by this client.
func (c *Client) LedgerCommitAndSettle(ctx context.Context, owner solana.PublicKey, root [32]byte, firstUsageID, lastUsageID, credits uint64) (solana.Signature, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, err
	}
	credit, err := LedgerCreditAddress(programID, owner)
	if err != nil {
		return solana.Signature{}, err
	}
	config, err := LedgerConfigAddress(programID)
	if err != nil {
		return solana.Signature{}, err
	}
	ix := solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(c.payer.PublicKey(), true, true),
			solana.NewAccountMeta(config, false, false),
			solana.NewAccountMeta(owner, false, false),
			solana.NewAccountMeta(credit, true, false),
		},
		ledgerCommitAndSettleData(root, firstUsageID, lastUsageID, credits),
	)
	return c.sendLedger(ctx, []solana.Instruction{ix})
}

// ledgerReconcile is the one call the mirror makes, as a function field so the
// skip-and-converge logic is testable without a cluster.
func (c *Client) ledgerReconcile(ctx context.Context, owner solana.PublicKey, targetCredits uint64) (solana.Signature, bool, error) {
	return c.LedgerReconcileMint(ctx, owner, targetCredits)
}

// LedgerMirror keeps the on-chain credit accounts in step with the deposit
// rail's absolute minted totals. The watcher reports totals rather than deltas
// precisely so a replay converges, and the mirror inherits that property: it
// only ever submits a total beyond the highest one it has already confirmed,
// so a rescan that finds no new deposit costs nothing.
//
// A mirror failure never fails the deposit. The local ledger stays the
// reservation and display authority, and the next pass reconciles the chain to
// the same number - the on-chain balance converges rather than being required
// to be instantaneous. What the lag costs is settlement: the settler may find
// less on-chain spendable than local pending covers and simply retry next tick,
// which loses no usage.
//
// The map is guarded because the watcher's rescans, and any future caller, may
// not share a goroutine.
type LedgerMirror struct {
	reconcile func(ctx context.Context, owner solana.PublicKey, targetCredits uint64) (solana.Signature, bool, error)

	mu   sync.Mutex
	seen map[string]uint64
}

// NewLedgerMirror builds a mirror over a ledger-enabled client. It returns nil
// when no program is configured, so a caller can hold the result and call Sync
// unconditionally behind a nil check.
func NewLedgerMirror(c *Client) *LedgerMirror {
	if c == nil || !c.LedgerEnabled() {
		return nil
	}
	return &LedgerMirror{reconcile: c.ledgerReconcile, seen: make(map[string]uint64)}
}

// Sync mirrors one account's absolute credited total, in whole credits. It is
// safe to call repeatedly with an unchanged or lower total.
func (m *LedgerMirror) Sync(ctx context.Context, accountID string, targetCredits uint64) error {
	m.mu.Lock()
	if last, ok := m.seen[accountID]; ok && last >= targetCredits {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	owner, err := PubkeyForAccountID(accountID)
	if err != nil {
		return err
	}
	// A successful reconcile leaves the chain holding at least `targetCredits`,
	// whether or not it had anything to submit. Only a success is recorded, so
	// a failed pass is retried by the next rescan instead of being skipped
	// forever.
	if _, _, err := m.reconcile(ctx, owner, targetCredits); err != nil {
		return err
	}
	m.mu.Lock()
	if targetCredits > m.seen[accountID] {
		m.seen[accountID] = targetCredits
	}
	m.mu.Unlock()
	return nil
}

// LedgerTransfer moves spendable credits between two owners in one
// instruction: the market sale. Both sides are checked to be the named
// owners' credit PDAs, so credits cannot be pushed into or pulled out of an
// account the signer does not control.
func (c *Client) LedgerTransfer(ctx context.Context, from, to solana.PublicKey, credits uint64) (solana.Signature, error) {
	programID, err := c.requireLedger()
	if err != nil {
		return solana.Signature{}, err
	}
	fromCredit, err := LedgerCreditAddress(programID, from)
	if err != nil {
		return solana.Signature{}, err
	}
	toCredit, err := LedgerCreditAddress(programID, to)
	if err != nil {
		return solana.Signature{}, err
	}
	ix := solana.NewInstruction(
		programID,
		solana.AccountMetaSlice{
			solana.NewAccountMeta(from, true, true),
			solana.NewAccountMeta(fromCredit, true, false),
			solana.NewAccountMeta(to, false, false),
			solana.NewAccountMeta(toCredit, true, false),
		},
		ledgerU64Data(ixLedgerTransfer, credits),
	)
	return c.sendLedger(ctx, []solana.Instruction{ix})
}
