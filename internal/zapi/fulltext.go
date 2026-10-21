package zapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

func (a *API) getFulltextVersions(c *gin.Context) {
	l := lib(c)
	since, err := parseSince(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	m, err := l.Reader.FulltextVersions(c.Request.Context(), since)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	c.JSON(http.StatusOK, m)
}

func (a *API) getFulltext(c *gin.Context) {
	l := lib(c)
	key := c.Param("key")
	if !zotero.ValidKey(key) {
		Fail(c, http.StatusNotFound, "Not found")
		return
	}
	f, err := l.Reader.Fulltext(c.Request.Context(), key)
	if errors.Is(err, repo.ErrNotFound) {
		Fail(c, http.StatusNotFound, "Not found")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	c.JSON(http.StatusOK, zotero.FulltextContent{
		Content:      f.Content,
		IndexedChars: deref(f.IndexedChars), TotalChars: deref(f.TotalChars),
		IndexedPages: deref(f.IndexedPages), TotalPages: deref(f.TotalPages),
	})
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

type fulltextBody struct {
	Key          string `json:"key"`
	Content      string `json:"content"`
	IndexedChars *int   `json:"indexedChars"`
	TotalChars   *int   `json:"totalChars"`
	IndexedPages *int   `json:"indexedPages"`
	TotalPages   *int   `json:"totalPages"`
}

func (f fulltextBody) toRepo(key string, version int64) repo.Fulltext {
	return repo.Fulltext{
		ItemKey: key, Content: f.Content, Version: version,
		IndexedChars: f.IndexedChars, TotalChars: f.TotalChars,
		IndexedPages: f.IndexedPages, TotalPages: f.TotalPages,
	}
}

// sameAs reports whether storing f would change nothing. Clients re-upload
// full text after every local reindex, and each real write would bump the
// library version and wake every other client for no reason.
func (f fulltextBody) sameAs(cur repo.Fulltext) bool {
	eq := func(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	return f.Content == cur.Content &&
		eq(f.IndexedChars, cur.IndexedChars) && eq(f.TotalChars, cur.TotalChars) &&
		eq(f.IndexedPages, cur.IndexedPages) && eq(f.TotalPages, cur.TotalPages)
}

// unchangedFulltext reports whether the item already has exactly this text.
func unchangedFulltext(ctx context.Context, tx repo.LibraryReader, key string, in fulltextBody) (bool, error) {
	cur, err := tx.Fulltext(ctx, key)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return in.sameAs(cur), nil
}

func (f fulltextBody) valid() bool {
	for _, p := range []*int{f.IndexedChars, f.TotalChars, f.IndexedPages, f.TotalPages} {
		if p != nil && *p < 0 {
			return false
		}
	}
	return true
}

func (a *API) putFulltext(c *gin.Context) {
	l, ok := a.requireWrite(c)
	if !ok {
		return
	}
	key := c.Param("key")
	if !zotero.ValidKey(key) {
		Fail(c, http.StatusNotFound, "Not found")
		return
	}
	b, err := readBody(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	var in fulltextBody
	if err := json.Unmarshal(b, &in); err != nil || !in.valid() {
		Fail(c, http.StatusBadRequest, "Invalid full-text data")
		return
	}
	pre, err := parsePrecondition(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		if err := checkLibraryVersion(tx, pre); err != nil {
			return err
		}
		found, err := tx.Get(ctx, repo.TypeItem, []string{key})
		if err != nil {
			return err
		}
		if _, ok := found[key]; !ok {
			return &httpError{status: http.StatusNotFound, version: tx.Version(), msg: "Item not found"}
		}
		if same, err := unchangedFulltext(ctx, tx, key, in); err != nil || same {
			return err
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		return tx.PutFulltext(ctx, []repo.Fulltext{in.toRepo(key, ver)})
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}

// postFulltext stores full text for several items and answers like an object
// write, so the client can reuse its handling of failed entries.
func (a *API) postFulltext(c *gin.Context) {
	l, ok := a.requireWrite(c)
	if !ok {
		return
	}
	pre, err := a.preconditionRequired(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	raws, err := readArray(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	ctx := c.Request.Context()
	resp := zotero.NewWriteResponse()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		if err := checkLibraryVersion(tx, pre); err != nil {
			return err
		}
		type item struct {
			idx int
			in  fulltextBody
		}
		var items []item
		var keys []string
		for i, raw := range raws {
			var in fulltextBody
			idx := strconv.Itoa(i)
			if err := json.Unmarshal(raw, &in); err != nil || !in.valid() {
				resp.Failed[idx] = zotero.WriteFailure{Key: in.Key, Code: http.StatusBadRequest, Message: "Invalid full-text data"}
				continue
			}
			if !zotero.ValidKey(in.Key) {
				resp.Failed[idx] = zotero.WriteFailure{Key: in.Key, Code: http.StatusBadRequest, Message: "Invalid key"}
				continue
			}
			items = append(items, item{i, in})
			keys = append(keys, in.Key)
		}
		found, err := tx.Get(ctx, repo.TypeItem, keys)
		if err != nil {
			return err
		}
		var puts []item
		for _, it := range items {
			if _, ok := found[it.in.Key]; !ok {
				resp.Failed[strconv.Itoa(it.idx)] = zotero.WriteFailure{
					Key: it.in.Key, Code: http.StatusNotFound, Message: "Item doesn't exist (it may have been deleted)",
				}
				continue
			}
			same, err := unchangedFulltext(ctx, tx, it.in.Key, it.in)
			if err != nil {
				return err
			}
			if same {
				resp.Unchanged[strconv.Itoa(it.idx)] = it.in.Key
				continue
			}
			puts = append(puts, it)
		}
		if len(puts) == 0 {
			return nil
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		fts := make([]repo.Fulltext, len(puts))
		for i, it := range puts {
			fts[i] = it.in.toRepo(it.in.Key, ver)
			idx := strconv.Itoa(it.idx)
			resp.Success[idx] = it.in.Key
			resp.Successful[idx] = json.RawMessage(fmt.Sprintf(`{"key":%q,"version":%d}`, it.in.Key, ver))
		}
		return tx.PutFulltext(ctx, fts)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.JSON(http.StatusOK, resp)
}
