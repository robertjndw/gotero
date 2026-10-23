# gotero

A self-hosted sync server for Zotero, written in Go.

Point your Zotero desktop app at gotero instead of zotero.org and it syncs your library, your groups and your attachment files with a server you control. The client needs no patch or plugin, only two changed preferences.

- Works with unmodified Zotero desktop. CI tests against real Zotero builds (current and previous major release).
- Attachments go to a local directory, any S3-compatible service or a WebDAV server.
- Multiple users and shared group libraries with owner, admin and member roles. An admin adds users directly or sends invite links.
- Sign-in with passwords, OpenID Connect or SAML, with optional automatic account creation per provider.
- Web UI for managing users, groups, invites, quotas and API keys, and for approving Zotero logins.
- Supports Zotero's streaming endpoint, so other devices hear about changes as they happen.
- Can import an existing library from zotero.org, including its files.
- Ships as a single static binary; the only other dependency is PostgreSQL.

> gotero is an independent project and is not affiliated with or endorsed by the Zotero project. "Zotero" is a trademark of the Corporation for Digital Scholarship.

## Quick start (Docker Compose)

```sh
git clone https://github.com/robertjndw/gotero && cd gotero
cp deploy/.env.example deploy/.env
# Edit deploy/.env: set GOTERO_SECRET (openssl rand -hex 32) and GOTERO_ADMIN_PASSWORD
docker compose -f deploy/compose.yaml up -d
```

Open http://localhost:8080, sign in as `admin`, and follow **Connect your Zotero** on the dashboard.

For anything beyond trying it out, put gotero behind HTTPS (see [Installation](docs/install.md)). Zotero needs a trusted certificate for `https` URLs.

## Connect Zotero

1. In Zotero open **Settings → Advanced → Config Editor**.
2. Set `extensions.zotero.api.url` to `https://your-server/api/` (keep the trailing slash).
3. Set `extensions.zotero.streaming.url` to `wss://your-server/stream`.
4. Restart Zotero and sign in under **Settings → Sync** with your gotero username and password.

Details, mobile apps and troubleshooting are in [Connecting clients](docs/clients.md).

## Documentation

| | |
|---|---|
| [Installation](docs/install.md) | Docker, Compose, reverse proxy, upgrades, backups |
| [Configuration](docs/configuration.md) | Every environment variable |
| [Connecting clients](docs/clients.md) | Zotero desktop setup and troubleshooting |
| [File storage](docs/storage.md) | Local, S3 and WebDAV backends, quotas |
| [Importing from zotero.org](docs/import.md) | Moving an existing library |
| [Operations](docs/operations.md) | CLI, health checks, logs, garbage collection |
| [Development](docs/development.md) | Building, the test suites, project layout |

## Reusable Go libraries

gotero is built from libraries that other projects can use on their own:

| Module | What it is |
|---|---|
| [`zotero`](zotero/) | The Zotero data model: keys, schema tables, ordered JSON, patch merging, stream protocol types |
| [`zoteroclient`](zoteroclient/) | A typed Zotero Web API v3 client with retries, backoff and streaming file sync |
| [`storage`](storage/) | A streaming blob storage interface with local, S3 and WebDAV backends, plus a conformance suite |
| [`conformance`](conformance/) | A protocol test kit that checks any Zotero-compatible sync server |

## License

[Apache License 2.0](LICENSE).
