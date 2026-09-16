# Deployments

This directory is the **single source of truth for every deployed address
and URL**. `.env` carries what a local run needs; this directory carries what
was actually deployed, and the two must not be confused.

An address pasted anywhere else - a README, a slide, a chat message - is an
address that will be wrong in one of the two places. If a value belongs to a
deployment, it belongs here.

## Status: nothing is deployed

There is no public deployment and therefore no record file. `docs/` and this
directory were empty until 6 Oct 2026; `deployments/` had no tracked file at
all, which is why it did not survive a clone. This README is the first entry.

The honest consequence: **there are no live testnet URLs.** The original
Slotfare had a public testnet with Postgres and two metering replicas behind
it; this port does not have one yet. See
[`docs/testnet-runbook.md`](../docs/testnet-runbook.md) for the procedure and
the current verified/unverified split.

## What a record must contain

One file per environment (`devnet.json`, then `mainnet-beta.json`), plus
`local.json` for the `solana-test-validator` walkthrough. Required keys:

| Key | Meaning |
|---|---|
| `environment` | `local`, `devnet` or `mainnet-beta` |
| `cluster` | the Solana cluster label, used in explorer links |
| `rpc` | the RPC endpoint the daemon reads |
| `treasury` | the **token account** that receives deposits (not the owner) |
| `mint` | the SPL mint deposits are made in |
| `feePayer` | the **public** key that funds memo transactions; never the private key |
| `commitProgram` | the Memo program id (`MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr`) |
| `consoleUrl` | the deployed console origin |
| `apiUrl` | the daemon's public base URL |
| `deployedAt` | ISO-8601 timestamp of the deployment |
| `gitRevision` | the revision deployed, so a record can be tied to code |

Never put a private key in this directory. `SLOTFARE_SOLANA_FEE_PAYER` and
`SLOTFARE_RECEIPT_KEY` are secrets and stay in `.env`; only their **public**
counterparts are recorded.

## Local demo record

`scripts/demo-local.sh` mints throwaway keys and a test mint on a local
validator each run, so it is not recorded here: its addresses are not stable
and mean nothing after the validator stops. What it proves is the deposit →
mint path, and it reports its own result.

## Adding a record

1. Copy [`testnet.example.json`](./testnet.example.json) to the environment's
   file name.
2. Fill every key; leave nothing as the placeholder.
3. Verify before committing: the deposit walkthrough mints the expected
   credits, and the commit path lands a decodable memo
   (`docs/testnet-runbook.md`, Steps 3, 6 and 11).
4. Add the file in the same change as any code that points at it.
