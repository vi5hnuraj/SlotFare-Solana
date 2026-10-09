#!/bin/sh
# Write the app's runtime config before nginx starts.
#
# The app fetches config.json at boot and uses its apiUrl as the console API
# base URL when present. An unset or empty API_BASE_URL produces
# {"apiUrl":""}, which means same-origin relative URLs - exactly the dev
# behavior. The document is always written so a stale one can never leak in.
#
# NETWORK names the Slotfare network the console fronts ("testnet" or
# "mainnet"); the app keeps its compiled default (testnet) for any other
# value, so leaving it unset is safe and only mainnet needs to say so.
#
# Operator contract: API_BASE_URL is a plain URL without double quotes or
# backslashes, NETWORK a bare word; nothing validates them here.
set -eu

# nginx's docker-entrypoint.sh only runs these scripts when the command is
# nginx, and it invokes them with no arguments - so no argv guard here.

printf '{"apiUrl":"%s","network":"%s"}\n' "${API_BASE_URL:-}" "${NETWORK:-}" > /usr/share/nginx/html/config.json
