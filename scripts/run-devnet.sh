#!/usr/bin/env sh
# Run slotfared against the devnet deployment. The daemon reads OS
# environment only, so the generated .env is sourced here; everything else
# is ordinary exec. Logs to stdout/stderr - redirect where you like.
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
[ -f .env ] || { echo "run-devnet: no .env; run scripts/devnet-up.sh first" >&2; exit 1; }
set -a
. ./.env
set +a
exec go run ./cmd/slotfared
