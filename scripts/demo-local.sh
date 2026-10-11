#!/usr/bin/env bash
# End-to-end local demo: a USDC-like SPL token deposit is watched by
# slotfared and mints prepaid credits to the sender's account.
#
#   scripts/demo-local.sh
#
# Requires the Agave toolchain (solana, solana-keygen, spl-token,
# solana-test-validator) and Go on PATH; a local validator is started if
# nothing listens on :8899. Prints E2E PASS on success.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${SLOTFARE_DEMO_DIR:-/tmp/slotfare-demo}"
RPC="${SLOTFARE_SMOKE_RPC:-http://127.0.0.1:8899}"
LISTEN=127.0.0.1:8088

# Manual Agave installs live here (README); non-interactive shells do not
# read the rc files that put it on PATH.
if ! command -v solana >/dev/null && [ -x "$HOME/.local/solana-release/bin/solana" ]; then
	PATH="$HOME/.local/solana-release/bin:$PATH"
fi

for bin in solana solana-keygen spl-token solana-test-validator go python3 curl; do
	command -v "$bin" >/dev/null || {
		echo "missing $bin (install the Agave toolchain and Go, then retry)" >&2
		exit 1
	}
done

rm -rf "$WORK"
mkdir -p "$WORK"
cd "$WORK"

FDPID=""
VALPID=""
cleanup() {
	[ -n "$FDPID" ] && kill "$FDPID" 2>/dev/null || true
	[ -n "$VALPID" ] && kill "$VALPID" 2>/dev/null || true
}
trap cleanup EXIT

# Local validator: reuse anything already on :8899, otherwise start one.
# SLOTFARE_VALIDATOR_ARGS appends flags (e.g. --gossip-port 8123
# --dynamic-port-range 8124-8200 when something occupies the defaults).
if ! nc -z 127.0.0.1 8899 2>/dev/null; then
	# shellcheck disable=SC2086
	solana-test-validator --ledger "$WORK/ledger" --quiet ${SLOTFARE_VALIDATOR_ARGS:-} >validator.log 2>&1 &
	VALPID=$!
	for _ in $(seq 1 30); do
		nc -z 127.0.0.1 8899 2>/dev/null && break
		sleep 1
	done
fi
nc -z 127.0.0.1 8899 2>/dev/null || {
	echo "validator did not come up on :8899 (see $WORK/validator.log and $WORK/ledger/validator.log)" >&2
	exit 1
}

# Throwaway keys: fee payer signs everything, user is the depositor,
# treasury owns the deposit token account.
for k in payer user treasury; do
	solana-keygen new --no-bip39-passphrase -s -o "$k.json" --force >/dev/null 2>&1
done
solana airdrop 10 "$(solana-keygen pubkey payer.json)" --url "$RPC" >/dev/null
solana airdrop 10 "$(solana-keygen pubkey user.json)" --url "$RPC" >/dev/null

# A test mint (6 decimals, like USDC) plus both associated token accounts.
MINT=$(spl-token create-token --decimals 6 --fee-payer payer.json \
	--mint-authority payer.json --url "$RPC" 2>&1 | sed -n 's/^Address:  //p')
TREASURY_ATA=$(spl-token create-account "$MINT" --owner treasury.json \
	--fee-payer payer.json --url "$RPC" 2>&1 | sed -n 's/^Creating account //p')
USER_ATA=$(spl-token create-account "$MINT" --owner user.json \
	--fee-payer payer.json --url "$RPC" 2>&1 | sed -n 's/^Creating account //p')
[ -n "$MINT" ] && [ -n "$TREASURY_ATA" ] && [ -n "$USER_ATA" ] || {
	echo "token setup failed" >&2
	exit 1
}
spl-token mint "$MINT" 10 "$USER_ATA" --mint-authority payer.json \
	--fee-payer payer.json --url "$RPC" >/dev/null

# Fee payer as the base58 key slotfared expects, and the account the deposit
# must mint to: 0x-hex of the user wallet's 32-byte public key.
WIF=$(python3 - "$WORK/payer.json" <<'PY'
import json, sys
A = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
b = bytes(json.load(open(sys.argv[1])))
n, s = int.from_bytes(b, "big"), ""
while n:
    n, r = divmod(n, 58)
    s = A[r] + s
print("1" * (len(b) - len(b.lstrip(b"\0"))) + s)
PY
)
EXPECT=$(python3 - "$WORK/user.json" <<'PY'
import json, sys
print("0x" + bytes(json.load(open(sys.argv[1]))[32:]).hex())
PY
)

# slotfared with the chain surface enabled; poll fast so the demo is quick.
(cd "$ROOT" && go build -o "$WORK/slotfared" ./cmd/slotfared)
SLOTFARE_SOLANA_RPC="$RPC" \
	SLOTFARE_SOLANA_CLUSTER=localnet \
	SLOTFARE_SOLANA_FEE_PAYER="$WIF" \
	SLOTFARE_SOLANA_TREASURY="$TREASURY_ATA" \
	SLOTFARE_SOLANA_MINT="$MINT" \
	SLOTFARE_SOLANA_POLL=2s \
	SLOTFARE_UPSTREAM=http://127.0.0.1:9999 \
	SLOTFARE_LISTEN="$LISTEN" \
	SLOTFARE_DB="$WORK/slotfared.db" \
	"$WORK/slotfared" >"$WORK/slotfared.log" 2>&1 &
FDPID=$!
for _ in $(seq 1 30); do
	curl -s -o /dev/null "http://$LISTEN/health" && break
	sleep 0.5
done
curl -s -o /dev/null "http://$LISTEN/health" || {
	echo "slotfared did not come up (see $WORK/slotfared.log)" >&2
	exit 1
}

# 2.5 USDC in: 2,500,000 base units x 10 micro-credits = 25,000,000.
spl-token transfer "$MINT" 2.5 "$TREASURY_ATA" --owner user.json \
	--fee-payer payer.json --url "$RPC" >/dev/null

want="deposit mint: $EXPECT now 25000000 micro-credits"
for _ in $(seq 1 30); do
	grep -q "$want" "$WORK/slotfared.log" && break
	sleep 1
done
if grep -q "$want" "$WORK/slotfared.log"; then
	grep "$want" "$WORK/slotfared.log"
	echo "E2E PASS: 2.5 USDC deposit minted 2,500 credits to $EXPECT"
else
	echo "FAIL: $want not seen (log tail:)" >&2
	tail -20 "$WORK/slotfared.log" >&2
	exit 1
fi
