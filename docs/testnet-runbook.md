# Testnet runbook

How to bring up `slotfared` locally, and the intended path to a public
testnet deployment. Every command here is run from the repository root
unless it says otherwise.

## Status: nothing is deployed

There is **no public URL yet**. `deployments/` is the single source of truth
and it is empty, because no deployment has been performed. Do not read a
command below as "this is already running" - it is the procedure, not a
record.

Verified locally as of 6 Oct 2026:

- `scripts/demo-local.sh` passes end to end against `solana-test-validator`
  (deposit 2.5 tokens, watcher mints 2,500 credits).
- The live commit path is verified against a local validator
  (`SLOTFARE_LIVE=1`, see Step 6).
- The on-chain ledger program is deployed against `solana-test-validator`,
  initialised with `slotfared ledger-init`, and its rejection rules are
  asserted on chain (`TestLiveLedgerProgram`, see Step 6).
- The offline suite is green: `go build ./...`, `go vet ./...`,
  `go test ./... -count=1`.

Not yet verified: a devnet deposit with real faucet USDC, and any public
deployment.

## Step 1 - Prerequisites

Go 1.25 or newer; Node 20 or newer for the console. For chain work, the
Agave toolchain: `solana`, `solana-test-validator`, `spl-token`.

## Step 2 - Offline bring-up (no chain at all)

    go run ./cmd/slotfared

Metering, receipts and the console API come up with SQLite. Unset chain
variables always yield a working offline deployment; there is no minimum
configuration.

## Step 3 - Local chain walkthrough

    scripts/demo-local.sh

It starts a validator if one is not already listening, creates a test mint,
runs `slotfared`, deposits 2.5 tokens, and asserts the watcher minted exactly
2,500 credits to the sender's account. Expected final line:
`E2E PASS: 2.5 USDC deposit minted 2,500 credits to 0x…`.

## Step 4 - Metered inference (optional)

`SLOTFARE_INFERENCE_UPSTREAM` points at a Fireworks-compatible endpoint; the
default is NVIDIA's free catalog (`https://integrate.api.nvidia.com`) with a
key from `SLOTFARE_INFERENCE_UPSTREAM_KEY` (`nvapi-…`). Rate limits are
`SLOTFARE_KEY_RATE_LIMIT` (per key) and `SLOTFARE_MODEL_RATE_LIMIT` (per model),
both per minute. `SLOTFARE_MODELS` is a JSON registry of
`{id, upstreamId, name, capabilities, pricing}`.

If the catalog is unreachable or rate-limited, drop the whole inference block
from `.env` and the daemon starts without it - `/rpc` and the console do not
depend on inference. Discovery paths (`/v1/models`, `/health` on the
inference service) are free.

## Step 5 - Devnet configuration

Fill `.env` (gitignored; copy from `.env.example`):

    SLOTFARE_SOLANA_RPC=https://api.devnet.solana.com
    SLOTFARE_SOLANA_CLUSTER=devnet
    SLOTFARE_SOLANA_FEE_PAYER=<base58 throwaway key with SOL>
    SLOTFARE_SOLANA_TREASURY=<the USDC token account that receives deposits>
    # SLOTFARE_SOLANA_MINT=4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU

Two things to get right:

- `SLOTFARE_SOLANA_TREASURY` is a **token account**, not a wallet. The
  watcher identifies the treasury by its token account, so pointing this at
  an owner address silently mints from the wrong transfers.
- `SLOTFARE_SOLANA_FEE_PAYER` is a throwaway key that funds memo transactions.
  Never a key that has touched mainnet; it needs SOL for fees.

If the mint is unset it defaults to Circle's faucet USDC on devnet and native
USDC on mainnet-beta. An explicit value always wins.

## Step 5b - The on-chain ledger program (optional, recommended)

Setting `SLOTFARE_LEDGER_PROGRAM` upgrades settlement from a Memo to a program
instruction that holds balances, gates the burn on a committed root, and
returns a refusal instead of a convention. It is optional: without it,
everything above still works exactly as described.

Build and deploy it (see [onchain-ledger.md](./onchain-ledger.md) for what it
does and does not close):

    cd programs/slotfare-ledger && ./build.sh
    solana program deploy \
        --program-id target/deploy/slotfare_ledger-keypair.json \
        --keypair <funded key> --url "$SLOTFARE_SOLANA_RPC" \
        target/deploy/slotfare_ledger.so

Use `./build.sh` rather than a bare `cargo build-sbf`: it pins `--arch v3`,
which current clusters require (SIMD-0500 disables SBPF v0-v2 deployment while
the compiler still defaults to v0).

Then initialise it **in the same sitting**, because `InitConfig` is
first-caller-wins:

    SLOTFARE_LEDGER_PROGRAM=<program id> go run ./cmd/slotfared ledger-init

`ledger-init` installs `SLOTFARE_SOLANA_FEE_PAYER` as the program's authority
and binds the configured mint and treasury. It is idempotent, prints what is
bound on a re-run, and refuses to continue if that disagrees with the
environment. Add `--account 0x…` (repeatable) to open credit accounts.

## Step 6 - Verify the commit path before trusting a deployment

    SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
        go test ./internal/solana -run Live -count=1 -v

Build, sign, send, land, fetch and decode a memo transaction. On public
devnet, set `SLOTFARE_SMOKE_KEY` to a funded base58 key; the shared faucet is
often rate limited, and the test skips rather than fails without one.

With the ledger program deployed and initialised, verify its rules the same
way - this one asserts the chain's refusals, not just the happy path, so it is
the check that a deployment's trust story is real rather than aspirational:

    SLOTFARE_LIVE=1 SLOTFARE_SMOKE_RPC=http://127.0.0.1:8899 \
        SLOTFARE_SMOKE_KEY=<funded base58 key> \
        SLOTFARE_LEDGER_PROGRAM=<program id> \
        go test ./internal/solana -run LiveLedger -count=1 -v

The key must be the program's config authority, since minting and settling are
restricted to it; the test skips with that message when it is not.

Console sign-in smoke, against a running daemon:

    SLOTFARE_CONSOLE_SMOKE=http://127.0.0.1:8443 \
        go test ./internal/console -run TestLiveConsoleSignIn -count=1 -v

## Step 7 - Console

Run `slotfared` on `127.0.0.1:8443`, then in `web/` run `npx ng serve`. The dev
server proxies `/console` and `/rpc` to the daemon
(`web/proxy.conf.json`), so open `http://localhost:4200/`, connect Phantom
and sign the nonce.

The sign-in message names whatever origin the console is served from - the
page's `Origin`, or the request Host - because Phantom validates a Sign-In
with Solana message against the requesting page's own host. There is nothing
to configure for either the local or a deployed console ([decisions.md](./decisions.md),
D19).

For a deployed console, set `SLOTFARE_CORS_ORIGINS` to the console's exact
origin and `SLOTFARE_PUBLIC_URL` to the daemon's public base URL.

## Step 8 - Storage

SQLite is the default (`SLOTFARE_DB`, checkpointed). For more than one replica,
set `SLOTFARE_DATABASE_URL` to a Postgres URL: Postgres becomes the balance
authority and `SLOTFARE_DB` is ignored. An existing SQLite database can be
moved over with:

    go run ./cmd/slotfared import-sqlite --database-url "$SLOTFARE_DATABASE_URL" slotfared.db

Postgres is what makes more than one metering replica safe: balances live in
a shared table with the store's locking, not in each process's memory.

## Step 9 - Probes

- `GET /health` - liveness: the process is up. No dependency is checked.
- `GET /ready` - readiness: the backing store answers within the timeout.
- `GET /.well-known/slotfare-receipt-key` - the public key receipts are signed
  with, so a client can verify one without being told out of band.

Point the platform's liveness probe at `/health` and its readiness probe at
`/ready`. A replica that fails `/ready` should not take metered traffic.

## Step 10 - Repricing without a redeploy

`SLOTFARE_WEIGHTS` names a JSON file that overrides the built-in weight table.
A missing file uses the defaults; only present keys override. See
[metering.md](./metering.md) for the format and the built-in Solana table.

## Step 11 - Record the deployment

When something is actually deployed, write it into `deployments/` - program
ids, treasury, mint, cluster, URLs, and the git revision deployed. That
directory is the record; this runbook is only the procedure. See
[`deployments/README.md`](../deployments/README.md).
