package meter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEachClassReturnsConfiguredCost(t *testing.T) {
	w := DefaultWeights()
	cases := []struct {
		method string
		want   uint64
		class  Class
	}{
		{"getSlot", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"getBlockHeight", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"getLatestBlockhash", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"getBalance", 2 * MicroCreditsPerCredit, ClassStandard},
		{"getAccountInfo", 2 * MicroCreditsPerCredit, ClassStandard},
		{"getTransaction", 2 * MicroCreditsPerCredit, ClassStandard},
		{"sendTransaction", 2 * MicroCreditsPerCredit, ClassStandard},
		{"getBlock", 2 * MicroCreditsPerCredit, ClassStandard},
		{"getProgramAccounts", 20 * MicroCreditsPerCredit, ClassHeavy},
		{"getSignaturesForAddress", 20 * MicroCreditsPerCredit, ClassHeavy},
		{"getTokenLargestAccounts", 20 * MicroCreditsPerCredit, ClassHeavy},
		// The EVM passthrough block still prices, so a non-Solana upstream
		// is not charged the generic standard weight for every call.
		{"eth_chainId", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"net_version", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"eth_blockNumber", 1 * MicroCreditsPerCredit, ClassTrivial},
		{"eth_call", 2 * MicroCreditsPerCredit, ClassStandard},
		{"eth_getBalance", 2 * MicroCreditsPerCredit, ClassStandard},
		{"eth_getTransactionByHash", 2 * MicroCreditsPerCredit, ClassStandard},
		{"eth_getLogs", 20 * MicroCreditsPerCredit, ClassHeavy},
	}
	for _, c := range cases {
		cost, class, known := w.Cost(c.method, nil)
		if cost != c.want || class != c.class || !known {
			t.Errorf("Cost(%q) = (%d, %s, %v), want (%d, %s, true)", c.method, cost, class, known, c.want, c.class)
		}
	}
}

func TestUnknownMethodIsStandardAndUnrecognized(t *testing.T) {
	w := DefaultWeights()
	for _, method := range []string{"eth_newMethod", "getSomethingNew"} {
		cost, class, known := w.Cost(method, nil)
		if cost != 2*MicroCreditsPerCredit || class != ClassStandard || known {
			t.Fatalf("Cost(%q) = (%d, %s, %v), want (standard weight in micro-credits, standard, false)", method, cost, class, known)
		}
	}
}

// A Solana block getter that asks for full transaction objects returns every
// transaction in the block, so it must cost the heavy weight the same way
// eth_getBlockByNumber does with its boolean full flag.
func TestGetBlockFullTransactionDetails(t *testing.T) {
	w := DefaultWeights()
	full, class, known := w.Cost("getBlock", json.RawMessage(`[100,{"transactionDetails":"full"}]`))
	if full != 20*MicroCreditsPerCredit || class != ClassHeavy || !known {
		t.Errorf("transactionDetails full: got (%d, %s, %v), want (heavy weight in micro-credits, heavy, true)", full, class, known)
	}
	for _, params := range []string{
		`[100,{"transactionDetails":"signatures"}]`,
		`[100,{"transactionDetails":"accounts"}]`,
		`[100,{"transactionDetails":"none"}]`,
		`[100,{}]`,
		`[100]`,
	} {
		cost, class, known := w.Cost("getBlock", json.RawMessage(params))
		if cost != 2*MicroCreditsPerCredit || class != ClassStandard || !known {
			t.Errorf("getBlock %s: got (%d, %s, %v), want (standard weight in micro-credits, standard, true)", params, cost, class, known)
		}
	}
}

func TestGetBlockMalformedParamsDoNotPanic(t *testing.T) {
	w := DefaultWeights()
	bad := []json.RawMessage{
		json.RawMessage(`"not-an-array"`),
		json.RawMessage(`[100`),
		json.RawMessage(`garbage`),
		json.RawMessage(`[100,"full"]`),
		json.RawMessage(`[100,{"transactionDetails":{"nested":true}}]`),
		json.RawMessage(nil),
	}
	for i, p := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			cost, class, known := w.Cost("getBlock", p)
			if cost != 2*MicroCreditsPerCredit || class != ClassStandard || !known {
				t.Errorf("case %d: got (%d, %s, %v), want (standard weight in micro-credits, standard, true)", i, cost, class, known)
			}
		}()
	}
}

func TestArchivePrefix(t *testing.T) {
	w := DefaultWeights()
	for _, method := range []string{"debug_traceTransaction", "trace_block"} {
		cost, class, known := w.Cost(method, nil)
		if cost != 60*MicroCreditsPerCredit || class != ClassArchive || !known {
			t.Errorf("Cost(%q) = (%d, %s, %v), want (archive weight in micro-credits, archive, true)", method, cost, class, known)
		}
	}
}

func TestGetBlockByNumberFullTransactions(t *testing.T) {
	w := DefaultWeights()
	full, _, _ := w.Cost("eth_getBlockByNumber", json.RawMessage(`["0x1",true]`))
	if full != 20*MicroCreditsPerCredit {
		t.Errorf("full transactions: cost = %d, want %d micro-credits", full, 20*MicroCreditsPerCredit)
	}
	plain, class, known := w.Cost("eth_getBlockByNumber", json.RawMessage(`["0x1",false]`))
	if plain != 2*MicroCreditsPerCredit || class != ClassStandard || !known {
		t.Errorf("plain block: got (%d, %s, %v), want (standard weight in micro-credits, standard, true)", plain, class, known)
	}
}

func TestGetBlockByNumberMalformedParamsDoNotPanic(t *testing.T) {
	w := DefaultWeights()
	bad := []json.RawMessage{
		json.RawMessage(`"not-an-array"`),
		json.RawMessage(`["0x1"`),
		json.RawMessage(`garbage`),
		json.RawMessage(`[1,2]`),
		json.RawMessage(nil),
	}
	for i, p := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			cost, class, _ := w.Cost("eth_getBlockByNumber", p)
			if cost != 2*MicroCreditsPerCredit || class != ClassStandard {
				t.Errorf("case %d: got (%d, %s), want (standard weight in micro-credits, standard)", i, cost, class)
			}
		}()
	}
}

func TestBatchCostsSumOfMembers(t *testing.T) {
	w := DefaultWeights()
	body := json.RawMessage(`[
		{"method":"eth_chainId"},
		{"method":"eth_blockNumber"},
		{"method":"eth_call","params":[]}
	]`)
	cost, err := w.BatchCost(body)
	if err != nil {
		t.Fatalf("BatchCost: %v", err)
	}
	if cost != 4*MicroCreditsPerCredit {
		t.Errorf("batch cost = %d, want %d micro-credits", cost, 4*MicroCreditsPerCredit)
	}

	// The same batching applies to a Solana upstream: 1 + 2 + 20 credits.
	solanaBody := json.RawMessage(`[
		{"method":"getSlot"},
		{"method":"getBalance","params":["pubkey"]},
		{"method":"getProgramAccounts","params":["programId"]}
	]`)
	cost, err = w.BatchCost(solanaBody)
	if err != nil {
		t.Fatalf("BatchCost: %v", err)
	}
	if cost != 23*MicroCreditsPerCredit {
		t.Errorf("solana batch cost = %d, want %d micro-credits", cost, 23*MicroCreditsPerCredit)
	}
}

func TestBatchAboveLimitErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(path, []byte(`{"batch_limit": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWeights(path)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	body := json.RawMessage(`[{"method":"eth_chainId"},{"method":"eth_chainId"},{"method":"eth_chainId"}]`)
	_, err = w.BatchCost(body)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("want limit error, got %v", err)
	}
}

func TestBatchNotAnArrayErrors(t *testing.T) {
	w := DefaultWeights()
	if _, err := w.BatchCost(json.RawMessage(`{"method":"eth_chainId"}`)); err == nil {
		t.Fatal("want error for non-array batch body")
	}
}

func TestLoadWeightsMissingFileReturnsDefaults(t *testing.T) {
	w, err := LoadWeights(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error, got %v", err)
	}
	cost, class, known := w.Cost("eth_call", nil)
	if cost != 2*MicroCreditsPerCredit || class != ClassStandard || !known {
		t.Fatalf("defaults after missing file: got (%d, %s, %v)", cost, class, known)
	}
}

func TestLoadWeightsSingleOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(path, []byte(`{"methods":{"eth_call":"heavy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWeights(path)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	if cost, _, _ := w.Cost("eth_call", nil); cost != 20*MicroCreditsPerCredit {
		t.Errorf("overridden eth_call cost = %d, want %d micro-credits", cost, 20*MicroCreditsPerCredit)
	}
	if cost, _, _ := w.Cost("eth_chainId", nil); cost != 1*MicroCreditsPerCredit {
		t.Errorf("untouched eth_chainId cost = %d, want %d micro-credits", cost, 1*MicroCreditsPerCredit)
	}
	if cost, _, _ := w.Cost("eth_getLogs", nil); cost != 20*MicroCreditsPerCredit {
		t.Errorf("untouched eth_getLogs cost = %d, want %d micro-credits", cost, 20*MicroCreditsPerCredit)
	}
}

func TestLoadWeightsClassOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(path, []byte(`{"classes":{"trivial":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWeights(path)
	if err != nil {
		t.Fatalf("LoadWeights: %v", err)
	}
	if cost, _, _ := w.Cost("eth_chainId", nil); cost != 3*MicroCreditsPerCredit {
		t.Errorf("overridden trivial cost = %d, want %d micro-credits", cost, 3*MicroCreditsPerCredit)
	}
	if cost, _, _ := w.Cost("eth_call", nil); cost != 2*MicroCreditsPerCredit {
		t.Errorf("untouched standard cost = %d, want %d micro-credits", cost, 2*MicroCreditsPerCredit)
	}
}

func TestLoadWeightsRejectsUnknownClass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(path, []byte(`{"methods":{"eth_call":"expensive"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWeights(path); err == nil {
		t.Fatal("want error for unknown class name")
	}
}

func TestLoadWeightsRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weights.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWeights(path); err == nil {
		t.Fatal("want error for malformed JSON")
	}
}

func TestClassString(t *testing.T) {
	cases := map[Class]string{
		ClassTrivial:  "trivial",
		ClassStandard: "standard",
		ClassHeavy:    "heavy",
		ClassArchive:  "archive",
		Class(99):     "unknown",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", c, got, want)
		}
	}
}
