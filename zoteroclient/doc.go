// Package zoteroclient is a typed client for the Zotero Web API v3. It works
// against api.zotero.org and against self-hosted servers that speak the same
// protocol, such as gotero.
//
// # Basics
//
// Create a Client with an API key, then pick a library:
//
//	client := zoteroclient.New(key)
//	lib := client.User(12345) // or client.Group(678)
//
// Every request carries Zotero-API-Version and Zotero-API-Key. To talk to a
// self-hosted server use WithBaseURL("https://host/api/").
//
// # Versions
//
// Methods that read or write a library return the library's
// Last-Modified-Version as an int64 next to the result: (result, version,
// error). The version is also returned when the call fails with
// ErrNotModified or an *APIError that carried the header, so a 412 tells you
// which version to resync from.
//
// # Syncing
//
// A pull sync asks for versions, fetches what changed, then asks what was
// deleted:
//
//	since := stored // library version from the last run
//	versions, ver, err := lib.Versions(ctx, zotero.TypeItem,
//		zoteroclient.VersionsQuery{Since: since, IncludeTrashed: true})
//	if zoteroclient.IsNotModified(err) {
//		return nil // nothing to do
//	}
//	var keys []string
//	for k := range versions { keys = append(keys, k) }
//	objs, _, err := lib.Objects(ctx, zotero.TypeItem, keys)
//	deleted, _, err := lib.Deleted(ctx, since)
//
// # Rate limiting
//
// A Backoff header pauses every later request of the Client until its
// deadline. 429 and 503 responses are retried after Retry-After (seconds or
// an HTTP date), with jittered exponential delays when the header is
// missing, up to WithMaxRetries attempts. Server errors are only retried for
// idempotent requests. All waits stop when the context ends.
//
// # Errors
//
// Failed requests return *APIError. Use IsPreconditionFailed, IsNotFound,
// IsForbidden, IsConflict and IsNotModified instead of comparing statuses.
//
// # Files
//
// UploadFile and DownloadFile stream file content and never hold a whole
// file in memory. See Library.UploadFile for the retry rules of the body.
//
// # Stability
//
// The module is v0. Names and signatures may change before v1.
package zoteroclient
