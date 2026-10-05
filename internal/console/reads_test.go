package console

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gsolana "github.com/gagliardetto/solana-go"
	_ "modernc.org/sqlite"

	"slotfare/internal/funding"
	"slotfare/internal/keys"
	"slotfare/internal/market"
	"slotfare/internal/meter"
	"slotfare/internal/proxy"
	"slotfare/internal/session"
)

// readsEnv is a Handler with every read subsystem attached, all sharing one
// temp SQLite file the way slotfared does in production. Stream intervals are
// short so tests observe events without waiting real seconds.
type readsEnv struct {
	h             *Handler
	keysStore     keys.Store
	sessionsStore *session.SQLite
	meterStore    meter.Store
	tracker       *Tracker
	marketStore   market.Store
	ledger        *meter.Ledger
	meterPath     string
}

func newReadsEnv(t *testing.T) *readsEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slotfared.db")
	ks, err := keys.Open(path)
	if err != nil {
		t.Fatalf("keys.Open: %v", err)
	}
	ms, err := meter.Open(path)
	if err != nil {
		t.Fatalf("meter.Open: %v", err)
	}
	fs, err := funding.NewStore(path, 1)
	if err != nil {
		t.Fatalf("funding.NewStore: %v", err)
	}
	mk, err := market.NewStore(path)
	if err != nil {
		t.Fatalf("market.NewStore: %v", err)
	}
	ss, err := session.NewStore(path)
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	tracker := NewTracker(fs)
	ledger := meter.NewLedger()
	h := New(
		WithKeys(ks),
		WithSessions(ss),
		WithLedger(ledger),
		WithUsage(ms),
		WithFunding(tracker),
		withStreamIntervals(20*time.Millisecond, 150*time.Millisecond),
	)
	t.Cleanup(func() {
		ks.Close()
		ms.Close()
		fs.Close()
		mk.Close()
		ss.Close()
	})
	return &readsEnv{h: h, keysStore: ks, sessionsStore: ss, meterStore: ms, tracker: tracker, marketStore: mk, ledger: ledger, meterPath: path}
}

// testAccount mints a fresh Solana-native account id: 0x-hex of a random
// 32-byte public key, the shape AccountIDFromPublicKey produces.
func testAccount(t *testing.T) string {
	t.Helper()
	pub := gsolana.NewWallet().PublicKey()
	return "0x" + hex.EncodeToString(pub[:])
}

// inferenceProxy stands up the inference service under /infer against an
// httptest upstream that always answers with upstreamBody, with usage
// logging wired to env's meter store.
func inferenceProxy(t *testing.T, env *readsEnv, upstreamBody string) *proxy.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(upstream.Close)
	upURL, _ := url.Parse(upstream.URL)
	p := proxy.New(env.keysStore, meter.NewLocalAuthority(env.ledger), []proxy.Service{{
		Name:     "inference",
		Prefix:   "/infer",
		Upstream: upURL,
		Pricer:   proxy.NewInferencePricer(meter.DefaultTokenPrices()),
		Allow:    []string{"/v1/chat/completions"},
	}})
	p.Usage = env.meterStore
	return p
}

// failingRPCProxy stands up an RPC service under /rpc whose upstream always
// answers 500, so every metered call records an error row.
func failingRPCProxy(t *testing.T, env *readsEnv) *proxy.Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	upURL, _ := url.Parse(upstream.URL)
	p := proxy.New(env.keysStore, meter.NewLocalAuthority(env.ledger), []proxy.Service{{
		Name:     "rpc",
		Prefix:   "/rpc",
		Upstream: upURL,
		Pricer:   proxy.NewRPCPricer(meter.DefaultWeights()),
	}})
	p.Usage = env.meterStore
	return p
}

// infReqBody is one non-streaming inference request; its token counts come
// from the upstream's own usage report in inferenceRespBody.
const infReqBody = `{"model":"gpt-test","stream":false,"max_tokens":5000,"messages":[{"role":"user","content":"hello"}]}`

// inferenceRespBody reports 5000 prompt tokens (1000 cached) and 5000
// completion tokens, which settle to 191,000 micro-credits (19.1 credits)
// at the default prices.
const inferenceRespBody = `{"model":"gpt-test","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5000,"completion_tokens":5000,"prompt_tokens_details":{"cached_tokens":1000}}}`

// usageList GETs /usage with the given query and returns the parsed rows.
func usageList(t *testing.T, h *Handler, token, query string) []usageView {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, "/usage"+query, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage%s status = %d, want 200; body: %s", query, rec.Code, rec.Body.String())
	}
	var out struct {
		Usage []usageView `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /usage body not JSON: %v; body: %s", err, rec.Body.String())
	}
	return out.Usage
}

// 1. Usage rows carry no request or response content: a call whose bodies
// held a distinctive string puts that string nowhere in the usage table.
func TestUsageRowsCarryNoContent(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	env.ledger.SetMinted(id, 10000*meter.MicroCreditsPerCredit)
	token := consoleTokenFor(t, env.h, id)
	key, prefix := issueKey(t, env.h, token, "")

	const sentinel = "SENTINEL-4f8a"
	respBody := `{"model":"gpt-test","choices":[{"message":{"content":"` + sentinel + `"}}],"usage":{"prompt_tokens":5000,"completion_tokens":5000,"prompt_tokens_details":{"cached_tokens":1000}}}`
	reqBody := `{"model":"gpt-test","stream":false,"max_tokens":5000,"messages":[{"role":"user","content":"` + sentinel + `"}]}`
	p := inferenceProxy(t, env, respBody)
	rec := doRequest(t, p, http.MethodPost, "/infer/v1/chat/completions", key, []byte(reqBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("inference call status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// The row is visible through /usage with spend fields and no text.
	rows := usageList(t, env.h, token, "")
	if len(rows) != 1 {
		t.Fatalf("GET /usage returned %d rows, want 1; rows: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Model != "gpt-test" || row.Service != "inference" || row.Status != "ok" {
		t.Errorf("row service/model/status = %q/%q/%q, want inference/gpt-test/ok", row.Service, row.Model, row.Status)
	}
	if row.PromptTokens != 5000 || row.CachedTokens != 1000 || row.CompletionTokens != 5000 {
		t.Errorf("row tokens = %d/%d/%d, want 5000/1000/5000", row.PromptTokens, row.CachedTokens, row.CompletionTokens)
	}
	if row.Credits != 191_000 {
		t.Errorf("row credits = %d, want 191000 micro-credits", row.Credits)
	}
	if row.KeyPrefix != prefix {
		t.Errorf("row keyPrefix = %q, want %q", row.KeyPrefix, prefix)
	}

	// Raw-table check: open the SQLite file directly and scan every value
	// of every usage row for the sentinel.
	db, err := sql.Open("sqlite", "file:"+env.meterPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	raw, err := db.QueryContext(context.Background(), `SELECT * FROM usage`)
	if err != nil {
		t.Fatalf("SELECT * FROM usage: %v", err)
	}
	defer raw.Close()
	cols, err := raw.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	n := 0
	for raw.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := raw.Scan(ptrs...); err != nil {
			t.Fatalf("scan usage row: %v", err)
		}
		for i, v := range vals {
			switch x := v.(type) {
			case []byte:
				if strings.Contains(string(x), sentinel) {
					t.Errorf("usage column %q holds request/response content: %s", cols[i], x)
				}
			case string:
				if strings.Contains(x, sentinel) {
					t.Errorf("usage column %q holds request/response content: %q", cols[i], x)
				}
			}
		}
		n++
	}
	if n != 1 {
		t.Errorf("usage table holds %d rows, want 1", n)
	}
}

// TestUsageCursorPagination walks the full usage history with a before-id
// cursor: no gaps, no repeats across batch boundaries, an absent cursor
// serves today's newest batch, and exhaustion omits nextCursor.
func TestUsageCursorPagination(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	ctx := context.Background()

	// Seed five rows; ids are assigned in insertion order.
	var ids []int64
	for i := 0; i < 5; i++ {
		row := meter.UsageRow{
			AccountID: id,
			KeyPrefix: "attk_test",
			Timestamp: time.Date(2026, 1, 1, i, 0, 0, 0, time.UTC),
			Service:   "rpc",
			Credits:   uint64(i + 1),
			Status:    "ok",
		}
		if err := env.meterStore.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}
	rows, err := env.meterStore.RecentUsage(ctx, id, meter.UsageCap, "", false, 0)
	if err != nil {
		t.Fatalf("RecentUsage: %v", err)
	}
	if len(rows) != 5 {
		t.Fatalf("seeded %d rows, want 5", len(rows))
	}
	for _, r := range rows {
		ids = append(ids, r.RowID) // newest first
	}

	// Regression guard: no cursor is exactly today's newest batch.
	fresh := usageList(t, env.h, token, "")
	if len(fresh) != 5 {
		t.Fatalf("no-cursor /usage returned %d rows, want 5 (all of them)", len(fresh))
	}

	// Walk with ?limit=2 and the returned nextCursor until exhausted.
	type page struct {
		Usage      []usageView `json:"usage"`
		NextCursor int64       `json:"nextCursor"`
		HasNext    bool        `json:"-"`
	}
	seen := []int64{}
	dup := map[int64]bool{}
	before := int64(0)
	steps := 0
	for {
		q := "?limit=2"
		if before > 0 {
			q += "&before=" + strconv.FormatInt(before, 10)
		}
		rec := doRequest(t, env.h, http.MethodGet, "/usage"+q, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /usage%s status = %d, want 200; body: %s", q, rec.Code, rec.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("body not JSON: %v; body: %s", err, rec.Body.String())
		}
		var p page
		if err := json.Unmarshal(raw["usage"], &p.Usage); err != nil {
			t.Fatalf("usage not JSON: %v", err)
		}
		if nc, ok := raw["nextCursor"]; ok {
			if err := json.Unmarshal(nc, &p.NextCursor); err != nil {
				t.Fatalf("nextCursor not JSON: %v", err)
			}
			p.HasNext = true
		}
		if len(p.Usage) == 0 {
			// Exhaustion: no nextCursor key.
			if p.HasNext {
				t.Errorf("exhausted page carries nextCursor=%d, want it absent", p.NextCursor)
			}
			break
		}
		for _, u := range p.Usage {
			_ = u
		}
		// Map back to row ids by credits (credits = index+1, unique per row).
		for _, u := range p.Usage {
			var rid int64
			for i, r := range rows {
				if r.Credits == u.Credits {
					rid = r.RowID
					_ = i
					break
				}
			}
			if dup[rid] {
				t.Errorf("row id %d repeated across batches", rid)
			}
			dup[rid] = true
			seen = append(seen, rid)
		}
		if !p.HasNext || p.NextCursor == 0 {
			t.Fatalf("non-exhausted page has no nextCursor; body: %s", rec.Body.String())
		}
		before = p.NextCursor
		steps++
		if steps > 10 {
			t.Fatalf("walk did not terminate after %d steps", steps)
		}
	}
	// The walk must equal the full newest-first sequence with no gaps.
	if len(seen) != len(ids) {
		t.Fatalf("walk visited %d rows, want %d; seen=%v ids=%v", len(seen), len(ids), seen, ids)
	}
	for i := range ids {
		if seen[i] != ids[i] {
			t.Errorf("walk order diverges at %d: got %d, want %d (seen=%v ids=%v)", i, seen[i], ids[i], seen, ids)
		}
	}

	// Invalid cursors are rejected like limit.
	for _, bad := range []string{"/usage?before=abc", "/usage?before=0", "/usage?before=-1"} {
		if rec := doRequest(t, env.h, http.MethodGet, bad, token, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", bad, rec.Code, rec.Body.String())
		} else if got := errorReason(t, rec); got != "invalid before" {
			t.Errorf("%s reason = %q, want \"invalid before\"", bad, got)
		}
	}
}

// TestUsageServesDurationVerbatim checks the console payload serves
// duration_ms as stored: a recorded value comes through verbatim and a
// pre-column row serves null - the key stays present, never fabricated as 0.
func TestUsageServesDurationVerbatim(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	ctx := context.Background()

	dur := uint64(555)
	row := meter.UsageRow{
		AccountID: id, KeyPrefix: "attk_test",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Service:   "rpc", Model: "new", Status: "ok",
		Credits: 7, DurationMS: &dur,
	}
	if err := env.meterStore.RecordUsage(ctx, row); err != nil {
		t.Fatalf("RecordUsage(duration): %v", err)
	}
	// A legacy row written without the duration_ms column, exactly as an
	// old binary would have.
	db, err := sql.Open("sqlite", "file:"+env.meterPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO usage (account_id, key_prefix, ts, service, model,
		prompt_tokens, cached_tokens, completion_tokens, credits, status)
		VALUES (?, 'attk_test', 0, 'rpc', 'old', 5, 0, 13, 7, 'ok')`, id); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	rows := usageList(t, env.h, token, "")
	if len(rows) != 2 {
		t.Fatalf("GET /usage returned %d rows, want 2; rows: %+v", len(rows), rows)
	}
	// Newest first by row id: the legacy row was inserted second.
	legacy, recorded := rows[0], rows[1]
	if legacy.Model != "old" || legacy.DurationMS != nil {
		t.Errorf("legacy row = %+v, want nil duration (absent, not zero)", legacy)
	}
	if recorded.Model != "new" || recorded.DurationMS == nil || *recorded.DurationMS != 555 {
		t.Errorf("recorded row = %+v, want DurationMS 555 served verbatim", recorded)
	}

	// The raw wire payload keeps the key present with a null value for the
	// legacy row: null stays null, it is never dropped or zeroed.
	rec := doRequest(t, env.h, http.MethodGet, "/usage", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Usage []struct {
			Model      string  `json:"model"`
			DurationMS *uint64 `json:"duration_ms"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /usage body not JSON: %v; body: %s", err, rec.Body.String())
	}
	for _, u := range out.Usage {
		switch u.Model {
		case "old":
			if u.DurationMS != nil {
				t.Errorf("legacy row duration_ms = %d, want null on the wire", *u.DurationMS)
			}
		case "new":
			if u.DurationMS == nil || *u.DurationMS != 555 {
				t.Errorf("recorded row duration_ms = %v, want 555", u.DurationMS)
			}
		default:
			t.Errorf("unexpected model %q in /usage payload", u.Model)
		}
	}
}

// 2. Filtering by key prefix and by errors-only returns the right subsets.
func TestUsageFilters(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	env.ledger.SetMinted(id, 10000*meter.MicroCreditsPerCredit)
	token := consoleTokenFor(t, env.h, id)
	key1, prefix1 := issueKey(t, env.h, token, "k1")
	key2, prefix2 := issueKey(t, env.h, token, "k2")

	ctx := context.Background()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := func(keyPrefix, status string) {
		row := meter.UsageRow{
			AccountID: id,
			KeyPrefix: keyPrefix,
			Timestamp: ts,
			Service:   "rpc",
			Credits:   7,
			Status:    status,
		}
		if status == "error" {
			row.Credits = 0
		}
		if err := env.meterStore.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	seed(prefix1, "ok")
	seed(prefix1, "error")
	seed(prefix2, "ok")

	// One successful proxy call on key1 and one failing call on key2.
	okProxy := newTestProxy(t, env.keysStore, env.ledger)
	okProxy.Usage = env.meterStore
	if rec := rpcCall(t, okProxy, key1); rec.Code != http.StatusOK {
		t.Fatalf("successful proxy call status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	badProxy := failingRPCProxy(t, env)
	if rec := rpcCall(t, badProxy, key2); rec.Code != http.StatusBadGateway {
		t.Fatalf("failing proxy call status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}

	all := usageList(t, env.h, token, "")
	if len(all) != 5 {
		t.Fatalf("no filter: %d rows, want 5; rows: %+v", len(all), all)
	}
	// Newest first: the last recorded row is key2's error.
	if all[0].KeyPrefix != prefix2 || all[0].Status != "error" {
		t.Errorf("newest row = %+v, want key2's error row", all[0])
	}
	if all[0].Credits != 0 {
		t.Errorf("error row credits = %d, want 0", all[0].Credits)
	}

	byPrefix := usageList(t, env.h, token, "?prefix="+prefix1)
	if len(byPrefix) != 3 {
		t.Errorf("?prefix=%s: %d rows, want 3; rows: %+v", prefix1, len(byPrefix), byPrefix)
	}
	for _, r := range byPrefix {
		if r.KeyPrefix != prefix1 {
			t.Errorf("row outside requested prefix: %+v", r)
		}
	}

	byErrors := usageList(t, env.h, token, "?errors=true")
	if len(byErrors) != 2 {
		t.Errorf("?errors=true: %d rows, want 2; rows: %+v", len(byErrors), byErrors)
	}
	for _, r := range byErrors {
		if r.Status != "error" {
			t.Errorf("non-error row in errors-only list: %+v", r)
		}
	}

	both := usageList(t, env.h, token, "?prefix="+prefix1+"&errors=true")
	if len(both) != 1 || both[0].KeyPrefix != prefix1 || both[0].Status != "error" {
		t.Errorf("prefix+errors: %+v, want exactly prefix1's seeded error row", both)
	}

	capped := usageList(t, env.h, token, "?limit=2")
	if len(capped) != 2 {
		t.Errorf("?limit=2: %d rows, want 2", len(capped))
	}

	// Auth and parameter edge cases.
	if rec := doRequest(t, env.h, http.MethodGet, "/usage", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := doRequest(t, env.h, http.MethodGet, "/usage?limit=abc", token, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=abc: status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	} else if got := errorReason(t, rec); got != "invalid limit" {
		t.Errorf("limit=abc reason = %q, want \"invalid limit\"", got)
	}
	if rec := doRequest(t, env.h, http.MethodGet, "/usage?limit=0", token, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: status = %d, want 400", rec.Code)
	}
}

// 3. Funding reports per-chain tip and ceiling separately; a purchase whose
// block its chain's tip has passed but the builder ceiling has not stays at
// stage 3, and each row carries its chain's name.
func TestFundingReportsTipAndCeilingSeparately(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	env.tracker.SetChainNames(map[uint64]string{1: "Ethereum", 3: "Mainnet"})
	env.tracker.SetTip(1, 1000)
	env.tracker.SetCeiling(1, 900)

	ctx := context.Background()
	record := func(p funding.Purchase) {
		p.AccountID = id
		if err := env.tracker.Record(ctx, p); err != nil {
			t.Fatalf("Record %s: %v", p.SourceTx, err)
		}
	}
	record(funding.Purchase{SourceTx: "0xaa", SourceBlock: 950, AttestedHeight: 950, ChainKey: 1, DepositID: "1"}) // attested, builder lagging
	record(funding.Purchase{SourceTx: "0xbb", SourceBlock: 800, ChainKey: 1, DepositID: "2"})                      // provable
	record(funding.Purchase{SourceTx: "0xcc", ChainKey: 1, DepositID: "3"})                                        // recorded, no source block
	record(funding.Purchase{SourceTx: "0xdd", SourceBlock: 2000, ChainKey: 1, DepositID: "4"})                     // confirmed, frontiers behind
	record(funding.Purchase{SourceTx: "0xee", SourceBlock: 50, ChainKey: 3, DepositID: "5"})                       // mainnet row: no frontiers yet

	rec := doRequest(t, env.h, http.MethodGet, "/funding", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funding status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out FundingView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /funding body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Purchases) != 5 {
		t.Fatalf("purchases = %+v, want 5 entries", out.Purchases)
	}
	stages := map[string]uint8{}
	for _, p := range out.Purchases {
		stages[p.SourceTx] = p.Stage
		switch {
		case p.ChainKey == 1:
			if p.Tip != 1000 || p.Ceiling != 900 || p.ChainName != "Ethereum" {
				t.Errorf("%s tip/ceiling/name = %d/%d/%q, want 1000/900/Ethereum repeated per row", p.SourceTx, p.Tip, p.Ceiling, p.ChainName)
			}
		case p.ChainKey == 3:
			if p.Tip != 0 || p.Ceiling != 0 || p.ChainName != "Mainnet" {
				t.Errorf("%s tip/ceiling/name = %d/%d/%q, want zero frontiers and Mainnet", p.SourceTx, p.Tip, p.Ceiling, p.ChainName)
			}
		}
	}
	want := map[string]uint8{"0xaa": 3, "0xbb": 4, "0xcc": 1, "0xdd": 2, "0xee": 2}
	for tx, stage := range want {
		if got, ok := stages[tx]; !ok {
			t.Errorf("purchase %s missing from list", tx)
		} else if got != stage {
			t.Errorf("%s stage = %d, want %d", tx, got, stage)
		}
	}

	// Credited purchases stay listed at stage 5: they are the history.
	if err := env.tracker.MarkCredited(ctx, id, "0xbb"); err != nil {
		t.Fatalf("MarkCredited: %v", err)
	}
	rec = doRequest(t, env.h, http.MethodGet, "/funding", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funding after credit status = %d, want 200", rec.Code)
	}
	out = FundingView{}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Purchases) != 5 {
		t.Errorf("after MarkCredited: %d purchases, want 5; purchases: %+v", len(out.Purchases), out.Purchases)
	}
	for _, p := range out.Purchases {
		if p.SourceTx == "0xbb" && (p.Stage != 5 || !p.Credited) {
			t.Errorf("credited purchase 0xbb = stage %d credited %v, want stage 5 credited", p.Stage, p.Credited)
		}
	}
	if out.Purchases[0].SourceTx != "0xee" {
		t.Errorf("first purchase = %s, want the newest (0xee) first", out.Purchases[0].SourceTx)
	}
}

// TestFundingServesProofAnatomy checks that /funding serves every fact the
// store holds about an attested deposit's journey - source chain key, name and
// explorer base, source tx and block, attested height, Solana mint tx and
// block, and the started/credited timestamps - while a direct (chain-0)
// deposit keeps its attestation fields absent on the wire.
func TestFundingServesProofAnatomy(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	env.tracker.SetChainNames(map[uint64]string{1: "Ethereum", 0: "Solana"})
	env.tracker.SetChainExplorers(map[uint64]string{1: "https://etherscan.io/tx/", 0: "https://explorer.solana.com/tx/"})
	ctx := context.Background()

	// One attested proven purchase on chain 1, closed by its mint.
	if err := env.tracker.Record(ctx, funding.Purchase{
		AccountID: id, ChainKey: 1, DepositID: "1",
		SourceTx: "0xaa", SourceBlock: 950, AttestedHeight: 950,
		Token: "mUSDC", UsdMicros: 25_000_000, StartedAt: "2026-08-23T23:00:00Z",
	}); err != nil {
		t.Fatalf("Record proven: %v", err)
	}
	if matched, err := env.tracker.Store().MarkCreditedByDeposit(ctx, id, 1, "1", "0xcc", 424242, 25000, "2026-08-23T23:10:00Z"); err != nil || !matched {
		t.Fatalf("MarkCreditedByDeposit: matched %v err %v", matched, err)
	}
	// One direct deposit on chain 0: credited on arrival, no source fields.
	if err := env.tracker.Record(ctx, funding.Purchase{
		AccountID: id, ChainKey: 0, DepositID: "2",
		Credited: true, CCTx: "0xdirect", CCBlock: 777, Credits: 50000,
		Token: "mUSD1", UsdMicros: 5_000_000, StartedAt: "2026-09-01T12:00:00Z", CreditedAt: "2026-09-01T12:00:00Z",
	}); err != nil {
		t.Fatalf("Record direct: %v", err)
	}

	rec := doRequest(t, env.h, http.MethodGet, "/funding", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /funding status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out FundingView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /funding body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Purchases) != 2 {
		t.Fatalf("purchases = %+v, want 2 entries", out.Purchases)
	}
	var proven, direct *PurchaseView
	for i := range out.Purchases {
		switch out.Purchases[i].ChainKey {
		case 1:
			proven = &out.Purchases[i]
		case 0:
			direct = &out.Purchases[i]
		}
	}
	if proven == nil || direct == nil {
		t.Fatalf("rows = %+v, want one chain 1 and one chain 0", out.Purchases)
	}
	// Every anatomy fact is present and correct on the proven row.
	if proven.ChainName != "Ethereum" || proven.Explorer != "https://etherscan.io/tx/" {
		t.Errorf("proven chain label = %q/%q, want Ethereum with its explorer base", proven.ChainName, proven.Explorer)
	}
	if proven.SourceTx != "0xaa" || proven.SourceBlock != 950 || proven.AttestedHeight != 950 {
		t.Errorf("proven source facts = %+v, want tx 0xaa block 950 attested 950", proven)
	}
	if proven.CCTx != "0xcc" || proven.CCBlock != 424242 || proven.Credits != 25000 {
		t.Errorf("proven solana facts = %+v, want ccTx 0xcc block 424242 credits 25000", proven)
	}
	if proven.StartedAt != "2026-08-23T23:00:00Z" || proven.CreditedAt != "2026-08-23T23:10:00Z" {
		t.Errorf("proven timestamps = %q/%q, want started and credited", proven.StartedAt, proven.CreditedAt)
	}
	// The direct row labels itself but carries no attestation fields.
	if direct.ChainName != "Solana" || direct.Explorer != "https://explorer.solana.com/tx/" {
		t.Errorf("direct chain label = %q/%q, want Solana with its explorer base", direct.ChainName, direct.Explorer)
	}
	if direct.SourceTx != "" || direct.SourceBlock != 0 || direct.AttestedHeight != 0 {
		t.Errorf("direct row carries source fields: tx=%q block=%d height=%d", direct.SourceTx, direct.SourceBlock, direct.AttestedHeight)
	}
	if direct.CCTx != "0xdirect" || direct.CCBlock != 777 || direct.Credits != 50000 {
		t.Errorf("direct row = %+v, want ccTx 0xdirect block 777 credits 50000", direct)
	}
}

// The pure stage function, table driven.
func TestFundingStageTable(t *testing.T) {
	cases := []struct {
		desc    string
		p       funding.Purchase
		tip     uint64
		ceiling uint64
		want    uint8
	}{
		{"credited wins over everything", funding.Purchase{SourceBlock: 10, Credited: true}, 20, 20, 5},
		{"recorded without a source block", funding.Purchase{SourceBlock: 0}, 100, 100, 1},
		{"confirmed while both frontiers lag", funding.Purchase{SourceBlock: 100}, 50, 50, 2},
		{"attested while the builder lags", funding.Purchase{SourceBlock: 90}, 100, 80, 3},
		{"provable once both frontiers pass", funding.Purchase{SourceBlock: 90}, 100, 95, 4},
		{"ceiling past but tip behind", funding.Purchase{SourceBlock: 90}, 80, 100, 2},
	}
	for _, c := range cases {
		if got := fundingStage(c.p, c.tip, c.ceiling); got != c.want {
			t.Errorf("%s: fundingStage(%+v, %d, %d) = %d, want %d", c.desc, c.p, c.tip, c.ceiling, got, c.want)
		}
	}
}

// sseEvent is one parsed server-sent event from /console/stream.
type sseEvent struct {
	name string
	data string
}

// streamEvents opens GET /console/stream with the token against the real
// server and delivers parsed events until the connection closes. stop
// tears the client connection down; the handler's request context cancels
// on the disconnect, so the stream goroutine exits and a later
// httptest.Server.Close cannot block on the connection.
func streamEvents(t *testing.T, tsrv *httptest.Server, token string) (<-chan sseEvent, func()) {
	t.Helper()
	ch := make(chan sseEvent, 128)
	req, err := http.NewRequest(http.MethodGet, tsrv.URL+"/console/stream", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var name string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				select {
				case ch <- sseEvent{name: name, data: strings.TrimPrefix(line, "data: ")}:
				case <-stopped:
					return
				default:
				}
			}
		}
	}()
	return ch, func() {
		select {
		case <-stopped:
		default:
			close(stopped)
		}
		// Closing the unread response body destroys the connection; the
		// server's per-connection context then cancels the handler.
		resp.Body.Close()
	}
}

// nextEvent waits up to timeout for an event with the given name, skipping
// anything else the stream sends meanwhile.
func nextEvent(t *testing.T, ch <-chan sseEvent, wantName string, timeout time.Duration) sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed while waiting for %q event", wantName)
			}
			if ev.name == wantName {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %q event", timeout, wantName)
		}
	}
}

// parseBalance decodes a balance event payload.
func parseBalance(t *testing.T, data string) balanceView {
	t.Helper()
	var b balanceView
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("balance data %q not JSON: %v", data, err)
	}
	return b
}

// 4. The stream emits a balance event when spendable changes, a usage event
// per settled call, and a heartbeat on an idle connection.
func TestStreamEvents(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	env.ledger.SetMinted(id, 1000*meter.MicroCreditsPerCredit)
	token := consoleTokenFor(t, env.h, id)
	key, _ := issueKey(t, env.h, token, "")

	p := inferenceProxy(t, env, inferenceRespBody)

	root := http.NewServeMux()
	root.Handle("/console/", http.StripPrefix("/console", env.h))
	tsrv := httptest.NewServer(root)
	defer tsrv.Close()

	events, stopStream := streamEvents(t, tsrv, token)

	// The initial snapshot arrives on connect.
	ev := nextEvent(t, events, "balance", 5*time.Second)
	bal := parseBalance(t, ev.data)
	if bal.Spendable != 1000*meter.MicroCreditsPerCredit || bal.Minted != 1000*meter.MicroCreditsPerCredit {
		t.Fatalf("initial balance = %+v, want spendable/minted %d micro-credits", bal, 1000*meter.MicroCreditsPerCredit)
	}

	// A reservation drops the spendable and produces a balance event.
	if err := env.ledger.Reserve(id, 40*meter.MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	ev = nextEvent(t, events, "balance", 5*time.Second)
	bal = parseBalance(t, ev.data)
	if bal.Spendable != 960*meter.MicroCreditsPerCredit || bal.Pending != 40*meter.MicroCreditsPerCredit {
		t.Fatalf("post-reserve balance = %+v, want spendable %d pending %d micro-credits", bal,
			960*meter.MicroCreditsPerCredit, 40*meter.MicroCreditsPerCredit)
	}

	// A settled call produces a usage event carrying that row's credits.
	rec := doRequest(t, p, http.MethodPost, "/infer/v1/chat/completions", key, []byte(infReqBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("inference call status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	ev = nextEvent(t, events, "usage", 5*time.Second)
	var u usageView
	if err := json.Unmarshal([]byte(ev.data), &u); err != nil {
		t.Fatalf("usage data %q not JSON: %v", ev.data, err)
	}
	if u.Credits != 191_000 || u.Model != "gpt-test" || u.Status != "ok" {
		t.Errorf("usage event = %+v, want credits 191000 model gpt-test status ok", u)
	}

	// An idle connection gets a heartbeat within the interval.
	ev = nextEvent(t, events, "heartbeat", 2*time.Second)
	var hb struct {
		TS string `json:"ts"`
	}
	if err := json.Unmarshal([]byte(ev.data), &hb); err != nil || hb.TS == "" {
		t.Errorf("heartbeat data = %q, want {\"ts\":...}", ev.data)
	}
	stopStream()
}

// 4b. A lock change alone emits a balance event carrying the new locked
// figure, and spendable drops with it while minted stays put.
func TestStreamEmitsOnLockChange(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	env.ledger.SetMinted(id, 1000*meter.MicroCreditsPerCredit)
	token := consoleTokenFor(t, env.h, id)

	root := http.NewServeMux()
	root.Handle("/console/", http.StripPrefix("/console", env.h))
	tsrv := httptest.NewServer(root)
	defer tsrv.Close()

	events, stopStream := streamEvents(t, tsrv, token)
	defer stopStream()

	// The initial snapshot arrives on connect.
	ev := nextEvent(t, events, "balance", 5*time.Second)
	bal := parseBalance(t, ev.data)
	if bal.Minted != 1000*meter.MicroCreditsPerCredit || bal.Locked != 0 || bal.Spendable != 1000*meter.MicroCreditsPerCredit {
		t.Fatalf("initial balance = %+v, want minted %d locked 0 spendable %d micro-credits", bal,
			1000*meter.MicroCreditsPerCredit, 1000*meter.MicroCreditsPerCredit)
	}

	// No usage at all: only the lock moves. The stream must emit on that.
	env.ledger.SetLocked(id, 300*meter.MicroCreditsPerCredit)
	ev = nextEvent(t, events, "balance", 5*time.Second)
	bal = parseBalance(t, ev.data)
	if bal.Locked != 300*meter.MicroCreditsPerCredit || bal.Minted != 1000*meter.MicroCreditsPerCredit ||
		bal.Spendable != 700*meter.MicroCreditsPerCredit {
		t.Fatalf("post-lock balance = %+v, want locked %d minted %d spendable %d micro-credits", bal,
			300*meter.MicroCreditsPerCredit, 1000*meter.MicroCreditsPerCredit, 700*meter.MicroCreditsPerCredit)
	}

	// Lifting the lock emits again.
	env.ledger.SetLocked(id, 0)
	ev = nextEvent(t, events, "balance", 5*time.Second)
	bal = parseBalance(t, ev.data)
	if bal.Locked != 0 || bal.Spendable != 1000*meter.MicroCreditsPerCredit {
		t.Fatalf("post-release balance = %+v, want locked 0 spendable %d micro-credits", bal, 1000*meter.MicroCreditsPerCredit)
	}
}

// 5. A token for account A never receives account B's events.
func TestStreamIsolation(t *testing.T) {
	env := newReadsEnv(t)
	idA := testAccount(t)
	idB := testAccount(t)
	env.ledger.SetMinted(idA, 1000*meter.MicroCreditsPerCredit)
	tokenA := consoleTokenFor(t, env.h, idA)
	tokenB := consoleTokenFor(t, env.h, idB)
	keyA, _ := issueKey(t, env.h, tokenA, "")

	p := inferenceProxy(t, env, inferenceRespBody)

	root := http.NewServeMux()
	root.Handle("/console/", http.StripPrefix("/console", env.h))
	tsrv := httptest.NewServer(root)
	defer tsrv.Close()

	eventsB, stopStreamB := streamEvents(t, tsrv, tokenB)

	// B's initial snapshot is B's own zeros.
	ev := nextEvent(t, eventsB, "balance", 5*time.Second)
	bal := parseBalance(t, ev.data)
	if bal.Minted != 0 || bal.Spendable != 0 {
		t.Fatalf("B's initial balance = %+v, want zeros", bal)
	}

	// Fire A's activity while B's connection is open: a reservation and a
	// settled call.
	if err := env.ledger.Reserve(idA, 40*meter.MicroCreditsPerCredit); err != nil {
		t.Fatalf("Reserve A: %v", err)
	}
	rec := doRequest(t, p, http.MethodPost, "/infer/v1/chat/completions", keyA, []byte(infReqBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("A's inference call status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	// Collect everything B receives across several heartbeat cycles. B may
	// see heartbeats and its own unchanged balance - nothing else.
	sawHeartbeat := false
	window := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(window) {
		select {
		case ev, ok := <-eventsB:
			if !ok {
				t.Fatal("B's stream closed during observation window")
			}
			switch ev.name {
			case "heartbeat":
				sawHeartbeat = true
			case "balance":
				b := parseBalance(t, ev.data)
				if b.Minted != 0 || b.Pending != 0 || b.Spendable != 0 {
					t.Errorf("B's stream saw a foreign balance: %s", ev.data)
				}
			default:
				t.Errorf("B's stream received %q event meant for A: %s", ev.name, ev.data)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawHeartbeat {
		t.Error("no heartbeat observed on B's idle connection during the window")
	}
	stopStreamB()
}

// The new routes answer 405 on wrong methods and 501 when their subsystem
// is absent, matching the Keys/Ledger convention.
func TestNewRoutesMethodAndAvailability(t *testing.T) {
	h := New(WithSessions(testSessionStore(t)))
	token := consoleTokenFor(t, h, "0xtest")

	for _, path := range []string{"/usage", "/funding", "/chains", "/market", "/stream", "/attestation"} {
		if rec := doRequest(t, h, http.MethodPost, path, token, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", path, rec.Code)
		}
	}
	// The listing write subroutes take POST; a GET is a method error, and a
	// bad id shape is a miss (404), not a method error.
	for _, path := range []string{"/listings/1/cancel", "/listings/1/pay"} {
		if rec := doRequest(t, h, http.MethodGet, path, token, nil); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: status = %d, want 405", path, rec.Code)
		}
	}
	if rec := doRequest(t, h, http.MethodPost, "/listings/nope/pay", token, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST /listings/nope/pay: status = %d, want 404", rec.Code)
	}

	// No subsystems attached: all three read routes are unavailable.
	h = New(WithSessions(testSessionStore(t)), WithLedger(meter.NewLedger()))
	token = consoleTokenFor(t, h, "0xtest")
	if rec := doRequest(t, h, http.MethodGet, "/usage", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /usage without Usage: status = %d, want 501", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/funding", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /funding without Funding: status = %d, want 501", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/market", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /market without Market: status = %d, want 501", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/listings", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /listings without Market: status = %d, want 501", rec.Code)
	}
	// The write routes are 501 the same way, per missing subsystem.
	if rec := doRequest(t, h, http.MethodPost, "/listings", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("POST /listings without MarketWrite: status = %d, want 501", rec.Code)
	}
	for _, path := range []string{"/listings/1/cancel", "/listings/1/pay"} {
		if rec := doRequest(t, h, http.MethodPost, path, token, nil); rec.Code != http.StatusNotImplemented {
			t.Errorf("POST %s without MarketWrite: status = %d, want 501", path, rec.Code)
		}
	}
	if rec := doRequest(t, h, http.MethodGet, "/attestation", token, nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /attestation without Attestation: status = %d, want 501", rec.Code)
	}
	hNoLedger := New(WithSessions(testSessionStore(t)))
	if rec := doRequest(t, hNoLedger, http.MethodGet, "/stream", consoleTokenFor(t, hNoLedger, "0xtest"), nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /stream without Ledger: status = %d, want 501", rec.Code)
	}
}

// TestChainsRoute checks GET /chains: it serves the configured chain
// metadata (Solana is the one row), requires auth like the other console
// routes, and answers 501 when slotfared was started without a chain.
func TestChainsRoute(t *testing.T) {
	sol := ChainInfo{ChainKey: 0, Name: "Solana",
		RPC: "http://127.0.0.1:8899", Deposit: "TreasuryTokenAccount111111111111111111111111111",
		Explorer: "https://explorer.solana.com"}

	// The configured row is served verbatim.
	h := New(WithSessions(testSessionStore(t)), WithChains([]ChainInfo{sol}))
	token := consoleTokenFor(t, h, "0xtest")
	rec := doRequest(t, h, http.MethodGet, "/chains", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /chains status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Chains []ChainInfo `json:"chains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /chains body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(out.Chains) != 1 {
		t.Fatalf("chains = %+v, want 1 row (Solana)", out.Chains)
	}
	if out.Chains[0] != sol {
		t.Errorf("row = %+v, want %+v", out.Chains[0], sol)
	}

	// The row carries its public RPC on the wire when set; the swap key is
	// gone from the payload entirely (there is nothing to swap on one chain).
	var rawStruct struct {
		Chains []map[string]json.RawMessage `json:"chains"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawStruct); err != nil {
		t.Fatalf("raw chains not JSON: %v", err)
	}
	row := rawStruct.Chains[0]
	if v, ok := row["rpc"]; !ok {
		t.Error("Solana row missing rpc key on the wire")
	} else if string(v) != `"http://127.0.0.1:8899"` {
		t.Errorf("Solana row rpc = %s, want the configured endpoint", v)
	}
	if _, ok := row["swap"]; ok {
		t.Error("Solana row carries a swap key, want it absent")
	}

	// Auth and method rules match the other console routes.
	if rec := doRequest(t, h, http.MethodGet, "/chains", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodPost, "/chains", token, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /chains: status = %d, want 405", rec.Code)
	}

	// No chain configured: the offline demo path answers 501.
	hEmpty := New(WithSessions(testSessionStore(t)))
	if rec := doRequest(t, hEmpty, http.MethodGet, "/chains", consoleTokenFor(t, hEmpty, "0xtest"), nil); rec.Code != http.StatusNotImplemented {
		t.Errorf("GET /chains without chains: status = %d, want 501", rec.Code)
	}
}

// stubMinted is a MintedReader with canned balances that counts its reads.
type stubMinted struct {
	minted map[string]uint64
	calls  int
}

func (s *stubMinted) BalanceOf(_ context.Context, id string) (uint64, error) {
	s.calls++
	return s.minted[id], nil
}

// TestAccountSeedsMintedForUnknownAccount checks that the first /account read
// for an account the ledger has never seen consults the contract once and
// reports that balance as spendable; later reads come from the ledger.
func TestAccountSeedsMintedForUnknownAccount(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	stub := &stubMinted{minted: map[string]uint64{id: 30000}}
	env.h.Minted = stub
	token, _, err := env.h.tokens.issue(context.Background(), id, env.h.now())
	if err != nil {
		t.Fatalf("tokens.issue: %v", err)
	}

	read := func() map[string]any {
		req := httptest.NewRequest("GET", "/account", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		env.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	first := read()
	wantMicro := float64(30000 * meter.MicroCreditsPerCredit)
	if first["mintedMicro"] != wantMicro || first["spendableMicro"] != wantMicro {
		t.Fatalf("first read = %v, want minted and spendable %v micro-credits", first, wantMicro)
	}
	read()
	if stub.calls != 1 {
		t.Fatalf("contract reads = %d, want 1 (seed once, then the ledger)", stub.calls)
	}
}

// pagedUsage GETs /usage in paged mode and returns the parsed response.
func pagedUsage(t *testing.T, h *Handler, token, query string) struct {
	Usage  []usageView `json:"usage"`
	Total  int         `json:"total"`
	Facets struct {
		Prefixes []string `json:"prefixes"`
		Models   []string `json:"models"`
	} `json:"facets"`
} {
	t.Helper()
	rec := doRequest(t, h, http.MethodGet, "/usage"+query, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage%s status = %d, want 200; body: %s", query, rec.Code, rec.Body.String())
	}
	var out struct {
		Usage  []usageView `json:"usage"`
		Total  int         `json:"total"`
		Facets struct {
			Prefixes []string `json:"prefixes"`
			Models   []string `json:"models"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /usage%s body not JSON: %v; body: %s", query, err, rec.Body.String())
	}
	return out
}

// TestUsagePagedPagination covers the numbered pagination mode of /usage:
// paging walks the filtered set with correct totals, facets match the
// seeded rows, invalid params are rejected, and the default shape is
// unchanged.
func TestUsagePagedPagination(t *testing.T) {
	env := newReadsEnv(t)
	id := testAccount(t)
	token := consoleTokenFor(t, env.h, id)
	ctx := context.Background()

	const baseTS = 1_700_000_000
	// 12 rows: i even -> attk_aaaa / model-x (odd credits), i odd ->
	// attk_bbbb / model-y (even credits); ts ascending with i so row id
	// order equals ts order; i%5 == 0 rows are errors.
	prefixOf := func(i int) string {
		if i%2 == 0 {
			return "attk_aaaa"
		}
		return "attk_bbbb"
	}
	modelOf := func(i int) string {
		if i%2 == 0 {
			return "model-x"
		}
		return "model-y"
	}
	statusOf := func(i int) string {
		if i%5 == 0 {
			return "error"
		}
		return "ok"
	}
	for i := 0; i < 12; i++ {
		row := meter.UsageRow{
			AccountID: id, KeyPrefix: prefixOf(i),
			Timestamp: time.Unix(baseTS+int64(i), 0).UTC(),
			Service:   "rpc", Model: modelOf(i),
			Credits: uint64(i + 1), Status: statusOf(i),
		}
		if err := env.meterStore.RecordUsage(ctx, row); err != nil {
			t.Fatalf("RecordUsage %d: %v", i, err)
		}
	}

	// Page 2 of size 2: total matches the unfiltered count and the slice is
	// the right one of the newest-first sequence.
	p := pagedUsage(t, env.h, token, "?page=2&limit=2")
	if p.Total != 12 {
		t.Errorf("total = %d, want 12", p.Total)
	}
	if len(p.Usage) != 2 || p.Usage[0].Credits != 10 || p.Usage[1].Credits != 9 {
		t.Errorf("page 2 limit 2 credits = %v, want [10 9]", viewCredits(p.Usage))
	}

	// Walking every page reproduces the full newest-first sequence.
	var walked []uint64
	for page := 1; page <= 6; page++ {
		p := pagedUsage(t, env.h, token, fmt.Sprintf("?page=%d&limit=2", page))
		if p.Total != 12 {
			t.Errorf("page %d total = %d, want 12", page, p.Total)
		}
		for _, u := range p.Usage {
			walked = append(walked, u.Credits)
		}
	}
	if len(walked) != 12 {
		t.Fatalf("walk visited %d rows, want 12: %v", len(walked), walked)
	}
	for i, c := range walked {
		if c != uint64(12-i) {
			t.Errorf("walk order at %d = %d, want %d: %v", i, c, uint64(12-i), walked)
		}
	}

	// The combined filter narrows to exactly i=10 (credits 11): prefix
	// attk_aaaa, model-x, ts in [base+9, base+11], errors=true.
	p = pagedUsage(t, env.h, token,
		fmt.Sprintf("?page=1&prefix=attk_aaaa&model=model-x&from=%d&to=%d&errors=true", baseTS+9, baseTS+11))
	if p.Total != 1 || len(p.Usage) != 1 || p.Usage[0].Credits != 11 {
		t.Errorf("combined filters = %+v (total %d), want exactly credits 11", viewCredits(p.Usage), p.Total)
	}
	// Facets are account-wide, not filtered: both prefixes and both models.
	if len(p.Facets.Prefixes) != 2 || p.Facets.Prefixes[0] != "attk_aaaa" || p.Facets.Prefixes[1] != "attk_bbbb" {
		t.Errorf("facets prefixes = %v, want [attk_aaaa attk_bbbb]", p.Facets.Prefixes)
	}
	if len(p.Facets.Models) != 2 || p.Facets.Models[0] != "model-x" || p.Facets.Models[1] != "model-y" {
		t.Errorf("facets models = %v, want [model-x model-y]", p.Facets.Models)
	}

	// An empty result still carries total 0 and empty slices, not null.
	rec := doRequest(t, env.h, http.MethodGet, "/usage?page=1&prefix=attk_none", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage?page=1&prefix=attk_none status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if string(raw["usage"]) != "[]" {
		t.Errorf("empty paged usage = %s, want []", raw["usage"])
	}
	if string(raw["total"]) != "0" {
		t.Errorf("empty paged total = %s, want 0", raw["total"])
	}
	if string(raw["facets"]) == "" {
		t.Error("paged response missing facets")
	}

	// Invalid params: each rejected with its own reason.
	bad := map[string]string{
		"/usage?page=0":          "invalid page",
		"/usage?page=-1":         "invalid page",
		"/usage?page=abc":        "invalid page",
		"/usage?page=1&before=5": "invalid page",
		"/usage?page=1&from=abc": "invalid from",
		"/usage?page=1&to=abc":   "invalid to",
	}
	for q, wantReason := range bad {
		if rec := doRequest(t, env.h, http.MethodGet, q, token, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body: %s", q, rec.Code, rec.Body.String())
		} else if got := errorReason(t, rec); got != wantReason {
			t.Errorf("%s reason = %q, want %q", q, got, wantReason)
		}
	}

	// Default shape unchanged: no ?page means no total or facets keys, and
	// nextCursor behaves as before.
	rec = doRequest(t, env.h, http.MethodGet, "/usage", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /usage default status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	raw = map[string]json.RawMessage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("default body not JSON: %v; body: %s", err, rec.Body.String())
	}
	if _, ok := raw["total"]; ok {
		t.Error("default /usage carries a total key, want it absent")
	}
	if _, ok := raw["facets"]; ok {
		t.Error("default /usage carries a facets key, want it absent")
	}
	var def struct {
		Usage      []usageView `json:"usage"`
		NextCursor int64       `json:"nextCursor"`
	}
	json.Unmarshal(raw["usage"], &def.Usage)
	json.Unmarshal(raw["nextCursor"], &def.NextCursor)
	if len(def.Usage) != 12 {
		t.Errorf("default /usage returned %d rows, want 12", len(def.Usage))
	}
	if def.NextCursor == 0 {
		t.Error("default /usage lost nextCursor")
	}
}

func viewCredits(v []usageView) []uint64 {
	out := make([]uint64, len(v))
	for i, u := range v {
		out[i] = u.Credits
	}
	return out
}
