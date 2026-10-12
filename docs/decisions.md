# Decision log

Each entry names a decision, why it was taken, what it costs, and where it
lives in the code. Entries are append-only: superseding an entry means adding
a new one that says so, never rewriting the old one.

## D1 - `deployments/` is the single source of truth for addresses

An address pasted anywhere else is an address that will be wrong in one of
the two places. `.env` is untracked and may hold a treasury for local work;
`deployments/` holds what was actually deployed. Consequence: this file must
be updated as part of any deployment, not after it.

## D2 - One deposit rail

The original design had three ways in: a deposit contract on a source chain,
a Uniswap swap peripheral, and direct intake of its native token. This port
keeps **one**: a USDC SPL transfer to the treasury token account. The swap
peripheral is dropped rather than ported.

Why: the swap added a quoting service, a peripheral deployment and a second
failure mode for a path no judge would exercise, while the metering product
does not care where the USDC came from.

Cost: no token-agnostic intake; a user holding something else must swap
outside Slotfare. (The console's `SwapLeg` wire field and the swap code are
gone with it.)

## D3 - The local ledger is the balance authority

Credits live in SQLite (in-memory, checkpointed) or Postgres (a shared
accounts table), not in a chain contract as in the original.

Why: the port has one chain and a single minter; moving balances on chain
would add a contract, an instruction set, and a coupling to Solana
throughput for no product gain in this window.

Cost: **the trust model is weaker than the original.** The chain no longer
holds balances, and the mint is derived by a process we run. This is the
largest single regression of the port and it is documented in
[trust-model.md](./trust-model.md), not buried.

## D4 - Settlement commits through a Memo, not a usage contract

Each settle pass lands the batch's Merkle root as a Memo program
transaction (`slotfare-usage-root/v1`), burning the covered spend in the same
store transaction.

Why: a Memo needs no program deployment, no upgrade authority, and no
on-chain rent; the root still gets a public, timestamped home.

Cost: the original's settlement contract could reject a malformed
claim on chain. A Memo cannot - it is opaque bytes, and anyone can post one.
It evidences authorship and ordering, not validity.

## D5 - No cross-chain deposit proof

The original's deposit-proof worker and its proof verifier are not ported.
Deposits are seen by a watcher reading Solana, at a fixed conversion.

Why: with a single chain there is no second chain to attest against; the
proof machinery would exist to prove something the deposit rail no longer
does.

Cost: no chain-enforced deposit proof. Deposits stay **chain-derivable**
(public transfer history plus a fixed rate), but "derivable by a verifier"
is not "proven by the chain". See [trust-model.md](./trust-model.md).

## D6 - Price Solana natively, keep EVM methods as passthrough

The weight table prices Solana JSON-RPC methods in their own classes, and
keeps a small EVM block so a non-Solana `SLOTFARE_UPSTREAM` is not charged the
generic standard weight for every call.

Why: before this, every `getBalance`/`getProgramAccounts` call fell through
to the generic standard weight, so the gateway mispriced exactly the calls
this product serves.

Cost: one table with two dialects. It is documented as such in
[metering.md](./metering.md), and `SLOTFARE_WEIGHTS` can drop either.

## D7 - Chain key 0 is Solana

The console and the funding store keep their historical chain-key column.
Key 0 is this deployment's chain; key 1 remains the stamp on legacy
pre-existing rows.

Why: renaming the column would touch every storage path and every import,
for no behavioural gain. The meaning is pinned by convention and asserted in
tests.

Cost: a reader must know that key 0 is the live chain and anything else is
imported history.

## D8 - A purchase is direct by chain, not by source block

The console used to decide "credited in the paying transaction" from
`sourceBlock == 0`. Solana deposits carry a real slot, so every live deposit
read as a three-step proof journey - complete with an attestation step this
deployment does not have.

The rule is now: **chain key 0 means direct** (one deposit step); any other
chain keeps the legacy three-step anatomy, because for those rows the
deposit, the attestation and the mint really were separate events.

Why: showing a proof journey that does not run is a false claim about the
product, not a cosmetic bug.

Cost: the proven-purchase UI is only reachable for imported rows. That is
the truth of the deployment - see [trust-model.md](./trust-model.md).

## D9 - Metered inference proxies NVIDIA's free catalog

Inference is served through a Fireworks-compatible upstream
(`SLOTFARE_INFERENCE_UPSTREAM`), defaulting to NVIDIA's free catalog, with a
model registry that carries per-million credit prices.

Why: a judge can touch inference without an account or a payment, and the
credit math is the same math the RPC path uses.

Cost: pricing tracks a free external catalog; a model can disappear under us.

## D10 - Units stay integer micro-credits, 1,000 credits per USD

Every balance, reservation and settle total is an integer in micro-credits
(10,000 per credit). The mint rate is fixed at 10 micro-credits per USDC
base unit.

Why: floats in a ledger produce invisible drift; the original's headline
rate is preserved so the console's numbers read the same.

Cost: every boundary converts once, and a missed conversion is a factor of
10,000 bug. The conversion points are in
[`internal/solana/watcher.go`](../internal/solana/watcher.go) and
`cmd/slotfared/main.go`.

## D11 - Console sign-in is a server-built message, signed by Phantom

Sign-in builds the message server side, the wallet signs ed25519 over it, and
the server verifies, with nonce replay and wrong-signer rejection covered by
a live smoke test.

Why: the client must not choose what it is signing; a client-built message is
a signature over attacker-chosen text.

Cost: the session API needs the exact message bytes, so the console and the
daemon are coupled on a small format that must not drift.

## D12 - Historical specs are not rewritten

`web/docs/figures.md` is the spec-first record of the original console's
figures: what each screen shows and where each number comes from. It is kept
as history; only the old project's chain and brand names have been replaced
so the record reads as this project's.

Why: the record of what was asked for is evidence about the project, and
editing it would destroy that. Current behaviour is described in
[trust-model.md](./trust-model.md), [metering.md](./metering.md) and this
log; where they disagree with a historical request, the current docs win.

## D13 - An on-chain ledger program, superseding part of D3 and D4

`programs/slotfare-ledger` puts balances, locks and settlement roots in
program-owned PDAs and gates the burn on a committed root inside the same
instruction. Setting `SLOTFARE_LEDGER_PROGRAM` selects it for both the deposit
mirror and settlement; leaving it unset keeps D3 and D4 exactly as they were.

Why: D3 named the local balance authority as the port's largest regression and
D4 named the Memo's inability to reject a malformed claim as its cost. Both
were consequences of not having a contract. A Solana program is the analogue of
the original's settlement contract holding the ledger's spender role, so the
regression is closable rather than inherent, and it turns our three settlement
invariants from conventions this codebase follows into rules the chain
enforces.

Cost: a deployed program, an upgrade authority, and rent for every credit
account. The mint is still trusted - the chain cannot see a deposit, so
`Credit` is gated on the authority key, not on proof. Balances are also now
held in two places while both rails run, with the chain reconciled from the
deposit rail rather than gating the hot path; a lag shows up as a settlement
that waits a tick, which loses no usage but is real. And the artifact is
version-sensitive: Agave's SIMD-0500 disables SBPF v0-v2 deployment while
`cargo-build-sbf` still defaults to v0, so the build must pin `--arch v3` or
nothing deploys. That is `programs/slotfare-ledger/build.sh`'s whole job.

Supersedes: D3's "not in a chain contract", and D4's "a Memo needs no program
deployment" as the settlement mechanism - both remain true for a deployment
without the program. Full description:
[onchain-ledger.md](./onchain-ledger.md).

## D14 - The ledger rail is opt-in, not a replacement, during the port

`SLOTFARE_LEDGER_PROGRAM` unset is a fully working deployment; set, it upgrades
settlement and the deposit mirror. The daemon does not refuse to start a Memo
settler when a program is configured, and the market still moves credits
locally only.

Why: the market's `LedgerLock`/`LedgerTransfer` wiring and a ledger-only mode
are still to do, and a half-wired program that silently replaced the working
rail would be worse than two rails that each do what they say. The live test
proves the program's rules; this decision keeps the gap visible instead of
hiding it behind a flag that looks finished.

Cost: two code paths for one concept, and a reader has to know which one a
deployment took. Every affected doc states both, and
[onchain-ledger.md](./onchain-ledger.md) ends with an honest wiring table
rather than a claim of completeness.

## D15 - The market is an on-chain escrow, and a sale is one transaction

The program grows the original `CreditMarket`: `List` escrows a seller's credits
in a program-owned PDA, `Buy` pays the seller and releases the escrow in one
atomic transition (an SPL `TransferChecked` CPI plus the credit move), and
`CancelList` returns the escrow. Tags 7-9 are appended; nothing is renumbered.

Why: with the program's existing `Transfer` alone the seller is the signer, and
the seller is offline when a buyer arrives, so a native buy was impossible
without either trusting the server or an escrow. The escrow is what the original
did - `list` moved credits into the contract so `buy` released credits already
given up - so this is parity, not invention. It also makes the console's own
copy true: one transaction does both.

Cost: a listing account (125 bytes) and an escrow account (76 bytes) per open
listing, plus rent. The escrow moves the seller's credits to `spent` at list
time and back to `minted` at cancel, so `spent` stays monotonic but `minted`
grows for a seller who lists and cancels, which the docs state rather than
hide. The buy needs both token accounts to exist, so the buyer's transaction
carries an idempotent associated-token-account create - the seller never has to
have held USDC before.

Supersedes: the plan in D14's cost note to wire `LedgerLock`/`LedgerTransfer`
into the console's existing routes. The escrow market is the design to wire;
`Lock`/`Unlock` remain in the program for the pre-escrow testing they were
built for, and `Transfer` remains the direct owner-to-owner move.

## D16 - The console writes market state only after re-reading the chain

The console's market is two round trips instead of one route per action.
`POST /console/listings/onchain/prepare` builds the instruction and returns the
standard Solana wire transaction, base64, with the caller as fee payer;
`POST /console/listings/onchain/confirm` takes the signature back and records a
row only once the listing account says the write happened.

Why: the daemon never holds a user's key, so it cannot submit a market
instruction itself, and a route that merely trusted the client's claim to have
signed would let a caller invent a listing the program never opened. Reading the
listing back is what makes the row a fact about the chain rather than a
statement by the caller: a confirm that finds nothing, or finds a listing sold
to somebody else, is refused. The row's credits, price and mint come from the
listing account's own bytes, so a client cannot record terms it did not commit
to. `confirm` on a list is idempotent - a wallet retrying after a dropped
response gets the same row rather than a second escrow.

Cost: two HTTP calls and a stored `listing_nonce` per row (the PDA is
`[b"listing", seller, nonce]`, so the row must carry it to be actionable), plus
a pending-transaction window in which the user can simply not sign. Nothing is
written before the signature, so a prepare that is abandoned leaves no trace.

Coexistence: with `SLOTFARE_LEDGER_PROGRAM` unset both routes answer 501 and
the previous local-mirror routes (`POST /listings`, `/listings/{id}/cancel`,
`/listings/{id}/pay`) keep serving exactly as they did. That is deliberate -
the same dual-rail shape as `CommitAndSettle` - so a deployment without an
escrow is not broken by this change.

## D17 - The console records its own trades, not the whole market

The original's console was a **mirror** of the contracts: `internal/console/
feeder.go` subscribed to `CreditsPurchased`, `CreditsMinted`, `CreditsListed`
and `ListingCancelled` over `ethclient` and wrote a row for each event, so a
listing or a sale appeared in the console however it had been made. Slotfare
records a row only when the console itself drove the write - a prepare/confirm
pair re-reading the listing account (D16) - and has no event indexer.

Why: the console's row is a consequence of chain state it can verify
directly, which is a stronger guarantee per row than an event log replay, and
it needs no second subscriber to be correct. Reading every event also means
backfill, reorg handling and a cursor, none of which a single-writer console
needs.

Cost, and it is a real one: **the console only knows about trades it made.**
A listing opened, bought or cancelled by a wallet talking to the program
directly - which is entirely possible, the program is public - is invisible in
`/console/market` and `/console/listings`, and so is its seller's `seller`
address until something else records it. On the original the feeder made the
console complete; here it is complete only over its own traffic. Anyone
comparing the two should know that before reading the market screen as a full
picture. Closing it means the indexer the original had: a subscriber per
program event with a cursor, writing rows the same way `confirm` does.

## D18 - The console polyfills `Buffer` for the browser

The console did not boot in a browser: it served a blank page with
`ReferenceError: Buffer is not defined`. `@solana/spl-token` and its BigInt
helpers touch the Node `Buffer` global at module scope, and the Angular dev
server (Vite underneath) externalizes Node built-ins for the browser, so the
global is absent while the dependency graph is still evaluating - the throw
lands before `bootstrapApplication` runs, so nothing catches it and nothing
renders. Fixed by a `src/polyfills.ts` entry, listed ahead of the application
bundle, that installs the `buffer` package on `globalThis`, with `buffer`
promoted from a transitive dependency to a direct one (`web/package.json`).

Why it went unnoticed until the console was actually loaded: the two gates
that should have caught it cannot. `ng test` runs under Node, where `Buffer`
is a real global, so every spec passes; `ng build` only proves the bundle
holds together, and a missing runtime global is not a bundling error. It took
running the dev server and reading the page, which is the point worth keeping:
a green suite and a clean build are not a rendering check.

Cost: one direct dependency and one build entry, and a shim that has to keep
up with how far the Solana packages lean on Node globals. The alternative -
re-deriving the SPL token instructions on `@solana/web3.js` primitives to drop
`spl-token` from the browser bundle - is real work and was not taken here.

## D19 - The sign-in message names the page the user is on

Sign-in with a real wallet failed: Phantom refused with "The app's signature
request cannot be shown due to invalid formatting" and never showed a prompt.
The fault was the message, not the signature. `Domain` and `URI` defaulted to
the placeholders `slotfare` and `https://slotfare`, and Phantom validates any
message it recognises as Sign-In with Solana against the requesting page:
the domain must be that page's exact host, port included, and the uri's origin
must match the page's origin. A placeholder matches no page, so the message
was unshowable everywhere - the wallet refuses before the user is involved.

Both are now derived per request in `signInDomainURI`: the browser's `Origin`
header first, then the request Host (which behind the dev-server proxy is the
console's host anyway), then a configured value. The message therefore names
whatever origin the console is really served from - the dev server on `:4200`,
a deployed console, or localhost - and `WithDomain`/`WithURI` remain as a pin.

Cost: the domain is decided by the caller rather than configured, so the
message differs per origin and a message captured at one origin will not
verify at another. That is the anti-phishing binding the standard asks for,
but it does mean a deployment that pins a domain other than the page's host
is refused by the wallet exactly as it was before this change.

Why neither gate caught it: the unit tests sign with a local ed25519 key and
the live smoke test rebuilds the message from the nonce response. Both are the
server talking to itself, so neither exercises the wallet's own validation.
It took a real Phantom prompt, the same way D18 took a real page load.

## D20 - A rescan can never lower a minted total, and the market gets its feeder after all

Three audit fixes, decided together because they touch the same accounting
surface.

**The rescan guard.** `SetMinted` was absolute: whatever a rescan last saw
became the recorded total. Against an RPC with bounded transaction history
that is a fund-management bug - a rescan that only sees newer deposits
rewrites 2,500 credits down to 1,000 while the chain's PDA still says 2,500.
The write is now split by caller: `SetMinted` stays absolute for the settler
(the reconcile path that wins is on-chain state, and it may legitimately
lower an inflated row), while the deposit rail's `RaiseMinted` is monotonic -
its SQL carries a `CASE` guard that keeps the higher of current and scanned,
in both the SQLite stand-in and Postgres, and the deposit rail returns
`errDepositScanGap` when a scan lands below the recorded total instead of
quietly accepting it. The watcher then treats the pass as failed and retries
on its next sweep, so a gap is surfaced, not swallowed. The invariant - a
rescan can never lower minted - is pinned by regression tests at every layer
(ledger, store SQL, pg store, deposit rail), including restart-rescan.

**Deposited vs received.** `minted` documented credits the deposit rail
created, but a marketplace `buy` rode the same accounting, so Bob's
purchased credits looked like Bob's deposits. Pay now rides the signed
`Adjust` axis and never `Minted`: a purchase adjusts the buyer's spendable
down and the seller's up, conservation holds, and `Minted` stays the
deposit-rail counter. Pinned by an Alice-sells/Bob-buys test asserting Bob's
`Minted` stays zero.

**The event indexer.** Slotfare inherited the gap D17 recorded: the console
only knew trades it made itself. The port now closes it with the feeder's
Solana equivalent - not a subscription, since Solana programs emit no events.
`internal/solana/indexer.go` replays the ledger program's transaction
signature history page by page, decodes each confirmed transaction, reads
the instruction tags (List=7, Buy=8, CancelList=9 - the same tags the Go
client and the Rust program pin), and resolves a sale or cancel's listing
PDA for its seller and nonce. The console's `MarketMirror` adapter feeds the
same listings table the prepare/confirm route writes, reusing the guarded,
id-based moves, so an indexer row and a console row are one row. The unit of
exactly-once is the same derivation both sides agree on: the listing PDA's
last 8 little-endian bytes, which makes a rescan an INSERT OR IGNORE and a
replayed sale or cancel lose the state='open' guard. A buy of a listing that
cannot be read is skipped, not applied. The daemon runs it on a ticker
(`SLOTFARE_INDEXER_INTERVAL_SECONDS`) whenever `SLOTFARE_LEDGER_PROGRAM` is
set.

Cost: the indexer is a full rescan per pass rather than a cursor, which is
fine at hackathon scale and self-healing by construction, but a long
signature history makes each pass O(history). ed25519 receipts are a second
change riding this window: the receipt envelope is version 2, signed with
Solana Ed25519 keys (no keccak, no ecrecover); v1 secp256k1 envelopes are
explicitly rejected rather than verified, so a pre-migration receipt stops
validating instead of silently passing.
