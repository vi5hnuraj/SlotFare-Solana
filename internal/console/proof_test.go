package console

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"slotfare/internal/meter"
)

// seedProofRows records n usage rows for the account and returns their ids in
// insertion order (SQLite autoincrement, so 1..n for a fresh table).
func seedProofRows(t *testing.T, env *readsEnv, accountID string, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		row := meter.UsageRow{
			AccountID:        accountID,
			KeyPrefix:        "attk_abcdefg",
			Timestamp:        time.Unix(int64(1000+i), 0).UTC(),
			Service:          "rpc",
			Model:            "m",
			PromptTokens:     uint64(10 + i),
			CachedTokens:     0,
			CompletionTokens: uint64(20 + i),
			Credits:          uint64((5 + i) * meter.MicroCreditsPerCredit),
			Status:           "ok",
		}
		if err := env.meterStore.RecordUsage(context.Background(), row); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
		// Read the row back to learn its assigned id.
		rows, err := env.meterStore.RecentUsage(context.Background(), accountID, 1, "", false, 0)
		if err != nil {
			t.Fatalf("RecentUsage: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("recent rows = %d, want 1", len(rows))
		}
		ids = append(ids, rows[0].RowID)
	}
	return ids
}

// commitBatch computes the root over the account's rows and persists a batch
// record covering firstID..lastID, returning the committed root.
func commitBatch(t *testing.T, env *readsEnv, accountID string, firstID, lastID int64) [32]byte {
	t.Helper()
	rows, err := env.meterStore.BatchRangeRows(context.Background(), accountID, firstID, lastID)
	if err != nil {
		t.Fatalf("BatchRangeRows: %v", err)
	}
	root, err := meter.UsageRoot(rows)
	if err != nil {
		t.Fatalf("UsageRoot: %v", err)
	}
	var sum uint64
	for _, r := range rows {
		sum += r.Credits
	}
	b := meter.SettleBatch{
		AccountID:      accountID,
		Root:           root,
		FirstUsageID:   uint64(firstID),
		LastUsageID:    uint64(lastID),
		Credits:        sum / meter.MicroCreditsPerCredit,
		CoveredCredits: sum,
		TXHash:         "0x" + "ab",
		CommittedAt:    time.Now().Unix(),
	}
	if err := env.meterStore.CommitRecord(context.Background(), b, sum); err != nil {
		t.Fatalf("CommitRecord: %v", err)
	}
	return root
}

// fetchProof GETs /usage/{id}/proof with the caller's token and returns the
// parsed body plus the recorder.
func fetchProof(t *testing.T, h *Handler, token string, id int64) (*httptest.ResponseRecorder, proofView) {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, "/usage/"+strconv.FormatInt(id, 10)+"/proof", token, nil)
	var out proofView
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

//  1. First, middle and last rows of a committed batch all verify: the served
//     proof walks the leaf to the committed root and the batch record matches.
func TestUsageProofRoundtrip(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	ids := seedProofRows(t, env, id, 3)
	// Reads through the handler's Proof reader must be the same store.
	// (newReadsEnv attaches WithUsage but not WithProof; build a handler that
	// does so, sharing the same store.)
	h := withProofReader(env, env.meterStore)

	root := commitBatch(t, env, id, ids[0], ids[2])

	_, localID, ok, err := env.meterStore.BatchForRow(context.Background(), id, ids[1])
	if err != nil {
		t.Fatalf("BatchForRow: %v", err)
	}
	if !ok {
		t.Fatalf("BatchForRow: batch not found for committed row")
	}
	for _, target := range []int64{ids[0], ids[1], ids[2]} {
		rec, out := fetchProof(t, h, token, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /usage/%d/proof = %d, want 200; body %s", target, rec.Code, rec.Body.String())
		}
		if !out.Settled {
			t.Errorf("row %d: settled = false, want true", target)
		}
		leaf, err := meter.UsageLeaf(rowByIDStore(t, env, id, target))
		if err != nil {
			t.Fatalf("UsageLeaf(%d): %v", target, err)
		}
		wantHash := hex.EncodeToString(leaf[:])
		if out.LeafHash != wantHash {
			t.Errorf("row %d: leafHash = %s, want %s", target, out.LeafHash, wantHash)
		}
		// Recompute and verify the served proof path against the committed root.
		var steps []meter.ProofStep
		for _, s := range out.Proof {
			hs, err := hex.DecodeString(s.Hash)
			if err != nil {
				t.Fatalf("row %d: bad proof hash %q: %v", target, s.Hash, err)
			}
			var arr [32]byte
			copy(arr[:], hs)
			steps = append(steps, meter.ProofStep{Hash: arr, Direction: s.Direction})
		}
		if !meter.VerifyBatchProof(leaf, steps, root) {
			t.Errorf("row %d: served proof does not verify against the committed root", target)
		}
		if out.Batch == nil {
			t.Fatalf("row %d: no batch record in a settled response", target)
		}
		if out.Batch.Root != hex.EncodeToString(root[:]) {
			t.Errorf("row %d: batch root = %s, want %s", target, out.Batch.Root, hex.EncodeToString(root[:]))
		}
		if out.Batch.BatchIndex != uint64(localID-1) {
			t.Errorf("row %d: batchIndex = %d, want local id - 1 = %d", target, out.Batch.BatchIndex, localID-1)
		}
	}
}

//  2. A pending row - not yet covered by any batch - answers honestly:
//     settled false, leaf and hash present, no proof path or batch.
func TestUsageProofPendingRow(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	ids := seedProofRows(t, env, id, 1)
	h := withProofReader(env, env.meterStore)

	rec, out := fetchProof(t, h, token, ids[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pending proof = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if out.Settled {
		t.Error("pending row reported settled = true")
	}
	leaf, err := meter.UsageLeaf(rowByIDStore(t, env, id, ids[0]))
	if err != nil {
		t.Fatalf("UsageLeaf: %v", err)
	}
	if out.LeafHash != hex.EncodeToString(leaf[:]) {
		t.Errorf("pending leafHash = %s, want %s", out.LeafHash, hex.EncodeToString(leaf[:]))
	}
	if len(out.Proof) != 0 {
		t.Errorf("pending row has %d proof steps, want none", len(out.Proof))
	}
	if out.Batch != nil {
		t.Error("pending row returned a batch record")
	}
}

//  3. Foreign accounts cannot fetch others' proofs: a row belonging to account
//     A is invisible to B's caller and answers 404.
func TestUsageProofForeignAccount(t *testing.T) {
	env := newReadsEnv(t)
	idA := testAccount(t)
	idB := testAccount(t)
	tokenB := consoleTokenFor(t, env.h, idB)
	idsA := seedProofRows(t, env, idA, 2)
	commitBatch(t, env, idA, idsA[0], idsA[1])
	h := withProofReader(env, env.meterStore)

	rec, _ := fetchProof(t, h, tokenB, idsA[0])
	if rec.Code != http.StatusNotFound {
		t.Errorf("B fetching A's row = %d, want 404", rec.Code)
	}
}

// 4. A missing row id for a valid account answers 404.
func TestUsageProofMissingRow(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	seedProofRows(t, env, id, 1)
	h := withProofReader(env, env.meterStore)

	if rec, _ := fetchProof(t, h, token, 999999); rec.Code != http.StatusNotFound {
		t.Errorf("missing row = %d, want 404", rec.Code)
	}
}

// 5. A non-numeric row id is a bad request.
func TestUsageProofInvalidID(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	h := withProofReader(env, env.meterStore)

	rec := doRequest(t, h, http.MethodGet, "/usage/abc/proof", token, nil)
	if rec.Code != http.StatusNotFound {
		// abc is not the /usage/{id}/proof shape, so it falls through to 404.
		t.Errorf("non-numeric id = %d, want 404", rec.Code)
	}
}

// 6. The wrong method on the proof route is 405.
func TestUsageProofWrongMethod(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	seedProofRows(t, env, id, 1)
	h := withProofReader(env, env.meterStore)

	rec := doRequest(t, h, http.MethodPost, "/usage/1/proof", token, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST proof = %d, want 405", rec.Code)
	}
}

// 7. Without WithProof the route answers 501, like the other subsystems.
func TestUsageProofReaderNotAttached(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	seedProofRows(t, env, id, 1)
	// env.h was built without WithProof.
	if rec, _ := fetchProof(t, env.h, token, 1); rec.Code != http.StatusNotImplemented {
		t.Errorf("unattached proof route = %d, want 501", rec.Code)
	}
}

// 8. The proof response exposes the row's server-assigned request nonce, so
// a receipt's nonce can be checked against the recorded row.
func TestUsageProofExposesRowNonce(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	h := withProofReader(env, env.meterStore)

	row := meter.UsageRow{
		AccountID:        id,
		KeyPrefix:        "attk_abcdefg",
		Timestamp:        time.Unix(1750000000, 0).UTC(),
		Service:          "inference",
		Model:            "m",
		PromptTokens:     400_000,
		CompletionTokens: 300_000,
		Credits:          7 * meter.MicroCreditsPerCredit,
		Status:           "ok",
		Nonce:            "deadbeefdeadbeefdeadbeefdeadbeef",
	}
	if err := env.meterStore.RecordUsage(context.Background(), row); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	stored, err := env.meterStore.RecentUsage(context.Background(), id, 1, "", false, 0)
	if err != nil || len(stored) != 1 {
		t.Fatalf("RecentUsage: %v, %d rows", err, len(stored))
	}

	rec, out := fetchProof(t, h, token, stored[0].RowID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage/%d/proof = %d, want 200; body %s", stored[0].RowID, rec.Code, rec.Body.String())
	}
	if out.Nonce != row.Nonce {
		t.Errorf("proof nonce = %q, want %q (the recorded row's nonce)", out.Nonce, row.Nonce)
	}
}

// withProofReader builds a handler that shares every read subsystem of env but
// also attaches the meter store as its Proof reader - the shape slotfared wires
// in production. It reuses env's session store so consoleTokenFor tokens stay
// valid against it.
func withProofReader(env *readsEnv, r ProofReader) *Handler {
	h := New(
		WithKeys(env.keysStore),
		WithSessions(env.sessionsStore),
		WithLedger(env.ledger),
		WithUsage(env.meterStore),
		WithProof(r),
	)
	return h
}

// rowByIDStore reads one stored usage row back by id for leaf recomputation in
// the tests.
func rowByIDStore(t *testing.T, env *readsEnv, accountID string, id int64) meter.UsageRow {
	t.Helper()
	row, found, err := env.meterStore.UsageByID(context.Background(), accountID, id)
	if err != nil {
		t.Fatalf("UsageByID(%d): %v", id, err)
	}
	if !found {
		t.Fatalf("UsageByID(%d): not found", id)
	}
	return row
}
