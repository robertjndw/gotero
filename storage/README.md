# gotero/storage

A small, streaming blob store abstraction (`storage.Backend`) with three
backends, plus a conformance suite for writing your own. gotero uses it to keep
attachment files, content-addressed by md5, but nothing in here is specific to
Zotero.

```
github.com/robertjndw/gotero/storage            interfaces, ErrNotFound, key validation
github.com/robertjndw/gotero/storage/local      files below a directory
github.com/robertjndw/gotero/storage/s3         S3 and compatible (minio-go)
github.com/robertjndw/gotero/storage/webdav     hand-written minimal WebDAV client
github.com/robertjndw/gotero/storage/storagetest  conformance suite for any Backend
```

## Model

Uploads never hold the whole object in memory. `Create` starts an upload into a
staging area (`.staging/` below the root or prefix), you `Write` to it, and
`Commit(ctx, key)` makes it visible under its final key. If the key already
exists, Commit succeeds and discards the staged copy, so content-addressed
callers can upload concurrently without coordination. `Abort` throws the upload
away and is a no-op after Commit, so `defer up.Abort(ctx)` is always safe.

Keys are slash separated and validated by `storage.ValidateKey`: no empty,
absolute, `.` or `..` segments, no backslashes, nothing starting with `.staging`.
Invalid keys fail with `storage.ErrInvalidKey`.

```go
b, err := local.New("/var/lib/gotero/files")
if err != nil { ... }

up, err := b.Create(ctx)
if err != nil { ... }
defer up.Abort(ctx)
if _, err := io.Copy(up, src); err != nil { ... }
if err := up.Commit(ctx, "ab/abcdef0123456789"); err != nil { ... }

r, info, err := b.Open(ctx, "ab/abcdef0123456789")
if errors.Is(err, storage.ErrNotFound) { ... }
defer r.Close()
http.ServeContent(w, req, "", info.ModTime, r.(io.ReadSeeker)) // all bundled readers seek
```

Run `SweepStaging(ctx, 24*time.Hour)` periodically to remove uploads left behind
by crashed processes.

The upload context passed to `Create` drives the S3 and WebDAV transfers:
cancelling it aborts the upload, so give it a lifetime that covers the whole
upload (a request context is fine).

## Backends

### local

`local.New(root, opts...)`. Staging files live in `<root>/.staging/` with random
names. Commit fsyncs the file, hard-links it into place (atomic "first writer
wins"), fsyncs the parent directory and removes the staging name. Files are
0640, directories 0750. `Open` returns an `*os.File`.

### s3

```go
b, err := s3.New(ctx, s3.Config{
    Endpoint: "s3.example.com", Bucket: "gotero", Prefix: "files",
    AccessKey: "...", SecretKey: "...", UseSSL: true,
    PartSize: 16 << 20, CreateBucket: false,
})
```

Uploads stream through a pipe into a multipart `PutObject` of unknown size at
`<prefix>/.staging/<random>`; Commit server-side copies it to the final key
(`CopyObject`, or `ComposeObject` above 5 GiB) and deletes the staging object.

**Part size and memory.** Peak memory per in-flight upload is about one part:
`PartSize` (default 16 MiB, minimum 5 MiB). Budget `PartSize x concurrent
uploads`. The largest object is `PartSize x 10000` (160 GB by default). A
256 MiB upload moved the Go heap by about 32 MiB in the test suite.

Notes: the copy-if-absent step is a Stat followed by a copy, so two racing
commits of the same key may both copy; with content-addressed keys they carry
identical bytes. Zero-byte objects are written with a plain PUT. S3 lifecycle
rules on `<prefix>/.staging/` and "abort incomplete multipart uploads" are a good
belt-and-braces addition to `SweepStaging`.

### webdav

```go
b, err := webdav.New(webdav.Config{URL: "https://dav.example.com/gotero/", Username: "u", Password: "p"})
```

Uses PUT, GET, DELETE, MKCOL, MOVE (with `Overwrite: F`, where 412 means "already
there") and PROPFIND (`Depth: 1`, recursive for `Walk`). Parent collections are
created on demand. `Open` returns a reader that seeks via `Range` requests.

**Chunked PUT compatibility.** By default uploads stream with chunked transfer
encoding (no Content-Length). Most servers accept this (Apache mod_dav, rclone,
recent nginx dav), but some proxies and appliances answer `411 Length Required`
or buffer the whole body. For those set `Config.StageLocally: true`: the upload
is written to a temp file (`Config.TempDir`) and sent with a Content-Length on
Commit. Trade-off: you need local disk equal to the object size, and nothing
reaches the server until Commit.

`SweepStaging` relies on the server's `getlastmodified`, so mind clock skew.
Servers built on `x/net/webdav` may answer 423 Locked briefly under concurrency;
the client retries those.

## Testing your own backend

```go
func TestMyBackend(t *testing.T) {
    storagetest.Run(t, func(t *testing.T) storage.Backend {
        return newMyBackendInEmptyNamespace(t) // fresh and isolated per call
    })
}
```

`storagetest.RunWithOptions` takes `Options` for servers with quirks (for example
`SweepMayMissAbandoned` when in-flight uploads cannot be listed). The large
streaming test is skipped with `-short`. `storagetest.HeapGrowth` is a helper to
assert bounded memory for your own uploads.

The bundled S3 and WebDAV tests start `rclone/rclone:1.72` with testcontainers and
skip themselves under `-short` or when Docker is unavailable.

```
go test -short ./...        # no Docker, no large objects
go test -race ./...         # everything
go test -fuzz FuzzParseMultistatus -fuzztime 10s ./webdav
go test -fuzz FuzzValidateKey -fuzztime 10s .
```
