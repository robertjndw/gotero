# conformance

A protocol test kit for Zotero-compatible sync servers. It speaks raw HTTP (and WebSocket for the stream), so status codes, headers and byte-level JSON ordering are asserted exactly as a Zotero client would see them. It does not depend on any gotero server code, so any implementation can run it.

Every assertion traces to `docs/dev/protocol-notes.md` or the public [Zotero Web API v3 docs](https://www.zotero.org/support/dev/web_api/v3/).

## Running against gotero

Start a server with an admin account, then:

```sh
export GOTERO_CONFORMANCE_URL=http://localhost:8080/api/
export GOTERO_CONFORMANCE_STREAM_URL=ws://localhost:8080/stream   # optional
export GOTERO_CONFORMANCE_ADMIN_USER=admin
export GOTERO_CONFORMANCE_ADMIN_PASS=secret
go test ./gotero/ -run TestConformance -v
```

Without `GOTERO_CONFORMANCE_URL` the test skips, so `go test ./...` passes with no server.

Other variables: `GOTERO_CONFORMANCE_ADMIN_URL` (default `<origin>/admin/api`), `GOTERO_CONFORMANCE_SUITES` and `GOTERO_CONFORMANCE_SKIP` (comma separated).

Outside `go test`:

```sh
go run ./cmd/zconform -url http://localhost:8080/api/ -stream-url ws://localhost:8080/stream \
  -provisioner gotero -admin-user admin -admin-pass secret -run 'files/' -v
```

`-run` is a regexp matched per level (`suite/test`). `-provisioner static -users 'id:user:pass:key,...'` uses pre-made accounts. The command prints a pass/fail/skip summary and exits non-zero on failure.

Each test provisions its own user and runs in parallel, so the server needs to accept a few hundred accounts per run.

## Other servers

Implement `conformance.Provisioner` and call `conformance.Run`:

```go
func TestZoteroCompat(t *testing.T) {
    conformance.Run(t, conformance.Config{
        BaseURL:     "http://localhost:9000/api/",
        Provisioner: myProvisioner{},
    })
}
```

`NewUser` must return a user whose key has full access (user library, notes, files, write, all groups). `NewGroup` may return `conformance.ErrUnsupported`; tests needing it skip. Optional interfaces, each enabling extra tests when implemented:

| Interface | Enables |
|---|---|
| `MemberAdder` | group version bump on membership change |
| `KeyRevoker` | out of band key revocation |
| `QuotaSetter` | 413 on quota exceeded |
| `SessionApprover` | browser login session flow |

`StaticProvisioner` hands out pre-made users and skips everything needing more.

## Suites

`auth`, `groups`, `write`, `read`, `deleted`, `settings`, `tags`, `fulltext`, `files` (ZFS authorize, upload, register, download, 20 MiB streaming unless `-short`), `stream` (needs `StreamURL`), `sessions`, `quirks` (attachment field order, null md5/mtime, integer Retry-After, relation URIs).

## Multi-client simulation

`syncsim` runs several simulated Zotero clients against one library, concurrently and optionally over a lossy network, then checks that they converge and that nothing the server acknowledged went missing. Clients upload files with `If-Match` set to the md5 they last synced, as Zotero does, and every acknowledged upload must have replaced exactly that file, so a server that lets the last upload win fails even though its clients still converge. It uses the same `Provisioner`:

```go
rep, err := syncsim.Run(ctx, syncsim.Config{
    BaseURL: "http://localhost:9000/api/", StreamURL: "ws://localhost:9000/stream",
    Provisioner: myProvisioner{}, Users: 3, Devices: 2, Group: true,
    Concurrent: true, Files: true, FaultRate: 0.1, Seed: 1,
})
// rep.Failures is empty when the server passed.
```

`syncsim.Test(t, cfg)` does the same inside a test and reports failures with their seed. Against gotero: `go test ./gotero -run TestSyncSim` with the variables above.

Two hooks test operations. `Config.Restart` is called at random points to crash and restart the server, mid-request in concurrent runs; clients must recover. `Config.BeforeCheck` runs once everything has settled, and the checks then run against whatever server it leaves, such as one restored from a backup. Against a deployment, `TestSyncSimRestart` and `TestSyncSimBackupRestore` run a shell command for each (`GOTERO_SYNCSIM_RESTART_CMD`, `GOTERO_SYNCSIM_BACKUP_RESTORE_CMD`); `scripts/ops-test.sh` in the gotero repository shows both.

## Known deviations

List tests in `Config.Skip` as `name` or `suite/name`, with a comment saying why:

```go
Skip: []string{
    "auth/APIVersion2Rejected", // we serve v2 clients too
},
```
