package zapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// keyInfoOut extends zotero.KeyInfo with the optional emails member.
type keyInfoOut struct {
	zotero.KeyInfo
	Emails []string `json:"emails,omitempty"`
}

func (a *API) registerKeys(g *gin.RouterGroup) {
	g.GET("/keys/current", a.Authenticated(), a.keysCurrent)
	g.DELETE("/keys/current", a.Authenticated(), a.deleteKeysCurrent)
	g.GET("/keys/:key", a.keysByKey)
	g.POST("/keys", a.body(), a.legacyLogin)
}

// accessFromKey renders a key's flags the way zotero.org reports them.
// groups.all is always present because the client reads it unconditionally.
func accessFromKey(k repo.KeyAccess) zotero.Access {
	acc := zotero.Access{
		User: zotero.UserAccess{
			Library: k.Library,
			Files:   k.Library && k.Files,
			Notes:   k.Library && k.Notes,
			Write:   k.Library && k.Write,
		},
		Groups: map[string]zotero.GroupAccess{},
	}
	switch k.Groups {
	case "all":
		acc.Groups[zotero.AllGroups] = zotero.GroupAccess{Library: true, Write: true}
	case "read":
		acc.Groups[zotero.AllGroups] = zotero.GroupAccess{Library: true}
	default:
		acc.Groups[zotero.AllGroups] = zotero.GroupAccess{}
	}
	return acc
}

// keyAccessFromRequest is the inverse, used by the legacy login.
func keyAccessFromRequest(acc *zotero.Access) repo.KeyAccess {
	if acc == nil {
		return repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"}
	}
	k := repo.KeyAccess{
		Library: acc.User.Library, Notes: acc.User.Notes,
		Write: acc.User.Write, Files: acc.User.Files, Groups: "none",
	}
	// A key limited to specific groups cannot be represented, so any group
	// grant widens to "all" for reads and stays read-only unless every
	// granted group is writable.
	var read, write bool
	for _, g := range acc.Groups {
		read = read || g.Library || g.Write
		write = write || g.Write
	}
	switch {
	case write:
		k.Groups = "all"
	case read:
		k.Groups = "read"
	}
	return k
}

func (a *API) keyInfo(u repo.User, key string, acc repo.KeyAccess, withEmail bool) keyInfoOut {
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	out := keyInfoOut{KeyInfo: zotero.KeyInfo{
		Key: key, UserID: u.ID, Username: u.Username, DisplayName: name, Access: accessFromKey(acc),
	}}
	if withEmail && u.Email != "" {
		out.Emails = []string{u.Email}
	}
	return out
}

func (a *API) keysCurrent(c *gin.Context) {
	p, _ := PrincipalFrom(c)
	c.JSON(http.StatusOK, a.keyInfo(p.User, p.Raw, p.Key.Access, c.Query("includeEmails") == "1"))
}

func (a *API) deleteKeysCurrent(c *gin.Context) {
	p, _ := PrincipalFrom(c)
	hash := auth.HashToken(p.Raw)
	if err := a.store.Keys().DeleteByHash(c.Request.Context(), hash); err != nil && !errors.Is(err, repo.ErrNotFound) {
		a.FailErr(c, err)
		return
	}
	a.auth.Invalidate(hash)
	a.notify.AccessChanged(p.User.ID)
	c.Status(http.StatusNoContent)
}

// keysByKey describes the key in the path. Like zotero.org it needs no
// credentials beyond knowing the key, but it never reveals other keys.
func (a *API) keysByKey(c *gin.Context) {
	want := c.Param("key")
	if presented := auth.KeyFromRequest(c.Request); presented != "" && presented != want {
		Fail(c, http.StatusForbidden, "Forbidden")
		return
	}
	p, err := a.auth.Authenticate(c.Request.Context(), want)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, a.keyInfo(p.User, p.Raw, p.Key.Access, c.Query("includeEmails") == "1"))
	case errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrInvalidKey):
		Fail(c, http.StatusNotFound, "Key not found")
	default:
		a.FailErr(c, err)
	}
}

type loginRequest struct {
	Username string         `json:"username"`
	Password string         `json:"password"`
	Name     string         `json:"name"`
	Access   *zotero.Access `json:"access"`
}

// legacyLogin is the Zotero 7 password login: it trades credentials for a
// fresh API key.
func (a *API) legacyLogin(c *gin.Context) {
	var req loginRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		if isTooLarge(err) {
			Fail(c, http.StatusRequestEntityTooLarge, "Request body too large")
			return
		}
		Fail(c, http.StatusBadRequest, "Invalid JSON body")
		return
	}
	ctx := c.Request.Context()
	if a.login == nil {
		Fail(c, http.StatusForbidden, "Invalid username/password")
		return
	}
	// httpx.ClientIP only trusts X-Forwarded-For from configured proxies;
	// c.ClientIP() would let a caller pick its own rate limit bucket.
	u, err := a.login(ctx, req.Username, req.Password, httpx.ClientIP(ctx))
	var limited *accounts.RateLimitedError
	switch {
	case errors.As(err, &limited):
		c.Header("Retry-After", httpx.RetryAfter(limited.RetryAfter))
		Fail(c, http.StatusTooManyRequests, "Too many login attempts")
		return
	case errors.Is(err, accounts.ErrBadCredentials), errors.Is(err, accounts.ErrDisabled):
		// Same answer for both so a disabled account is not revealed.
		Fail(c, http.StatusForbidden, "Invalid username/password")
		return
	case err != nil:
		a.FailErr(c, err)
		return
	}
	raw := zotero.NewAPIKey()
	access := keyAccessFromRequest(req.Access)
	name := req.Name
	if len(name) > 200 {
		name = name[:200]
	}
	if _, err := a.store.Keys().Create(ctx, repo.APIKey{
		UserID: u.ID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: name, Access: access,
	}); err != nil {
		a.FailErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, a.keyInfo(u, raw, access, false))
}
