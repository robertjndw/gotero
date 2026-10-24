# Zotero sync protocol notes

These are the facts gotero relies on, gathered from the Zotero Web API v3 docs and from watching how Zotero desktop 7 and 10 behave.

gotero is a **clean-room** implementation. Don't copy code from the Zotero client, zotero/dataserver or altero, all of which are AGPL. Reading them to understand behavior is fine.

## Pointing a client at gotero
- `extensions.zotero.api.url = https://HOST/api/`, with a trailing slash. This one URL covers both data sync and file sync (ZFS).
- `extensions.zotero.streaming.url = wss://HOST/stream`.
  - If you leave it unset, the client sends gotero keys to `wss://stream.zotero.org`.
  - Alternatively, turn the stream off with `extensions.zotero.streaming.enabled = false`.
- Over https, the client needs a trusted TLS certificate. Plain http is fine for local testing and CI.

## Request headers sent by the client
- `Zotero-API-Version: 3` is sent on every request.
- The API key arrives in `Zotero-API-Key: <key>`. Other clients may use `Authorization: Bearer <key>` or `?key=` instead, so accept all three.
- `Zotero-Schema-Version: <n>` gives the version of the client's bundled schema.
- `Content-Encoding: gzip` appears on bodies of 1000 bytes or more. Decode them with limits on both the compressed and the decompressed size.
- `If-Unmodified-Since-Version: <v>` comes with writes, and `If-Modified-Since-Version: <v>` with `GET settings`.
- Sync never sends `Zotero-Write-Token`, but other API users do: 32 characters, remembered per library for 12 h. Replaying a token returns 412.

## Response rules
- **Every** library-scoped response carries `Last-Modified-Version: <library version>`, including 304, 412, 204 and file register. Without it the client throws an error.
- `If-Modified-Since-Version` at or above the current version returns 304.
- A library newer than `If-Unmodified-Since-Version` returns 412, and the body explains why.
  - A client claiming a version *ahead* of the server is accepted, but log it.
- `Backoff: <s>` makes the client pause.
- 429 and 503 carry an integer `Retry-After` of at least 1.
  - gotero answers 503 with a jittered `Retry-After` of 3-8 s when a library lock or a pool connection can't be had in time, and for transient database failures (deadlock, serialization failure, statement timeout, too many connections, server shutting down, lost connection).
- Only API version 3 is served. Any other `Zotero-API-Version` returns 400.

## Keys and IDs
- Object keys match `^[23456789ABCDEFGHIJKLMNPQRSTUVWXYZ]{8}$`.
- API keys are 24 alphanumeric characters (zotero.org uses `[A-Za-z0-9]{24}`).
- User and group IDs are positive integers. Once a profile has synced, its userID must not change, or the client offers to reset its local data.

## Endpoints the desktop calls (in sync order)
1. `GET keys/current` returns
   `{key, userID, username, displayName, access:{user:{library,files,notes,write}, groups:{all:{library,write}}}}`.
   - `groups.all` must be present.
   - 403 means the key is invalid.
   - `?includeEmails=1` adds `emails: []`.
   - `DELETE keys/current` revokes the key ("unlink").
2. `GET users/{id}/groups?format=versions` returns `{"<gid>": version}`.
   Then the client calls `GET groups/{gid}` for each group, which returns
   `{id, version, links, meta, data:{id, version, name, owner, type:"Private", description, url, libraryEditing:"members"|"admins", libraryReading:"all"|"members", fileEditing:"none"|"admins"|"members", admins:[uid...], members:[uid...]}}`.
   - `admins` and `members` must be arrays (possibly empty), or members end up syncing the library read-only.
   - Any membership change bumps the group's version.
3. For each library (`users/{id}` or `groups/{id}`), the client **uploads first**:
   - `POST settings` with `{"tagColors": {"value": [...], "version": N}}`, at most 270 settings per request, and `DELETE settings?settingKey=a,b`.
   - `POST collections`, `POST searches` and `POST items`, each a JSON array of at most 50 objects (the client sends 10).
   - `DELETE {collections|searches|items}?{collectionKey|searchKey|itemKey}=K1,K2`, with at most 50 keys.
   - `DELETE tags?tag=a || b`, where the separator is ` || `. A bare `DELETE tags` returns 204.
4. Then the client **downloads**:
   - `GET settings?since=V`, sent with `If-Modified-Since-Version: V`.
   - `GET collections?format=versions&since=V`, likewise for `searches`, then `items/top?format=versions&since=V&includeTrashed=1` and `items?format=versions&since=V&includeTrashed=1`.
     - These return `{"KEY": version}`. They are **not paginated** and have no default limit.
   - `GET {type}?{type}Key=K1,...&includeTrashed=1` with **up to 100** keys per request.
   - `GET deleted?since=V` returns `{collections:[], searches:[], items:[], tags:[], settings:[]}`.
     - If `since` is older than the oldest delete-log entry still kept, it returns 409.
       - **The client does not recover from this.** `getDeleted` returns `false`, the engine reads that as a library version change and retries with the same `since` until it gives up with "library in use". It never falls back to a full sync. zotero.org never sends this 409.
       - So gotero keeps the delete log forever by default (`GOTERO_DELETED_LOG_RETENTION=0`). The floor and the entries are read in one statement, so a prune can't slip between them.
   - If `Last-Modified-Version` changes partway through a download, the client starts over.
   - Reads are not one snapshot: the header is read first, then the listing. A write committing in between can put an object newer than the header into the listing. That is safe, because the next response carries the newer version and restarts the download. Reading the header last would not be safe: it could claim a version whose deletions the listing missed.
5. Full sync uses `deleted?since=0`, `settings` without `since`, and `format=versions` without `since`.
6. Full text:
   - `GET fulltext?since=V` returns `{itemKey: version}`.
   - `GET items/{k}/fulltext` returns `{content, indexedChars, totalChars}` or `{content, indexedPages, totalPages}`, or 404.
   - `POST fulltext` takes `[{key, content, indexedChars, totalChars, indexedPages, totalPages}]`, needs `If-Unmodified-Since-Version`, and answers in the write-response format.
     - Unlike object writes, fulltext `unchanged` entries must be objects with a `key` (`{"0": {"key": "K"}}`). The client reads `.key` from both `successful` and `unchanged`; with bare strings it never marks the text as synced and re-uploads it every sync.
   - `PUT items/{k}/fulltext` sets a single item and returns 204.
7. `POST users/{id}/removestoragefiles` deletes every stored file of the library and returns 204.

## Write semantics
- `POST /{type}` takes an array of at most 50 objects. Each object needs a `key` (or the server generates one) and usually a `version`. 413 if there are more.
- POST is a **partial patch per object**: an omitted field stays unchanged. `PUT /{type}/{key}` replaces the whole object, and `PATCH /{type}/{key}` patches it.
- Per-object version checks:
  - A missing object with `version > 0` fails with 404.
  - An existing object whose stored version is **newer** than the sent `version` fails with 412. That includes an explicit `version: 0`, as on zotero.org.
    - Exception: a `version: 0` whose content already matches what's stored goes into `unchanged`. That is a create retried after its response was lost, and a 412 would cost a full resync.
  - A sent `version` *ahead* of the stored one is accepted, like an ahead library version.
  - No `version` at all means create, or overwrite.
  - Settings follow the same rules, but a stale setting fails the whole request with 412.
- One request means **one library-version bump**. Every changed object gets that new version, and `Last-Modified-Version` returns it.
- An object whose stored content is identical after merging goes into `unchanged` and keeps its old version. If nothing changed, the library version doesn't move.
- The response is keyed by array index (as strings):
  ```json
  {"successful": {"0": {"key":"ABCD2345","version":12,"library":{...},"links":{},"meta":{},"data":{...}}},
   "success":    {"0": "ABCD2345"},
   "unchanged":  {"1": "EFGH6789"},
   "failed":     {"2": {"key":"JKLM2345","code":412,"message":"Item has been modified since specified version"}}}
  ```
  - A `failed` entry with 412 or 404 makes the client reset that library (one automatic full sync; a second one in the same sync throws), so only return those codes when they are actually true. A request-level 412 is milder: the client downloads, then retries.
  - 400 and 409 count as permanent errors, with one exception: a 409 whose `data` names a missing dependency makes the client mark that object for upload and retry. zotero.org sends `{"code":409,"message":"Collection K not found","data":{"collection":"K"}}`, the same with `Parent collection K not found`, and `{"message":"Parent item K not found","data":{"parentItem":"K"}}`. Without `data` the object fails on every sync, so gotero sends it too.
  - A parent in the same request counts as existing only if its own write succeeds. A child whose in-batch parent fails gets that 409 instead of being stored with a dangling parent. zotero.org processes a batch in order and behaves the same way.
- Each write locks its library row (`SELECT ... FOR UPDATE`), so writes to one library run one after another.
- `DELETE` records each key in the deleted log at the new version and returns 204. Deleting a missing key is not an error.
  - Cascades: children go with their parent item, sub-collections with their parent collection.
  - Items leave a deleted collection **without a new version**, as on zotero.org. The client drops the membership locally when it erases the collection, without marking the items changed, and it takes the DELETE's `Last-Modified-Version` as its library version. Bumping the items would leave the deleting client with stale item versions, and its next edit of one would get a per-object 412 and force a full sync.
  - `DELETE tags` does bump the items it strips, unlike zotero.org. The client deletes a tag by marking its items changed and uploading them before the tag deletion, so normally nothing carries the tag any more and nothing moves. If an item picked the tag up in between, the bump is the only way other clients learn about the removal, because Zotero ignores `deleted.tags`.
- **Settings:** writing an unchanged value doesn't bump the version. Re-creating a deleted setting removes its deleted-log entry.
- **Object JSON:**
  - Item `data` holds `key`, `version`, `itemType`, the fields, `creators`, `tags:[{tag, type?}]`, `collections:[]`, `relations:{}`, `dateAdded`, `dateModified`, and optionally `parentItem` and `deleted`.
  - Attachments add `linkMode`, `contentType`, `charset`, `filename`, `md5` and `mtime`.
    - Zotero 9+ adds `lastRead` (integer Unix seconds; `null`, `""` or `false` clear it). It's user libraries only and is left out of responses for `Zotero-Schema-Version` below 42.
    - `path` is only valid on `linked_file` attachments, and only in user libraries.
    - `md5`/`mtime` sent as `null` or `""` never overwrite stored values. When nothing is stored, `""` is kept as `null`.
  - Tags whose name is empty or only whitespace are dropped silently, not rejected. `"0"` is a valid tag.
  - `relations: []` is accepted as `{}`.
  - Malformed keys in `?itemKey=` (and the other key lists) are skipped. A list with no valid key matches nothing.
  - Annotations add the `annotation*` fields.
- Store relation URIs (`http://zotero.org/users/{id}/items/{key}`) exactly as sent.
- **Attachment field order matters.** The client fails if `filename` comes before `linkMode`. Serialize as: `key, version, parentItem, itemType, linkMode, title, accessDate, url, note, contentType, charset, filename, md5, mtime, path, lastRead, tags, collections, relations, dateAdded, dateModified`. Order other item types by the schema's field order.
- **Schema filtering:** the client doesn't call `/schema`; it uses its bundled schema. When a client sees a field it doesn't know, it raises `ZoteroInvalidDataError`. Leave out any field newer than the request's `Zotero-Schema-Version`.

## File sync (ZFS)
1. **Authorize:** `POST items/{k}/file`, form-encoded, with `md5`, `filename`, `filesize`, `mtime` (int64 ms) and optionally `zipMD5`, `zipFilename`, `params=1`.
   - Requires `If-None-Match: *` (new file) or `If-Match: <current md5>`. If neither is present, return 428.
   - 412 if the precondition doesn't hold. That 412 must carry `Last-Modified-Version` set to **the attachment item's version**.
   - 404 if the item is missing or isn't a file attachment. 403 if the key lacks files/write access. 413 if over quota, with `Zotero-Storage-Usage` and `Zotero-Storage-Quota` in MB.
   - If the server already holds the md5 for this library, answer `{"exists": 1}`, update the item's md5/mtime and bump the version.
   - Otherwise answer `{"url": <absolute>, "contentType": "...", "prefix": "", "suffix": "", "uploadKey": "<32 hex>"}`.
   - With `params=1`, answer `{"url", "params": {...}, "uploadKey"}` and expect a multipart POST.
2. **Upload:** the client POSTs `prefix + bytes + suffix` to `url` with the given `contentType`, and the server must answer **201**.
   - gotero verifies the size and md5 while streaming. For multi-file snapshots the uploaded zip is checked against `zipMD5`.
   - A repeat POST to an upload key that already received its bytes answers 201 again (the body is drained and discarded), so a lost 201 doesn't fail the sync.
3. **Register:** `POST items/{k}/file` with `upload=<uploadKey>` and the same If-Match/If-None-Match. Answer **204** with `Last-Modified-Version`.
   - The server writes `md5`, `mtime` and `filename` into the attachment and bumps its version, because ZFS clients don't send md5/mtime in their item uploads.
   - A repeat register of a key that is already registered, while the item still shows that file, answers 204 again with no version bump. Without that, the retried If-Match / If-None-Match would fail with 412 because the first call already changed the file.
4. **Download:** `GET items/{k}/file` answers **302** (never 200).
   - `Zotero-File-MD5` must describe the bytes behind `Location`, or the client rejects the download. gotero reads the file row before and after the item and retries if an upload registered in between.
   - Headers: `Location: <absolute self-authenticating URL>`, `Zotero-File-MD5`, `Zotero-File-Size`, `Zotero-File-Modification-Time` (ms), `Zotero-File-Compressed: Yes|No` (`Yes` for zipped snapshots), and `Cache-Control: no-store`.
   - The client follows the redirect **without** the API key, so the target carries a short-lived (5 min) HMAC token bound to the md5.
   - Return 404 when no file exists.

## Login
- **Legacy (Zotero 7):** `POST keys` with `{username, password, name, access}` returns 201 `{key, userID, username, displayName, access}`, or 403 for bad credentials.
- **Browser flow (newer clients):**
  1. `POST keys/sessions`, optionally with `{userID}`, returns 201 `{sessionToken, loginURL}`. `loginURL` must already contain a `?` because Android appends `&app=1`. gotero uses `{public}/login?session=TOKEN`.
  2. The client polls `GET keys/sessions/{token}` every 3 s for up to 10 min. The answer is `{status: "pending"}`, then `{status: "completed", apiKey, userID, username, displayName}`, or `{status: "cancelled"}`. Unknown sessions return 404, expired ones 410. Sessions last 30 min.
  3. `DELETE keys/sessions/{token}` cancels.
  4. Approval is refused if the session's userID belongs to someone other than the approving account.
  5. The stream topic `login-session:{token}` gets a `loginComplete` event when the session completes.

## Streaming WebSocket (`/stream`)
1. On connect the server sends `{"event":"connected","retry":10000}`.
2. The client subscribes in one of two ways:
   - `{"action":"createSubscriptions","subscriptions":[{"apiKey":"K"}]}` (an apiKey alone means all topics for that key), or
   - `{"apiKey":"K","topics":["/users/1"]}`.
   - The client also sends `{"topics":["styles","translators"]}`. Accept it; nothing is ever published there.
3. The reply is `{"event":"subscriptionsCreated","subscriptions":[{apiKey?, topics:[...]}],"errors":[]}`. `errors` must always be present.
4. Events are sent only **after the transaction commits**:
   - `{"event":"topicUpdated","topic":"/users/1","version":N}`
   - `{"event":"topicAdded","apiKey":"K","topic":"/groups/2"}`
   - `{"event":"topicRemoved","apiKey":"K","topic":"/groups/2"}`
   - `{"event":"loginComplete", ...}` for `login-session:{token}`
5. `{"action":"deleteSubscriptions","subscriptions":[{apiKey, topic?}]}` returns `subscriptionsDeleted`. Deleting a subscription that doesn't exist is ignored.
6. A connection can hold at most 50 subscriptions. Close codes: 4400 bad request, 4403 forbidden, 4413 too many. Codes 4400-4499, except 4429, stop the client from reconnecting.

## Known traps
- `Retry-After` must be a whole number of seconds.
- `mtime` is an int64 in milliseconds and is serialized as a JSON number.
- `md5` and `mtime` are `null` until a file has been uploaded.
- Behind a reverse proxy, every absolute URL (upload url, download Location, loginURL) must use the public scheme and host. Take it from `GOTERO_PUBLIC_URL`, or from `X-Forwarded-*` only when the request comes from a trusted proxy.
- `/retractions/list` and `tts/*` may return 404.

## How the desktop client syncs

Read from Zotero's sync engine (`syncEngine.js`, `syncLocal.js`). The multi-client simulation in `conformance/syncsim` models these rules, so a mismatch there points at the server.

- **Upload before download.** Objects go up in batches with `If-Unmodified-Since-Version`, then deletions. After each successful batch the client stores the response's `Last-Modified-Version` as its library version.
- **Patch mode.** An object synced before goes up as a patch against the cached server copy; cleared fields are sent as empty values. New objects go up in full with `version: 0`.
- **`unchanged` objects** get the request's library version locally, so local object versions can be ahead of the server's.
- **Settings** go up as `{"key": {"value": ...}}` without a version.
- **Remote deletions:** tags are ignored. A setting is cleared only if unchanged locally. Synced objects are erased. An item changed locally becomes a conflict, unless it is in the local trash. A collection or search changed locally is kept with its old version; its next upload then fails with 404, and the full sync re-uploads it as new.
- **Deleted here, changed there:** collections and searches are restored automatically, and the restored collection's items are fetched from `collections/K/items/top`. Items become a conflict, unless the remote copy is in the trash.
- **Full sync** replays `deleted?since=0` first, then downloads every object whose local version is older, then resets every local object the server does not list to version 0 for re-upload.
- **Download order:** each batch is applied top-level objects first. An object that cannot be saved yet, for example because its parent is missing or deleted locally, is retried on later syncs. A requested key the server no longer returns is only dropped from that retry queue; a local copy stays.

Known client-side gaps, which no server can fix:

- A local cascade erases descendants on the local tree. A sub-collection moved under a collection locally is erased when another client deletes that collection, even though on the server it lives elsewhere.
- A collection kept after a remote deletion comes back as new, but its items' membership does not, because those items stay marked as synced.
- A restored collection's membership is repaired from `collections/K/items/top`, which leaves out trashed items, and Zotero relies on that: it untrashes whatever comes back. A trashed member therefore stays out of the collection on that client while the server still has it there.
- A child whose parent went away together with a parent this client deleted keeps a stale local copy, because the key the server no longer returns is only dropped from the retry queue.
- Deleting a collection detaches its items without new versions (see above). If another client re-creates the collection before a third one syncs, the deletion never reaches the third client, and its items stay in the collection there. Bumping the detached items when the collection comes back would fix that client but give the re-creating client stale item versions, the full-sync problem the no-bump rule avoids, so gotero matches zotero.org here.
