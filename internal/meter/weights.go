package meter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Class is the pricing class of an RPC method. The credits a class costs are
// configured, not compiled in; the default table is in DefaultWeights. Costs
// are configured in credits and returned by Cost and BatchCost in
// micro-credits, the internal metering unit.
type Class uint8

// Pricing classes, cheapest first.
const (
	ClassTrivial Class = iota + 1
	ClassStandard
	ClassHeavy
	ClassArchive
)

// String names the class for logs. Unknown values report "unknown" so a bad
// config never prints garbage.
func (c Class) String() string {
	switch c {
	case ClassTrivial:
		return "trivial"
	case ClassStandard:
		return "standard"
	case ClassHeavy:
		return "heavy"
	case ClassArchive:
		return "archive"
	default:
		return "unknown"
	}
}

// Weights prices JSON-RPC requests: a credit cost per class, configured in
// credits, and a table of method to class. An unknown method costs the
// standard weight; the caller decides what to log. Cost and BatchCost
// return micro-credits.
type Weights struct {
	classCost  map[Class]uint64
	methods    map[string]Class
	batchLimit int
}

// DefaultWeights returns the compiled-in table from the specification:
// trivial 1, standard 2, heavy 20, archive 60 credits. Batches may hold up
// to 100 members unless a config file says otherwise.
//
// The table prices Solana JSON-RPC, the chain this deployment meters: the
// metered gateway forwards to a Solana cluster, so a Solana method must hit
// its own class instead of falling through to the generic standard weight.
// Methods are grouped cheapest first, and the split follows what a Solana
// node actually does per call: trivial calls read a cached header or a
// single slot, standard calls deserialize one account, transaction or
// (plain) block, and heavy calls scan a set whose size the caller chooses -
// every account owned by a program, an address's whole signature history, a
// slot range, or an epoch's leader schedule.
//
// A small block of EVM methods is kept below the Solana table. It is not
// leftover wiring: the gateway is a generic HTTP proxy, so an operator who
// points SLOTFARE_UPSTREAM at an EVM endpoint still prices sanely rather than
// charging every eth_* call the standard weight. Drop the block with
// SLOTFARE_WEIGHTS if you want a Solana-only table.
func DefaultWeights() Weights {
	return Weights{
		classCost: map[Class]uint64{
			ClassTrivial:  1,
			ClassStandard: 2,
			ClassHeavy:    20,
			ClassArchive:  60,
		},
		methods: map[string]Class{
			// --- Solana: trivial (1) -------------------------------------
			// Cluster/mint metadata and single-value reads that need no
			// account deserialization.
			"getHealth":                         ClassTrivial,
			"getVersion":                        ClassTrivial,
			"getGenesisHash":                    ClassTrivial,
			"getIdentity":                       ClassTrivial,
			"getSlot":                           ClassTrivial,
			"getBlockHeight":                    ClassTrivial,
			"getEpochInfo":                      ClassTrivial,
			"getEpochSchedule":                  ClassTrivial,
			"getFirstAvailableBlock":            ClassTrivial,
			"getLatestBlockhash":                ClassTrivial,
			"getRecentBlockhash":                ClassTrivial, // deprecated alias of getLatestBlockhash
			"getFeeForMessage":                  ClassTrivial,
			"getMinimumBalanceForRentExemption": ClassTrivial,
			"getMinimumLedgerSlot":              ClassTrivial,
			"getTransactionCount":               ClassTrivial,
			"getSlotLeader":                     ClassTrivial,
			"getHighestSnapshotSlot":            ClassTrivial,
			"getMaxRetransmitSlot":              ClassTrivial,
			"getMaxShredInsertSlot":             ClassTrivial,
			"getStakeMinimumDelegation":         ClassTrivial,
			"getSupply":                         ClassTrivial,
			"getInflationRate":                  ClassTrivial,
			"getInflationGovernor":              ClassTrivial,
			"getVoteAccounts":                   ClassTrivial,
			"isBlockhashValid":                  ClassTrivial,

			// --- Solana: standard (2) ------------------------------------
			// One account, balance, transaction, plain block or submission.
			"getAccountInfo":              ClassStandard,
			"getMultipleAccounts":         ClassStandard,
			"getBalance":                  ClassStandard,
			"getTokenAccountBalance":      ClassStandard,
			"getTokenSupply":              ClassStandard,
			"getTokenAccountsByOwner":     ClassStandard,
			"getTokenAccountsByDelegate":  ClassStandard,
			"getTransaction":              ClassStandard,
			"getSignatureStatuses":        ClassStandard,
			"getBlockTime":                ClassStandard,
			"getBlockCommitment":          ClassStandard,
			"getStakeActivation":          ClassStandard,
			"getClusterNodes":             ClassStandard,
			"getRecentPerformanceSamples": ClassStandard,
			"getRecentPrioritizationFees": ClassStandard,
			"getInflationReward":          ClassStandard,
			"sendTransaction":             ClassStandard,
			"simulateTransaction":         ClassStandard,
			// Solana's getBlock returns transaction signatures by default and
			// is priced heavy when params ask for full transaction objects.
			"getBlock": ClassStandard,

			// --- Solana: heavy (20) --------------------------------------
			// The result set size is the caller's to choose: a full program
			// account scan, an address's signature history, a slot range.
			"getProgramAccounts":      ClassHeavy,
			"getSignaturesForAddress": ClassHeavy,
			"getBlocks":               ClassHeavy,
			"getBlocksWithLimit":      ClassHeavy,
			"getLargestAccounts":      ClassHeavy,
			"getTokenLargestAccounts": ClassHeavy,
			"getSlotLeaders":          ClassHeavy,
			"getLeaderSchedule":       ClassHeavy,
			"getBlockProduction":      ClassHeavy,

			// --- EVM passthrough (kept for a non-Solana SLOTFARE_UPSTREAM) ---
			"eth_chainId":              ClassTrivial,
			"net_version":              ClassTrivial,
			"eth_blockNumber":          ClassTrivial,
			"eth_call":                 ClassStandard,
			"eth_getBalance":           ClassStandard,
			"eth_getTransactionByHash": ClassStandard,
			"eth_getBlockByNumber":     ClassStandard, // heavy when full transactions are requested
			"eth_getLogs":              ClassHeavy,
		},
		batchLimit: 100,
	}
}

// Cost returns the micro-credit cost of one call, its pricing class, and
// whether the method was recognized (table hit or archive prefix). An
// unknown method is charged the standard weight with false, so it can be
// logged without being refused or made free.
//
// Params only affect price for the two block getters that can return whole
// blocks: eth_getBlockByNumber (second parameter true) and Solana's getBlock
// (config object with transactionDetails "full") make the call heavy. Every
// other method is priced from its name alone; params are never inspected on
// this hot path.
//
// The debug_/trace_ prefixes are the archive tier: that is the EVM
// trace namespace, which Solana does not have, so on a Solana upstream the
// tier stays reachable only through an explicit SLOTFARE_WEIGHTS override.
func (w Weights) Cost(method string, params json.RawMessage) (uint64, Class, bool) {
	if strings.HasPrefix(method, "debug_") || strings.HasPrefix(method, "trace_") {
		return w.classCost[ClassArchive] * MicroCreditsPerCredit, ClassArchive, true
	}
	class, ok := w.methods[method]
	if !ok {
		return w.classCost[ClassStandard] * MicroCreditsPerCredit, ClassStandard, false
	}
	switch method {
	case "eth_getBlockByNumber":
		if wantsFullTransactions(params) {
			return w.classCost[ClassHeavy] * MicroCreditsPerCredit, ClassHeavy, true
		}
	case "getBlock":
		if wantsFullBlock(params) {
			return w.classCost[ClassHeavy] * MicroCreditsPerCredit, ClassHeavy, true
		}
	}
	return w.classCost[class] * MicroCreditsPerCredit, class, true
}

// wantsFullTransactions reports whether eth_getBlockByNumber params ask for
// full transaction objects: a JSON array whose second element is boolean
// true. Anything else - missing, malformed, non-array, wrong type - is
// treated as plain, so a bad body can never panic or inflate the price.
func wantsFullTransactions(params json.RawMessage) bool {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return false
	}
	if len(args) < 2 {
		return false
	}
	var full bool
	if err := json.Unmarshal(args[1], &full); err != nil {
		return false
	}
	return full
}

// wantsFullBlock reports whether Solana getBlock params ask for full
// transaction objects: [slot, {..., "transactionDetails": "full"}].
// transactionDetails is a string enum ("full", "accounts", "signatures",
// "none"), not a boolean, so this is the Solana counterpart of
// wantsFullTransactions. Anything else - missing, malformed, non-array, a
// non-object config, an unknown value - is treated as plain, so a bad body
// can never panic or inflate the price.
func wantsFullBlock(params json.RawMessage) bool {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return false
	}
	if len(args) < 2 {
		return false
	}
	var cfg struct {
		TransactionDetails string `json:"transactionDetails"`
	}
	if err := json.Unmarshal(args[1], &cfg); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(cfg.TransactionDetails), "full")
}

// BatchRequest is one member of a JSON-RPC batch.
type BatchRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// BatchCost prices a JSON-RPC batch as the sum of its members, in
// micro-credits, and rejects a batch above the configured member limit with
// an error naming both counts.
// A body that is not a JSON array is an error.
func (w Weights) BatchCost(body json.RawMessage) (uint64, error) {
	var members []BatchRequest
	if err := json.Unmarshal(body, &members); err != nil {
		return 0, fmt.Errorf("meter: batch body is not a JSON array: %v", err)
	}
	if len(members) > w.batchLimit {
		return 0, fmt.Errorf("meter: batch of %d exceeds limit of %d", len(members), w.batchLimit)
	}
	var total uint64
	for _, m := range members {
		cost, _, _ := w.Cost(m.Method, m.Params)
		total += cost
	}
	return total, nil
}

// LoadWeights reads a JSON file that overrides the defaults, so pricing is
// configuration rather than a redeploy. A missing file returns the defaults
// and no error; a present file must be valid.
//
// Format (all keys optional, flat):
//
//	{
//	  "classes": {"trivial": 1, "standard": 2, "heavy": 20, "archive": 60},
//	  "methods": {"getBalance": "trivial"},
//	  "batch_limit": 50
//	}
//
// method keys are matched exactly, so a key the default table does not list
// (a Solana method added to the spec after this build, or an EVM method in
// the passthrough block) can be priced without a redeploy.
//
// Only the keys that are present override anything; everything else keeps
// its default. Unknown class names are an error.
func LoadWeights(path string) (Weights, error) {
	w := DefaultWeights()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return w, nil
		}
		return w, fmt.Errorf("meter: read weights file: %w", err)
	}
	var cfg struct {
		Classes    map[string]uint64 `json:"classes"`
		Methods    map[string]string `json:"methods"`
		BatchLimit *int              `json:"batch_limit"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return w, fmt.Errorf("meter: parse weights file %s: %w", path, err)
	}
	for name, cost := range cfg.Classes {
		class, ok := parseClass(name)
		if !ok {
			return w, fmt.Errorf("meter: unknown class %q in %s", name, path)
		}
		w.classCost[class] = cost
	}
	for method, name := range cfg.Methods {
		class, ok := parseClass(name)
		if !ok {
			return w, fmt.Errorf("meter: unknown class %q for method %s in %s", name, method, path)
		}
		w.methods[method] = class
	}
	if cfg.BatchLimit != nil {
		if *cfg.BatchLimit <= 0 {
			return w, fmt.Errorf("meter: batch_limit must be positive in %s", path)
		}
		w.batchLimit = *cfg.BatchLimit
	}
	return w, nil
}

// parseClass maps a config class name onto its constant.
func parseClass(name string) (Class, bool) {
	switch name {
	case "trivial":
		return ClassTrivial, true
	case "standard":
		return ClassStandard, true
	case "heavy":
		return ClassHeavy, true
	case "archive":
		return ClassArchive, true
	default:
		return 0, false
	}
}
