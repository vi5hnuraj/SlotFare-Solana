#!/usr/bin/env bash
# Run an Angular CLI command, in the main checkout or in a task worktree.
#
# node_modules is gitignored and a git worktree materialises tracked files
# only, so a fresh worktree has no dependencies at all and node_modules/.bin/ng
# does not exist. Worse, a task that adds a dependency writes it to
# package.json while the linked node_modules still lacks it, which is how
# EP-007-T02 failed twice on 'Could not resolve "viem"'.
#
# Link what the main checkout has, then reconcile against package.json.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -e node_modules ]; then
  common=$(git rev-parse --git-common-dir)
  case "$common" in /*) ;; *) common="$PWD/$common" ;; esac
  main_root=$(cd "$(dirname "$common")" && pwd)
  if [ -d "$main_root/node_modules" ] && [ "$main_root" != "$PWD" ]; then
    ln -s "$main_root/node_modules" node_modules
  fi
fi

# Every dependency package.json declares must actually resolve. A missing one
# means this task, or an earlier one, added it without installing.
missing=$(node -e '
const fs=require("fs");
const pkg=JSON.parse(fs.readFileSync("package.json","utf8"));
const deps={...(pkg.dependencies||{}),...(pkg.devDependencies||{})};
const gone=Object.keys(deps).filter(d=>!fs.existsSync("node_modules/"+d));
process.stdout.write(gone.join(" "));
' 2>/dev/null || echo "node_modules")

if [ -n "$missing" ]; then
  echo "ng-gate: installing, missing: $missing"
  rm -f node_modules   # drop the symlink; this worktree needs its own tree
  npm ci --offline --no-audit --no-fund 2>/dev/null \
    || npm install --no-audit --no-fund
fi

exec node_modules/.bin/ng "$@"
