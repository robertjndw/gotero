# Client JS contract tests

These tests run Zotero desktop's own sync client code (unmodified) against a
running gotero, using Node 22 and no npm dependencies. If the real client
accepts gotero's headers, status codes and JSON shapes, a real Zotero will too.

This is different from `conformance/` (an independent Go kit that checks the
protocol from the outside). Here the oracle is Zotero's actual JavaScript.

## How it works
- `fetch.mjs` downloads `xpcom/sync/syncAPIClient.js` and `xpcom/storage/zfs.js`
  from `raw.githubusercontent.com` at the tags pinned in `zotero-ref.json`, and
  checks each against a recorded sha256 (so a moved tag is noticed).
- `shim/` is our own minimal `Zotero` global (HTTP on top of `fetch`, prefs,
  debug, a tiny fake item/file layer). The fetched files are loaded into it with
  `node:vm`.
- `tests/` drive the real `Zotero.Sync.APIClient` and the real
  `Zotero.Sync.Storage.Mode.ZFS.uploadFile` / `downloadFile`, and assert on what
  the sync engine would receive.
- Test users and API keys are created per test file through the admin API and
  deleted afterwards.
- `tests/shim-selftest.test.mjs` runs the real ZFS code against a small in-process
  fake, so a shim bug can be told apart from a server bug.
- `tests/guard.test.mjs` fails if git tracks anything under `.cache/`.

## Why the Zotero files are only fetched
Zotero's client is AGPL-3.0; gotero is Apache-2.0 and a clean-room
implementation. The Zotero files therefore live only in the gitignored
`e2e/contract/.cache/`, are downloaded at test time, and must never be committed,
copied or adapted into this repo. Everything under `shim/` and `tests/` is our
own code. The guard test enforces the "never tracked" part.

## Running locally
```sh
export GOTERO_URL=http://localhost:8080          # default
export GOTERO_ADMIN_USER=admin                   # default
export GOTERO_ADMIN_PASSWORD=admin-dev-password  # default
e2e/contract/run.sh                  # every pinned ref
ZOTERO_REF=10.0.6 e2e/contract/run.sh   # one ref
```
Without npm: `cd e2e/contract && node fetch.mjs && node --test "tests/*.test.mjs"`.
If the server is unreachable the server-backed tests are skipped with a message.

Other switches: `ZOTERO_DEBUG=1` prints the client's debug log;
`GOTERO_ZFS_TODO=1` reports ZFS failures as `todo` for servers without file sync.

## Bumping the pinned Zotero version
1. Pick the new tag (`gh api repos/zotero/zotero/tags`).
2. `node fetch.mjs --hash <tag>` prints the sha256 of both files; review the
   upstream diff of those files first.
3. Add or replace the entry in `zotero-ref.json` (current stable plus, optionally,
   the previous major) and update the workflow matrix in
   `.github/workflows/contracts.yml`.
4. Run `run.sh`. If the client started needing a new global, extend `shim/`.
