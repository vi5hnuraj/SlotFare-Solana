#!/usr/bin/env sh
# Bring the Slotfare stack up on Solana devnet. Idempotent: re-running
# reuses whatever already exists (mint, treasury, program) and rewrites
# .env and deployments/devnet.json from what is actually on chain.
#
# Prereqs:
#   - solana, spl-token, cargo-build-sbf on PATH (Agave toolchain)
#   - a throwaway fee-payer keypair with >= 1 SOL devnet at
#     ~/.config/solana/id.json (`solana airdrop 1`, or faucet.solana.com)
#   - SLOTFARE_OWNER set to the wallet that will own the treasury and the
#     ledger program's upgrade authority (defaults to the console wallet
#     Cq1JifKBnU6EyWNVgj3eEXx5Th5u44camD2PsEmGJ9BJ)
#
# Never prints or stores the fee-payer private key anywhere except the
# gitignored .env it writes; deployments/devnet.json gets public keys only.
set -eu

RPC=${SLOTFARE_RPC_URL:-https://api.devnet.solana.com}
CLUSTER=devnet
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
if [ "$RPC" = "https://api.devnet.solana.com" ]; then
  say "note: using the shared public RPC; set SLOTFARE_RPC_URL to a free"
  say "      Helius/RPC Fast endpoint for a demo that never rate-limits"
fi

PAYER=${SLOTFARE_DEVNET_PAYER:-$HOME/.config/solana/id.json}
PAYER_PUB=$(solana-keygen pubkey "$PAYER")
# The wallet that owns the treasury and the program's upgrade authority.
OWNER=${SLOTFARE_OWNER:-Cq1JifKBnU6EyWNVgj3eEXx5Th5u44camD2PsEmGJ9BJ}
LEDGER_PROGRAM=2GiCtSotdJ5PtCRvLtkYnrd4JT5TjkkugzJzqbmDw226
SO=programs/slotfare-ledger/target/deploy/slotfare_ledger.so
SO_PAIR=programs/slotfare-ledger/target/deploy/slotfare_ledger-keypair.json
RECORD=deployments/devnet.json

say() { printf 'devnet-up: %s\n' "$*"; }
die() { printf 'devnet-up: %s\n' "$*" >&2; exit 1; }

# --- 1. funding -------------------------------------------------------------
bal=$(solana balance "$PAYER_PUB" -u "$RPC" | awk '{print int($1)}')
[ "$bal" -ge 1 ] || die "fee payer $PAYER_PUB has $bal SOL on devnet; need >= 1 (solana airdrop 1, or https://faucet.solana.com)"
say "fee payer $PAYER_PUB ($bal SOL)"

# --- 2. ledger program (deploy once; upgrade authority always the wallet) ---
if solana program show "$LEDGER_PROGRAM" -u "$RPC" >/dev/null 2>&1; then
  say "program $LEDGER_PROGRAM already deployed"
else
  [ -f "$SO" ] || die "missing $SO; run programs/slotfare-ledger/build.sh first"
  flags=$(od -An -tx4 -j48 -N4 "$SO" | tr -d ' \n')
  [ "$flags" = 00000003 ] || die "$SO is not SBPF v3 (e_flags=$flags); rebuild with build.sh"
  say "deploying program $LEDGER_PROGRAM ($(wc -c <"$SO" | tr -d ' ') bytes)"
  solana program deploy -u "$RPC" --keypair "$PAYER" --program-id "$SO_PAIR" "$SO"
fi
# The contract is only under the operator's wallet if that wallet can
# upgrade it: move the upgrade authority off the deploy key every run.
CUR_AUTH=$(solana program show "$LEDGER_PROGRAM" -u "$RPC" \
  | sed -n 's/^Upgrade Authority: //p' | grep -oE '[1-9A-HJ-NP-Za-km-z]{32,44}' || true)
if [ "$CUR_AUTH" != "$OWNER" ]; then
  say "setting program upgrade authority → $OWNER"
  # The new authority is an external wallet (Phantom) that cannot sign
  # here; the CLI otherwise requires its signature by default.
  solana program set-upgrade-authority "$LEDGER_PROGRAM" -u "$RPC" \
    --keypair "$PAYER" --new-upgrade-authority "$OWNER" \
    --skip-new-upgrade-authority-signer-check
fi

# --- 3. mint + treasury (create once; recoverable from the record) ---------
if [ -f "$RECORD" ]; then
  MINT=$(sed -n 's/.*"mint": *"\([^"]*\)".*/\1/p' "$RECORD" | head -1)
  TREASURY=$(sed -n 's/.*"treasury": *"\([^"]*\)".*/\1/p' "$RECORD" | head -1)
fi
if [ -z "${MINT:-}" ] || [ -z "${TREASURY:-}" ]; then
  say "creating 6-decimal test-USDC mint (authority $PAYER_PUB)"
  MINT=$(spl-token create-token --decimals 6 --url "$RPC" \
    --fee-payer "$PAYER" --mint-authority "$PAYER" 2>&1 \
    | sed -n 's/^Address:  //p')
  [ -n "$MINT" ] || die "could not parse mint address from create-token"
  TREASURY=$(spl-token address --token "$MINT" --owner "$OWNER" --verbose -u "$RPC" \
    | sed -n 's/^Associated token address: //p')
  [ -n "$TREASURY" ] || die "could not derive treasury token account"
  spl-token create-account "$MINT" --owner "$OWNER" \
    --url "$RPC" --fee-payer "$PAYER" >/dev/null 2>&1 || true
  say "mint $MINT, treasury $TREASURY (owner $OWNER)"
else
  say "reusing mint $MINT, treasury $TREASURY"
fi

# --- 4. test-credit stockpile -----------------------------------------------
# Faucet stock sits in the fee payer's token account; the owner wallet gets
# a spendable balance so the console's PAY flow works straight away.
PAYER_ATA=$(spl-token address --token "$MINT" --owner "$PAYER_PUB" --verbose -u "$RPC" \
  | sed -n 's/^Associated token address: //p')
spl-token create-account "$MINT" --owner "$PAYER_PUB" \
  --url "$RPC" --fee-payer "$PAYER" >/dev/null 2>&1 || true
stock=$(spl-token balance "$MINT" --owner "$PAYER_PUB" --url "$RPC" 2>/dev/null || echo 0)
if [ "${stock%.*}" -lt 1000 ]; then
  say "minting 1000000 test-USDC to faucet stock $PAYER_ATA"
  spl-token mint "$MINT" 1000000 "$PAYER_ATA" --mint-authority "$PAYER" --url "$RPC" >/dev/null
fi
OWNER_ATA=$(spl-token address --token "$MINT" --owner "$OWNER" --verbose -u "$RPC" \
  | sed -n 's/^Associated token address: //p')
spl-token create-account "$MINT" --owner "$OWNER" \
  --url "$RPC" --fee-payer "$PAYER" >/dev/null 2>&1 || true
owner_bal=$(spl-token balance "$MINT" --owner "$OWNER" --url "$RPC" 2>/dev/null || echo 0)
if [ "${owner_bal%.*}" -lt 100 ]; then
  say "minting 1000 test-USDC to owner wallet $OWNER"
  spl-token mint "$MINT" 1000 "$OWNER_ATA" --mint-authority "$PAYER" --url "$RPC" >/dev/null
fi
say "owner wallet $OWNER holds ${owner_bal%.*} test-USDC (0x$(printf '%s' "$OWNER" | python3 -c '
import sys
A="123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
s=sys.stdin.read().strip(); n=0
for c in s: n=n*58+A.index(c)
h=n.to_bytes((n.bit_length()+7)//8,"big").hex()
print(("00"*((32-len(h)//2)))+h)'))"

# --- 5. ledger-init (binds authority/mint/treasury in the program config) ---
FEE_B58=$(python3 - "$PAYER" <<'PY'
import json, sys
A = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
def b58e(b):
    n = int.from_bytes(b, "big"); s = ""
    while n:
        n, r = divmod(n, 58); s = A[r] + s
    return "1" * (len(b) - len(b.lstrip(b"\x00"))) + s
print(b58e(bytes(json.load(open(sys.argv[1])))))
PY
)
SLOTFARE_SOLANA_RPC="$RPC" SLOTFARE_SOLANA_CLUSTER="$CLUSTER" \
SLOTFARE_SOLANA_FEE_PAYER="$FEE_B58" \
SLOTFARE_SOLANA_TREASURY="$TREASURY" SLOTFARE_SOLANA_MINT="$MINT" \
SLOTFARE_LEDGER_PROGRAM="$LEDGER_PROGRAM" \
  go run ./cmd/slotfared ledger-init

# --- 6. deployment record ---------------------------------------------------
REV=$(git rev-parse --short HEAD 2>/dev/null || echo "uncommitted")
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Provider URLs carry ?api-key=...; the record is committed, so strip it.
RPC_PUBLIC=$(printf '%s' "$RPC" | sed 's/?.*$//')
cat > "$RECORD" <<EOF
{
  "_comment": "Live devnet deployment. Public keys only; the fee-payer private key and any provider API key live in the gitignored .env. See deployments/README.md.",
  "environment": "devnet",
  "cluster": "devnet",
  "rpc": "$RPC_PUBLIC",
  "treasury": "$TREASURY",
  "mint": "$MINT",
  "feePayer": "$PAYER_PUB",
  "treasuryOwner": "$OWNER",
  "commitProgram": "MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr",
  "ledgerProgram": "$LEDGER_PROGRAM",
  "upgradeAuthority": "$OWNER",
  "consoleUrl": "http://127.0.0.1:4200",
  "apiUrl": "http://127.0.0.1:8443",
  "deployedAt": "$NOW",
  "gitRevision": "$REV"
}
EOF
say "wrote $RECORD"

# --- 7. .env for the daemon -------------------------------------------------
cat > .env <<EOF
# Generated by scripts/devnet-up.sh ($NOW). Gitignored - holds the fee-payer
# spending key. Re-run the script to regenerate from what is on chain.

# Chain RPC. Swap in a dedicated provider endpoint (Helius, RPC Fast) for
# the public demo: the watcher, indexer and settlement loop are RPC-heavy
# and the shared public endpoint rate-limits.
SLOTFARE_SOLANA_RPC=$RPC
SLOTFARE_SOLANA_CLUSTER=$CLUSTER
SLOTFARE_SOLANA_FEE_PAYER=$FEE_B58
SLOTFARE_SOLANA_TREASURY=$TREASURY
SLOTFARE_SOLANA_MINT=$MINT
SLOTFARE_LEDGER_PROGRAM=$LEDGER_PROGRAM
SLOTFARE_LEDGER_REQUIRED=1
SLOTFARE_SOLANA_POLL=5s

# Metering proxy upstream: the same endpoint, so /rpc requests ride the
# provider's free tier instead of the shared public limits.
SLOTFARE_UPSTREAM=$RPC
SLOTFARE_LISTEN=127.0.0.1:8443
SLOTFARE_DB=slotfared.db
SLOTFARE_PUBLIC_URL=http://127.0.0.1:8443
SLOTFARE_SETTLE_INTERVAL=10s
SLOTFARE_SETTLE_THRESHOLD=1

# Inference stays off until an NVIDIA key is set; the daemon starts without it.
# SLOTFARE_INFERENCE_UPSTREAM=https://integrate.api.nvidia.com
# SLOTFARE_INFERENCE_UPSTREAM_KEY=nvapi-...
EOF
say "wrote .env"
say "done. restart the daemon with: scripts/run-devnet.sh"
