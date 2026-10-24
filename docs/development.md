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
| Conformance kit | see below | gotero answers every protocol request the way Zotero expects: 128 tests across auth, groups, writes, reads, deletions, settings, tags, full text, files, streaming, login sessions and known quirks |
| Multi-client simulation | part of `make test` | Several simulated Zotero clients edit one library concurrently, over a network that loses requests and responses and through server crashes, and still converge without losing an acknowledged write (see below) |
| Operations | `scripts/ops-test.sh [OLD_REF]` | With the real images: clients sync through an upgrade from `OLD_REF` and repeated `docker kill`s, the conformance kit passes on the upgraded data, and a backup made as [install](install.md#backups) describes restores into a server every client accepts unchanged |
| Load | `GOTERO_LOAD=1 go test -run TestLoad -v -timeout 60m ./internal/server` | Seeds a 55,000-item library, times a new device's full sync, then 50 clients syncing and editing at once, and fails on any error. `GOTERO_LOAD_ITEMS`, `_CLIENTS` and `_SECONDS` resize it |
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

### Multi-client simulation

`conformance/syncsim` simulates Zotero desktop clients, each with its own local database, syncing one library the way the real client does. That covers upload-then-download with `If-Unmodified-Since-Version`, three-way merges, delete conflicts, full syncs after a per-object 412, ZFS file sync and stream-triggered syncs. The client rules it follows are listed in [protocol notes](dev/protocol-notes.md#how-the-desktop-client-syncs). After the random workload, it checks that:

- every client agrees with the server, objects, settings and file bytes alike;
- no acknowledged write was lost or changed at the same version;
- versions only move forward and no two writes share one;
- the server has no dangling parents, collection cycles or objects both alive and deleted, and incremental listings match the full one;
- every client heard the final version on the stream.

`TestMultiClientSync` in `internal/server` runs it against the full application, stream hub included, with one seed per scenario. Scenarios cover several devices of one user, group members, real concurrency, lost requests and responses, heavy contention, and server crashes: the `crash` scenarios cut every connection, close the database pool mid-transaction and start a fresh instance on the same database and files. Stepped scenarios interleave deterministically, so their failures replay exactly.

Where Zotero itself leaves a client out of step, which would happen against zotero.org too, the simulation repairs that client's state and says so in its trace, so the gap does not hide server bugs. The gaps are listed in [protocol notes](dev/protocol-notes.md#how-the-desktop-client-syncs).

```sh
go test ./internal/server -run TestMultiClientSync                                    # as in make test
SYNCSIM_RUNS=50 go test -race ./internal/server -run TestMultiClientSync               # soak
SYNCSIM_SEED=1007 go test ./internal/server -run 'TestMultiClientSync/stepped/devices'  # replay a failure
SYNCSIM_ARTIFACTS=/tmp/sim go test ./internal/server -run TestMultiClientSync           # save client traces of failures
```

A failure prints its seed, every inconsistency and each client's recent requests and decisions. `SYNCSIM_STEPS` changes the workload length. The same simulation runs against the Docker image in `conformance.yml` (`TestSyncSim` in `conformance/gotero`), and `syncsim-soak.yml` runs 50 fresh seeds per scenario plus `scripts/ops-test.sh` nightly.

CI runs every layer: `ci.yml` (lint, tests, fuzzing, licenses, image build), `conformance.yml` (each storage backend, conformance kit and simulation), `syncsim-soak.yml` (nightly soak and operations test), `contracts.yml` and `desktop-e2e.yml` (nightly, on tags, and on pull requests labelled `e2e-desktop`).

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
