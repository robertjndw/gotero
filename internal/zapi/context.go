package zapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

type libraryKind int

const (
	kindUser libraryKind = iota
	kindGroup
)

const (
	ctxPrincipal = "zapi.principal"
	ctxLibrary   = "zapi.library"
)

// LibraryContext is the resolved library of a request, available to handlers
// behind the Library middleware.
type LibraryContext struct {
	Type string // "user" or "group"
	// ID is the public user or group ID from the URL.
	ID int64
	// LibraryID is the internal library row used with the Store.
	LibraryID int64
	Name      string
	Access    auth.LibraryAccess
	Principal auth.Principal
	// Group is set for group libraries.
	Group *repo.Group
	// Reader is a lock-free view taken when the request started.
	Reader repo.LibraryReader
}

// Topic is the stream topic of the library, which doubles as its API path
// (e.g. /users/1).
func (l *LibraryContext) Topic() string {
	return fmt.Sprintf("/%ss/%d", l.Type, l.ID)
}

// LibraryFrom returns the library resolved by the Library middleware.
func LibraryFrom(c *gin.Context) (*LibraryContext, bool) {
	l, ok := c.Get(ctxLibrary)
	if !ok {
		return nil, false
	}
	return l.(*LibraryContext), true
}

func lib(c *gin.Context) *LibraryContext {
	l, _ := LibraryFrom(c)
	return l
}

// PrincipalFrom returns the authenticated principal.
func PrincipalFrom(c *gin.Context) (auth.Principal, bool) {
	p, ok := c.Get(ctxPrincipal)
	if !ok {
		return auth.Principal{}, false
	}
	return p.(auth.Principal), true
}

// apiVersion rejects every API version but 3. A missing header means 3, like
// on zotero.org.
func (a *API) apiVersion() gin.HandlerFunc {
	return func(c *gin.Context) {
		v := c.GetHeader("Zotero-API-Version")
		if v == "" {
			v = c.Query("v")
		}
		if v != "" && v != "3" {
			Fail(c, http.StatusBadRequest, "Invalid Zotero-API-Version: only version 3 is supported")
		}
	}
}

// body applies size and gzip limits to routes that read a JSON body.
func (a *API) body() gin.HandlerFunc {
	return decodeBody(a.maxReq)
}

// Authenticated resolves the API key. A missing or unknown key is a 403,
// which is what the Zotero client treats as "key is invalid".
func (a *API) Authenticated() gin.HandlerFunc {
	return func(c *gin.Context) {
		p, err := a.auth.Authenticate(c.Request.Context(), auth.KeyFromRequest(c.Request))
		switch {
		case err == nil:
			c.Set(ctxPrincipal, p)
		case errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrInvalidKey):
			Fail(c, http.StatusForbidden, "Forbidden")
		default:
			a.FailErr(c, err)
		}
	}
}

// Library resolves the library named by the :id path parameter, checks that
// the key may read it, and stamps Last-Modified-Version on the response so
// that every later answer, errors included, carries it. It also answers 304
// for GETs whose If-Modified-Since-Version is current.
func (a *API) Library(kind libraryKind) gin.HandlerFunc {
	return func(c *gin.Context) {
		p, ok := PrincipalFrom(c)
		if !ok {
			Fail(c, http.StatusForbidden, "Forbidden")
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			Fail(c, http.StatusBadRequest, "Invalid library ID")
			return
		}
		ctx := c.Request.Context()
		l := &LibraryContext{ID: id, Principal: p}
		switch kind {
		case kindUser:
			l.Type = "user"
			l.Access = auth.UserLibraryAccess(p, id)
			if !l.Access.Read {
				Fail(c, http.StatusForbidden, "Forbidden")
				return
			}
			l.LibraryID = p.User.LibraryID
			l.Name = p.User.Username
		case kindGroup:
			l.Type = "group"
			g, err := a.store.Groups().Get(ctx, id)
			if errors.Is(err, repo.ErrNotFound) {
				Fail(c, http.StatusNotFound, "Group not found")
				return
			}
			if err != nil {
				a.FailErr(c, err)
				return
			}
			role, err := a.roleIn(c, g, p.User.ID)
			if err != nil {
				a.FailErr(c, err)
				return
			}
			l.Access = auth.GroupLibraryAccess(p, g, role)
			if !l.Access.Read {
				Fail(c, http.StatusForbidden, "Forbidden")
				return
			}
			l.Group, l.LibraryID, l.Name = &g, g.LibraryID, g.Name
		}
		r, err := a.store.Library(ctx, l.LibraryID)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		l.Reader = r
		c.Set(ctxLibrary, l)
		setVersion(c, r.Version())

		if c.Request.Method == http.MethodGet {
			since, present, err := zotero.ParseVersionHeader(c.GetHeader("If-Modified-Since-Version"))
			if err != nil {
				Fail(c, http.StatusBadRequest, "Invalid If-Modified-Since-Version")
				return
			}
			if present && since >= r.Version() {
				c.AbortWithStatus(http.StatusNotModified)
			}
		}
	}
}

// roleIn returns the user's role in a group: "owner", "admin", "member" or
// "" for outsiders.
func (a *API) roleIn(c *gin.Context, g repo.Group, userID int64) (string, error) {
	if g.OwnerID == userID {
		return "owner", nil
	}
	members, err := a.store.Groups().Members(c.Request.Context(), g.ID)
	if err != nil {
		return "", err
	}
	for _, m := range members {
		if m.UserID == userID {
			return m.Role, nil
		}
	}
	return "", nil
}
