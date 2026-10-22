package zapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// precond is a parsed If-Unmodified-Since-Version header.
type precond struct {
	v  int64
	ok bool
}

func parsePrecondition(c *gin.Context) (precond, error) {
	v, present, err := zotero.ParseVersionHeader(c.GetHeader("If-Unmodified-Since-Version"))
	if err != nil {
		return precond{}, errStatus(http.StatusBadRequest, "Invalid If-Unmodified-Since-Version")
	}
	return precond{v: v, ok: present}, nil
}

// preconditionRequired is for destructive requests, which zotero.org refuses
// to run blind.
func (a *API) preconditionRequired(c *gin.Context) (precond, error) {
	p, err := parsePrecondition(c)
	if err != nil {
		return p, err
	}
	if !p.ok {
		return p, errStatus(http.StatusPreconditionRequired, "If-Unmodified-Since-Version not provided")
	}
	return p, nil
}

// checkLibraryVersion enforces If-Unmodified-Since-Version against the
// locked library. A client that claims to be ahead of us is let through:
// refusing would wedge it, and the next sync sorts things out.
func checkLibraryVersion(r repo.LibraryReader, p precond) error {
	if !p.ok {
		return nil
	}
	if cur := r.Version(); cur > p.v {
		return &httpError{
			status: http.StatusPreconditionFailed, version: cur,
			msg: fmt.Sprintf("Library has been modified since specified version (expected %d, found %d)", p.v, cur),
		}
	}
	return nil
}

// finishWrite stamps the final library version and tells the stream about it
// once, and only if this request really moved the version.
func (a *API) finishWrite(c *gin.Context, l *LibraryContext, before, after int64) {
	setVersion(c, after)
	if after != before {
		a.pub.Publish(Event{Topic: l.Topic(), Version: after})
	}
}

func (a *API) requireWrite(c *gin.Context) (*LibraryContext, bool) {
	l := lib(c)
	if !l.Access.Write {
		Fail(c, http.StatusForbidden, "Forbidden")
		return l, false
	}
	return l, true
}

func readBody(c *gin.Context) ([]byte, error) {
	b, err := io.ReadAll(c.Request.Body)
	if err != nil {
		if httpx.IsTooLarge(err) {
			return nil, errStatus(http.StatusRequestEntityTooLarge, "Request body too large")
		}
		return nil, errStatus(http.StatusBadRequest, "Could not read request body")
	}
	return b, nil
}

func writeToken(c *gin.Context) (string, error) {
	tok := c.GetHeader("Zotero-Write-Token")
	if tok != "" && len(tok) != 32 {
		return "", errStatus(http.StatusBadRequest, "Zotero-Write-Token must be 32 characters")
	}
	return tok, nil
}

func (a *API) planOptions(l *LibraryContext, t repo.ObjectType, replace bool) planOpts {
	return planOpts{
		Kind: t, Schema: a.schema, Now: a.now(), Replace: replace, NewKey: zotero.NewKey,
		DenyNotes: !l.Access.Notes, Group: l.Type == "group",
	}
}

// postObjects handles POST /{items|collections|searches}.
func (a *API) postObjects(t repo.ObjectType) gin.HandlerFunc {
	return func(c *gin.Context) {
		l, ok := a.requireWrite(c)
		if !ok {
			return
		}
		raws, err := readArray(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		pre, err := parsePrecondition(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		tok, err := writeToken(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}

		ctx := c.Request.Context()
		opts := a.planOptions(l, t, false)
		var (
			before   int64
			outcomes []outcome
		)
		v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
			before = tx.Version()
			if err := checkLibraryVersion(tx, pre); err != nil {
				return err
			}
			if tok != "" {
				fresh, err := tx.ClaimWriteToken(ctx, tok)
				if err != nil {
					return err
				}
				if !fresh {
					return &httpError{
						status: http.StatusPreconditionFailed, version: tx.Version(),
						msg: "Write token has already been used",
					}
				}
			}
			var perr error
			if outcomes, perr = planWrite(ctx, tx, opts, raws); perr != nil {
				return perr
			}
			_, perr = persist(ctx, tx, t, outcomes)
			return perr
		})
		if err != nil {
			a.FailErr(c, err)
			return
		}
		a.finishWrite(c, l, before, v)
		a.respondWrite(c, l, t, outcomes)
	}
}

// persist stores created and updated objects under one new library version
// and stamps that version on the outcomes. It returns 0 if nothing changed,
// in which case the library version must not move.
func persist(ctx context.Context, tx repo.LibraryTx, t repo.ObjectType, outcomes []outcome) (int64, error) {
	var idx []int
	var created []string
	for i, o := range outcomes {
		switch o.Status {
		case outCreated:
			created = append(created, o.Key)
			idx = append(idx, i)
		case outUpdated:
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return 0, nil
	}
	ver, err := tx.NextVersion(ctx)
	if err != nil {
		return 0, err
	}
	objs := make([]repo.Object, len(idx))
	for j, i := range idx {
		outcomes[i].Obj.Version = ver
		objs[j] = outcomes[i].Obj
	}
	if err := tx.PutObjects(ctx, t, objs); err != nil {
		return 0, err
	}
	// A key that comes back after a delete must not stay in the delete log.
	if err := tx.ClearDeleted(ctx, t, created); err != nil {
		return 0, err
	}
	if t == repo.TypeItem {
		// Same for tags: after DELETE tags?tag=foo, putting foo on an item
		// again must not leave it in deleted.tags, or other clients would
		// strip it from their copies on the next sync.
		if err := tx.ClearDeleted(ctx, repo.TypeTag, tagNames(objs)); err != nil {
			return 0, err
		}
	}
	return ver, nil
}

func tagNames(objs []repo.Object) []string {
	seen := map[string]bool{}
	var names []string
	for _, o := range objs {
		for _, t := range o.Tags {
			if !seen[t.Tag] {
				seen[t.Tag] = true
				names = append(names, t.Tag)
			}
		}
	}
	return names
}

func (a *API) respondWrite(c *gin.Context, l *LibraryContext, t repo.ObjectType, outcomes []outcome) {
	resp := zotero.NewWriteResponse()
	b := a.newBuilder(c, l)
	for _, o := range outcomes {
		idx := strconv.Itoa(o.Index)
		switch o.Status {
		case outCreated, outUpdated:
			raw, err := b.appendObject(nil, t, o.Obj, zotero.Meta{})
			if err != nil {
				a.FailErr(c, err)
				return
			}
			resp.Successful[idx] = raw
			resp.Success[idx] = o.Key
		case outUnchanged:
			resp.Unchanged[idx] = o.Key
		case outFailed:
			resp.Failed[idx] = o.Fail
		}
	}
	c.JSON(http.StatusOK, resp)
}

// putObject handles PUT and PATCH on a single object. Success is 204.
func (a *API) putObject(t repo.ObjectType, replace bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		l, ok := a.requireWrite(c)
		if !ok {
			return
		}
		key := c.Param("key")
		if !zotero.ValidKey(key) {
			Fail(c, http.StatusBadRequest, "Invalid key")
			return
		}
		body, err := readObjectBody(c, maxObjectKeys)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		var m zotero.OrderedMap
		if err := json.Unmarshal(body, &m); err != nil || m.Len() == 0 {
			Fail(c, http.StatusBadRequest, "Uploaded data must be a JSON object")
			return
		}
		if k, has := m.GetString("key"); has && k != key {
			Fail(c, http.StatusBadRequest, "Key in the body does not match the URL")
			return
		}
		m.Set("key", quote(key))
		raw, _ := json.Marshal(m)
		pre, err := parsePrecondition(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}

		ctx := c.Request.Context()
		opts := a.planOptions(l, t, replace)
		var before int64
		v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
			before = tx.Version()
			stored, err := tx.Get(ctx, t, []string{key})
			if err != nil {
				return err
			}
			cur, exists := stored[key]
			if !exists {
				return &httpError{status: http.StatusNotFound, version: tx.Version(), msg: kindLabel(t) + " not found"}
			}
			// For a single object the header refers to the object's version.
			if pre.ok && cur.Version > pre.v {
				return &httpError{
					status: http.StatusPreconditionFailed, version: tx.Version(),
					msg: fmt.Sprintf("%s has been modified since specified version (expected %d, found %d)",
						kindLabel(t), pre.v, cur.Version),
				}
			}
			outcomes, err := planWrite(ctx, tx, opts, []json.RawMessage{raw})
			if err != nil {
				return err
			}
			if o := outcomes[0]; o.Status == outFailed {
				return &httpError{status: o.Fail.Code, msg: o.Fail.Message, version: tx.Version()}
			}
			_, err = persist(ctx, tx, t, outcomes)
			return err
		})
		if err != nil {
			a.FailErr(c, err)
			return
		}
		a.finishWrite(c, l, before, v)
		c.Status(http.StatusNoContent)
	}
}

// deleteObjects handles DELETE /{type}?{type}Key=K1,K2.
func (a *API) deleteObjects(t repo.ObjectType) gin.HandlerFunc {
	return func(c *gin.Context) {
		l, ok := a.requireWrite(c)
		if !ok {
			return
		}
		pre, err := a.preconditionRequired(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		param := zotero.ObjectType(t).KeyParam()
		keys, err := zotero.ParseKeyListMax(c.Query(param), maxObjectsPerWrite)
		switch {
		case err != nil && isTooMany(err):
			Fail(c, http.StatusRequestEntityTooLarge, fmt.Sprintf("Too many keys (max %d)", maxObjectsPerWrite))
			return
		case errors.Is(err, zotero.ErrNoValidKeys):
			// Nothing to delete, so nothing to version either.
			a.deleteNothing(c, l, pre)
			return
		case err != nil:
			Fail(c, http.StatusBadRequest, "Invalid "+param)
			return
		case len(keys) == 0:
			Fail(c, http.StatusBadRequest, param+" not provided")
			return
		}
		ctx := c.Request.Context()
		var before int64
		v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
			before = tx.Version()
			if err := checkLibraryVersion(tx, pre); err != nil {
				return err
			}
			if err := denyHiddenNotes(ctx, tx, l, t, keys); err != nil {
				return err
			}
			return deleteKeys(ctx, tx, t, keys)
		})
		if err != nil {
			a.FailErr(c, err)
			return
		}
		a.finishWrite(c, l, before, v)
		c.Status(http.StatusNoContent)
	}
}

// deleteNothing answers a delete whose key list had only malformed keys. The
// precondition is still checked so a stale client sees the same 412 as it
// would for a real delete, but the library version stays put.
func (a *API) deleteNothing(c *gin.Context, l *LibraryContext, pre precond) {
	ctx := c.Request.Context()
	var v int64
	_, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		v = tx.Version()
		return checkLibraryVersion(tx, pre)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	setVersion(c, v)
	c.Status(http.StatusNoContent)
}

// deleteObject handles DELETE /{type}/{key}.
func (a *API) deleteObject(t repo.ObjectType) gin.HandlerFunc {
	return func(c *gin.Context) {
		l, ok := a.requireWrite(c)
		if !ok {
			return
		}
		pre, err := a.preconditionRequired(c)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		key := c.Param("key")
		if !zotero.ValidKey(key) {
			Fail(c, http.StatusNotFound, kindLabel(t)+" not found")
			return
		}
		ctx := c.Request.Context()
		var before int64
		v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
			before = tx.Version()
			stored, err := tx.Get(ctx, t, []string{key})
			if err != nil {
				return err
			}
			cur, exists := stored[key]
			if !exists {
				return &httpError{status: http.StatusNotFound, version: tx.Version(), msg: kindLabel(t) + " not found"}
			}
			if cur.Version > pre.v {
				return &httpError{
					status: http.StatusPreconditionFailed, version: tx.Version(),
					msg: fmt.Sprintf("%s has been modified since specified version (expected %d, found %d)",
						kindLabel(t), pre.v, cur.Version),
				}
			}
			if !l.Access.Notes && cur.ItemType == "note" {
				return &httpError{status: http.StatusForbidden, version: tx.Version(), msg: errNotesDenied.Msg}
			}
			return deleteKeys(ctx, tx, t, []string{key})
		})
		if err != nil {
			a.FailErr(c, err)
			return
		}
		a.finishWrite(c, l, before, v)
		c.Status(http.StatusNoContent)
	}
}

// denyHiddenNotes refuses a delete that names a note the key cannot see.
func denyHiddenNotes(ctx context.Context, tx repo.LibraryTx, l *LibraryContext, t repo.ObjectType, keys []string) error {
	if l.Access.Notes || t != repo.TypeItem {
		return nil
	}
	found, err := tx.Get(ctx, t, keys)
	if err != nil {
		return err
	}
	for _, o := range found {
		if o.ItemType == "note" {
			return &httpError{status: http.StatusForbidden, version: tx.Version(), msg: errNotesDenied.Msg}
		}
	}
	return nil
}

func isTooMany(err error) bool { return errors.Is(err, zotero.ErrTooManyKeys) }
