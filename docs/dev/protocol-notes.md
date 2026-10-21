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
   - `POST settings` with `{"tagColors": {"value": [...], "version": N}}`, and `DELETE settings?settingKey=a,b`.
   - `POST collections`, `POST searches` and `POST items`, each a JSON array of at most 50 objects (the client sends 10).
   - `DELETE {collections|searches|items}?{collectionKey|searchKey|itemKey}=K1,K2`, with at most 50 keys.
   - `DELETE tags?tag=a || b`, where the separator is ` || `. A bare `DELETE tags` returns 204.
4. Then the client **downloads**:
   - `GET settings?since=V`, sent with `If-Modified-Since-Version: V`.
   - `GET collections?format=versions&since=V`, likewise for `searches`, then `items/top?format=versions&since=V&includeTrashed=1` and `items?format=versions&since=V&includeTrashed=1`.
     - These return `{"KEY": version}`. They are **not paginated** and have no default limit.
   - `GET {type}?{type}Key=K1,...&includeTrashed=1` with **up to 100** keys per request.
   - `GET deleted?since=V` returns `{collections:[], searches:[], items:[], tags:[], settings:[]}`.
     - If `since` is older than the oldest delete-log entry still kept, it returns 409, which forces a full sync.
   - If `Last-Modified-Version` changes partway through a download, the client starts over.
5. Full sync uses `deleted?since=0`, `settings` without `since`, and `format=versions` without `since`.
6. Full text:
   - `GET fulltext?since=V` returns `{itemKey: version}`.
   - `GET items/{k}/fulltext` returns `{content, indexedChars, totalChars}` or `{content, indexedPages, totalPages}`, or 404.
   - `POST fulltext` takes `[{key, content, indexedChars, totalChars, indexedPages, totalPages}]`, needs `If-Unmodified-Since-Version`, and answers in the write-response format.
   - `PUT items/{k}/fulltext` sets a single item and returns 204.
7. `POST users/{id}/removestoragefiles` deletes every stored file of the library and returns 204.

## Write semantics
- `POST /{type}` takes an array of at most 50 objects. Each object needs a `key` (or the server generates one) and usually a `version`. 413 if there are more.
- POST is a **partial patch per object**: an omitted field stays unchanged. `PUT /{type}/{key}` replaces the whole object, and `PATCH /{type}/{key}` patches it.
- Per-object version checks:
  - A missing object with `version > 0` fails with 404.
  - An existing object whose `version` differs from the stored one fails with 412.
  - `version: 0` or no version means create, or overwrite if the client really means it (use the stored version).
- One request means **one library-version bump**. Every changed object gets that new version, and `Last-Modified-Version` returns it.
- An object whose stored content is identical after merging goes into `unchanged` and keeps its old version. If nothing changed, the library version doesn't move.
- The response is keyed by array index (as strings):
  ```json
  {"successful": {"0": {"key":"ABCD2345","version":12,"library":{...},"links":{},"meta":{},"data":{...}}},
   "success":    {"0": "ABCD2345"},
   "unchanged":  {"1": "EFGH6789"},
   "failed":     {"2": {"key":"JKLM2345","code":412,"message":"Item has been modified since specified version"}}}
  ```
  - A `failed` entry with 412 or 404 makes the client reset that library (a full sync), so only return those codes when they are actually true.
  - 400 and 409 count as permanent errors.
- Each write locks its library row (`SELECT ... FOR UPDATE`), so writes to one library run one after another.
- `DELETE` records each key in the deleted log at the new version and returns 204. Deleting a missing key is not an error.
- **Settings:** writing an unchanged value doesn't bump the version. Re-creating a deleted setting removes its deleted-log entry.
- **Object JSON:**
  - Item `data` holds `key`, `version`, `itemType`, the fields, `creators`, `tags:[{tag, type?}]`, `collections:[]`, `relations:{}`, `dateAdded`, `dateModified`, and optionally `parentItem` and `deleted`.
  - Attachments add `linkMode`, `contentType`, `charset`, `filename`, `md5` and `mtime`.
  - Annotations add the `annotation*` fields.
- Store relation URIs (`http://zotero.org/users/{id}/items/{key}`) exactly as sent.
- **Attachment field order matters.** The client fails if `filename` comes before `linkMode`. Serialize as: `key, version, parentItem, itemType, linkMode, title, accessDate, url, note, contentType, charset, filename, md5, mtime, tags, collections, relations, dateAdded, dateModified`. Order other item types by the schema's field order.
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
3. **Register:** `POST items/{k}/file` with `upload=<uploadKey>` and the same If-Match/If-None-Match. Answer **204** with `Last-Modified-Version`.
   - The server writes `md5`, `mtime` and `filename` into the attachment and bumps its version, because ZFS clients don't send md5/mtime in their item uploads.
4. **Download:** `GET items/{k}/file` answers **302** (never 200).
   - Headers: `Location: <absolute self-authenticating URL>`, `Zotero-File-MD5`, `Zotero-File-Modification-Time` (ms), `Zotero-File-Compressed: Yes|No` (`Yes` for zipped snapshots), and `Cache-Control: no-store`.
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
