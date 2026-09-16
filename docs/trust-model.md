# Trust model, stated plainly

This is the honest version of what Slotfare-on-Solana proves and what it
asks you to believe. It exists because the port changed the trust model of
the original, and a README that hides that is worse than one that
names it.

Source of truth: [`internal/solana/watcher.go`](../internal/solana/watcher.go)
(deposits), [`internal/solana/commit.go`](../internal/solana/commit.go)
(settlement), [`internal/meter/store.go`](../internal/meter/store.go) (the
local balance authority), and
[`programs/slotfare-ledger`](../programs/slotfare-ledger) with
[`internal/solana/ledger.go`](../internal/solana/ledger.go) (the on-chain
ledger). The Go and Rust suites for those are the executable statement of the
rules below, and
[`internal/solana/ledger_live_test.go`](../internal/solana/ledger_live_test.go)
is the one that asks a real cluster.

The port is described below in two states, because they differ where it
matters: a deployment **with** the on-chain ledger program
(`SLOTFARE_LEDGER_PROGRAM` set) and one **without** it.

## What runs

| Piece | Where the truth lives |
|---|---|
| Credit minting | the **deposit watcher**, a process we run, reading Solana |
| Credit balances | the **local ledger** (SQLite or Postgres) for reservations and display; **also** a program-owned PDA when the ledger program is configured |
| Usage | the local ledger, folded into a Merkle root |
| Settlement evidence | a **Memo transaction** (`slotfare-usage-root/v1`), or a **`CommitAndSettle` instruction** when the ledger program is configured |
| The burn | the local store, atomically with the evidence — and, with the program, inside the same instruction |
| Receipts | a key we hold, signing an `x-slotfare-receipt` header |

Without the ledger program, Solana is a chain we *write* evidences to; it is not
the ledger. With it, the chain holds a balance for every owner, holds the locks
that make a listed credit unspendable, and refuses a burn that a committed root
does not authorise.

## The deposit rail (single rail, watcher-based)

A deposit is one SPL token transfer: USDC to the treasury token account. The
watcher walks that account's full signature history with
`getSignaturesForAddress`, fetches each transaction, and takes the **balance
delta of the configured mint on the treasury** as the deposited amount. It
credits the sender at a fixed rate - 10 micro-credits per USDC base unit,
i.e. 1,000 credits per $1.00 - and writes an **absolute** minted total, not
an increment, so a restart or a replayed scan converges on the same number
instead of double-minting.

What that buys you: **credit minting is chain-derivable.** Given the treasury
address, the mint, and the fixed conversion, anyone can recompute the minted
total from public chain history. There is no admin top-up path.

What it does not buy you:

- **No cross-chain deposit proof.** The original design proved a deposit
  on a source chain through an attestation contract and a proof verifier on
  the destination chain.
  This port has no equivalent. If the deposit is wrong, the chain does not
  say so; our watcher does, and you are trusting our watcher's view of
  history.
- **No confirmation-depth policy beyond the RPC's.** We read at the
  commitment the client requests; the ledger does not wait for finality on
  its own schedule.
- **The treasury is identified as a token account, not by owner.** A
  misconfiguration that points `SLOTFARE_SOLANA_TREASURY` at the wrong
  account mints from the wrong transfers. Configuration is a trusted input.

## The ledger: two authorities, and which one you get

`BalanceOf` re-reads the local ledger. Balances in SQLite (in-memory, with
checkpoints) or Postgres (a shared accounts table) are the answer to "how many
credits do I have" on the hot path, and it is *our* answer, not the chain's.

With the ledger program configured there is a second, chain-side answer, and
it is the one that constrains what the daemon can do:

- every owner's `minted`, `spent` and `locked` live in a program-owned PDA;
- the burn happens **inside** `CommitAndSettle`, so there is no burn without a
  committed root and one root per burn — enforced by the program's state, not
  by our bookkeeping;
- a batch cannot rewind the usage frontier;
- locked credits cannot be settled, so a listing cannot be spent underneath a
  buyer;
- the program's own error codes say which rule was broken
  ([onchain-ledger.md](./onchain-ledger.md)).

The original kept its ledger in an on-chain contract for the same reason. So
this is closer to the original's trust model than the Memo-only port was, but
it is not identical, and the difference is worth stating precisely.

What this means concretely, with the program configured: an operator who runs
`slotfared` still cannot mint credits the deposit rail did not see, because the
mint is gated on the config authority and the deposit is chain-derivable; and
it cannot spend or burn more than the chain says an account holds.

What it still cannot stop: **the mint itself**. The chain cannot see a deposit,
so `Credit` trusts the authority key exactly as far as the original trusted its
deposit contract's role. A compromised operator can mint against deposits that
do not exist — it just cannot then do anything the chain's state forbids.
Without the program configured, the weaker sentence applies: the *spend and
burn* side is only as good as the process.

## Settlement: what a Memo actually proves

On each settle pass the daemon folds a batch of usage into a Merkle root,
posts the memo payload
`{"scheme":"slotfare-usage-root/v1","root":…,"account":…,"first":…,"last":…,"credits":…}`
(the Memo program caps it at 566 bytes), and only then burns the covered
spend inside the same store transaction. The chain evidence and the local
state are written as a pair, or neither is.

A verifier holding the usage leaves (served with inclusion proofs from the
console API) can recompute the root and confirm each leaf is in it, then
match the root against the chain.

A Memo transaction proves:

- that **the fee payer authored this root** at this slot, in this order;
- that the root, account, usage-id range and credit total are **pinned to a
  public timestamp**.

A Memo transaction does **not** prove:

- that the root is correct, that the usage inside it was actually served, or
  that the credits were ever minted - **anyone can post a Memo**. It is
  evidence of authorship and ordering, not of validity;
- anything at all if you do not fetch the leaves and recompute the root
  yourself. The proof is only as strong as the verifier who uses it.

The original landed the same idea as an on-chain settlement call, where
the chain could reject a malformed claim. Solana's Memo program cannot: the
memo is opaque data. That is the trade the Memo rail made.

## Settlement with the program: what it proves instead

With `SLOTFARE_LEDGER_PROGRAM` set, the same batch lands as a `CommitAndSettle`
instruction rather than a Memo, and the evidence gets stronger in the one place
the Memo was weakest.

It still proves what the Memo proved: the root, the account, the usage-id range
and the credit total are pinned to a public, timestamped transaction, and a
verifier holding the leaves can recompute the root.

It additionally proves, because the program refuses otherwise:

- **the account consented to the burn by construction** — the credits were
  minted to that PDA and the burn comes out of its `spent`, so a root cannot be
  posted against a balance that was not there;
- **the root is unique for that account** — a replay of the same root is
  rejected on chain, so a double-post is impossible rather than merely
  unattempted;
- **the batch is ordered** — the usage-id range must advance, so a batch cannot
  rewrite history the console has already shown;
- **locked credits are outside it** — a settlement cannot consume credits a
  listing has reserved.

It still does not prove that the usage inside the root was actually served, or
that the metered work was the work advertised. The root is our claim about our
own usage; the chain proves the claim was *well-formed, unique and affordable*,
not that it was *true*. That gap needs a verifier who holds the leaves, and it
is why the leaves are served with inclusion proofs.

The two rails are now genuinely different products rather than cosmetics, which
is why the live test asserts the rejections and not just the happy path.

## Receipts

Metered, non-streaming responses can carry an `x-slotfare-receipt` header
signed by a dedicated key (`SLOTFARE_RECEIPT_KEY`, secp256k1). A receipt lets
a client bind one response to one committed leaf. It is signed by a key we
hold, so it is as strong as our key management, and it says nothing about
whether the model behind the response was the one advertised.

## Trusted components, in order

1. **The `slotfared` process** - minter, balance authority, settler, receipt
   signer. Everything above reduces to this.
2. **The RPC endpoint** it reads (`SLOTFARE_SOLANA_RPC`). A lying endpoint
   can feed the watcher a fabricated history, and can misreport a ledger read.
3. **The fee payer key** (`SLOTFARE_SOLANA_FEE_PAYER`). A throwaway key that
   signs every memo and every ledger instruction; it needs SOL, never a key
   that has touched mainnet. With the program configured it is also the
   configured authority, so it can mint.
4. **Configuration**: treasury, mint, cluster. Wrong values mint from the
   wrong place. `ledger-init` refuses to continue when the program's bound
   config disagrees with the environment, which is what turns this from a
   silent failure into a loud one.
5. **The program's upgrade authority**, when the ledger program is deployed.
   Whoever holds it can replace the program's rules. Burning it after
   deployment is the honest end state, and until then this key is as trusted
   as the daemon.
6. **The receipt key**, for anything that verifies receipts.

## What is verifiably better than the original, and what is not

Better: deposits are reconstructable from public chain history at a fixed
rate; each settlement batch is pinned to a public, timestamped root; there is
no privileged mint path beyond the deposit rail's authority; and with the
ledger program configured, the ledger's rules are the chain's rules — a burn
without a committed root, a replayed root, a rewound frontier and a settlement
against locked credits are all refusals, not conventions.

Not as good: no cross-chain deposit proof, the mint still trusts the deposit
rail's key rather than proving a deposit on chain, and without the ledger
program the balance authority is a database we operate rather than a chain
contract.

Both statements belong in the pitch. The hackathon asks for a working
product and an honest one; this page is the second half.
