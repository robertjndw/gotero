# zoteroclient

A typed Go client for the [Zotero Web API v3](https://www.zotero.org/support/dev/web_api/v3/basics).
It works against api.zotero.org and against self-hosted servers such as
[gotero](https://github.com/robertjndw/gotero). Apache-2.0, standard library
plus the `zotero` module only.

```
go get github.com/robertjndw/gotero/zoteroclient
```

## Quick start

```go
client := zoteroclient.New(apiKey)                       // api.zotero.org
// client := zoteroclient.New(apiKey, zoteroclient.WithBaseURL("https://zotero.example.com/api/"))
lib := client.User(12345)                                // or client.Group(678)

info, err := client.CurrentKey(ctx)
```

Options: `WithBaseURL`, `WithHTTPClient`, `WithUserAgent`, `WithSchemaVersion`,
`WithMaxRetries`, `WithLogger`, `WithCompression`, `WithClock`.

## Results and versions

Library methods return `(result, libraryVersion, error)`. `libraryVersion` is
the `Last-Modified-Version` header; it is also filled in alongside
`ErrNotModified` and, where the server sent it, alongside an `*APIError`.

## Sync loop sketch

```go
versions, ver, err := lib.Versions(ctx, zotero.TypeItem,
	zoteroclient.VersionsQuery{Since: since, IncludeTrashed: true})
if zoteroclient.IsNotModified(err) {
	return // up to date
}
keys := slices.Collect(maps.Keys(versions))
objs, _, err := lib.Objects(ctx, zotero.TypeItem, keys) // 100 keys per request
if errors.Is(err, zoteroclient.ErrLibraryChanged) {
	// the library moved mid-download: start over
}
deleted, _, err := lib.Deleted(ctx, since)
if zoteroclient.IsConflict(err) {
	// delete log too old: do a full sync
}
// store ver as the new "since"
```

Writes are split into batches of 50 and the response maps use indexes into
your slice:

```go
resp, ver, err := lib.Write(ctx, zotero.TypeItem, objs,
	zoteroclient.WriteOptions{IfUnmodifiedSinceVersion: &since})
if zoteroclient.IsPreconditionFailed(err) {
	// pull first, then retry
}
```

## Files

```go
res, err := lib.UploadFile(ctx, itemKey, zoteroclient.FileInfo{
	MD5: md5hex, Filename: "paper.pdf", Size: st.Size(), MtimeMS: st.ModTime().UnixMilli(),
	Body: f, // *os.File: streamed, and replayable after 429/503
}, "") // "" = first upload, otherwise the MD5 of the stored file

d, err := lib.DownloadFile(ctx, itemKey)
defer d.Body.Close()
io.Copy(dst, d.Body)
```

Downloads follow the API's redirect without the API key, because the target
may be another host.

## Rate limiting and retries

- A `Backoff` header pauses every later request of the client, across goroutines.
- 429 and 503 are retried after `Retry-After` (seconds or HTTP date), or after
  a jittered exponential delay when the header is missing, up to
  `WithMaxRetries` times (default 5). Waits over five minutes are not taken.
- 5xx and network errors are retried only for idempotent requests: GET, HEAD,
  PUT and DELETE with a precondition. A POST is never replayed after a 5xx.
- Every wait honors the context.

## Errors

`*APIError` carries `StatusCode`, `Message` (the body) and `Header`. Use
`IsPreconditionFailed`, `IsNotFound`, `IsForbidden`, `IsConflict`,
`IsNotModified`. `Do` is a raw escape hatch for endpoints not covered.

## Stability

v0: names and signatures may change before v1. Until the `zotero` module is
tagged, building outside the repository workspace (`GOWORK=off`) fails to
resolve it.
