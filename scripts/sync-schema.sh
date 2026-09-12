#!/usr/bin/env bash
# Copy the canonical schema in contracts/sqlite into each language package.
#
# Neither go:embed nor importlib.resources can reach outside its own package
# directory, so each library keeps a copy. contracts/sqlite remains the single
# source of truth and this script is the only thing allowed to write the copies.
#
#   scripts/sync-schema.sh          refresh the copies
#   scripts/sync-schema.sh --check  fail if a copy has drifted (used by CI)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$root/contracts/sqlite"
dests=("$root/go/store/schema" "$root/py/src/tradekit/store/schema")

if [[ "${1:-}" == "--check" ]]; then
    status=0
    for dest in "${dests[@]}"; do
        if ! diff -r -q "$src" "$dest" >/dev/null 2>&1; then
            echo "schema drift: $dest differs from contracts/sqlite" >&2
            diff -r "$src" "$dest" >&2 || true
            status=1
        fi
    done
    exit $status
fi

for dest in "${dests[@]}"; do
    mkdir -p "$dest"
    rm -f "$dest"/*.sql
    cp "$src"/*.sql "$dest"/
    echo "synced -> ${dest#"$root"/}"
done
