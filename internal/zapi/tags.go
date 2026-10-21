package zapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

type tagScope int

const (
	tagsAllItems tagScope = iota
	tagsTopItems
	tagsOfItem
	tagsInCollection
)

type tagOut struct {
	Tag   string       `json:"tag"`
	Links zotero.Links `json:"links"`
	Meta  tagOutMeta   `json:"meta"`
}

type tagOutMeta struct {
	Type     int `json:"type"`
	NumItems int `json:"numItems"`
}

func (a *API) tagQuery(c *gin.Context, l *LibraryContext, sc tagScope) (repo.ListQuery, bool) {
	q := repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}}
	q.IncludeTrashed = c.Query("includeTrashed") == "1"
	q.ExcludeNotes = !l.Access.Notes
	switch sc {
	case tagsTopItems:
		q.TopOnly = true
	case tagsOfItem:
		key := c.Param("key")
		if !zotero.ValidKey(key) {
			Fail(c, http.StatusNotFound, "Not found")
			return q, false
		}
		q.Keys = []string{key}
		q.IncludeTrashed = true
	case tagsInCollection:
		key := c.Param("key")
		if !zotero.ValidKey(key) {
			Fail(c, http.StatusNotFound, "Not found")
			return q, false
		}
		q.Collection = key
	}
	since, err := parseSince(c)
	if err != nil {
		a.FailErr(c, err)
		return q, false
	}
	q.Since = since
	return q, true
}

func (a *API) tagObjects(c *gin.Context, tags []repo.ItemTag) []tagOut {
	l := lib(c)
	root := apiRoot(c)
	out := make([]tagOut, 0, len(tags))
	for _, t := range tags {
		href := root + l.Topic() + "/tags/" + url.PathEscape(t.Tag)
		out = append(out, tagOut{
			Tag:   t.Tag,
			Links: zotero.Links{"self": {Href: href, Type: "application/json"}, "alternate": {Href: href, Type: "text/html"}},
			Meta:  tagOutMeta{Type: t.Type, NumItems: t.NumItems},
		})
	}
	return out
}

func (a *API) listTags(sc tagScope) gin.HandlerFunc {
	return func(c *gin.Context) {
		l := lib(c)
		q, ok := a.tagQuery(c, l, sc)
		if !ok {
			return
		}
		tags, err := l.Reader.Tags(c.Request.Context(), q)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		// A tag with both manual and automatic uses appears once per type;
		// the API lists names, so keep the first (manual, type 0) one.
		seen := map[string]bool{}
		uniq := tags[:0:0]
		for _, t := range tags {
			if !seen[t.Tag] {
				seen[t.Tag] = true
				uniq = append(uniq, t)
			}
		}
		sort.SliceStable(uniq, func(i, j int) bool { return strings.ToLower(uniq[i].Tag) < strings.ToLower(uniq[j].Tag) })

		format := c.DefaultQuery("format", "json")
		if format != "json" {
			Fail(c, http.StatusBadRequest, "Invalid 'format' value")
			return
		}
		limit, start := defaultLimit, 0
		if s := c.Query("limit"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				Fail(c, http.StatusBadRequest, "Invalid 'limit' value")
				return
			}
			limit = min(n, maxLimit)
		}
		if s := c.Query("start"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				Fail(c, http.StatusBadRequest, "Invalid 'start' value")
				return
			}
			start = n
		}
		total := len(uniq)
		page := uniq[min(start, total):min(start+limit, total)]
		c.Header("Total-Results", strconv.Itoa(total))
		if link := linkHeader(c, start, limit, int64(total)); link != "" {
			c.Header("Link", link)
		}
		c.JSON(http.StatusOK, a.tagObjects(c, page))
	}
}

func (a *API) getTag(c *gin.Context) {
	l := lib(c)
	name := c.Param("name")
	tags, err := l.Reader.Tags(c.Request.Context(), repo.ListQuery{
		VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true, ExcludeNotes: !l.Access.Notes},
		Tag:           []string{name},
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	for _, t := range tags {
		if t.Tag == name {
			c.JSON(http.StatusOK, a.tagObjects(c, []repo.ItemTag{t})[0])
			return
		}
	}
	Fail(c, http.StatusNotFound, "Tag not found")
}

const maxDeleteTags = 50

// deleteTags removes tags from every item that has them and records the tag
// names in the delete log, so other clients drop them too.
func (a *API) deleteTags(c *gin.Context) {
	l := lib(c)
	if !l.Access.Write {
		Fail(c, http.StatusForbidden, "Forbidden")
		return
	}
	tags := zotero.ParseTagList(c.Query("tag"))
	if len(tags) == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	if len(tags) > maxDeleteTags {
		Fail(c, http.StatusRequestEntityTooLarge, "Too many tags (max 50)")
		return
	}
	pre, err := a.preconditionRequired(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	drop := make(map[string]bool, len(tags))
	for _, t := range tags {
		drop[t] = true
	}
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		if err := checkLibraryVersion(tx, pre); err != nil {
			return err
		}
		affected := map[string]repo.Object{}
		var existing []string
		for _, tag := range tags {
			found := false
			seq, err := tx.Objects(ctx, repo.ListQuery{
				VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true}, Tag: []string{tag},
			})
			if err != nil {
				return err
			}
			for o, err := range seq {
				if err != nil {
					return err
				}
				affected[o.Key] = o
				found = true
			}
			if found {
				existing = append(existing, tag)
			}
		}
		// Nothing carries these tags, so there is nothing to change. Moving
		// the version here would make every client resync for no reason.
		if len(existing) == 0 {
			return nil
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		objs := make([]repo.Object, 0, len(affected))
		for _, o := range affected {
			no, err := stripTags(o, drop, ver)
			if err != nil {
				return err
			}
			objs = append(objs, no)
		}
		if err := tx.PutObjects(ctx, repo.TypeItem, objs); err != nil {
			return err
		}
		return tx.LogDeleted(ctx, repo.TypeTag, existing, ver)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}

// stripTags removes the named tags from an item and rehashes it.
func stripTags(o repo.Object, drop map[string]bool, version int64) (repo.Object, error) {
	var m zotero.OrderedMap
	if err := json.Unmarshal(o.Data, &m); err != nil {
		return o, err
	}
	raw, _ := m.Get("tags")
	var tags []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tags); err != nil {
		return o, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(tags))
	for _, t := range tags {
		var name string
		_ = json.Unmarshal(t["tag"], &name)
		if !drop[name] {
			kept = append(kept, t)
		}
	}
	if err := m.SetValue("tags", kept); err != nil {
		return o, err
	}
	return rebuildItem(o.Key, m, version)
}
