# Slotfare on Solana

Metered access to inference and RPC endpoints, paid in prepaid credits. You
deposit USDC on Solana, the local ledger mints you credits, the metering
proxy spends them per request, and each settlement batch commits its usage
Merkle root on Solana as a Memo transaction: a public, timestamped audit
trail anyone can verify without trusting us.

**Colosseum World's Fair.** Autonomous metered compute gateway and on-chain credit settlement on Solana. Maintainer: vishnuraj.

## How it works

- **Deposits mint.** A USDC transfer to the configured treasury token
  account credits the sender's public key (encoded as `0x` + raw hex, no
  hashing) with `10` micro-credits per token base unit: `1 USDC = 1,000`
  credits. The deposit watcher rescans the treasury's transfer history and
  writes *absolute* minted totals to the balance authority, so a restart
  converges instead of double-minting.
- **The local ledger is the balance authority.** The chain holds no credit
  balances. `BalanceOf` re-reads the local authority each pass, in SQLite
  (in-memory ledger, checkpointed) or Postgres (shared accounts table).
- **The metering proxy spends.** API-key auth, per-request pricing, credit
  reservation before forwarding, receipts signed with a dedicated key.
- **Settlement commits.** Every settle batch folds pending usage into a
  Merkle root and lands it as a Memo program transaction
  (`slotfare-usage-root/v1` JSON: root, account, usage id range, credits).
  The same transaction burns the covered spend inside the store
  transaction, so chain evidence and local state stay paired.

## Architecture

| Path | Role |
|---|---|
| `cmd/slotfared` | the daemon: metering proxy, ledger, settler, deposit watcher, console API |
| `programs/slotfare-ledger` | on-chain ledger program: balances, locks and settlement in program-owned PDAs |
| `internal/solana` | Solana chain surface: RPC client, ledger-program client, Memo settlement commit, deposit watcher |
| `internal/meter` | ledger, pricing, reservations, flusher, settler |
| `internal/proxy` | HTTP metering proxy (auth, weights, forwarding, receipts) |
| `internal/console` | console HTTP layer: routing, handlers, sign-in flow, funding/market views |
| `internal/funding`, `internal/market`, `internal/session` | store contracts and SQLite drivers: purchases, listings, sessions and nonces |
| `internal/schema` | the SQLite DDL for those stores - tables and column migrations in one place |
| `internal/pgstore`, `internal/keys`, `internal/receipt`, `internal/cors` | Postgres implementations and support packages |
| `web/` | the Angular console: Phantom sign-in, keys, usage, proofs |
| `docs/` | metering spec, trust model, decision log, runbook |
| `deployments/` | the single source of truth for deployed addresses |

## Documentation

| Page | What it settles |
|---|---|
| [docs/metering.md](docs/metering.md) | the RPC price: classes, the built-in Solana table, the two parameter-sensitive methods, overrides |
| [docs/trust-model.md](docs/trust-model.md) | what this proves and what it asks you to believe, including what the port made weaker |
| [docs/onchain-ledger.md](docs/onchain-ledger.md) | the on-chain ledger program: account layouts, instruction set, PDA seeds, error codes, the deploy/init runbook, and what it does and does not close |
| [docs/decisions.md](docs/decisions.md) | why each port decision was taken, and what it costs |
| [docs/architecture.md](docs/architecture.md) | the layout after the refactor, the old-to-new path map, and the dependency rule |
| [LICENSE](LICENSE) | MIT License |
| [docs/testnet-runbook.md](docs/testnet-runbook.md) | local bring-up, devnet configuration, probes, and the deployment record |
| [deployments/README.md](deployments/README.md) | the shape of a deployment record |

## Build and test

Prerequisites: Go 1.25 or newer, Node 20 or newer (for the console). For
local chain work, the Agave toolchain (`solana`, `solana-test-validator`,
`spl-token`).

Quality gates, from the repository root:

    make check

which is the same as, one by one:

    go build ./...

    go vet ./...

    go test ./... -count=1

    cd web && npm ci && npm run build && npx ng test --watch=false

`make check` adds `gofmt -l`, `cargo fmt --check`, `cargo test`,
`cargo clippy` and the console's own gates; CI (`.gitea/workflows/ci.yml`)
runs exactly those targets as three independent jobs. The offline suite
never touches the network and is what CI runs.

Live smoke test (opt-in), verifies the full commit path — build, sign,
send, land, fetch, decode — against any cluster:

    solana-test-validator --ledger /tmp/slotfare-ledger &   # local, or use devnet
    SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
        go test ./internal/solana -run Live -count=1 -v

Console sign-in smoke (opt-in), drives nonce → ed25519 signature → verify
over HTTP, including nonce replay and wrong-signer rejection, against a
running `slotfared`:

    SLOTFARE_CONSOLE_SMOKE=http://127.0.0.1:8443 \
        go test ./internal/console -run TestLiveConsoleSignIn -count=1 -v

On public devnet the commit test needs a funded key (`SLOTFARE_SMOKE_KEY`,
base58); the shared faucet is often rate-limited, so the test skips rather
than fails without one.

## Running

One-command local walkthrough — starts a validator (if needed), creates a
test USDC-like mint, runs `slotfared`, deposits 2.5 tokens, and asserts the
watcher minted 2,500 credits to the sender's account:

    scripts/demo-local.sh

Expected final line: `E2E PASS: 2.5 USDC deposit minted 2,500 credits to 0x…`.

Offline demo (no chain): `go run ./cmd/slotfared` — metering, receipts and
console API come up with SQLite. Add `SLOTFARE_SOLANA_*` (see
`.env.example`) to enable settlement and the deposit watcher.

Console (web UI): run `slotfared` on `127.0.0.1:8443`, then in `web/` run
`npx ng serve`. The dev server proxies `/console` and `/rpc` to the daemon
(`web/proxy.conf.json`), so open `http://localhost:4200/`, connect Phantom,
and sign the nonce to open the console. Sign-in is ed25519 over a
server-built message; the session API also works from curl or scripts
without a wallet (see the console smoke test above).

## Configuration

`.env.example` documents every variable: the Solana surface
(`SLOTFARE_SOLANA_RPC`, `SLOTFARE_SOLANA_CLUSTER`, `SLOTFARE_SOLANA_FEE_PAYER`,
`SLOTFARE_SOLANA_TREASURY`, `SLOTFARE_SOLANA_MINT`), the metering proxy
(`SLOTFARE_*`), and the opt-in live test (`SLOTFARE_LIVE`, `SLOTFARE_SMOKE_RPC`,
`SLOTFARE_SMOKE_KEY`). Unset chain variables always yield a working offline
deployment.

## Status: what is built, what is pending

Built and verified:

- Metering proxy, ledger, flusher, receipts, console API: the full
  accounting path, chain layer targeting Solana only; offline suite green.
- Solana settlement commit (Memo program): unit-tested and verified
  end-to-end against a local `solana-test-validator` (tx lands, payload
  round-trips).
- Deposit watcher: unit-tested, plus a full local walkthrough
  (`scripts/demo-local.sh`) proving an SPL token transfer mints the exact
  credit amount to the sender's `0x`-hex account.
- Angular console port: Phantom wallet sign-in (ed25519, server-built
  message), commit transactions linked in the Solana explorer, dev-server
  proxy wired; 592 frontend tests green. The console boots in a browser too:
  a `Buffer` polyfill runs ahead of the app bundle, because the Solana
  packages reach for the Node global at module scope and the dev server
  externalizes Node built-ins ([docs/decisions.md](docs/decisions.md), D18).
- Console sign-in live smoke: HTTP nonce → sign → verify with replay and
  wrong-signer rejection (`SLOTFARE_CONSOLE_SMOKE`).
- Solana RPC pricing: the weight table now prices Solana JSON-RPC methods in
  their own classes (trivial / standard / heavy), including the
  `getBlock` full-transaction rule, so `getBalance`, `getTransaction` and
  `getProgramAccounts` no longer fall through to one generic weight.
  Specified in [docs/metering.md](docs/metering.md).
- Console truthfulness pass: a Solana deposit used to render as a three-step
  journey through chains it never crossed. Purchases are now
  classified by chain (chain 0 is Solana, one deposit step) and the leftover
  non-Solana copy is gone from the live screens.
- On-chain ledger program (`programs/slotfare-ledger`): balances, locks and
  settlement roots in program-owned PDAs, with the burn gated on a committed
  root inside one instruction, plus a Go client that drives it and a
  `slotfared ledger-init` deploy-side command. Deployed against
  `solana-test-validator` and its rules asserted on chain by
  `TestLiveLedgerProgram` (a replayed root, an overspend and a rewound usage
  frontier are each rejected with the program's own error code; the rejected
  batches leave state untouched). It closes most of the trust regression the
  Memo-only rail carried ([docs/onchain-ledger.md](docs/onchain-ledger.md),
  [docs/decisions.md](docs/decisions.md) D13-D14).
- On-chain escrow market, in the same program: `List` moves a seller's credits
  into a program-owned escrow, `Buy` pays the seller and releases the escrow in
  one atomic transaction (an SPL `TransferChecked` CPI), and `CancelList`
  returns the escrow. It is what makes a listed credit unspendable on chain
  and a sale atomic without the seller being online. Asserted on a live
  cluster by
  `TestLiveLedgerMarket` (a sold listing cannot be bought or cancelled twice; a
  seller cannot buy its own listing). Ledger-only mode
  (`SLOTFARE_LEDGER_REQUIRED`) refuses to start on the Memo rail when a program
  is required.
- The console sells and buys through that escrow, not through its own
  bookkeeping: `POST /listings/onchain/prepare` returns the wallet-signed
  transaction and `POST /listings/onchain/confirm` records a row **only after
  re-reading the listing account**, so the row is a consequence of chain state
  rather than a claim. The browser deserializes and signs it
  (`sendPreparedTransaction`). The daemon never holds a user's key, which is
  why the round trip is two calls. Verified over HTTP on a live cluster by
  `TestLiveOnchainMarketRoutes`.
- The market event indexer closes what D17 recorded open: a listing or sale
  made by a wallet talking to the program directly now reaches the console.
  `internal/solana/indexer.go` replays the ledger program's transaction
  history, decodes the List/Buy/CancelList instructions, and writes the same
  listings rows the prepare/confirm route writes - idempotent on the listing
  PDA's derived id, so replays and restarts converge instead of duplicating.
  Enabled whenever `SLOTFARE_LEDGER_PROGRAM` is set; the pass ticker is
  `SLOTFARE_INDEXER_INTERVAL_SECONDS` (default 30s).
- The deposit rail's minted write is monotonic (`RaiseMinted`): an incomplete
  RPC rescan can never lower a recorded balance, and a scan behind the
  recorded total is surfaced as a retryable gap instead of silently overwriting.
  Marketplace purchases ride the signed `Adjust` axis, so a buyer's received
  credits never look like deposits.

Pending (this window):

- No public deployment. `deployments/` is the single source of truth and is
  empty; there are no live testnet URLs yet. See
  [docs/testnet-runbook.md](docs/testnet-runbook.md).
- Live deposit walkthrough on devnet: Circle's faucet USDC into the
  treasury, same mint assertion.
- Treasury deposit address surfaced in the console UI (today it is read
  from `.env` / `deployments/`).
- No devnet deployment of the escrow program: it is verified on
  `solana-test-validator` only, so `TestLiveOnchainMarketRoutes` and
  `TestLiveLedgerMarket` need a local validator to run.

## Build log

- **Day 0 — 6 Oct 2026:** repository scaffolded with its layout
  (`cmd/slotfared`, `internal/solana`); Solana client, Memo commit and
  deposit watcher implemented; live commit verified on a local validator;
  deposit→mint walkthrough passes via `scripts/demo-local.sh` (fixing a
  treasury-matching bug found by the walkthrough: the treasury is identified
  by its token account, not by owner); offline gates green. Console:
  Phantom ed25519 sign-in server-side, Solana explorer links, dev-server
  proxy wired, 592 frontend tests and the Go gates green.
- **Day 0, later — 6 Oct 2026:** closed the two gaps that made the first
  cut look like a rename rather than a build. (1) The metered gateway priced
  Solana calls as generic standard because the weight table predated the
  Solana table; it now carries the Solana table plus a documented legacy
  passthrough block, with the `getBlock` `transactionDetails: "full"` rule
  mirroring `eth_getBlockByNumber`'s boolean. (2) The console still told a
  foreign-chain story: a Solana deposit carried a real slot, so the old
  `sourceBlock == 0` test read it as a proven purchase and drew an
  attestation step this deployment does not have. Classification is now by
  chain, the deposit step renders live until the credits land, and the
  stale chain copy is out of the live screens. `docs/` and
  `deployments/` got their first tracked files, including the honest
  statement of the weaker trust model. Gates: `go build`, `go vet`,
  `go test ./...` and 592 console tests green; `ng build` clean.
- **Day 1 — 7 Oct 2026:** the balance authority came back to the chain.
  `programs/slotfare-ledger` is a Solana program holding `minted`, `spent`
  and `locked` per owner in PDAs, with `CommitAndSettle` applying a usage
  root and the burn it authorises in one atomic transition — the fix for
  the one regression
  the Memo rail could not address. The rules are in `state.rs` and tested
  without a runtime; the authority, PDA and ownership checks are in
  `processor.rs`. Three findings came out of actually deploying it rather
  than building it: Agave's SIMD-0500 disables SBPF v0-v2 deployment while
  `cargo-build-sbf` still defaults to v0, so the artifact needs `--arch v3`
  (`programs/slotfare-ledger/build.sh` now pins it); solana-Go's
  `GetAccountInfo` answers at `finalized`, 31 slots behind, which made a
  config the program had just created read as absent and turned a re-run of
  the deploy flow into a second `InitConfig` — ledger reads now ask at
  `confirmed`; and a repeat `InitConfig` was surfacing as a bare system
  error, so the program now answers it with its own code (105).
  `slotfared ledger-init` makes the deploy side one idempotent command that
  reads the binding back and refuses when it disagrees with the
  environment. Verified end to end on `solana-test-validator`: deploy,
  init, credit-account creation, and `TestLiveLedgerProgram` asserting on
  chain that a replayed root (code 3), an overspend (code 1) and a rewound
  usage frontier (code 4) are refused and change nothing. Rust: 20 tests
  default / 23 with `onchain`. Go: `gofmt`, `go build`, `go vet`,
  `go test ./...` green.
- **Day 2 - 8 Oct 2026:** brought the whole stack up from a clean machine
  state (validator, program deploy, `ledger-init`, daemon, console) and
  loaded the console in a real browser, which is the check none of the gates
  perform. It rendered a blank page: `@solana/spl-token` reads the Node
  `Buffer` global at module scope and the dev server externalizes Node
  built-ins, so the dependency graph threw before bootstrap. `ng test` was
  green because vitest runs under Node, and `ng build` was clean because a
  missing runtime global is not a bundling error. Fixed with a `src/
  polyfills.ts` entry ahead of the app bundle and `buffer` as a direct
  dependency (D18). Re-verified: console renders the connect screen, the
  dev-server proxy reaches the daemon, `go test`/`ng test` (592) and `ng
  build` green, and a live 2.5 USDC deposit minted exactly 2,500 credits into
  the program's credit PDA (`SFCA`, 76 bytes) on chain. Loading that console
  with a real wallet found the second thing no gate could: sign-in failed
  before any prompt appeared, because the message's domain and uri were the
  placeholders `slotfare` / `https://slotfare`, and Phantom validates a
  Sign-In with Solana message against the requesting page's own host. Both are
  now derived from the request (D19) and verified over the dev-server proxy:
  the nonce names `127.0.0.1:4200` and `http://127.0.0.1:4200`.
- **Day 3 — 12 Oct 2026: architecture refactor.** `internal/console` was a
  10.6k-line god package that owned routing, domain models, three SQLite
  schemas and the Postgres store's SQL (pgstore imported the HTTP package
  for both). Split out `internal/session`, `internal/funding`,
  `internal/market` and a shared `internal/schema` (one DDL home instead of
  five), dropped the never-imported `go-ethereum` dependency and its ~30
  indirect modules, moved CI from `web/.gitea/` (where Gitea never loaded
  it) to the repository root behind `make check`, evicted rate-limiter
  buckets, tightened `.env` to 0600, and documented the layout and the
  old-to-new path map in `docs/architecture.md`. Moves only: routes,
  statements, program IDs and on-chain behaviour are unchanged, and
  [LICENSE](LICENSE) holds the MIT license. **Test
  report** (12 Oct 2026, `make check` exit 0): *passed* — `gofmt -l`,
  `go build ./...`, `go vet ./...`, `go test ./...` (433 tests, 13
  packages), `cargo fmt --check`, `cargo test` (27), `cargo clippy`,
  `ng build`, `ng test` (40 files / 593 tests), and the full
  `scripts/demo-local.sh` deposit→mint walkthrough (`E2E PASS`);
  *failed* — none; *skipped* — the five opt-in `TestLive*` suites, which
  skip by design without `SLOTFARE_LIVE` / a local validator;
  *unavailable* — a real Gitea runner (both workflow files were
  parse-checked locally, not executed) and a live Postgres (`pgstore`
  tests run against SQLite, a pre-existing gap).
