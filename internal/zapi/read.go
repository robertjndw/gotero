package zapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// scope narrows a listing to a part of the library.
type scope struct {
	top      bool // no parent
	trash    bool // only trashed items
	children bool // children of :key
	inColl   bool // contained in collection :key
}

var (
	itemsAll             = scope{}
	itemsTop             = scope{top: true}
	itemsTrash           = scope{trash: true}
	itemsChildren        = scope{children: true}
	itemsInCollection    = scope{inColl: true}
	itemsTopInCollection = scope{inColl: true, top: true}
	collectionsAll       = scope{}
	collectionsTop       = scope{top: true}
	collectionsChildren  = scope{children: true}
	searchesAll          = scope{}
)

const (
	defaultLimit = 25
	maxLimit     = 100
)

// listParams are the query parameters shared by every listing.
type listParams struct {
	format string
	since  int64
	keys   []string
	// noMatch is set when a key list was given but held only malformed keys.
	// keys stays empty then, which would otherwise read as "no filter".
	noMatch bool
	limit   int // 0 means unlimited
	start   int
	sort    string
	desc    bool
	tags    []string
	trashed bool
}

func parseSince(c *gin.Context) (int64, error) {
	s := c.Query("since")
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errStatus(http.StatusBadRequest, "Invalid 'since' value")
	}
	return n, nil
}

func parseListParams(c *gin.Context, t repo.ObjectType) (listParams, error) {
	p := listParams{format: c.DefaultQuery("format", "json")}
	switch p.format {
	case "json", "keys", "versions":
	default:
		return p, errStatus(http.StatusBadRequest, "Invalid 'format' value")
	}
	since, err := parseSince(c)
	if err != nil {
		return p, err
	}
	p.since = since
	if s := c.Query(zotero.ObjectType(t).KeyParam()); s != "" {
		keys, err := zotero.ParseKeyListMax(s, zotero.MaxKeyList)
		if errors.Is(err, zotero.ErrNoValidKeys) {
			p.noMatch = true
		} else if err != nil {
			return p, errStatus(http.StatusBadRequest, "Invalid "+zotero.ObjectType(t).KeyParam()+" value")
		}
		p.keys = keys
	}
	if s := c.Query("start"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, errStatus(http.StatusBadRequest, "Invalid 'start' value")
		}
		p.start = n
	}
	// The sync client lists keys and versions without any paging, so those
	// formats are unlimited unless the caller asks otherwise. The same goes
	// for itemKey=... requests: Zotero asks for up to 100 keys at once and
	// never follows Link, so paging them would silently drop objects.
	if p.format == "json" && len(p.keys) == 0 {
		p.limit = defaultLimit
	}
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return p, errStatus(http.StatusBadRequest, "Invalid 'limit' value")
		}
		p.limit = n
		if p.format == "json" && n > maxLimit {
			p.limit = maxLimit
		}
	}
	sortBy := c.Query("sort")
	switch sortBy {
	case "", "dateAdded", "dateModified", "title", "itemType", "date", "publisher", "publicationTitle", "version":
	case "creator", "year", "accessDate", "numChildren", "addedBy", "numItems", "tag":
		// Valid on zotero.org but not indexed here; keep the default order.
		sortBy = ""
	default:
		return p, errStatus(http.StatusBadRequest, "Invalid 'sort' value")
	}
	p.sort = sortBy
	switch d := c.Query("direction"); d {
	case "", "asc":
	case "desc":
		p.desc = true
	default:
		return p, errStatus(http.StatusBadRequest, "Invalid 'direction' value")
	}
	p.tags = c.QueryArray("tag")
	p.trashed = c.Query("includeTrashed") == "1" || c.Query("includeTrashed") == "true"
	return p, nil
}

// listQuery turns params and scope into a repository query.
func listQuery(t repo.ObjectType, p listParams, sc scope, scopeKey string) repo.ListQuery {
	q := repo.ListQuery{
		VersionsQuery: repo.VersionsQuery{
			Type: t, Since: p.since, IncludeTrashed: p.trashed, OnlyTrashed: sc.trash,
			TopOnly: sc.top, Keys: p.keys,
		},
		Limit: p.limit, Start: p.start, Sort: p.sort, Desc: p.desc, Tag: p.tags,
	}
	if sc.children {
		q.ParentKey = scopeKey
	}
	if sc.inColl {
		q.Collection = scopeKey
	}
	return q
}

func (a *API) listObjects(t repo.ObjectType, sc scope) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := lib(c)
		p, err := parseListParams(c, t)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		ctx := c.Request.Context()

		scopeKey := ""
		if sc.children || sc.inColl {
			scopeKey = c.Param("key")
			parentType := repo.TypeItem
			if sc.inColl || t == repo.TypeCollection {
				parentType = repo.TypeCollection
			}
			if !zotero.ValidKey(scopeKey) {
				Fail(c, http.StatusNotFound, "Not found")
				return
			}
			found, err := l.Reader.Get(ctx, parentType, []string{scopeKey})
			if err != nil {
				a.FailErr(c, err)
				return
			}
			if o, ok := found[scopeKey]; !ok || hiddenFrom(l, o) {
				Fail(c, http.StatusNotFound, "Not found")
				return
			}
		}
		// Sort defaults mirror zotero.org: most recently modified items first.
		// Collections and searches have no dates, so they go by version.
		if p.sort == "" && p.format == "json" && t == repo.TypeItem {
			p.sort = "dateModified"
			p.desc = p.desc || !c.Request.URL.Query().Has("direction")
		}
		if p.noMatch {
			a.writeNoMatch(c, p)
			return
		}
		q := listQuery(t, p, sc, scopeKey)
		// Filtered in SQL so Total-Results, paging and counts agree with
		// what the key may actually see.
		q.ExcludeNotes = !l.Access.Notes

		switch p.format {
		case "versions", "keys":
			a.streamVersions(c, l, q, p)
		default:
			a.writeObjects(c, l, q, p, t)
		}
	}
}

// writeNoMatch answers a key filter that named nothing valid: an empty
// listing in the requested format, not the whole library.
func (a *API) writeNoMatch(c *gin.Context, p listParams) {
	c.Header("Total-Results", "0")
	switch p.format {
	case "versions":
		c.Data(http.StatusOK, "application/json", []byte("{}"))
	case "keys":
		c.Data(http.StatusOK, "text/plain; charset=UTF-8", nil)
	default:
		c.Data(http.StatusOK, "application/json", []byte("[]"))
	}
}

// streamVersions writes {"KEY": version} or newline separated keys straight
// from the repository iterator, so a 100k item library never sits in memory.
func (a *API) streamVersions(c *gin.Context, l *LibraryContext, q repo.ListQuery, p listParams) {
	ctx := c.Request.Context()
	seq, err := a.versionsSeq(ctx, l, q)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	asKeys := p.format == "keys"
	if asKeys {
		c.Header("Content-Type", "text/plain; charset=UTF-8")
		if n, err := l.Reader.Count(ctx, q); err == nil {
			c.Header("Total-Results", strconv.FormatInt(n, 10))
		}
	} else {
		c.Header("Content-Type", "application/json")
	}
	c.Status(http.StatusOK)

	// A client that stops reading must not hold the cursor (and its pool
	// connection) open indefinitely.
	dw := httpx.NewDeadlineWriter(c.Writer, a.writeIdle)
	defer dw.Release()
	w := bufio.NewWriterSize(dw, 64<<10)
	flushed := false
	if !asKeys {
		_ = w.WriteByte('{')
	}
	var buf []byte
	i, written := 0, 0
	for kv, err := range seq {
		if err != nil {
			a.abortStream(c, flushed, err)
			return
		}
		i++
		if i <= p.start {
			continue
		}
		if p.limit > 0 && written >= p.limit {
			break
		}
		buf = buf[:0]
		if asKeys {
			buf = append(buf, kv.Key...)
			buf = append(buf, '\n')
		} else {
			if written > 0 {
				buf = append(buf, ',')
			}
			// Keys are validated against a fixed alphabet on write, so they
			// never need JSON escaping.
			buf = append(buf, '"')
			buf = append(buf, kv.Key...)
			buf = append(buf, '"', ':')
			buf = strconv.AppendInt(buf, kv.Version, 10)
		}
		if w.Available() < len(buf) && w.Buffered() > 0 {
			flushed = true
		}
		if _, err := w.Write(buf); err != nil {
			// The client is gone or stalled past the deadline; leaving the
			// loop closes the cursor.
			return
		}
		written++
	}
	if !asKeys {
		_ = w.WriteByte('}')
	}
	_ = w.Flush()
}

// versionsSeq picks the cheap index-only listing when it can answer the
// query. Tag and collection filters are only understood by the full object
// listing, so those fall back to it.
func (a *API) versionsSeq(ctx context.Context, l *LibraryContext, q repo.ListQuery) (iter.Seq2[repo.KeyVersion, error], error) {
	if len(q.Tag) == 0 && q.Collection == "" {
		return l.Reader.Versions(ctx, q.VersionsQuery)
	}
	q.Limit, q.Start, q.Sort = 0, 0, ""
	objs, err := l.Reader.Objects(ctx, q)
	if err != nil {
		return nil, err
	}
	return func(yield func(repo.KeyVersion, error) bool) {
		for o, err := range objs {
			if !yield(repo.KeyVersion{Key: o.Key, Version: o.Version}, err) || err != nil {
				return
			}
		}
	}, nil
}

// abortStream ends a response that failed midway. Before the first flush the
// client can still get a proper error; afterwards the only honest signal is
// to cut the connection.
func (a *API) abortStream(c *gin.Context, flushed bool, err error) {
	if !flushed {
		a.FailErr(c, err)
		return
	}
	a.log.ErrorContext(c.Request.Context(), "zapi: stream aborted", "path", c.Request.URL.Path, "err", err)
	panic(http.ErrAbortHandler)
}

func (a *API) writeObjects(c *gin.Context, l *LibraryContext, q repo.ListQuery, p listParams, t repo.ObjectType) {
	ctx := c.Request.Context()
	total, err := l.Reader.Count(ctx, q)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	seq, err := l.Reader.Objects(ctx, q)
	if err != nil {
		if errors.Is(err, repo.ErrUnsupportedSort) {
			Fail(c, http.StatusBadRequest, "Invalid 'sort' value")
			return
		}
		a.FailErr(c, err)
		return
	}
	// At most 100 objects, so collecting them is fine and lets us fetch child
	// counts for the whole page in one query.
	var objs []repo.Object
	for o, err := range seq {
		if err != nil {
			a.FailErr(c, err)
			return
		}
		objs = append(objs, o)
	}
	var counts map[string]int
	if t == repo.TypeItem && len(objs) > 0 {
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.Key
		}
		if counts, err = l.Reader.ChildCounts(ctx, keys, !l.Access.Notes); err != nil {
			a.FailErr(c, err)
			return
		}
	}

	b := a.newBuilder(c, l)
	out := make([]byte, 0, 1024*len(objs)+2)
	out = append(out, '[')
	for i, o := range objs {
		if i > 0 {
			out = append(out, ',')
		}
		var meta zotero.Meta
		if t == repo.TypeItem {
			n := counts[o.Key]
			meta.NumChildren = &n
		}
		if out, err = b.appendObject(out, t, o, meta); err != nil {
			a.FailErr(c, err)
			return
		}
	}
	out = append(out, ']')

	c.Header("Total-Results", strconv.FormatInt(total, 10))
	if link := linkHeader(c, p.start, p.limit, total); link != "" {
		c.Header("Link", link)
	}
	c.Data(http.StatusOK, "application/json", out)
}

// linkHeader builds the pagination Link header for a paged JSON listing.
func linkHeader(c *gin.Context, start, limit int, total int64) string {
	if limit <= 0 {
		return ""
	}
	base := apiRoot(c)
	rel := func(name string, s int) string {
		v := c.Request.URL.Query()
		v.Del("key") // never echo credentials into links
		v.Set("start", strconv.Itoa(s))
		v.Set("limit", strconv.Itoa(limit))
		u := url.URL{Path: c.Request.URL.Path, RawQuery: v.Encode()}
		return fmt.Sprintf(`<%s>; rel="%s"`, strings.TrimSuffix(base, pathPrefix(c))+u.String(), name)
	}
	var parts []string
	if start > 0 {
		parts = append(parts, rel("first", 0), rel("prev", max(start-limit, 0)))
	}
	if int64(start+limit) < total {
		parts = append(parts, rel("next", start+limit))
	}
	if total > 0 {
		last := int((total - 1) / int64(limit) * int64(limit))
		parts = append(parts, rel("last", last))
	}
	return strings.Join(parts, ", ")
}

// pathPrefix is the part of apiRoot that is path, not scheme and host.
func pathPrefix(c *gin.Context) string {
	root := apiRoot(c)
	i := strings.Index(root, "://")
	if i < 0 {
		return ""
	}
	if j := strings.IndexByte(root[i+3:], '/'); j >= 0 {
		return root[i+3+j:]
	}
	return ""
}

func (a *API) getObject(t repo.ObjectType) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := lib(c)
		key := c.Param("key")
		if !zotero.ValidKey(key) {
			Fail(c, http.StatusNotFound, "Not found")
			return
		}
		ctx := c.Request.Context()
		found, err := l.Reader.Get(ctx, t, []string{key})
		if err != nil {
			a.FailErr(c, err)
			return
		}
		o, ok := found[key]
		if !ok || hiddenFrom(l, o) {
			Fail(c, http.StatusNotFound, "Not found")
			return
		}
		var meta zotero.Meta
		switch t {
		case repo.TypeItem:
			counts, err := l.Reader.ChildCounts(ctx, []string{key}, !l.Access.Notes)
			if err != nil {
				a.FailErr(c, err)
				return
			}
			n := counts[key]
			meta.NumChildren = &n
		case repo.TypeCollection:
			items, err := l.Reader.Count(ctx, repo.ListQuery{
				VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, ExcludeNotes: !l.Access.Notes}, Collection: key,
			})
			if err != nil {
				a.FailErr(c, err)
				return
			}
			subs, err := l.Reader.Count(ctx, repo.ListQuery{
				VersionsQuery: repo.VersionsQuery{Type: repo.TypeCollection, ParentKey: key},
			})
			if err != nil {
				a.FailErr(c, err)
				return
			}
			ni, ns := int(items), int(subs)
			meta.NumItems, meta.NumCollections = &ni, &ns
		}
		out, err := a.newBuilder(c, l).appendObject(nil, t, o, meta)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		c.Data(http.StatusOK, "application/json", out)
	}
}

// hiddenFrom reports whether the key may not see o. Keys without the notes
// permission get no note items at all; they are not redacted, they are absent,
// so a single GET answers 404 like any unknown key. Only itemType "note" is
// filtered: the "note" text some attachments carry is left alone because it
// is part of the attachment's own data, not a separate note object.
func hiddenFrom(l *LibraryContext, o repo.Object) bool {
	return !l.Access.Notes && o.ItemType == "note"
}
