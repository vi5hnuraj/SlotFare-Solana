package solana

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// The goldens here are the daemon's side of the contract with
// programs/slotfare-ledger. The Rust tests
// (instruction::tests::tags_are_stable_on_the_wire and
// init_config_fields_land_in_the_right_offsets) assert the same tag bytes and
// offsets, and the Rust decoder rejects a body whose length is not exactly
// what its tag requires, so a change on either side fails a test rather than
// silently misreading a live transaction.

func TestLedgerInstructionTagsMatchTheProgram(t *testing.T) {
	cases := []struct {
		name string
		got  byte
		want byte
	}{
		{"InitCredit", ixLedgerInitCredit, 0},
		{"Credit", ixLedgerCredit, 1},
		{"Lock", ixLedgerLock, 2},
		{"Unlock", ixLedgerUnlock, 3},
		{"CommitAndSettle", ixLedgerCommitAndSettle, 4},
		{"Transfer", ixLedgerTransfer, 5},
		{"InitConfig", ixLedgerInitConfig, 6},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s tag = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestLedgerInstructionLengthsMatchTheProgramDecoder(t *testing.T) {
	// instruction.rs computes the exact length each tag requires and rejects
	// anything else, so an encoder that adds or drops a byte is a rejected
	// transaction, not a reinterpreted one.
	key := solana.PublicKey{}
	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"InitCredit", []byte{ixLedgerInitCredit}, 1},
		{"Credit", ledgerU64Data(ixLedgerCredit, 1), 9},
		{"Lock", ledgerU64Data(ixLedgerLock, 1), 9},
		{"Unlock", ledgerU64Data(ixLedgerUnlock, 1), 9},
		{"Transfer", ledgerU64Data(ixLedgerTransfer, 1), 9},
		{"CommitAndSettle", ledgerCommitAndSettleData([32]byte{}, 0, 0, 1), 57},
		{"InitConfig", ledgerInitConfigData(key, key, key), 97},
	}
	for _, c := range cases {
		if len(c.data) != c.want {
			t.Errorf("%s encodes to %d bytes, want %d", c.name, len(c.data), c.want)
		}
	}
}

func TestLedgerCommitAndSettleFieldOffsets(t *testing.T) {
	var root [32]byte
	for i := range root {
		root[i] = byte(i)
	}
	data := ledgerCommitAndSettleData(root, 1, 4096, 987654)
	if len(data) != 57 {
		t.Fatalf("length = %d, want 57", len(data))
	}
	// root, then three little-endian u64s, per instruction.rs's
	// TAG_COMMIT_AND_SETTLE layout.
	if got := hex.EncodeToString(data[1:33]); got != hex.EncodeToString(root[:]) {
		t.Errorf("root bytes = %s", got)
	}
	wantTail := "0100000000000000" + "0010000000000000" + "06120f0000000000"
	if got := hex.EncodeToString(data[33:]); got != wantTail {
		t.Errorf("usage range + credits = %s, want %s", got, wantTail)
	}
}

func TestLedgerInitConfigFieldOffsets(t *testing.T) {
	// The three pubkeys are the same shape, so a swapped offset would
	// round-trip happily and bind the wrong mint.
	a := solana.PublicKey{}
	b := solana.PublicKey{}
	c := solana.PublicKey{}
	for i := range a {
		a[i] = 0xAA
		b[i] = 0xBB
		c[i] = 0xCC
	}
	data := ledgerInitConfigData(a, b, c)
	if len(data) != 97 {
		t.Fatalf("length = %d, want 97", len(data))
	}
	if data[0] != ixLedgerInitConfig {
		t.Errorf("tag = %d", data[0])
	}
	if data[1] != 0xAA || data[33] != 0xBB || data[65] != 0xCC {
		t.Errorf("offsets: %#x %#x %#x", data[1], data[33], data[65])
	}
	if data[32] != 0xAA || data[64] != 0xBB || data[96] != 0xCC {
		t.Errorf("fields overlap: %#x %#x %#x", data[32], data[64], data[96])
	}
}

func TestLedgerAccountGoldenBytes(t *testing.T) {
	// The 76-byte layout from state.rs: magic(4) minted(8) spent(8) locked(8)
	// last_root(32) last_usage_id(8) batch_count(8), all little-endian.
	var root [32]byte
	for i := range root {
		root[i] = byte(i)
	}
	a := LedgerAccount{
		Minted:      1,
		Spent:       2,
		Locked:      3,
		LastRoot:    root,
		LastUsageID: 4,
		BatchCount:  5,
	}
	want := "53464341" +
		"0100000000000000" +
		"0200000000000000" +
		"0300000000000000" +
		"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f" +
		"0400000000000000" +
		"0500000000000000"
	if got := hex.EncodeToString(a.Encode()); got != want {
		t.Errorf("encoded account =\n%s\nwant\n%s", got, want)
	}
	if len(want) != LedgerAccountSize*2 {
		t.Fatalf("golden is %d bytes, layout is %d", len(want)/2, LedgerAccountSize)
	}

	back, err := DecodeLedgerAccount(a.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back != a {
		t.Errorf("round trip = %+v, want %+v", back, a)
	}
}

func TestLedgerAccountRejectsForeignBytes(t *testing.T) {
	if _, err := DecodeLedgerAccount(make([]byte, LedgerAccountSize-1)); err == nil {
		t.Error("a short buffer must be rejected")
	}
	foreign := make([]byte, LedgerAccountSize)
	copy(foreign[0:4], "XXXX")
	if _, err := DecodeLedgerAccount(foreign); err == nil {
		t.Error("a foreign magic must be rejected")
	}
}

func TestLedgerSpendableClampsInsteadOfUnderflowing(t *testing.T) {
	// A corrupt or hostile account must never report a huge balance.
	if got := (LedgerAccount{Minted: 10, Spent: 20}).Spendable(); got != 0 {
		t.Errorf("spent > minted spendable = %d, want 0", got)
	}
	if got := (LedgerAccount{Minted: 10, Locked: 20}).Spendable(); got != 0 {
		t.Errorf("locked > minted spendable = %d, want 0", got)
	}
	if got := (LedgerAccount{Minted: 100, Spent: 40, Locked: 30}).Spendable(); got != 30 {
		t.Errorf("spendable = %d, want 30", got)
	}
}

func TestAccountIDRoundTripsThroughPubkeyForAccountID(t *testing.T) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PublicKey()
	back, err := PubkeyForAccountID(AccountIDFor(pub))
	if err != nil {
		t.Fatalf("PubkeyForAccountID: %v", err)
	}
	if back != pub {
		t.Errorf("round trip = %s, want %s", back, pub)
	}
	for _, bad := range []string{"", "0xzz", "0x0102", AccountIDFor(pub) + "00"} {
		if _, err := PubkeyForAccountID(bad); err == nil {
			t.Errorf("account id %q must be rejected", bad)
		}
	}
}

func TestLedgerCreditAddressIsOwnerSpecific(t *testing.T) {
	program := solana.PublicKey{}
	program[0] = 7
	one, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	two, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	a, err := LedgerCreditAddress(program, one.PublicKey())
	if err != nil {
		t.Fatalf("credit address: %v", err)
	}
	a2, _ := LedgerCreditAddress(program, one.PublicKey())
	if a != a2 {
		t.Error("credit address must be deterministic")
	}
	b, _ := LedgerCreditAddress(program, two.PublicKey())
	if a == b {
		t.Error("two owners must not share a credit address")
	}
	config, err := LedgerConfigAddress(program)
	if err != nil {
		t.Fatalf("config address: %v", err)
	}
	if config == a {
		t.Error("the config account must not collide with a credit account")
	}
}

func TestLedgerMethodsRequireAConfiguredProgram(t *testing.T) {
	// An unconfigured client must fail loudly instead of signing a
	// transaction against a zero program id.
	c := &Client{}
	if c.LedgerEnabled() {
		t.Fatal("a bare client must not report a ledger program")
	}
	if _, err := c.requireLedger(); err == nil {
		t.Error("requireLedger must fail when no program is configured")
	}
	if NewLedgerMirror(c) != nil {
		t.Error("NewLedgerMirror must return nil when no program is configured")
	}
	if _, err := c.CommitAndSettle(context.Background(), [32]byte{}, "0x"+hex.EncodeToString(make([]byte, 32)), 1, 2, 3); err == nil {
		t.Error("CommitAndSettle without a payer must fail")
	}
	if _, err := c.LedgerCommitAndSettle(context.Background(), solana.PublicKey{}, [32]byte{}, 1, 2, 3); err == nil {
		t.Error("LedgerCommitAndSettle without a program must fail")
	}
}

func TestLedgerMirrorSkipsConfirmedTotalsAndRetriesFailures(t *testing.T) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	accountID := AccountIDFor(key.PublicKey())

	var calls int
	var lastTarget uint64
	fail := true
	m := &LedgerMirror{
		reconcile: func(_ context.Context, owner solana.PublicKey, target uint64) (solana.Signature, bool, error) {
			calls++
			lastTarget = target
			if owner != key.PublicKey() {
				t.Errorf("mirrored owner = %s, want %s", owner, key.PublicKey())
			}
			if fail {
				return solana.Signature{}, false, errors.New("rpc down")
			}
			return solana.Signature{}, true, nil
		},
		seen: map[string]uint64{},
	}

	// A failure must surface and must not be recorded, so the next rescan
	// tries the same total again.
	if err := m.Sync(context.Background(), accountID, 2500); err == nil {
		t.Fatal("a failed reconcile must be reported")
	}
	if err := m.Sync(context.Background(), accountID, 2500); err == nil {
		t.Fatal("a failed reconcile must be retried")
	}
	if calls != 2 || lastTarget != 2500 {
		t.Fatalf("calls = %d, target = %d", calls, lastTarget)
	}

	// Once it lands, the same total is a no-op.
	fail = false
	if err := m.Sync(context.Background(), accountID, 2500); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := m.Sync(context.Background(), accountID, 2500); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := m.Sync(context.Background(), accountID, 100); err != nil {
		t.Fatalf("a lower total must be a no-op: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (unchanged and lower totals must not reconcile)", calls)
	}

	// A larger total does go out.
	if err := m.Sync(context.Background(), accountID, 3000); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if calls != 4 || lastTarget != 3000 {
		t.Errorf("calls = %d, target = %d, want 4 and 3000", calls, lastTarget)
	}
}

func TestLedgerMirrorRejectsABadAccountID(t *testing.T) {
	m := &LedgerMirror{
		reconcile: func(context.Context, solana.PublicKey, uint64) (solana.Signature, bool, error) {
			t.Fatal("reconcile must not be reached for an undecodable account id")
			return solana.Signature{}, false, nil
		},
		seen: map[string]uint64{},
	}
	if err := m.Sync(context.Background(), "not-hex", 1); err == nil {
		t.Error("a bad account id must be rejected before any RPC call")
	}
}

func TestLedgerConfigLayoutMatchesTheProgram(t *testing.T) {
	// processor.rs writes magic(4) authority(32) mint(32) treasury(32) and
	// config.rs's CONFIG_SIZE pins 100. The three keys are the same shape, so
	// a swapped offset reads happily and confirms the wrong deployment.
	data := make([]byte, 0, ledgerConfigSize)
	data = append(data, []byte(ledgerConfigMagic)...)
	for i := 0; i < 96; i++ {
		if i < 32 {
			data = append(data, 0xAA)
		} else if i < 64 {
			data = append(data, 0xBB)
		} else {
			data = append(data, 0xCC)
		}
	}
	if len(data) != ledgerConfigSize {
		t.Fatalf("built %d bytes, want %d", len(data), ledgerConfigSize)
	}
	cfg, err := DecodeLedgerConfig(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantAuthority := solana.PublicKey{}
	wantUSDC := solana.PublicKey{}
	wantTreasury := solana.PublicKey{}
	for i := range wantAuthority {
		wantAuthority[i] = 0xAA
		wantUSDC[i] = 0xBB
		wantTreasury[i] = 0xCC
	}
	if cfg.Authority != wantAuthority {
		t.Errorf("authority = %s, want the bytes at offset 4", cfg.Authority)
	}
	if cfg.USDC != wantUSDC {
		t.Errorf("mint = %s, want the bytes at offset 36", cfg.USDC)
	}
	if cfg.Treasury != wantTreasury {
		t.Errorf("treasury = %s, want the bytes at offset 68", cfg.Treasury)
	}
}

func TestLedgerConfigRejectsForeignAndShortBytes(t *testing.T) {
	// A credit account is 76 bytes and a config is 100; neither magic may be
	// accepted where the other is expected.
	if _, err := DecodeLedgerConfig(make([]byte, ledgerConfigSize-1)); err == nil {
		t.Error("a short buffer must be rejected")
	}
	wrong := make([]byte, ledgerConfigSize)
	copy(wrong[0:4], ledgerAccountMagic)
	if _, err := DecodeLedgerConfig(wrong); err == nil {
		t.Error("a credit account's magic must not pass as a config")
	}
	credit := make([]byte, LedgerAccountSize)
	copy(credit[0:4], ledgerConfigMagic)
	if _, err := DecodeLedgerAccount(credit); err == nil {
		t.Error("a config's magic must not pass as a credit account")
	}
	if ledgerAccountMagic == ledgerConfigMagic {
		t.Error("the two magics must differ or neither check means anything")
	}
}

func TestLedgerConfigReadRequiresAConfiguredProgram(t *testing.T) {
	c := &Client{}
	if _, _, err := c.LedgerConfigRead(context.Background()); err == nil {
		t.Error("reading the config without a program must fail")
	}
	if _, _, err := c.LedgerAccount(context.Background(), solana.PublicKey{}); err == nil {
		t.Error("reading a credit account without a program must fail")
	}
	if _, _, err := c.LedgerEnsureCredit(context.Background(), solana.PublicKey{}); err == nil {
		t.Error("ensuring a credit account without a program must fail")
	}
	if _, _, err := c.LedgerReconcileMint(context.Background(), solana.PublicKey{}, 1); err == nil {
		t.Error("reconciling without a program must fail")
	}
	if _, err := c.LedgerTransfer(context.Background(), solana.PublicKey{}, solana.PublicKey{}, 1); err == nil {
		t.Error("transferring without a program must fail")
	}
}

func TestLedgerProgramConfigIsParsedAtStartup(t *testing.T) {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{RPC: "http://127.0.0.1:8899", FeePayer: key.String(), LedgerProgram: key.PublicKey().String()}
	client, err := NewClient(cfg, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !client.LedgerEnabled() {
		t.Fatal("a configured program must enable the ledger surface")
	}
	if client.LedgerProgram() != key.PublicKey() {
		t.Errorf("program = %s, want %s", client.LedgerProgram(), key.PublicKey())
	}

	cfg.LedgerProgram = "not-a-key"
	if _, err := NewClient(cfg, nil); err == nil {
		t.Error("a malformed program id must be a startup error")
	}
	if _, err := NewClient(Config{RPC: "http://127.0.0.1:8899", FeePayer: key.String()}, nil); err != nil {
		t.Fatalf("no program must stay valid: %v", err)
	}
}
