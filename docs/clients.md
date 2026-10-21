# Connecting clients

## Zotero desktop

Zotero can talk to any server that speaks its Web API, and you change the address in its hidden preferences. You don't need a plugin or a patched build.

1. Open **Settings** (on macOS, **Zotero → Settings**), go to **Advanced** and click **Config Editor**. Accept the warning.
2. Search for `extensions.zotero.api.url` and set it to your server's API URL **with the trailing slash**:
   ```
   https://zotero.example.org/api/
   ```
3. Search for `extensions.zotero.streaming.url` and set it to:
   ```
   wss://zotero.example.org/stream
   ```
   If you leave this preference unset, Zotero sends your gotero key to zotero.org's streaming server. To switch streaming off instead, set `extensions.zotero.streaming.enabled` to `false`.
4. Restart Zotero.
5. Open **Settings → Sync** and sign in:
   - **Newer Zotero versions** open a browser window. Sign in to gotero there and click **Approve**.
   - **Zotero 7** asks for a username and password directly. Use your gotero credentials.
6. File syncing uses **Zotero storage** (the default). Leave it as it is; your files go to gotero.

The web UI's dashboard shows both URLs with copy buttons.

### Moving from zotero.org

If this Zotero installation synced with zotero.org before, your local library still carries zotero.org's library versions and your zotero.org user ID.

- **Smoothest path:** have an admin create your gotero account with your zotero.org user ID (`gotero user add yourname --id 1234567`), then [import your library](import.md) before connecting. Zotero then sees the same user and continues normally.
- **Otherwise:** Zotero notices it is talking to a different account and offers to reset local data. Choose to keep your local data, and Zotero uploads everything to gotero.

Make a backup of your Zotero data directory before switching either way.

## Troubleshooting

| Symptom | Fix |
|---|---|
| "The Zotero sync server did not accept your username and password" | Check that `api.url` ends with `/api/` and that you can log in to the web UI with the same credentials. |
| Sync stays on "Connecting" or fails with a certificate error | Zotero needs a certificate it trusts for `https`. Self-signed certificates do not work; use Let's Encrypt or an internal CA that is installed on the client machine. |
| Files don't sync | **Settings → Sync → File Syncing** must use **Zotero**, not WebDAV. Check your quota on the dashboard. |
| Changes from another device take minutes to arrive | `extensions.zotero.streaming.url` is unset or blocked by a proxy. Your reverse proxy must allow WebSocket upgrades on `/stream`. |
| Upload or download links point to `http://` or the wrong host | Set `GOTERO_PUBLIC_URL` to the address clients use (see [Configuration](configuration.md)). |
| "Zotero must be updated" after syncing | A newer client added item fields that older clients don't know. Update every Zotero installation that shares the library. |

For detail, turn on **Help → Debug Output Logging → Enable** in Zotero, reproduce the problem, and look at the requests to your server. Server logs are in `docker compose logs gotero`.

## Mobile apps

The official iOS and Android apps have no setting for a custom server, so they can't sync with gotero.
