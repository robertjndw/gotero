# Operations

## Command line

The `gotero` binary also handles administration. In Docker, run it inside the container:

```sh
docker compose -f deploy/compose.yaml exec gotero gotero user list
```

| Command | What it does |
|---|---|
| `gotero serve` | Run the server. Migrations are applied first. |
| `gotero migrate` / `gotero migrate status` | Apply migrations, or list them. |
| `gotero user add <name> --password-stdin [--email E] [--admin] [--id N]` | Create a user. `--id` keeps a zotero.org user ID (see [Importing](import.md)). |
| `gotero user list` | List users. |
| `gotero user passwd <name> --password-stdin` | Set a password. |
| `gotero user disable <name>` / `enable <name>` | Block or unblock sign-in and sync. |
| `gotero user delete <name> --yes` | Delete a user with their library and files. |
| `gotero invite create [--admin] [--email E] [--hours 72]` | Print an invite link. |
| `gotero import --user <name> --api-key-stdin [--no-files] [--no-groups]` | Import a library from zotero.org. |
| `gotero healthcheck` | Exit 0 if the local server is healthy. |
| `gotero version` | Print the version. |

To pass a password without it showing up in your shell history:

```sh
printf '%s' 'the password' | docker compose -f deploy/compose.yaml exec -T gotero gotero user add alice --password-stdin
```

The web UI does everything except `serve` and `migrate`.

## Health and monitoring

- `GET /healthz` returns `200 {"status":"ok"}` when the database is reachable, and `503` when it isn't. The container health check uses it.
- Logs are JSON lines on stderr, one per request plus events such as migrations and garbage collection. Request logs carry a `request_id`, which is also sent back to the client in the `X-Request-Id` header. API keys and tokens never appear in logs; they are redacted from logged paths.
- Set `GOTERO_LOG_FORMAT=text` for human-readable logs and `GOTERO_LOG_LEVEL=debug` for more detail.

## Background maintenance

gotero runs these jobs by itself:

| Job | When | What |
|---|---|---|
| Prune | every ~10 min | Removes expired logins, sessions, upload tokens and invites, plus delete records older than `GOTERO_DELETED_LOG_RETENTION` (off by default, so delete records are kept forever; see [configuration](configuration.md)) |
| Garbage collection | every ~6 h | Deletes files nothing references any more (after a 24 h grace period) and abandoned partial uploads |

## Performance notes

- Writes to one library are processed one after another. Different libraries, and all reads, run in parallel.
- Change lists (`format=versions`) are streamed from an index; 100,000 items take about 15 ms.
- File uploads and downloads stream with constant memory. Uploads are verified against their MD5 checksum before they are stored.
- A key revocation or user deactivation takes effect immediately in the web UI. When it is done with the CLI, it can take up to 30 seconds, because the running server caches keys briefly.

## Security

- Passwords are hashed with Argon2id, and API keys, session IDs and invite tokens are stored only as hashes.
- Admin UI sessions use HttpOnly, SameSite cookies with CSRF tokens and an Origin check.
- Password logins (web UI and Zotero's older username/password sign-in) share one limit: 10 attempts per username and address, and 50 per address, every 15 minutes. Someone guessing from elsewhere cannot lock you out. Behind a reverse proxy, set `GOTERO_TRUSTED_PROXIES` so the limits see real client addresses.
- Request bodies must arrive promptly, and stalled uploads are cut off after a minute without progress.
- File download links are signed, tied to the exact file version, and expire after 5 minutes.
- Run gotero behind HTTPS, and set `GOTERO_PUBLIC_URL` so that generated links use the right address.
