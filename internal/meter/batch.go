package meter

// SettleBatch is one settle pass's on-chain commitment for an account: the
// Merkle root of the exact usage rows whose spend this batch burns, the
// row range it covers, what it burned, and - once the commit lands - the
// transaction hash. It is persisted in the batches table by CommitRecord
// so a later verifier can re-derive the root from the usage rows and check
// it against the chain.
type SettleBatch struct {
	AccountID      string
	Root           [32]byte
	FirstUsageID   uint64
	LastUsageID    uint64
	Credits        uint64 // whole credits burned on-chain
	CoveredCredits uint64 // micro-credits of the covered rows
	TXHash         string
	CommittedAt    int64 // unix seconds
}

// SelectBatch walks the account's usage rows (ASC by id, starting just
// after the committed cursor) and returns the batch they form: the first
// row is included unconditionally, then each further row while the running
// sum stays at or below targetMicro; the first row that would exceed it
// stops the walk. targetMicro is P, the capped pending the settler already
// computes, so the batch covers "the settled amount plus the carried
// remainder" and never over-covers by more than the first row alone. The
// second result is the covered sum in micro-credits. ok is false when
// rows is empty or when the covered sum has no whole-credit part: in both
// cases nothing is committed and nothing is marked settled - the account
// is deferred to a later pass exactly like today's whole == 0 case.
func SelectBatch(rows []UsageRow, targetMicro uint64) ([]UsageRow, uint64, bool) {
	if len(rows) == 0 {
		return nil, 0, false
	}
	sel := []UsageRow{rows[0]}
	sum := rows[0].Credits
	for _, r := range rows[1:] {
		if sum+r.Credits > targetMicro {
			break
		}
		sel = append(sel, r)
		sum += r.Credits
	}
	if sum/MicroCreditsPerCredit == 0 {
		return sel, sum, false
	}
	return sel, sum, true
}

// SQLBatchRows reads the account's usage rows after one row id, oldest
// first; both drivers execute it. No LIMIT: RecordUsage prunes each
// account to its newest UsageCap rows, so the walk sees at most 200 rows.
const SQLBatchRows = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms
	        FROM usage WHERE account_id = ? AND id > ? ORDER BY id ASC`

// SQLBatchCursor reads the account's committed cursor - the last usage id
// a previous batch covered - or 0 when the account has no batches yet;
// both drivers execute it.
const SQLBatchCursor = `SELECT COALESCE(MAX(last_usage_id), 0) FROM batches WHERE account_id = ?`

// SQLRecordBatchInsert persists one settle batch's bookkeeping; both
// drivers execute it. root is a 32-byte blob literal - BLOB in SQLite,
// BYTEA in Postgres - and both accept a []byte argument.
const SQLRecordBatchInsert = `INSERT INTO batches (account_id, root, first_usage_id, last_usage_id, credits, covered_credits, tx_hash, committed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// SQLUsageByID reads one of an account's usage rows by its row id; both
// drivers execute it. A missing row is sql.ErrNoRows, which the caller turns
// into found=false.
const SQLUsageByID = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms, nonce
	        FROM usage WHERE account_id = ? AND id = ?`

// SQLBatchForRow reads the settle batch that covers one row id; both drivers
// execute it. The local auto-increment id is returned first so the caller can
// derive the on-chain batch index (id - 1). A missing batch is sql.ErrNoRows.
const SQLBatchForRow = `SELECT id, account_id, root, first_usage_id, last_usage_id, credits, covered_credits, tx_hash, committed_at
	        FROM batches WHERE account_id = ? AND first_usage_id <= ? AND last_usage_id >= ?`

// SQLBatchRangeRows reads an account's usage rows over a committed batch's
// row range, oldest first; both drivers execute it. It feeds on-demand proof
// building - no proof material is stored beyond the batches table.
const SQLBatchRangeRows = `SELECT id, account_id, key_prefix, ts, service, model, kind,
	        prompt_tokens, cached_tokens, completion_tokens, credits, status, duration_ms
	        FROM usage WHERE account_id = ? AND id >= ? AND id <= ? ORDER BY id ASC`
