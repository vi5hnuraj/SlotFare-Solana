# Metering and RPC pricing

The metered `/rpc` gateway prices every JSON-RPC request before it forwards
it. This page is the specification for that price: the classes, the built-in
Solana table, the two methods whose price depends on their parameters, how a
deployment overrides the table, and what an unrecognized method costs.

Source of truth: [`internal/meter/weights.go`](../internal/meter/weights.go)
(the table and its tests), [`internal/proxy/proxy.go`](../internal/proxy/proxy.go)
(the pricer that reserves the credits). When this page and the code
disagree, the code is right and this page is a bug.

## Units

| Unit | Value |
|---|---|
| credit | the account-facing unit; 1,000 credits = $1.00 |
| micro-credit | 1/10,000 credit, the internal integer unit (`MicroCreditsPerCredit = 10_000`) |
| token base unit | the raw SPL amount; a 6-decimal USDC base unit mints 10 micro-credits (`MicroCreditsPerUSDCBase = 10`), i.e. 1 USDC = 1,000 credits |

Balances, reservations and every settlement total are integers in
micro-credits. Nothing rounds a float.

## Classes

Four classes, cheapest first. The cost is configured in credits:

| Class | Default | Typical call |
|---|---|---|
| `trivial` | 1 credit ($0.001) | cluster metadata, a single cached value |
| `standard` | 2 credits ($0.002) | one account, balance, transaction or plain block |
| `heavy` | 20 credits ($0.02) | a result set whose size the caller chooses |
| `archive` | 60 credits ($0.06) | historical/trace lookups (see below) |

The ladder is a design choice, not a measurement: the split follows what a
Solana node actually does per call. Trivial calls read a cached header or a
single slot. Standard calls deserialize one account, transaction or block.
Heavy calls scan a set the caller sizes: every account owned by a program,
an address's whole signature history, a slot range, an epoch's leader
schedule.

## The built-in table

The compiled-in table prices **Solana JSON-RPC**, the chain this deployment
meters.

**trivial (1 credit).** `getHealth`, `getVersion`, `getGenesisHash`,
`getIdentity`, `getSlot`, `getBlockHeight`, `getEpochInfo`,
`getEpochSchedule`, `getFirstAvailableBlock`, `getLatestBlockhash`,
`getRecentBlockhash` (deprecated alias), `getFeeForMessage`,
`getMinimumBalanceForRentExemption`, `getMinimumLedgerSlot`,
`getTransactionCount`, `getSlotLeader`, `getHighestSnapshotSlot`,
`getMaxRetransmitSlot`, `getMaxShredInsertSlot`, `getStakeMinimumDelegation`,
`getSupply`, `getInflationRate`, `getInflationGovernor`, `getVoteAccounts`,
`isBlockhashValid`.

**standard (2 credits).** `getAccountInfo`, `getMultipleAccounts`,
`getBalance`, `getTokenAccountBalance`, `getTokenSupply`,
`getTokenAccountsByOwner`, `getTokenAccountsByDelegate`, `getTransaction`,
`getSignatureStatuses`, `getBlockTime`, `getBlockCommitment`,
`getStakeActivation`, `getClusterNodes`, `getRecentPerformanceSamples`,
`getRecentPrioritizationFees`, `getInflationReward`, `sendTransaction`,
`simulateTransaction`, `getBlock` (see below).

**heavy (20 credits).** `getProgramAccounts`, `getSignaturesForAddress`,
`getBlocks`, `getBlocksWithLimit`, `getLargestAccounts`,
`getTokenLargestAccounts`, `getSlotLeaders`, `getLeaderSchedule`,
`getBlockProduction`.

**archive (60 credits).** No Solana method is in this tier by default.
Solana has no `debug_`/`trace_` namespace, so the archive tier is reached
two ways: the EVM-compatible `debug_*` and `trace_*` prefixes (see the
passthrough block), or an explicit `SLOTFARE_WEIGHTS` override for a method
that needs it on your endpoint.

### EVM passthrough block

The table also keeps `eth_chainId`, `net_version`, `eth_blockNumber`,
`eth_call`, `eth_getBalance`, `eth_getTransactionByHash`,
`eth_getBlockByNumber` and `eth_getLogs`. This is deliberate: the gateway is
a generic HTTP proxy, so an operator who points `SLOTFARE_UPSTREAM` at an EVM
endpoint still prices those calls sanely instead of charging every one the
generic standard weight. Delete the block with an explicit `SLOTFARE_WEIGHTS`
table if you want a Solana-only deployment.

## The two parameter-sensitive methods

A method is normally priced from its name alone; parameters are never
inspected on the hot path. Two methods are the exception, because a boolean
or an enum in the parameters changes how much a node returns:

| Method | Rule | Class |
|---|---|---|
| `eth_getBlockByNumber` | second parameter is boolean `true` (full transaction objects) | heavy |
| `getBlock` | second parameter is an object with `"transactionDetails": "full"` | heavy |

Anything else - missing, malformed, non-array, non-object, an unknown enum
value - prices as plain. A bad request body can never panic the pricer and
can never inflate the price.

## Batches

A JSON-RPC batch is priced as the sum of its members. A batch above the
member limit (default 100, Solana's own maximum) is rejected with an error
naming both counts, and a body that is not a JSON array is an error. The
whole batch is reserved or the whole batch is refused; there is no partial
charge.

## Unrecognized methods

An unknown method is charged the standard weight and reported as
unrecognized, which `internal/proxy` logs as
`slotfared: unrecognized method %q charged standard weight`. It is never free
and never refused: a method added to the Solana spec after a build still
works, and the log line tells you to price it.

## Overriding the table

`SLOTFARE_WEIGHTS` (or `--weights`) names a JSON file. A missing file uses the
defaults; a present file must be valid, and only the keys that are present
override anything.

    {
      "classes": {"trivial": 1, "standard": 2, "heavy": 20, "archive": 60},
      "methods": {"getBalance": "heavy", "getProgramAccounts": "archive"},
      "batch_limit": 50
    }

Method keys are matched exactly, so a method the compiled-in table does not
list can be priced without a redeploy. Pricing is configuration, not code.
An unknown class name is a startup error, not a silent fallback.
