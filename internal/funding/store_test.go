package funding

import (
	"context"
	"path/filepath"
	"testing"
)

// A store opens the funding tables on a fresh file and a second store on
// the same file reopens them: the DDL is idempotent by construction.
func TestNewStoreCreatesTheFundingTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "funding.db")
	first, err := NewStore(path, 1)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	first.Close()
	second, err := NewStore(path, 1)
	if err != nil {
		t.Fatalf("re-open NewStore: %v", err)
	}
	defer second.Close()
	if got, ok, err := second.Cursor(context.Background(), "never-set"); err != nil || ok {
		t.Fatalf("Cursor on a fresh store = (%d, %v, %v), want (0, false, nil)", got, ok, err)
	}
}

// The same purchase observed twice is one row (INSERT OR IGNORE on the
// (account, chain, deposit) key), and the swap leg round-trips.
func TestRecordIsIdempotentAndRoundTripsTheSwapLeg(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "funding.db"), 1)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	p := Purchase{
		AccountID: "0xabc", ChainKey: 1, DepositID: "42",
		SourceTx: "0xdead", SourceBlock: 100, AttestedHeight: 120,
		Token: "USDC", UsdMicros: 2_500_000, StartedAt: "2026-01-01T00:00:00Z",
		Credited: true, CCTx: "0xcredit", CCBlock: 130, Credits: 2500,
		CreditedAt: "2026-01-01T00:01:00Z",
		Swap:       &SwapLeg{TokenIn: "0xtoken", AmountIn: "1000", Native: true},
	}
	if err := s.Record(ctx, p); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Record(ctx, p); err != nil {
		t.Fatalf("re-Record: %v", err)
	}
	got, err := s.AllPurchases(ctx, "0xabc")
	if err != nil {
		t.Fatalf("AllPurchases: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("AllPurchases returned %d rows, want 1 (a repeat observation must not duplicate)", len(got))
	}
	row := got[0]
	if row.DepositID != "42" || row.Credits != 2500 || !row.Credited || row.ChainKey != 1 {
		t.Errorf("row = %+v, want deposit 42 with 2500 credited credits on chain 1", row)
	}
	if row.Swap == nil || row.Swap.TokenIn != "0xtoken" || !row.Swap.Native {
		t.Errorf("swap leg = %+v, want 0xtoken native", row.Swap)
	}
	// A credited purchase is history, not an open one.
	open, err := s.OpenPurchases(ctx, "0xabc")
	if err != nil {
		t.Fatalf("OpenPurchases: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("OpenPurchases = %d rows, want 0 for a credited purchase", len(open))
	}
}

// MarkCreditedByDeposit closes exactly the named purchase and reports the
// match. Repeating it is a no-op rather than an error - the statement sets
// the same values - and an unknown deposit id never matches, which is what
// makes an unmatched mint retryable instead of silently swallowed.
func TestMarkCreditedByDepositMatchesOnce(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "funding.db"), 1)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	if err := s.Record(ctx, Purchase{
		AccountID: "0xabc", ChainKey: 1, DepositID: "42", SourceTx: "0xdead",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	matched, err := s.MarkCreditedByDeposit(ctx, "0xabc", 1, "42", "0xcc", 130, 2500, "2026-01-01T00:01:00Z")
	if err != nil || !matched {
		t.Fatalf("MarkCreditedByDeposit = (%v, %v), want (true, nil)", matched, err)
	}
	// Repeating writes the same values: it still matches, and the row keeps
	// the same credits rather than earning them twice.
	matched, err = s.MarkCreditedByDeposit(ctx, "0xabc", 1, "42", "0xcc", 130, 2500, "2026-01-01T00:01:00Z")
	if err != nil || !matched {
		t.Fatalf("second MarkCreditedByDeposit = (%v, %v), want (true, nil): the identity still matches", matched, err)
	}
	all, err := s.AllPurchases(ctx, "0xabc")
	if err != nil || len(all) != 1 {
		t.Fatalf("AllPurchases = %d rows, err %v; want 1", len(all), err)
	}
	if all[0].Credits != 2500 || !all[0].Credited {
		t.Errorf("row = %+v, want it credited once with 2500 credits", all[0])
	}
	// An identity the store has never seen matches nothing.
	matched, err = s.MarkCreditedByDeposit(ctx, "0xabc", 1, "43", "0xcc", 130, 2500, "2026-01-01T00:01:00Z")
	if err != nil || matched {
		t.Fatalf("unknown deposit MarkCreditedByDeposit = (%v, %v), want (false, nil)", matched, err)
	}
}

// The scan cursor upserts: a second write to the same name replaces the
// first instead of adding a row.
func TestCursorUpserts(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "funding.db"), 1)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	if err := s.SetCursor(ctx, "deposit", 100); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}
	if err := s.SetCursor(ctx, "deposit", 250); err != nil {
		t.Fatalf("re-SetCursor: %v", err)
	}
	got, ok, err := s.Cursor(ctx, "deposit")
	if err != nil || !ok {
		t.Fatalf("Cursor = (%d, %v, %v)", got, ok, err)
	}
	if got != 250 {
		t.Errorf("Cursor = %d, want 250 (the later write wins)", got)
	}
}

// StampAttested only stamps open rows on the named chain whose source block
// the tip has passed and that carry no height yet; it never touches another
// chain's rows or rows already stamped.
func TestStampAttestedIsChainAndHeightScoped(t *testing.T) {
	ctx := context.Background()
	s, err := NewStore(filepath.Join(t.TempDir(), "funding.db"), 1)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer s.Close()
	rows := []Purchase{
		{AccountID: "0xa", ChainKey: 1, DepositID: "1", SourceTx: "0x1", SourceBlock: 100},
		{AccountID: "0xa", ChainKey: 3, DepositID: "2", SourceTx: "0x2", SourceBlock: 100},
		{AccountID: "0xa", ChainKey: 1, DepositID: "3", SourceTx: "0x3", SourceBlock: 100, AttestedHeight: 90},
	}
	for _, p := range rows {
		if err := s.Record(ctx, p); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := s.StampAttested(ctx, 1, 150); err != nil {
		t.Fatalf("StampAttested: %v", err)
	}
	all, err := s.AllPurchases(ctx, "0xa")
	if err != nil {
		t.Fatalf("AllPurchases: %v", err)
	}
	byDeposit := map[string]uint64{}
	for _, p := range all {
		byDeposit[p.DepositID] = p.AttestedHeight
	}
	if byDeposit["1"] != 150 {
		t.Errorf("chain 1 unstamped row attested_height = %d, want 150", byDeposit["1"])
	}
	if byDeposit["2"] != 0 {
		t.Errorf("chain 3 row attested_height = %d, want 0 (a tip never stamps another chain)", byDeposit["2"])
	}
	if byDeposit["3"] != 90 {
		t.Errorf("already-stamped row attested_height = %d, want 90 (an existing height is kept)", byDeposit["3"])
	}
}
