package solana

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func key(t *testing.T, seed byte) solana.PublicKey {
	t.Helper()
	var b [32]byte
	for i := range b {
		b[i] = seed
	}
	return solana.PublicKeyFromBytes(b[:])
}

func tb(idx uint16, mint, owner solana.PublicKey, amount string) rpc.TokenBalance {
	o := owner
	return rpc.TokenBalance{
		AccountIndex:  idx,
		Owner:         &o,
		Mint:          mint,
		UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: 6},
	}
}

// keysAt builds an account-key slice long enough for the largest index, so
// idx maps to keys[idx] exactly like a transaction's account array.
func keysAt(largest int, byIndex map[int]solana.PublicKey) solana.PublicKeySlice {
	keys := make(solana.PublicKeySlice, largest+1)
	for i := range keys {
		keys[i] = solana.PublicKey{}
		if k, ok := byIndex[i]; ok {
			keys[i] = k
		}
	}
	return keys
}

func newTestWatcher(t *testing.T, treasury solana.PublicKey, mint solana.PublicKey) *Watcher {
	t.Helper()
	return &Watcher{
		cfg: Config{
			Treasury: treasury.String(),
			Mint:     mint.String(),
		},
		mint:     mint,
		treasury: treasury,
	}
}

func TestParseDepositPlainTransfer(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	sender := key(t, 3)
	senderATA := key(t, 30)
	w := newTestWatcher(t, treasuryATA, mint)

	// Sender's wallet ATA: 2.0 -> 0.5 USDC. Treasury token account: 0 -> 1.5.
	keys := keysAt(7, map[int]solana.PublicKey{4: senderATA, 7: treasuryATA})
	pre := []rpc.TokenBalance{
		tb(4, mint, sender, "2000000"),
		tb(7, mint, sender, "0"),
	}
	post := []rpc.TokenBalance{
		tb(4, mint, sender, "500000"),
		tb(7, mint, sender, "1500000"),
	}
	d, err := w.parseDeposit("sig1", keys, pre, post)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected a deposit, got nil")
	}
	if d.From != sender.String() {
		t.Errorf("From = %s, want %s", d.From, sender)
	}
	if d.AccountID != AccountIDFor(sender) {
		t.Errorf("AccountID = %s, want %s", d.AccountID, AccountIDFor(sender))
	}
	if d.USDCBase != 1_500_000 {
		t.Errorf("USDCBase = %d, want 1500000", d.USDCBase)
	}
	// 1.5 USDC = 1,500 credits = 15,000,000 micro-credits.
	if got := d.microCredits(); got != 15_000_000 {
		t.Errorf("microCredits = %d, want 15000000", got)
	}
	if want := "0x" + hex.EncodeToString(sender[:]); d.AccountID != want {
		t.Errorf("AccountID = %s, want %s", d.AccountID, want)
	}
}

func TestParseDepositMatchesTokenAccountNotOwner(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	// An unrelated wallet whose OWNER equals the treasury's config must not
	// count: only the treasury's own token account credits.
	stranger := key(t, 3)
	strangerATA := key(t, 31)
	w := newTestWatcher(t, treasuryATA, mint)

	keys := keysAt(7, map[int]solana.PublicKey{4: strangerATA})
	pre := []rpc.TokenBalance{tb(4, mint, stranger, "0")}
	post := []rpc.TokenBalance{tb(4, mint, stranger, "1500000")}
	d, err := w.parseDeposit("sig-owner", keys, pre, post)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("expected nil deposit for a non-treasury token account, got %+v", d)
	}
}

func TestParseDepositIgnoresUnrelatedMoves(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	other := key(t, 4)
	otherATA := key(t, 40)
	w := newTestWatcher(t, treasuryATA, mint)

	// Another wallet's ATA changes; the treasury token account has no leg.
	keys := keysAt(7, map[int]solana.PublicKey{4: otherATA, 7: treasuryATA})
	pre := []rpc.TokenBalance{tb(4, mint, other, "9000000")}
	post := []rpc.TokenBalance{tb(4, mint, other, "8000000")}
	d, err := w.parseDeposit("sig2", keys, pre, post)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("expected nil deposit, got %+v", d)
	}
}

func TestParseDepositIgnoresZeroCredit(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	w := newTestWatcher(t, treasuryATA, mint)

	// An ATA creation round-trips a zero balance: a post entry exists with
	// amount 0 and no pre entry. Nothing was credited.
	keys := keysAt(9, map[int]solana.PublicKey{9: treasuryATA})
	post := []rpc.TokenBalance{tb(9, mint, key(t, 1), "0")}
	d, err := w.parseDeposit("sig3", keys, nil, post)
	if err != nil {
		t.Fatal(err)
	}
	if d != nil {
		t.Fatalf("expected nil deposit, got %+v", d)
	}
}

func TestParseDepositFallsBackToLargestDebit(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	sender := key(t, 3)
	fees := key(t, 5)
	senderATA := key(t, 30)
	feesATA := key(t, 50)
	w := newTestWatcher(t, treasuryATA, mint)

	// A split send: sender debited 2.0, a fee leg debited 0.01, treasury
	// credited 1.99. No single leg matches the credit exactly, so the
	// sender must be the largest debit that is not the treasury.
	keys := keysAt(7, map[int]solana.PublicKey{4: senderATA, 5: feesATA, 7: treasuryATA})
	pre := []rpc.TokenBalance{
		tb(4, mint, sender, "2000000"),
		tb(5, mint, fees, "10000"),
		tb(7, mint, fees, "0"),
	}
	post := []rpc.TokenBalance{
		tb(4, mint, sender, "0"),
		tb(5, mint, fees, "0"),
		tb(7, mint, fees, "1990000"),
	}
	d, err := w.parseDeposit("sig4", keys, pre, post)
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("expected a deposit, got nil")
	}
	if d.From != sender.String() {
		t.Errorf("From = %s, want %s (largest debit)", d.From, sender)
	}
	if d.USDCBase != 1_990_000 {
		t.Errorf("USDCBase = %d, want 1990000", d.USDCBase)
	}
}

func TestParseDepositRejectsOutOfRangeIndex(t *testing.T) {
	treasuryATA := key(t, 1)
	mint := key(t, 2)
	w := newTestWatcher(t, treasuryATA, mint)

	keys := keysAt(4, map[int]solana.PublicKey{})
	post := []rpc.TokenBalance{tb(9, mint, treasuryATA, "1000000")}
	if _, err := w.parseDeposit("sig5", keys, nil, post); err == nil {
		t.Fatal("expected an out-of-range error, got nil")
	}
}

func TestAccountIDForIsRawPubkeyHex(t *testing.T) {
	pub := key(t, 9)
	want := "0x" + hex.EncodeToString(pub[:])
	if got := AccountIDFor(pub); got != want {
		t.Errorf("AccountIDFor = %s, want %s", got, want)
	}
	if len(want) != 66 {
		t.Errorf("account id length = %d, want 66", len(want))
	}
}

// TestHealthTracksScanLifecycle checks the health snapshot the console's
// attestation card reads: a success stamps lastScan and head and clears a
// remembered failure; a failure keeps the last success's stamp so the lag
// keeps growing, and only the error moves.
func TestHealthTracksScanLifecycle(t *testing.T) {
	w := &Watcher{}
	if h := w.Health(); !h.LastScan.IsZero() || h.Head != 0 || h.Err != "" {
		t.Fatalf("fresh health = %+v, want zero", h)
	}

	w.noteScan(42)
	first := w.Health()
	if first.LastScan.IsZero() || first.Head != 42 || first.Err != "" {
		t.Fatalf("after success health = %+v, want stamped head 42", first)
	}

	w.noteScanErr(errBoom)
	failed := w.Health()
	if failed.LastScan != first.LastScan {
		t.Errorf("failure moved lastScan %v -> %v; lag must grow from the last success", first.LastScan, failed.LastScan)
	}
	if failed.Head != 42 {
		t.Errorf("failure changed head to %d, want the last observed 42", failed.Head)
	}
	if failed.Err == "" {
		t.Error("failure not remembered in Err")
	}

	w.noteScan(0)
	recovered := w.Health()
	if recovered.Err != "" {
		t.Errorf("recovery left Err = %q, want cleared", recovered.Err)
	}
	if recovered.Head != 42 {
		t.Errorf("head read of 0 overwrote %d; a missing head read must not zero it", recovered.Head)
	}
	if recovered.LastScan.Before(failed.LastScan) {
		t.Errorf("recovery did not advance lastScan")
	}
}

// errBoom is a stable failure for the health-lifecycle test.
var errBoom = errors.New("signatures: connection refused")
