# Desktop e2e: real Zotero clients against gotero

Runs official Zotero Linux builds headless in Docker, controls them through a small test add-on, and checks that two or more clients sync correctly through gotero. CI runs the current stable release, the previous major and the newest 7.0.x (legacy `POST /keys` login).

## How it works

- `versions.mjs` resolves versions. It follows zotero.org's `download/client/dl?channel=release&platform=linux-x86_64` redirect for the current stable and uses the `zotero/zotero` git tags (verified against the same `dl?...&version=X` endpoint) for older majors. Prints the CI matrix as JSON.
- `fetch-zotero.mjs` downloads a tarball into `.cache/` (gitignored, never committed) and unpacks it. It prefers the host's native arch (arm64 on Apple silicon) and falls back to x86_64.
- `Dockerfile` is a generic runner (Debian, GTK3, Xvfb, dbus, Node 22). Zotero is mounted read-only at `/zotero` instead of baked in: one image serves every version, CI can cache the tarballs, and no Zotero binaries get redistributed.
- `lib/profile.mjs` writes a throwaway profile: `user.js` with `api.url`, `streaming.url`, autosync off, no first-run or update prompts, a data dir inside the profile, the connector server on its own port, debug logging, and the add-on dropped into `extensions/` with unsigned add-ons allowed.
- `container/entrypoint.sh` starts Xvfb, dbus and Zotero (`-profile ... -no-remote -ZoteroDebugText`, log in `zotero.log`) plus `bridge.mjs`, which forwards a published port to Zotero's loopback-only connector server.
- `addon/` is the bootstrapped test add-on. It registers `POST /gotero-test/<command>` endpoints: `ping`, `setAPIKey`, `setLegacyLogin`, `setPref`, `libraries`, `createCollection`, `createItem(s)`, `editItem`, `trashItem`, `restoreItem`, `deleteItem`, `emptyTrash`, collection and tag commands, `importFileAttachment`, `modifyFile`, `sync`, `syncStatus`, `waitForStreamingSync`, `dump`, `setConflictResolution`, `conflictLog`, `eval`, `quit`.
- `lib/client.mjs` (one container per client) and `lib/harness.mjs` (`suite()` for scenarios) are the Node API. `lib/gotero.mjs` creates users, keys and groups through the admin API.

Conflict dialogs are stubbed: `setConflictResolution('local'|'remote'|'cancel')` replaces the merge-window functions so the answer is deterministic. The real merge window is not exercised.

## Running locally

```sh
e2e/desktop/run.sh smoke        # no server needed
export GOTERO_URL=http://host.docker.internal:8080 GOTERO_ADMIN_USER=admin GOTERO_ADMIN_PASSWORD=...
e2e/desktop/run.sh scenarios    # all scenarios
ZOTERO_VERSION=9.0.6 e2e/desktop/run.sh scenarios scenarios/02-edits-and-conflicts.test.mjs
```

Without a reachable server, scenarios skip with a message.

Environment: `GOTERO_URL`, `GOTERO_ADMIN_USER`, `GOTERO_ADMIN_PASSWORD`, optional `GOTERO_CLIENT_URL` (server as seen from containers), `GOTERO_PUBLIC_URL` (Origin sent on admin writes), `GOTERO_LOG_CMD` / `GOTERO_LOG_PATH` (server logs collected on failure), `ZOTERO_VERSION`, `E2E_ARCH=amd64|arm64`, `E2E_ARTIFACTS_DIR`, `E2E_START_TIMEOUT_MS`.

The server's public URL has to be a name the containers can also resolve, because admin writes check `Origin` against it. Using `host.docker.internal` for both sides works (add `127.0.0.1 host.docker.internal` to `/etc/hosts` on Linux).

## Adding a scenario

Create `scenarios/NN-name.test.mjs`:

```js
import { suite, assert } from '../lib/harness.mjs';
suite('my scenario', { clients: 2 }, (s) => {
  s.test('does a thing', async ({ A, B }) => {
    const key = await A.createItem({ itemType: 'book', fields: { title: 'x' } });
    await A.syncOk();
    await B.syncOk();
    // compare with (await B.dump()).libraries ...
  });
});
```

Need a new remote command? Add an entry to `commands` in `addon/bootstrap.js` and a wrapper in `lib/client.mjs` (or use `call('eval', ...)` for one-offs). Failed tests save Zotero logs, state dumps and server logs under `.cache/artifacts/<version>/...`.

## Troubleshooting

- No ping: read `.cache/run/<id>/<client>/zotero.log`. The add-on logs `[gotero-e2e] test driver ready`.
- `Invalid Host header` / 400 from the bridge: the bridge rewrites Host, so check the container is up (`docker ps`).
- Native runs on Apple silicon are fast. `E2E_ARCH=amd64` runs under emulation (startup still takes a few seconds in our tests, but expect slower syncs).
- Sync hangs with a dialog: a modal prompt is open in the container. Add a stub for it in the add-on.
- Orphan containers: `docker ps -a --filter name=gotero-e2e-`.
- 10.x checks local file edits via its file watcher; `modifyFile` triggers a rescan explicitly.
