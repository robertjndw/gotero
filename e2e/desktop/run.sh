#!/usr/bin/env bash
# One command to run the desktop e2e suite locally.
#
#   e2e/desktop/run.sh smoke                  # no server needed: launch Zotero, drive it, fail a sync on purpose
#   e2e/desktop/run.sh scenarios [files...]   # needs GOTERO_URL, GOTERO_ADMIN_USER, GOTERO_ADMIN_PASSWORD
#   ZOTERO_VERSION=9.0.6 e2e/desktop/run.sh scenarios
#
# ZOTERO_VERSION defaults to the current stable release. Docker must be running.
set -euo pipefail
cd "$(dirname "$0")"

mode="${1:-smoke}"
[ $# -gt 0 ] && shift

: "${ZOTERO_VERSION:=$(node versions.mjs --versions-only | node -pe 'JSON.parse(require("fs").readFileSync(0))[0]')}"
export ZOTERO_VERSION
info="$(node fetch-zotero.mjs "$ZOTERO_VERSION")"
arch="$(printf '%s' "$info" | node -pe 'JSON.parse(require("fs").readFileSync(0)).arch')"

echo "Zotero $ZOTERO_VERSION ($arch)" >&2
docker build -q --platform "linux/$arch" -t "gotero-e2e-runner:$arch" . >/dev/null

case "$mode" in
  smoke)     targets=(smoke/) ;;
  scenarios) targets=("${@:-scenarios/}") ;;
  *) echo "usage: run.sh smoke|scenarios [files]" >&2; exit 2 ;;
esac
# Clients share one Docker host and one server, so keep suites sequential
exec node --test --test-concurrency=1 ${NODE_TEST_ARGS:-} "${targets[@]}"
