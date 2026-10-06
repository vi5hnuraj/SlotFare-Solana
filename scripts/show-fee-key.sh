#!/usr/bin/env sh
# Print the devnet fee-payer key as base58 so it can be imported into
# Phantom (Settings -> Import private key). Runs locally; the key is read
# from the keypair file and never sent anywhere. Throwaway key: it funds
# transaction fees only.
set -eu
KEY=${1:-$HOME/.config/solana/id.json}
python3 - "$KEY" <<'PY'
import json, sys
A = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
def b58e(b):
    n = int.from_bytes(b, "big"); s = ""
    while n:
        n, r = divmod(n, 58); s = A[r] + s
    return "1" * (len(b) - len(b.lstrip(b"\x00"))) + s
raw = json.load(open(sys.argv[1]))
print("public: ", end="")
n = int.from_bytes(bytes(raw[32:]), "big"); s = ""
while n:
    n, r = divmod(n, 58); s = A[r] + s
print("1" * (32 - len(bytes(raw[32:]).lstrip(b"\x00"))) + s)
print("private:", b58e(bytes(raw)))
PY
