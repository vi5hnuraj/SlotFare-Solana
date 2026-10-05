package solana

import (
	"context"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// marketTestClient builds a ledger-enabled client with no cluster behind it:
// the instruction builders and PDA derivations never touch the network, so a
// key pair of fake ids is enough to pin the wire format.
func marketTestClient(t *testing.T) (*Client, solana.PublicKey) {
	t.Helper()
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("random key: %v", err)
	}
	program, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("random key: %v", err)
	}
	c, err := NewClient(Config{
		RPC:           "http://127.0.0.1:8899",
		FeePayer:      key.String(),
		LedgerProgram: program.PublicKey().String(),
	}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, program.PublicKey()
}

func TestLedgerMarketWireFormatMatchesTheProgram(t *testing.T) {
	// instruction.rs encodes List as tag(1) credits(8) price(8) nonce(8)
	// pay_mint(32) = 57 bytes, and Buy / CancelList as a bare tag. The Rust
	// side has its own golden test; these are the same bytes.
	var mint solana.PublicKey
	for i := range mint {
		mint[i] = byte(i + 1)
	}
	got := ledgerListData(0x0807060504030201, 0x1817161514131211, 0x2827262524232221, mint)
	if len(got) != 57 {
		t.Fatalf("List payload is %d bytes, want 57", len(got))
	}
	if got[0] != ixLedgerList {
		t.Errorf("tag = %d, want %d", got[0], ixLedgerList)
	}
	if got[1] != 0x01 || got[8] != 0x08 {
		t.Errorf("credits are not little-endian: % x", got[1:9])
	}
	if got[9] != 0x11 || got[16] != 0x18 {
		t.Errorf("price is not at byte 9: % x", got[9:17])
	}
	if got[17] != 0x21 || got[24] != 0x28 {
		t.Errorf("nonce is not at byte 17: % x", got[17:25])
	}
	if got[25] != 0x01 || got[56] != 0x20 {
		t.Errorf("pay_mint is not at byte 25: % x", got[25:57])
	}
	if ixLedgerBuy != 8 || ixLedgerCancelList != 9 {
		t.Errorf("Buy/CancelList tags drifted: %d, %d", ixLedgerBuy, ixLedgerCancelList)
	}
}

func TestLedgerListingLayoutMatchesTheProgram(t *testing.T) {
	// state.rs writes magic(4) seller(32) buyer(32) pay_mint(32) credits(8)
	// price(8) nonce(8) state(1), and LISTING_SIZE pins 125. The three keys are
	// the same shape, so a swapped offset reads happily and buys the wrong
	// thing.
	data := make([]byte, 0, LedgerListingSize)
	data = append(data, []byte(ledgerListingMagic)...)
	for i := 0; i < 96; i++ {
		switch {
		case i < 32:
			data = append(data, 0xA5)
		case i < 64:
			data = append(data, 0xB6)
		default:
			data = append(data, 0xC7)
		}
	}
	var buf [16]byte
	putU64 := func(v uint64) {
		putU64LE(buf[:], v)
		data = append(data, buf[:8]...)
	}
	putU64(0x1122334455667788) // credits
	putU64(0x99AABBCCDDEEFF00) // price
	putU64(0x0102030405060708) // nonce
	data = append(data, LedgerListingOpen)
	if len(data) != LedgerListingSize {
		t.Fatalf("built %d bytes, want %d", len(data), LedgerListingSize)
	}
	l, err := DecodeLedgerListing(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i := range l.Seller {
		if l.Seller[i] != 0xA5 || l.Buyer[i] != 0xB6 || l.PayMint[i] != 0xC7 {
			t.Fatalf("seller/buyer/mint offsets are wrong at %d", i)
		}
	}
	if l.Credits != 0x1122334455667788 || l.Price != 0x99AABBCCDDEEFF00 || l.Nonce != 0x0102030405060708 {
		t.Errorf("terms are wrong: credits=%#x price=%#x nonce=%#x", l.Credits, l.Price, l.Nonce)
	}
	if !l.Open() || l.State != LedgerListingOpen {
		t.Errorf("state = %d, want open", l.State)
	}

	// The magic is the protection against reading one account kind as another.
	if _, err := DecodeLedgerListing(append([]byte("SFCA"), data[4:]...)); err == nil {
		t.Error("a credit-account magic must not decode as a listing")
	}
	if _, err := DecodeLedgerListing(data[:60]); err == nil {
		t.Error("a short buffer must be rejected")
	}
	bad := append([]byte(nil), data...)
	bad[124] = 9
	if _, err := DecodeLedgerListing(bad); err == nil {
		t.Error("an unknown state byte must be rejected")
	}
}

func TestLedgerListingAndEscrowAddressesAreDeterministic(t *testing.T) {
	_, program := marketTestClient(t)
	a := solana.NewWallet().PublicKey()
	b := solana.NewWallet().PublicKey()

	l1 := mustListingAddr(t, program, a, 1)
	l2 := mustListingAddr(t, program, a, 2)
	lb := mustListingAddr(t, program, b, 1)
	if l1 == l2 {
		t.Error("two nonces must derive different listings")
	}
	if l1 == lb {
		t.Error("two sellers must derive different listings")
	}
	if l1 != mustListingAddr(t, program, a, 1) {
		t.Error("derivation is not deterministic")
	}
	credit, err := LedgerCreditAddress(program, a)
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	if l1 == credit {
		t.Error("a listing must not share the credit account's address")
	}
	escrow, err := LedgerEscrowAddress(program, l1)
	if err != nil {
		t.Fatalf("escrow address: %v", err)
	}
	if escrow == l1 || escrow == credit {
		t.Error("a listing's escrow must be its own account")
	}
}

func mustListingAddr(t *testing.T, program, seller solana.PublicKey, nonce uint64) solana.PublicKey {
	t.Helper()
	addr, err := LedgerListingAddress(program, seller, nonce)
	if err != nil {
		t.Fatalf("listing address: %v", err)
	}
	return addr
}

func TestLedgerBuyIxCarriesTheAccountCreateFirst(t *testing.T) {
	// A seller who has never held the mint has no token account, so the buyer's
	// transaction must open one before the program tries to pay into it.
	c, _ := marketTestClient(t)
	buyer := solana.NewWallet().PublicKey()
	seller := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()

	ixs, err := c.LedgerBuyIx(buyer, seller, 7, mint)
	if err != nil {
		t.Fatalf("buy ix: %v", err)
	}
	if len(ixs) != 2 {
		t.Fatalf("buy carries %d instructions, want 2 (create then buy)", len(ixs))
	}
	if ixs[0].ProgramID() != solana.SPLAssociatedTokenAccountProgramID {
		t.Errorf("first instruction is %s, want the associated-token program", ixs[0].ProgramID())
	}
	buy := ixs[1]
	if buy.ProgramID() != c.LedgerProgram() {
		t.Errorf("buy program = %s, want %s", buy.ProgramID(), c.LedgerProgram())
	}
	accounts := buy.Accounts()
	if len(accounts) != 9 {
		t.Fatalf("buy names %d accounts, want 9", len(accounts))
	}
	if accounts[0].PublicKey != buyer || !accounts[0].IsSigner {
		t.Error("account 0 must be the signing buyer")
	}
	if accounts[5].PublicKey == accounts[6].PublicKey {
		t.Error("the buyer's and seller's token accounts must differ")
	}
	data, err := buy.Data()
	if err != nil {
		t.Fatalf("buy data: %v", err)
	}
	if len(data) != 1 || data[0] != ixLedgerBuy {
		t.Errorf("buy payload = % x, want the bare tag %d", data, ixLedgerBuy)
	}
}

func TestLedgerMarketInstructionsRequireAProgram(t *testing.T) {
	// Unconfigured, every market builder must refuse rather than build an
	// instruction pointed at the zero key.
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("random key: %v", err)
	}
	c, err := NewClient(Config{RPC: "http://127.0.0.1:8899", FeePayer: key.String()}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	owner := key.PublicKey()
	if _, err := c.LedgerListIx(owner, 1, 1, 1, owner); err == nil {
		t.Error("List must refuse without a program")
	}
	if _, err := c.LedgerCancelListIx(owner, 1); err == nil {
		t.Error("CancelList must refuse without a program")
	}
	if _, err := c.LedgerBuyIx(owner, owner, 1, owner); err == nil {
		t.Error("Buy must refuse without a program")
	}
	if _, _, err := c.LedgerListingRead(context.Background(), owner, 1); err == nil {
		t.Error("listing read must refuse without a program")
	}
	if _, err := c.PrepareLedgerTx(context.Background(), owner, nil); err == nil {
		t.Error("preparing an empty instruction set must refuse")
	}
}

// putU64LE writes v little-endian into the first eight bytes of b.
func putU64LE(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
