#!/usr/bin/env bash
# Runs the client JS contract tests against a live gotero.
#   GOTERO_URL            default http://localhost:8080
#   GOTERO_ADMIN_USER     default admin
#   GOTERO_ADMIN_PASSWORD default admin-dev-password
#   ZOTERO_REF            run a single pinned ref (default: every ref in zotero-ref.json)
#   GOTERO_ZFS_TODO=1     report ZFS failures as todo (for servers without file sync)
set -euo pipefail
cd "$(dirname "$0")"
node --version | grep -q '^v\(2[2-9]\|[3-9][0-9]\)' || { echo "Node 22 or newer is required" >&2; exit 1; }
node fetch.mjs ${ZOTERO_REF:-}
exec node --test --test-concurrency=1 "tests/*.test.mjs"
