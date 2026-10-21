# Development

## Requirements

- Go 1.26+
- Docker (integration tests start PostgreSQL and storage servers in containers)
- Node 22+ and pnpm (web UI, JS contract tests, desktop end-to-end tests)

## Layout

```
cmd/gotero          the binary
internal/           server code (not importable)
  zapi              Zotero Web API v3 handlers
  adminapi          JSON API for the web UI
  accounts, auth    users, passwords, API keys, permissions
  files, jobs       file sync storage side, maintenance jobs
  stream            streaming WebSocket
  repo, repo/pg     persistence interfaces and the PostgreSQL implementation
  server            wiring
web/                admin UI (React 19, Vite, shadcn/ui)
zotero/             library: Zotero data model and schema
zoteroclient/       library: Web API client
storage/            library: blob storage backends
conformance/        library: protocol test kit
e2e/contract/       Zotero's own sync client code run against gotero
e2e/desktop/        real Zotero desktop builds driven against gotero
deploy/             Dockerfile and Compose files
docs/dev/           protocol notes and the admin API contract
```

The libraries are separate Go modules tied together by `go.work`, and gotero uses them like any other consumer. Within the repository they resolve locally through `go.work`. The `v0.0.0` requirements are placeholders until the first tags.

## Building and running

```sh
make build                                   # web UI + static binary in bin/
cd web && pnpm dev                           # UI with mocked API on :5173
```

### Local dev stack

`compose.yaml` in the repository root builds gotero from your working tree and runs it with PostgreSQL. It needs no `.env`.

```sh
make dev                  # build and start on http://localhost:8080 (admin / gotero-admin)
make dev-down             # stop, keep data
make dev-reset            # stop and delete all dev data
```

Run `make dev` again after code changes; it rebuilds the image. Without make:

```sh
docker compose up --build
```

Useful while developing:

- **Web UI with hot reload against the real server:** `cd web && VITE_API_URL=http://localhost:8080 pnpm dev`, then open http://localhost:5173.
- **A Zotero profile pointed at it:** set `extensions.zotero.api.url` to `http://localhost:8080/api/` and `extensions.zotero.streaming.url` to `ws://localhost:8080/stream`.
- **The database:** `psql postgres://gotero:gotero@localhost:5433/gotero`.
- **Logs:** `docker compose logs -f gotero`. They are text format at debug level.
- **Conformance kit against it:** `GOTERO_CONFORMANCE_URL=http://localhost:8080/api/ GOTERO_CONFORMANCE_ADMIN_USER=admin GOTERO_CONFORMANCE_ADMIN_PASS=gotero-admin go test ./conformance/gotero -run TestConformance`.
- **Different ports:** set `GOTERO_DEV_PORT` and `GOTERO_DEV_PG_PORT`.

The dev stack is a separate Compose project (`gotero-dev`) with its own volumes, so it does not touch a production-style stack started from `deploy/compose.yaml`. It uses a fixed secret and admin password and allows open registration, so never expose it.

## Tests

There are several layers of tests:

| Layer | Command | What it proves |
|---|---|---|
| Unit and integration | `make test` | Every package, against real PostgreSQL, S3 (versitygw) and WebDAV containers, with the race detector |
| Fuzzing | `scripts/fuzz.sh 30s` | Parsers and protocol messages never panic or misbehave on hostile input |
| Conformance kit | see below | gotero answers every protocol request the way Zotero expects: 104 tests across auth, groups, writes, reads, deletions, settings, tags, full text, files, streaming, login sessions and known quirks |
| JS contracts | `e2e/contract/run.sh` | Zotero's own sync client code (fetched at a pinned release, never committed) accepts gotero's responses |
| Desktop end-to-end | `e2e/desktop/run.sh` | Real Zotero desktop builds sync with each other through gotero, including conflicts, files and groups |

Run the conformance kit against any running server:

```sh
GOTERO_CONFORMANCE_URL=http://localhost:8080/api/ \
GOTERO_CONFORMANCE_STREAM_URL=ws://localhost:8080/stream \
GOTERO_CONFORMANCE_ADMIN_USER=admin GOTERO_CONFORMANCE_ADMIN_PASS=... \
  go test ./conformance/gotero -run TestConformance -v
```

The kit also runs in-process in `internal/zapi`'s tests, so `make test` covers it as well.

CI runs every layer: `ci.yml` (lint, tests, fuzzing, licenses, image build), `conformance.yml` (each storage backend), `contracts.yml` and `desktop-e2e.yml` (nightly, on tags, and on pull requests labelled `e2e-desktop`).

## Linting

```sh
make lint
```

golangci-lint v2.12 cannot read export data from Go 1.27, so the Makefile runs it with the Go 1.26 toolchain (`GOTOOLCHAIN=go1.26.5`).

## Clean-room rule

Zotero's client, its dataserver and other sync server implementations are AGPL-licensed. gotero is Apache-2.0 and written from the public API documentation and observed client behaviour. **Do not copy code from those projects.** The JS contract tests and desktop tests download Zotero at test time into gitignored `.cache` folders, and CI fails if anything from there gets committed.

## Releasing

1. Tag any changed libraries first, for example `git tag zotero/v0.2.0`. `release.yml` checks that each one builds on its own.
2. Update the `require` lines in the root `go.mod` to those versions.
3. Tag the server, for example `git tag v0.2.0`. CI publishes multi-arch images to `ghcr.io/robertjndw/gotero` and creates a GitHub release.
