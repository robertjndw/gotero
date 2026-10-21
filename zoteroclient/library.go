package zoteroclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/robertjndw/gotero/zotero"
)

// Limits the Zotero API puts on a single request.
const (
	maxReadKeys   = 100
	maxWriteItems = 50
	maxDeleteKeys = 50
)

// Library is a handle on one user or group library. Methods return the
// library's Last-Modified-Version as the int64 next to the result (or next to
// the error, where the server sent one), so a sync loop can store it without a
// second request. Create handles with Client.User and Client.Group.
type Library struct {
	c      *Client
	prefix string // "users/123" or "groups/456"
	group  bool
}

// User returns the handle for the user library with the given ID.
func (c *Client) User(id int64) *Library {
	return &Library{c: c, prefix: "users/" + strconv.FormatInt(id, 10)}
}

// Group returns the handle for the group library with the given ID. See
// GetGroup for group metadata.
func (c *Client) Group(id int64) *Library {
	return &Library{c: c, prefix: "groups/" + strconv.FormatInt(id, 10), group: true}
}

func (l *Library) path(rel string) string { return l.prefix + "/" + rel }

func versionHdr(name string, v int64) http.Header {
	h := http.Header{}
	h.Set(name, strconv.FormatInt(v, 10))
	return h
}

// VersionsQuery selects what Versions returns.
type VersionsQuery struct {
	// Since limits the result to objects modified after this library version.
	// When it is positive, If-Modified-Since-Version is sent too, so an
	// unchanged library answers ErrNotModified cheaply.
	Since int64
	// IncludeTrashed also lists items in the trash.
	IncludeTrashed bool
	// Top lists only top-level items (or collections).
	Top bool
}

// Versions returns a key -> version map for the object type, the first step of
// every sync. The int64 is the library version the answer corresponds to.
func (l *Library) Versions(ctx context.Context, t zotero.ObjectType, q VersionsQuery) (map[string]int64, int64, error) {
	rel := t.Plural()
	if q.Top {
		rel += "/top"
	}
	v := url.Values{"format": {"versions"}}
	var hdr http.Header
	if q.Since > 0 {
		v.Set("since", strconv.FormatInt(q.Since, 10))
		hdr = versionHdr("If-Modified-Since-Version", q.Since)
	}
	if q.IncludeTrashed {
		v.Set("includeTrashed", "1")
	}
	var out map[string]int64
	ver, err := l.c.get(ctx, l.path(rel), v, hdr, &out)
	if err != nil {
		return nil, ver, err
	}
	return out, ver, nil
}

// Objects fetches the full objects for keys, 100 keys per request. Trashed
// items are included, as a sync needs them. If the library version differs
// between chunks it stops with ErrLibraryChanged and the objects read so far,
// because the result would mix two library states.
func (l *Library) Objects(ctx context.Context, t zotero.ObjectType, keys []string) ([]zotero.Object, int64, error) {
	var out []zotero.Object
	var first int64
	for i, chunk := range slices.Collect(slices.Chunk(keys, maxReadKeys)) {
		q := url.Values{
			t.KeyParam():     {strings.Join(chunk, ",")},
			"includeTrashed": {"1"},
			"limit":          {strconv.Itoa(maxReadKeys)},
		}
		var page []zotero.Object
		ver, err := l.c.get(ctx, l.path(t.Plural()), q, nil, &page)
		if err != nil {
			return out, ver, err
		}
		if i == 0 {
			first = ver
		} else if ver != first {
			return out, ver, ErrLibraryChanged
		}
		out = append(out, page...)
	}
	return out, first, nil
}

// Object fetches one object by key.
func (l *Library) Object(ctx context.Context, t zotero.ObjectType, key string) (*zotero.Object, int64, error) {
	var o zotero.Object
	ver, err := l.c.get(ctx, l.path(t.Plural()+"/"+url.PathEscape(key)), nil, nil, &o)
	if err != nil {
		return nil, ver, err
	}
	return &o, ver, nil
}

// ListQuery configures ListObjects.
type ListQuery struct {
	// Since limits the result to objects modified after this version.
	Since int64
	// Top lists only top-level objects.
	Top bool
	// IncludeTrashed also lists trashed items.
	IncludeTrashed bool
	// Limit is the page size (default and maximum 100).
	Limit int
	// Params adds further query parameters such as "tag" or "q".
	Params url.Values
	// OnVersion, if set, is called with each page's Last-Modified-Version.
	// The iterator can't return it, and a change between pages means the
	// listing is not a consistent snapshot.
	OnVersion func(int64)
}

// ListObjects iterates over all objects of a type, fetching page after page.
// It follows the Link rel="next" header and falls back to start/limit
// arithmetic for servers that don't send one. The iteration stops after the
// first error, which is yielded as the final element.
func (l *Library) ListObjects(ctx context.Context, t zotero.ObjectType, q ListQuery) iter.Seq2[zotero.Object, error] {
	return func(yield func(zotero.Object, error) bool) {
		limit := q.Limit
		if limit <= 0 || limit > 100 {
			limit = 100
		}
		rel := t.Plural()
		if q.Top {
			rel += "/top"
		}
		v := url.Values{}
		for k, vs := range q.Params {
			v[k] = slices.Clone(vs)
		}
		v.Set("limit", strconv.Itoa(limit))
		if q.Since > 0 {
			v.Set("since", strconv.FormatInt(q.Since, 10))
		}
		if q.IncludeTrashed {
			v.Set("includeTrashed", "1")
		}
		next := l.c.newRequest(http.MethodGet, l.path(rel), v, nil).url
		start := 0
		for next != "" {
			r := l.c.newRequest(http.MethodGet, "", nil, nil)
			r.url = next
			resp, err := l.c.call(ctx, r)
			if err != nil {
				yield(zotero.Object{}, err)
				return
			}
			var page []zotero.Object
			err = decodeJSON(resp.Body, &page)
			_ = resp.Body.Close()
			if err != nil {
				yield(zotero.Object{}, fmt.Errorf("zoteroclient: decode %s: %w", next, err))
				return
			}
			if q.OnVersion != nil {
				q.OnVersion(versionHeader(resp.Header))
			}
			for _, o := range page {
				if !yield(o, nil) {
					return
				}
			}
			next = ""
			links := resp.Header.Values("Link")
			if len(links) > 0 {
				target, ok := parseLink(strings.Join(links, ","))["next"]
				if !ok {
					return
				}
				abs, err := l.c.sameOrigin(target)
				if err != nil {
					yield(zotero.Object{}, err)
					return
				}
				next = abs
			} else if len(page) >= limit {
				start += len(page)
				v.Set("start", strconv.Itoa(start))
				next = l.c.newRequest(http.MethodGet, l.path(rel), v, nil).url
			}
		}
	}
}

// sameOrigin resolves a Link target against the base URL and refuses other
// hosts, since the request would carry the API key.
func (c *Client) sameOrigin(target string) (string, error) {
	u, err := c.base.Parse(target)
	if err != nil {
		return "", fmt.Errorf("zoteroclient: bad next link %q: %w", target, err)
	}
	if u.Scheme != c.base.Scheme || u.Host != c.base.Host {
		return "", fmt.Errorf("zoteroclient: next link %q points outside the base URL", target)
	}
	return u.String(), nil
}

// WriteOptions control Write.
type WriteOptions struct {
	// IfUnmodifiedSinceVersion sends If-Unmodified-Since-Version. When Write
	// splits the objects into several requests, later requests use the
	// version the previous one produced, so only the first has to match.
	IfUnmodifiedSinceVersion *int64
	// WriteToken sets Zotero-Write-Token (32 chars). A token is good for one
	// request, so it can't be combined with more than 50 objects.
	WriteToken string
}

// rawWrite mirrors zotero.WriteResponse with tolerant decoding.
type rawWrite struct {
	Successful json.RawMessage `json:"successful"`
	Success    json.RawMessage `json:"success"`
	Unchanged  json.RawMessage `json:"unchanged"`
	Failed     json.RawMessage `json:"failed"`
}

func decodeWrite(raw rawWrite) (*zotero.WriteResponse, error) {
	var w zotero.WriteResponse
	var err error
	if w.Successful, err = lenientMap[json.RawMessage](raw.Successful); err != nil {
		return nil, fmt.Errorf("successful: %w", err)
	}
	if w.Success, err = lenientMap[string](raw.Success); err != nil {
		return nil, fmt.Errorf("success: %w", err)
	}
	if w.Unchanged, err = lenientMap[string](raw.Unchanged); err != nil {
		return nil, fmt.Errorf("unchanged: %w", err)
	}
	if w.Failed, err = lenientMap[zotero.WriteFailure](raw.Failed); err != nil {
		return nil, fmt.Errorf("failed: %w", err)
	}
	return &w, nil
}

// mergeWrite folds a chunk's response into agg, shifting indexes by offset so
// they refer to positions in the caller's slice.
func mergeWrite(agg, w *zotero.WriteResponse, offset int) {
	idx := func(k string) string {
		n, err := strconv.Atoi(k)
		if err != nil {
			return k
		}
		return strconv.Itoa(n + offset)
	}
	for k, v := range w.Successful {
		agg.Successful[idx(k)] = v
	}
	for k, v := range w.Success {
		agg.Success[idx(k)] = v
	}
	for k, v := range w.Unchanged {
		agg.Unchanged[idx(k)] = v
	}
	for k, v := range w.Failed {
		agg.Failed[idx(k)] = v
	}
}

// Write creates or updates objects with POST, 50 per request. The result
// maps are keyed by index into objs, whatever the chunking. The int64 is the
// library version after the last request.
//
// If a request fails as a whole (412 for the library precondition, for
// example) Write stops, and returns what the earlier requests achieved
// together with the error. Per-object failures do not stop it, they are in
// the response's Failed map.
func (l *Library) Write(ctx context.Context, t zotero.ObjectType, objs []zotero.OrderedMap, opts WriteOptions) (*zotero.WriteResponse, int64, error) {
	if opts.WriteToken != "" && len(objs) > maxWriteItems {
		return nil, 0, errors.New("zoteroclient: a write token can't be used with more than 50 objects")
	}
	agg := zotero.NewWriteResponse()
	cond := opts.IfUnmodifiedSinceVersion
	var ver int64
	for offset := 0; offset < len(objs); offset += maxWriteItems {
		chunk := objs[offset:min(offset+maxWriteItems, len(objs))]
		body := []byte{'['}
		for i, o := range chunk {
			if i > 0 {
				body = append(body, ',')
			}
			body = o.AppendJSON(body)
		}
		body = append(body, ']')

		hdr := http.Header{}
		if cond != nil {
			hdr.Set("If-Unmodified-Since-Version", strconv.FormatInt(*cond, 10))
		}
		if opts.WriteToken != "" {
			hdr.Set("Zotero-Write-Token", opts.WriteToken)
		}
		var raw rawWrite
		v, err := l.c.sendJSON(ctx, http.MethodPost, l.path(t.Plural()), nil, hdr, body, &raw)
		if err != nil {
			return agg, max(v, ver), err
		}
		w, err := decodeWrite(raw)
		if err != nil {
			return agg, v, fmt.Errorf("zoteroclient: decode write response: %w", err)
		}
		mergeWrite(agg, w, offset)
		ver = v
		if cond != nil && v > 0 {
			cond = &v
		}
	}
	return agg, ver, nil
}

// Delete removes objects by key (collections, searches or items), 50 per
// request, and returns the library version after the last one. Later requests
// are conditioned on the version the previous one produced.
func (l *Library) Delete(ctx context.Context, t zotero.ObjectType, keys []string, ifUnmodifiedSince int64) (int64, error) {
	return l.deleteChunks(ctx, t.Plural(), t.KeyParam(), keys, ",", ifUnmodifiedSince)
}

// DeleteTags removes tags by name, 50 per request. The names go over the wire
// joined with " || " as Zotero does.
func (l *Library) DeleteTags(ctx context.Context, tags []string, ifUnmodifiedSince int64) (int64, error) {
	return l.deleteChunks(ctx, "tags", "tag", tags, " || ", ifUnmodifiedSince)
}

func (l *Library) deleteChunks(ctx context.Context, rel, param string, names []string, sep string, cond int64) (int64, error) {
	var ver int64
	for chunk := range slices.Chunk(names, maxDeleteKeys) {
		q := url.Values{param: {strings.Join(chunk, sep)}}
		r := l.c.newRequest(http.MethodDelete, l.path(rel), q, versionHdr("If-Unmodified-Since-Version", cond))
		v, err := l.c.exec(ctx, r, nil)
		if err != nil {
			return max(v, ver), err
		}
		ver = v
		if v > 0 {
			cond = v
		}
	}
	return ver, nil
}

// Setting is one entry of the library settings (tagColors, lastPageIndex_...).
type Setting struct {
	// Value is the setting's JSON value.
	Value json.RawMessage `json:"value"`
	// Version is the library version of its last change. Leave it zero when
	// writing to say "whatever is current".
	Version int64 `json:"version,omitzero"`
}

// Settings returns the library settings. With since > 0 only newer entries
// come back, and an unchanged library yields ErrNotModified.
func (l *Library) Settings(ctx context.Context, since int64) (map[string]Setting, int64, error) {
	var q url.Values
	var hdr http.Header
	if since > 0 {
		q = url.Values{"since": {strconv.FormatInt(since, 10)}}
		hdr = versionHdr("If-Modified-Since-Version", since)
	}
	var out map[string]Setting
	ver, err := l.c.get(ctx, l.path("settings"), q, hdr, &out)
	if err != nil {
		return nil, ver, err
	}
	return out, ver, nil
}

// WriteSettings stores settings. ifUnmodifiedSince may be nil to skip the
// precondition. It returns the new library version.
func (l *Library) WriteSettings(ctx context.Context, settings map[string]Setting, ifUnmodifiedSince *int64) (int64, error) {
	body, err := json.Marshal(settings)
	if err != nil {
		return 0, err
	}
	var hdr http.Header
	if ifUnmodifiedSince != nil {
		hdr = versionHdr("If-Unmodified-Since-Version", *ifUnmodifiedSince)
	}
	return l.c.sendJSON(ctx, http.MethodPost, l.path("settings"), nil, hdr, body, nil)
}

// DeleteSettings removes settings by key and returns the new library version.
func (l *Library) DeleteSettings(ctx context.Context, keys []string, ifUnmodifiedSince int64) (int64, error) {
	q := url.Values{"settingKey": {strings.Join(keys, ",")}}
	r := l.c.newRequest(http.MethodDelete, l.path("settings"), q, versionHdr("If-Unmodified-Since-Version", ifUnmodifiedSince))
	return l.c.exec(ctx, r, nil)
}

// Deleted lists what was deleted after the given library version. IsConflict
// is true for the error when since is older than the server's delete log, in
// which case a full sync is required.
func (l *Library) Deleted(ctx context.Context, since int64) (*zotero.DeletedResponse, int64, error) {
	var out zotero.DeletedResponse
	ver, err := l.c.get(ctx, l.path("deleted"), url.Values{"since": {strconv.FormatInt(since, 10)}}, nil, &out)
	if err != nil {
		return nil, ver, err
	}
	return &out, ver, nil
}
