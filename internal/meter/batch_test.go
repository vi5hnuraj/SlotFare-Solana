package meter

import (
	"testing"
)

// TestSelectBatchTargetExactlyMet checks a covered sum that lands exactly on
// P: every row is included and ok is true.
func TestSelectBatchTargetExactlyMet(t *testing.T) {
	rows := []UsageRow{
		{RowID: 1, Credits: 6 * MicroCreditsPerCredit},
		{RowID: 2, Credits: 4 * MicroCreditsPerCredit},
	}
	sel, sum, ok := SelectBatch(rows, 10*MicroCreditsPerCredit)
	if !ok {
		t.Fatalf("SelectBatch ok = false, want true")
	}
	if len(sel) != 2 || sum != 10*MicroCreditsPerCredit {
		t.Errorf("SelectBatch = %d rows, sum %d, want 2 rows, sum %d", len(sel), sum, 10*MicroCreditsPerCredit)
	}
}

// TestSelectBatchRemainderStopsAtOvershoot checks the walk stops at the
// first row that would exceed P: the remainder stays for a later batch.
func TestSelectBatchRemainderStopsAtOvershoot(t *testing.T) {
	rows := []UsageRow{
		{RowID: 1, Credits: 3 * MicroCreditsPerCredit},
		{RowID: 2, Credits: 4 * MicroCreditsPerCredit},
		{RowID: 3, Credits: 5 * MicroCreditsPerCredit}, // 3+4+5 = 12 > 10 credits
	}
	sel, sum, ok := SelectBatch(rows, 10*MicroCreditsPerCredit)
	if !ok {
		t.Fatalf("SelectBatch ok = false, want true")
	}
	if len(sel) != 2 || sel[1].RowID != 2 || sum != 7*MicroCreditsPerCredit {
		t.Errorf("SelectBatch = %d rows (last id %d), sum %d, want 2 rows (last id 2), sum %d",
			len(sel), sel[len(sel)-1].RowID, sum, 7*MicroCreditsPerCredit)
	}
}

// TestSelectBatchSubCreditOnlyDefers checks a covered sum with no
// whole-credit part reports ok = false: nothing is committed.
func TestSelectBatchSubCreditOnlyDefers(t *testing.T) {
	rows := []UsageRow{{RowID: 1, Credits: 44}}
	sel, sum, ok := SelectBatch(rows, MicroCreditsPerCredit)
	if ok {
		t.Fatalf("SelectBatch ok = true for a sub-credit-only sum, want false")
	}
	if len(sel) != 1 || sum != 44 {
		t.Errorf("SelectBatch = %d rows, sum %d, want 1 row, sum 44", len(sel), sum)
	}
}

// TestSelectBatchEmptyRowsDefers checks an empty row set reports ok = false
// and an empty selection.
func TestSelectBatchEmptyRowsDefers(t *testing.T) {
	sel, sum, ok := SelectBatch(nil, MicroCreditsPerCredit)
	if ok || len(sel) != 0 || sum != 0 {
		t.Errorf("SelectBatch(nil) = (%v, %d, %v), want ([], 0, false)", sel, sum, ok)
	}
}

// TestSelectBatchFirstRowAloneExceedsTarget checks the only overshoot: the
// first row is included alone even when it exceeds P on its own.
func TestSelectBatchFirstRowAloneExceedsTarget(t *testing.T) {
	rows := []UsageRow{
		{RowID: 1, Credits: 15 * MicroCreditsPerCredit},
		{RowID: 2, Credits: MicroCreditsPerCredit},
	}
	sel, sum, ok := SelectBatch(rows, 10*MicroCreditsPerCredit)
	if !ok {
		t.Fatalf("SelectBatch ok = false, want true")
	}
	if len(sel) != 1 || sel[0].RowID != 1 || sum != 15*MicroCreditsPerCredit {
		t.Errorf("SelectBatch = %d rows, sum %d, want 1 row (id 1), sum %d", len(sel), sum, 15*MicroCreditsPerCredit)
	}
}
