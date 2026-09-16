# Architecture

The shape of the repository after the October 2026 refactor: what each
directory is for, the map from the paths it replaced, and the rules the
layout enforces. Why each decision was taken is in
[decisions.md](./decisions.md); this file is the map.

## Layout

```text
slotfare-solana/
├── cmd/slotfared/          the daemon: configuration, wiring, lifecycle,
│                           and its two subcommands (import-sqlite,
│                           ledger-init). No business logic lives here.
├── internal/
│   ├── meter/              the credit ledger: balances, pricing,
│   │                       reservations, flusher, settler, proofs. Depends
│   │                       on nothing else in the repo.
│   ├── solana/             the chain surface: RPC client, deposit watcher,
│   │                       Memo settlement commit, ledger-program client,
│   │                       market indexer. Depends on nothing else in the
│   │                       repo.
│   ├── funding/            purchases: the store contract, its SQL, the
│   │                       SQLite driver.
│   ├── market/             listings: the row, its state moves, the lock
│   │                       sums, the store contract and driver, and the
│   │                       Mirror the indexer writes through.
│   ├── session/            sessions and nonces: the contract, its SQL, the
│   │                       SQLite driver.
│   ├── schema/             the SQLite DDL for funding, funding_cursor and
│   │                       listings, plus the column helpers the
│   │                       migrations use. One home per table family.
│   ├── console/            the HTTP layer: routing, handlers, the sign-in
│   │                       flow, and the funding/market views it renders.
│   ├── proxy/              the metering reverse proxy: auth, weights,
│   │                       rate limits, forwarding, receipts.
│   ├── pgstore/            the Postgres implementations of the store
│   │                       contracts, and the Postgres DDL.
│   ├── keys/ receipt/ cors/   support packages.
├── programs/slotfare-ledger/  the on-chain program (native solana-program,
│                              built by cargo-build-sbf; there is no Anchor
│                              workspace) and its state machine tests.
├── web/                    the Angular console, with its own toolchain,
│                            Dockerfile and gates.
├── scripts/                demo, devnet and deployment scripts.
├── deployments/            the single source of truth for addresses.
├── Makefile                the quality gates CI runs.
└── .gitea/workflows/       CI at the repository root (Gitea only loads
                            workflows from the root; the copy that used to
                            live under web/ never ran).
```

## Old path to new path

| Was | Now | Why |
|---|---|---|
| `internal/console/sessions.go` (store half) | `internal/session` | persistence had no business inside the HTTP package |
| `internal/console/funding.go` (purchase half) | `internal/funding` | same |
| `internal/console/funding.go` (listing half) | `internal/market` | purchases and listings are separate responsibilities with separate interfaces |
| `internal/console/funding.go` (DDL, column helpers) | `internal/schema` | the schema was written in three files and had to be written once |
| `internal/console/marketmirror.go` | `internal/market/mirror.go` | it writes listings; it belongs with them |
| `internal/console/market.go`, `marketwrite.go` (interface declarations only) | `internal/market` | storage must not depend on the transport that reads it |
| `internal/console/{funding_listingid,funding_nonce}_test.go` | `internal/market/listing_{id,nonce}_test.go` | tests moved with the code they exercise |
| `web/.gitea/workflows/` | `.gitea/workflows/` | Gitea loads workflows only from the repository root |
| *(new)* | `Makefile` | the gates README described as prose had no command behind them |

Everything else stayed: `cmd/slotfared`, `internal/meter`,
`internal/solana`, `internal/proxy`, `internal/pgstore`, `internal/keys`,
`internal/receipt`, `internal/cors`, `programs/`, `web/`, `scripts/`,
`deployments/` and `docs/`.

## The dependency rule

Imports point downward, and nothing points back up:

```text
cmd/slotfared  →  console, proxy, pgstore, funding, market, session,
                  meter, keys, receipt, cors, solana
console        →  funding, market, session, meter, keys, solana
proxy          →  meter, keys, receipt
receipt        →  meter
pgstore        →  funding, market, session, meter, keys
funding        →  schema
market         →  schema, solana
session        →  (nothing)
schema         →  (nothing)
meter, solana, keys, cors → (nothing in this repo)
```

Before the refactor `internal/pgstore` imported `internal/console`: storage
depended on the transport layer for its data models *and* for its SQL. That
edge is gone, and no package under `internal/` imports `internal/console`
except `cmd/slotfared`, which is the composition root.

## What is fixed by the layout

- **One home per table family.** `funding`, `funding_cursor` and `listings`
  each have exactly one `CREATE TABLE` and one place for the `ALTER`s that
  bring an old file forward (`internal/schema`). The Postgres DDL is
  separate and lives with the Postgres driver, which is the one place a
  dialect can differ.
- **Contracts where both sides can see them.** `funding.Store`,
  `market.Store`, `market.Reader`, `market.WriteStore` and `session.Store`
  are defined in the package that owns the data, so the HTTP layer and the
  storage layer both depend on it - never on each other.
- **A transport that renders rather than persists.** `internal/console` now
  holds routing, handlers, the sign-in flow and the views.

## Compatibility constraints (what the refactor did not change)

- HTTP routes, method handling and JSON response shapes.
- Table names, column names, statements and both schema dialects; every
  migration still runs on a database written before the refactor.
- On-chain: program ids, PDA seeds, instruction tag bytes 0-9, account
  layouts, and the `slotfare-usage-root/v1` memo payload.
- Configuration: every `SLOTFARE_*` variable, every CLI flag, both
  subcommands, and `deployments/` records.
- Clean module boundaries: `internal/session`, `internal/funding`, `internal/market`,
  and `internal/schema` provide clear separation of concerns split out of `internal/console`.
