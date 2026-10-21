#!/usr/bin/env bash
# Runs every Fuzz* target in every module for the given duration (default 30s).
set -euo pipefail
dur="${1:-30s}"
for mod in . zotero zoteroclient storage conformance; do
  (
    cd "$mod"
    for pkg in $(go list ./...); do
      for fn in $(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true); do
        echo "==> $pkg $fn"
        go test -run '^$' -fuzz "^${fn}\$" -fuzztime "$dur" "$pkg"
      done
    done
  )
done
