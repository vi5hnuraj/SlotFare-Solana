# The on-chain ledger program

`programs/slotfare-ledger` is the part of the port that puts balances, locks
and settlement roots **back on chain**, which is the one thing the original had
and the straight Memo port gave up.

The original kept its ledger in an on-chain contract and settled through a
single `commitAndSettle` call, where the contract held the ledger's spender
role, so the chain, not the operator, refused a burn without a committed root.
This program is that contract, rewritten for Solana:

- balances and locks live in program-owned PDAs, not in our database alone;
- a burn is a `CommitAndSettle` instruction, and the program rejects it unless
  the root is new, the usage range advances, and the caller is the configured
  authority.

Source of truth: [`src/state.rs`](../programs/slotfare-ledger/src/state.rs) (every
transition rule), [`src/processor.rs`](../programs/slotfare-ledger/src/processor.rs)
(authority, PDA and ownership checks), [`src/instruction.rs`](../programs/slotfare-ledger/src/instruction.rs)
(the wire format). The Go client that drives it is
[`internal/solana/ledger.go`](../internal/solana/ledger.go), and the two wire
formats are pinned to each other by golden-byte tests on both sides.

## Two build modes, on purpose

The crate has no Solana dependency unless the `onchain` feature is on:

| Build | Command | What it is |
|---|---|---|
| Rules only | `cargo test` | `state.rs` + `instruction.rs`, no runtime. 27 tests. |
| Deployable | `./build.sh` | The SBF artifact, `target/deploy/slotfare_ledger.so`. |

Use `build.sh`, not a bare `cargo build-sbf`. It pins **`--arch v3`**, which is
not optional on a current cluster: Agave activates SIMD-0500 ("Disable
deployment of SBPF v0, v1 and v2 programs") while `cargo-build-sbf`'s own
default is still v0, so the plain command produces an artifact the loader
refuses with

    Detected sbpf_version required by the executable which are not enabled

`solana-test-validator` activates that feature from genesis, so the failure
appears locally as well as on devnet. To check an artifact before shipping it,
read the ELF's `e_flags` at byte offset 48: v3 reads `0x03000000`, and anything
else is the undeployable build. `build.sh` prints it.

Why: every rule that decides whether a transition is legal — overspend,
double-commit, locked credits, monotonic usage ids, checked arithmetic — is
testable in milliseconds without an Agave toolchain. The feature gates exactly
the code that needs a runtime: account loading, PDA derivation, CPIs. So the
rules stay covered even on a machine with no `solana` CLI.

`cargo test --features onchain` adds 6 more tests (PDA determinism for both the
credit and the listing/escrow spaces, the error-code table, config-magic
disjointness, and the token-program bytes).

## Account model

Two PDA kinds, both with an ASCII magic so reading the wrong account is a
decode error and not a plausible-looking balance.

### Credit account — `[b"credit", owner]`, 76 bytes

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `SFCA` |
| 4 | 8 | `minted` — credits ever minted to this owner by the deposit rail |
| 12 | 8 | `spent` — credits consumed (burned by settlement, or moved out by a transfer) |
| 20 | 8 | `locked` — credits reserved by an open listing |
| 28 | 32 | `last_root` — the most recently committed usage root |
| 60 | 8 | `last_usage_id` — the highest usage id settled |
| 68 | 8 | `batch_count` — batches settled against this account |

`spendable = minted − spent − locked`, clamped at zero rather than allowed to
underflow into a large balance. Every field is little-endian.

Note what is **not** here: `pending`. Pending usage is an off-chain reservation
the metering proxy holds between reserving and settling; the chain only ever
sees the settled figure. The daemon subtracts its own pending reservations on
top of the on-chain `spendable`.

### Config account — `[b"config"]`, 100 bytes

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `SFCG` |
| 4 | 32 | `authority` — the only key allowed to mint or settle |
| 36 | 32 | `usdc_mint` — the deposit mint |
| 68 | 32 | `treasury` — the USDC token account deposits land in |

There is exactly one config account, and `InitConfig` is **first-caller-wins**.
That is why deployment and initialisation are one flow, not two: whoever calls
`InitConfig` first installs themselves as the authority. The instruction
therefore requires the signer to *be* the authority it names, so nobody can
install a third party, but it cannot stop a stranger reaching an uninitialised
program before you. Deploy and initialise in the same sitting — see the
runbook below.

## Instruction set

Every variant is one tag byte and a fixed, length-checked payload. The decoder
computes the exact total length each tag requires and rejects truncation,
trailing bytes and unknown tags, so a malformed message cannot be reinterpreted
as a different instruction.

| Tag | Instruction | Payload | Accounts |
|---|---|---|---|
| 0 | `InitCredit` | — | `[payer (signer, writable), owner, credit, system]` |
| 1 | `Credit` | `amount: u64` | `[authority (signer), config, owner, credit]` |
| 2 | `Lock` | `amount: u64` | `[owner (signer), credit]` |
| 3 | `Unlock` | `amount: u64` | `[owner (signer), credit]` |
| 4 | `CommitAndSettle` | `root: [u8;32]`, `first: u64`, `last: u64`, `credits: u64` | `[authority (signer), config, owner, credit]` |
| 5 | `Transfer` | `amount: u64` | `[from (signer, writable), from_credit, to, to_credit]` |
| 6 | `InitConfig` | `authority`, `usdc_mint`, `treasury` (3×`[u8;32]`) | `[authority (signer, writable), config, system]` |
| 7 | `List` | `credits: u64`, `price: u64`, `nonce: u64`, `pay_mint: [u8;32]` | `[seller (signer, writable), seller_credit, listing, escrow, system]` |
| 8 | `Buy` | — | `[buyer (signer, writable), buyer_credit, seller_credit, listing, escrow, buyer_token, seller_token, mint, token_program]` |
| 9 | `CancelList` | — | `[seller (signer), listing, escrow, seller_credit]` |

Tags never get renumbered — they are a deployed interface. `InitConfig` field
offsets (1, 33, 65) are pinned by a test on each side, because the three
pubkeys are positionally identical in shape and a swapped offset would
round-trip happily while binding the wrong mint.

### Where the authority checks live

The split between `state.rs` and `processor.rs` is deliberate: `state.rs` owns
every rule that decides whether a transition is legal and is tested with no
runtime; `processor.rs` owns the rules that depend on *who is calling*.

- **`InitCredit`** needs no authority beyond rent — creating an account is
  harmless.
- **`Credit`** and **`CommitAndSettle`** require the config authority's
  signature. The chain cannot see the deposit, so it trusts the rail's key
  exactly as far as the original trusted its deposit contract's role.
- **`Lock`**, **`Unlock`** require the owner to sign; **`Transfer`** requires
  the sender to sign, with both sides verified as the named owners' PDAs, so
  credits cannot be pushed into or pulled out of an account the signer does not
  control.
- Every credit account is verified against `[b"credit", owner]`, and every
  account about to be written is checked to be owned by this program. Without
  the ownership check an attacker could pass a look-alike account they control
  and the program would write balances into it.

### Error codes

`state.rs` rejections map to `ProgramError::Custom` 1–7 in the enum's
declaration order:

| Code | Meaning |
|---|---|
| 1 | insufficient spendable credits |
| 2 | insufficient locked credits |
| 3 | usage root already committed |
| 4 | usage ids must advance |
| 5 | a settlement of zero credits has no root to commit |
| 6 | credit arithmetic overflowed |
| 7 | account data is not a Slotfare credit account |
| 8 | the listing is not open |

`processor.rs` adds its own, 100+:

| Code | Meaning |
|---|---|
| 100 | signer is not the configured authority |
| 101 | account is not the expected PDA (or is the system program when it should not be) |
| 102 | config account is missing or not initialised |
| 103 | account is not owned by this program (including "this account does not exist yet") |
| 104 | account is shorter than its declared layout |
| 105 | config is already initialised — `InitConfig` is first-caller-wins |
| 106 | the listing is not open (bought or cancelled already) |
| 107 | signer is not the listing's seller |
| 108 | the seller cannot buy its own listing |
| 109 | listing terms are empty (zero credits, zero price or zero mint) |
| 110 | a listing already exists at that address |
| 111 | the token program is not SPL Token |
| 112 | a token account or mint is malformed |
| 113 | the payment mint is not the listing's mint |

Changing a code changes what an existing client reports, so both tables are
appended to, never reordered.

## The market: escrow listings

The original's `CreditMarket` is why the market could settle without the seller
online: `list` moved a seller's credits into the contract, so `buy` released
credits the seller had already given up. This program is that market on
Solana, and it is the reason the whole `List` / `Buy` / `CancelList` trio
exists.

### Listing account — `[b"listing", seller, nonce]`, 125 bytes

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `SFLS` |
| 4 | 32 | `seller` — the only key allowed to cancel, and the sale's payee |
| 36 | 32 | `buyer` — set on a sale, zero while open |
| 68 | 32 | `pay_mint` — the mint the price is denominated in |
| 100 | 8 | `credits` — the escrowed amount |
| 108 | 8 | `price` — pay-token base units the buyer must part with |
| 116 | 8 | `nonce` — with the seller, derives this listing's address |
| 124 | 1 | `state` — 0 open, 1 sold, 2 cancelled |

The `nonce` is the caller's, so one seller may hold many listings. The escrow
the terms point at is a separate credit account at `[b"escrow", listing]`; it
is a plain [`CreditAccount`](#credit-account--bcredit-owner-76-bytes) the
program owns, so the same tested transitions move its balance.

### What the three instructions enforce

- **`List`** moves the credits out of the seller's spendable balance into the
escrow (`transfer_out` then `receive`), so a listed credit is unspendable for
as long as the listing is open — the discipline the Memo port had no way to
enforce. The listing and escrow accounts must both be fresh: a re-run cannot
rewrite terms under an escrow that already holds credits.
- **`Buy`** is the atomic trade, and the one instruction that makes an SPL
CPI. In one transition it calls `TransferChecked` to move the buyer's
`price` of `pay_mint` to the seller's token account, then moves the escrowed
credits to the buyer's credit account, then marks the listing sold. Either
half failing reverts the whole transaction, so there is no state in which a
buyer paid and did not get credits, or got credits without paying. The buyer
signs; the seller need not be online.
- **`CancelList`** returns the escrow to the seller's spendable balance, and
only the listing's own seller may call it.

The buy's mint checks are not decoration. The mint named must equal the
listing's `pay_mint`, the mint account must be SPL Token's, and both token
accounts must belong to the token program and hold that same mint — without
which a buyer could pay in a worthless token that happens to share the account
list shape. Decimals are read from the mint, so `TransferChecked` is exact
rather than guessed.

The program encodes `TransferChecked` by hand (tag 12, amount, decimals) and
checks the token program against its literal bytes, so the crate keeps its
"no dependency beyond `solana_program`" property. `processor.rs`'s
`the_token_program_bytes_are_the_canonical_spid` test pins those bytes to
`TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA`.

## Verified against a live cluster

`TestLiveLedgerProgram` in
[`internal/solana/ledger_live_test.go`](../internal/solana/ledger_live_test.go)
deploys against a real cluster and asserts the rules through the interface the
daemon uses, because a rule the chain enforces is only known to be enforced by
asking the chain. It is opt-in (`SLOTFARE_LIVE=1`) and rerunnable.

    SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
        SLOTFARE_SMOKE_KEY=<funded base58 key> \
        SLOTFARE_LEDGER_PROGRAM=<program id> \
        go test ./internal/solana -run LiveLedger -v

What it confirms on chain, with the program error code each rejection returns:

- an authority-signed mint reaches an exact absolute total, and reconciling to
  the same total submits nothing;
- a lock removes exactly its own amount from spendable, and cancelling returns
  it intact;
- `CommitAndSettle` burns the credits and stores the root, usage range and
  batch count in one transition;
- **replaying the committed root is rejected** (custom 3);
- **settling more than the spendable balance is rejected** (custom 1);
- **a batch that does not advance the usage frontier is rejected** (custom 4);
- the rejected batches leave the account's `spent` and `batch_count` untouched;
- a transfer credits the recipient exactly, and a transfer beyond the sender's
  spendable balance is rejected (custom 1).

The test needs the key to be the program's config authority, since minting and
settling are restricted to it; it skips with a clear message when it is not.

### The market, too

`TestLiveLedgerMarket` in
[`internal/solana/ledger_market_live_test.go`](../internal/solana/ledger_market_live_test.go)
drives a whole trade on the same cluster. It stands up its own SPL mint and
token accounts in Go, so it is self-contained and rerunnable, and it asserts
what a market must guarantee:

- a seller lists credits and the escrow removes **exactly** that amount from
the seller's on-chain spendable balance (`spent` advances by the listed
credits);
- one buyer transaction both pays the seller and takes the credits: the
buyer's `pay_mint` balance falls by the price and their credit balance rises
by the listed amount, while the seller's spendable does not move again;
- **buying a sold listing is rejected** (custom 106), and so is **cancelling
  one** (custom 106), so the escrow moves once;
- cancelling a fresh listing returns its credits to the seller's spendable
  balance exactly;
- **a seller buying its own listing is rejected** (custom 108).

The escrow is why this works without the seller being online, and the test is
the evidence that the chain, not the daemon, holds the credits while a listing
is open.

### Through the console's own routes

`TestLiveOnchainMarketRoutes` in
[`internal/console/market_onchain_live_test.go`](../internal/console/market_onchain_live_test.go)
proves the half the browser actually touches. It stands the console up with
the real settlement client and drives the two routes over HTTP, with a local
key standing in for Phantom:

1. `prepare` for a list, decode the base64 transaction, assert the console
   named the caller as fee payer, sign it with the seller key and send it;
2. `confirm` the list and check the row's credits, price and seller came from
   the **listing account** rather than from the request;
3. `confirm` again for a nonce the program never opened, and check it does
   **not** create a row;
4. `prepare` a buy as a different wallet, sign, send, confirm — then check the
   row is sold to that wallet, that the row carries the slot the sale landed
   in, and that the buyer's USDC actually left their token account;
5. `prepare` a second buy of the sold listing and check it is **409**, so the
   UI cannot walk a buyer into a signature the program would reject.

Everything else in the path is the real one — the wire format, the signature,
the broadcast, the read-back. Only the wallet popup is local.

## What this closes, and what it does not

`docs/trust-model.md` said the port's largest regression was that the balance
authority was a database we operate. With the program deployed and driving
settlement, that regression narrows:

**Now enforced by the chain, not by our process:**

- a burn cannot happen without a root committed in the same transition;
- one root cannot be committed twice — a replayed settle pass is rejected by
  state, not merely avoided by our bookkeeping;
- a batch cannot rewrite the usage frontier backwards;
- locked credits are not settleable, so a listing cannot be spent underneath a
  buyer;
- a settle cannot touch credits the owner does not have minted and unlocked,
  with all arithmetic checked.

Those were *our* rules in the Memo port. They are the program's rules now, and
the settler cannot override them.

**Still true, and still worth saying:**

- **The mint is still trusted.** `Credit` is gated on the authority key, but
  the chain cannot verify that a deposit happened. The deposit side stays
  chain-*derivable* (public transfer history at a fixed rate), not
  chain-*proven* - there is no attestation equivalent on Solana. The program
  narrows what a compromised operator can do, not what an operator can mint.
- **The local ledger is still the reservation and display authority.** Pending
  usage lives off chain, `/rpc` reads balances locally for the hot path, and the
  on-chain figure converges from the deposit rail rather than gating every
  request. If the mirror lags, the settler finds less on-chain spendable than
  local pending covers and retries next tick; that loses no usage, and it is the
  behaviour to expect after a restart, not a fault.
- **The program is upgradeable by whoever holds its upgrade authority** unless
  that authority is burned. Burning it after deployment is the honest end
  state.
- **Nothing here is deployed.** No program id, no authority, no config account.
  See `deployments/README.md`.

## Deploy and initialise

Not yet performed against devnet — this is the procedure, verified end to end
against `solana-test-validator`. Requires the Agave toolchain (`cargo-build-sbf`,
`solana`, `solana-keygen`).

    cd programs/slotfare-ledger
    ./build.sh                                # tests, then the v3 artifact

The keypair at `target/deploy/slotfare_ledger-keypair.json` is the program's
identity; keep it, and give the deploying key SOL for the deployment.

    solana program deploy \
        --program-id target/deploy/slotfare_ledger-keypair.json \
        --keypair <funded key> \
        --url "$SLOTFARE_SOLANA_RPC" \
        target/deploy/slotfare_ledger.so

Then point the environment at the program and initialise it **in the same
sitting**, because `InitConfig` is first-caller-wins:

    export SLOTFARE_SOLANA_RPC=<cluster>
    export SLOTFARE_SOLANA_FEE_PAYER=<base58 key with SOL>
    export SLOTFARE_SOLANA_TREASURY=<the USDC token account deposits land in>
    export SLOTFARE_LEDGER_PROGRAM=<program id>

    go run ./cmd/slotfared ledger-init

`ledger-init` installs the fee payer as the authority and binds the configured
mint and treasury, so the config can only be as wrong as the environment is. It
is idempotent: it reads the config first, and a re-run prints what is bound and
refuses to continue if that disagrees with the environment. Because the
submission is only preflighted and not awaited, it polls briefly for the account
to appear and says so plainly rather than asserting a binding it did not read.

That polling is not incidental. solana-Go's plain `GetAccountInfo` leaves the
commitment unset, so the node answers at its own default of `finalized`, which
trails `confirmed` by 31 slots — long enough that a config the program has just
created reads as absent, turning a re-run into a second `InitConfig`. Every
ledger read therefore asks at `confirmed`, the same commitment the writer
preflights at. The program also answers a repeat `InitConfig` with its own
error (custom 105) instead of a bare system error, so the race surfaces as a
sentence rather than a mystery.

Pass `--account 0x…` to open a credit account as part of the same command,
repeatable:

    go run ./cmd/slotfared ledger-init --account 0x<32-byte hex pubkey>

Program ids, the authority, the treasury and the cluster belong in
`deployments/` once they exist, because that directory is the single source of
truth for addresses.

### Order of operations, and why

1. **Fund the fee payer.** It pays for every credit account and every settle.
2. **`ledger-init`** — before any deposit is mirrored, or the mint has no
   authority to check against.
3. **The deposit rail mirrors.** As deposits are seen, the daemon opens each
   owner's credit account and reconciles it to the absolute minted total.
4. **Settlement commits through the program** instead of the Memo.

## Wiring status

Honest accounting of what exists versus what is connected, because the
difference matters to anyone reading the code:

| Piece | State |
|---|---|
| Program (credit + listing state, 10 instructions, processor, PDAs, authority, SPL CPI) | built, tested both ways, deployed to a local validator |
| Go client (encode, PDAs, config read, ensure, reconcile, lock, unlock, commit, transfer, list, buy, cancel, listing read, prepare) | built, golden-byte tested, exercised on a live cluster |
| Deploy-side init (`ledger-init`: config install, `--account` open) | wired and verified end to end |
| Deposit mirror (`LedgerMirror.Sync` on every minted total) | wired into the deposit callback in `cmd/slotfared/main.go` |
| Settlement (`CommitAndSettle` routes to the program when configured) | wired; falls back to the Memo when `SLOTFARE_LEDGER_PROGRAM` is unset |
| Escrow market `List` / `Buy` / `CancelList` | built, golden-byte tested, and verified end to end on a live cluster by `TestLiveLedgerMarket` |
| Ledger-only mode (`SLOTFARE_LEDGER_REQUIRED`, startup config check) | wired: a required-but-absent program refuses to start, and a configured program with no config account fails startup with the fix named |
| `listing_nonce` on the `listings` row (both backends) | wired: the listing PDA is `[b"listing", seller, nonce]`, so buy and cancel recover it from the row. SQLite adds it with `ensureColumn`, Postgres with a tolerated-duplicate `ALTER` |
| The console's market routes driving the escrow program | **wired** — `POST /listings/onchain/prepare` returns the wallet-signed transaction and `POST /listings/onchain/confirm` records the row **only after re-reading the listing account**, so a confirm cannot invent a listing. Verified through the HTTP interface on a live cluster by `TestLiveOnchainMarketRoutes` |
| The browser's signing step | **wired** — `sendPreparedTransaction` (`web/src/app/core/solana.ts`) deserializes the prepared transaction, hands it to the wallet, submits it and waits for confirmation; the sell and buy flows call it |
| `LedgerLock` / `LedgerUnlock` on the console listing route | superseded: the escrow is the lock. Still present as the no-program rail |
| `LedgerTransfer` on the market pay route | superseded by `Buy`, which pays and moves credits atomically. Still present as the no-program rail |

The two rails coexist by design during the port: `SLOTFARE_LEDGER_PROGRAM`
unset gives the Memo path exactly as it shipped, and setting it upgrades
settlement and deposits to the program. Set `SLOTFARE_LEDGER_REQUIRED=1` to
refuse the Memo rail outright, so a deployment that means to have on-chain
enforcement cannot silently run without it.

### The market round trip

The daemon never holds a user's key, so every market instruction is two round
trips: the console builds it, the wallet signs it, the console records what
the program then holds.

| Step | What happens |
|---|---|
| `POST /console/listings/onchain/prepare` | Body `{action, listingId?, credits?, price?}`. The console resolves the caller's wallet from the session, builds the instruction against the escrow PDAs and returns `{action, nonce, transaction}` — the transaction is the **base64** of the standard Solana wire form, with the caller as fee payer. Nothing is written: a prepare that is never signed leaves no trace |
| the wallet | `sendPreparedTransaction` decodes the base64, hands the transaction to the wallet, submits it and waits for `confirmed`. The daemon never sees the key |
| `POST /console/listings/onchain/confirm` | Body `{action, listingId?, nonce, signature}`. The console re-reads the listing account and only records the row if the program agrees: a list must be open, a buy must be `Sold` **to this caller**, a cancel must be `Cancelled`. The row's credits, price and mint come from the listing's own bytes, never from the request, and `LedgerTxSlot` dates the row with the slot the transaction landed in |

`confirm` on a list is idempotent: a wallet that retries after a dropped
response gets the same row back rather than a second escrow. On an unset
`SLOTFARE_LEDGER_PROGRAM` both routes answer **501** and the console serves
the local-mirror market exactly as before, so a deployment without an escrow
keeps working.

One caveat worth naming while both rails run. `LedgerReconcileMint` derives its
mint from the account's current `minted`, so it is absolute only as far as the
read is current: a read that misses a credit that has landed but is not yet
confirmed would submit the delta again and over-mint. Two things keep that
narrow — reads are at `confirmed`, and `LedgerMirror` records only the highest
total it has confirmed and never resubmits it — but the honest fix, if the
mirror is ever driven by anything faster than the deposit watcher's poll, is a
per-account mint lock or a `minted` check inside the program.
