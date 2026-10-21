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

## Known deviations

List tests in `Config.Skip` as `name` or `suite/name`, with a comment saying why:

```go
Skip: []string{
    "auth/APIVersion2Rejected", // we serve v2 clients too
},
```
