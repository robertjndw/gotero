// Package adminapi implements the JSON API behind the web UI, documented in
// docs/dev/admin-api.md. It is separate from the Zotero API: it uses cookie
// sessions with CSRF protection instead of API keys.
package adminapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/jobs"
	"github.com/robertjndw/gotero/internal/repo"
)

// GC is implemented by the garbage collector for orphaned blobs
// (*jobs.Runner).
type GC interface {
	GC(ctx context.Context, dryRun bool) (jobs.GCResult, error)
}

// Notifier lets the streaming layer push events after the API changes state.
type Notifier interface {
	// LoginCompleted fires once a login session has been approved.
	LoginCompleted(token string)
	// AccessChanged fires when a user gains or loses access to a group
	// library, so connected clients get topicAdded/topicRemoved.
	AccessChanged(userID int64)
}

func (a *API) accessChanged(userIDs ...int64) {
	if a.Notifier == nil {
		return
	}
	for _, id := range userIDs {
		a.Notifier.AccessChanged(id)
	}
}

// ImportKeyCheck is what zotero.org said about an API key.
type ImportKeyCheck struct {
	// Valid is false when zotero.org rejected the key.
	Valid bool
	// Library is whether the key may read the user's library.
	Library bool
	// Files is whether the key may download attachment files.
	Files bool
}

// ImportValidator asks zotero.org whether a key can be imported from. A
// non-nil error means zotero.org could not be reached or misbehaved, which
// is different from rejecting the key.
type ImportValidator interface {
	ValidateImportKey(ctx context.Context, apiKey string) (ImportKeyCheck, error)
}

type Deps struct {
	Store    repo.Store
	Auth     *auth.Authenticator
	Accounts *accounts.Service
	Log      *slog.Logger
	Config   *config.Config
	Now      func() time.Time
	Version  string

	// GC and Notifier are optional.
	GC       GC
	Notifier Notifier
	// ImportValidator checks zotero.org keys when an import is queued. Without
	// one, any well-formed key is accepted and fails later in the worker.
	ImportValidator ImportValidator
}

type API struct {
	Deps
}

func New(d Deps) *API {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &API{Deps: d}
}

// Register mounts all routes on r (the server uses /admin/api).
func (a *API) Register(r gin.IRouter) {
	r.Use(a.noStore)

	// Public routes: no session, no CSRF token, but POSTs still check Origin
	// so a foreign page cannot make a victim's browser create accounts or
	// probe passwords.
	pub := r.Group("", a.checkOrigin)
	pub.GET("/setup", a.handle(a.getSetup))
	pub.GET("/invites/:token", a.handle(a.getInvite))
	pub.POST("/login", a.handle(a.login))
	pub.POST("/register", a.handle(a.register))

	// Logout works without a valid session so a stale cookie can be cleared.
	r.POST("/logout", a.checkOrigin, a.handle(a.logout))

	u := r.Group("", a.requireUser)
	u.GET("/me", a.handle(a.getMe))
	u.PATCH("/me", a.handle(a.patchMe))
	u.POST("/me/password", a.handle(a.changePassword))
	u.GET("/me/keys", a.handle(a.listKeys))
	u.POST("/me/keys", a.handle(a.createKey))
	u.DELETE("/me/keys/:id", a.handle(a.deleteKey))
	u.GET("/me/groups", a.handle(a.myGroups))

	u.GET("/login-sessions/:token", a.handle(a.getLoginSession))
	u.POST("/login-sessions/:token/approve", a.handle(a.approveLoginSession))
	u.POST("/login-sessions/:token/cancel", a.handle(a.cancelLoginSession))

	u.GET("/groups", a.handle(a.listGroups))
	u.POST("/groups", a.handle(a.createGroup))
	u.GET("/groups/:id", a.handle(a.getGroup))
	u.PATCH("/groups/:id", a.handle(a.patchGroup))
	u.DELETE("/groups/:id", a.handle(a.deleteGroup))
	u.GET("/groups/:id/members", a.handle(a.listMembers))
	u.PUT("/groups/:id/members/:userId", a.handle(a.putMember))
	u.DELETE("/groups/:id/members/:userId", a.handle(a.removeMember))
	u.GET("/users/lookup", a.handle(a.lookupUsers))

	u.GET("/imports", a.handle(a.listImports))
	u.POST("/imports", a.handle(a.createImport))
	u.GET("/imports/:id", a.handle(a.getImport))
	u.POST("/imports/:id/cancel", a.handle(a.cancelImport))

	adm := r.Group("/admin", a.requireUser, requireAdmin)
	adm.GET("/stats", a.handle(a.stats))
	adm.GET("/users", a.handle(a.adminListUsers))
	adm.POST("/users", a.handle(a.adminCreateUser))
	adm.PATCH("/users/:id", a.handle(a.adminPatchUser))
	adm.DELETE("/users/:id", a.handle(a.adminDeleteUser))
	adm.GET("/invites", a.handle(a.adminListInvites))
	adm.POST("/invites", a.handle(a.adminCreateInvite))
	adm.DELETE("/invites/:id", a.handle(a.adminDeleteInvite))
	adm.GET("/settings", a.handle(a.getSettings))
	adm.PATCH("/settings", a.handle(a.patchSettings))
	adm.POST("/gc", a.handle(a.runGC))
}

// noStore keeps browsers and proxies from caching anything that depends on
// the session.
func (a *API) noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
}

// apiError is an error with a defined HTTP shape.
type apiError struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]string
	Header  map[string]string
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newErr(status int, code, msg string) *apiError {
	return &apiError{Status: status, Code: code, Message: msg}
}

func (e *apiError) withFields(f map[string]string) *apiError {
	e.Fields = f
	return e
}

func fieldError(field, msg string) *apiError {
	return newErr(http.StatusUnprocessableEntity, "validation", "Validation failed.").withFields(map[string]string{field: msg})
}

var (
	errUnauthenticated = newErr(http.StatusUnauthorized, "unauthenticated", "Please log in.")
	errForbidden       = newErr(http.StatusForbidden, "forbidden", "You are not allowed to do that.")
	errAdminOnly       = newErr(http.StatusForbidden, "forbidden", "Administrator access required.")
	errCSRF            = newErr(http.StatusForbidden, "csrf", "Invalid or missing CSRF token.")
	errOrigin          = newErr(http.StatusForbidden, "csrf", "Request origin does not match this server.")
	errNotFound        = newErr(http.StatusNotFound, "not_found", "Not found.")
)

// handle adapts an error-returning handler. Anything that is not an
// *apiError after translation is a server fault and logged.
func (a *API) handle(fn func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			a.fail(c, err)
		}
	}
}

func (a *API) fail(c *gin.Context, err error) {
	ae := a.translate(c, err)
	for k, v := range ae.Header {
		c.Header(k, v)
	}
	body := gin.H{"code": ae.Code, "message": ae.Message}
	if len(ae.Fields) > 0 {
		body["fields"] = ae.Fields
	}
	// Abort so a failing middleware stops the chain.
	c.AbortWithStatusJSON(ae.Status, gin.H{"error": body})
}

// translate maps domain and repo errors to the contract. Client mistakes
// never end up as 500.
func (a *API) translate(c *gin.Context, err error) *apiError {
	var ae *apiError
	var ve *accounts.ValidationError
	var ce *accounts.ConflictError
	switch {
	case errors.As(err, &ae):
		return ae
	case errors.As(err, &ve):
		return newErr(http.StatusUnprocessableEntity, "validation", "Validation failed.").withFields(ve.Fields)
	case errors.As(err, &ce):
		return newErr(http.StatusConflict, "conflict", "That value is already in use.").withFields(ce.Fields)
	case errors.Is(err, accounts.ErrRegistrationClosed):
		return newErr(http.StatusForbidden, "registration_closed", "Registration is by invitation only.")
	case errors.Is(err, accounts.ErrInvalidInvite):
		return newErr(http.StatusForbidden, "invalid_invite", "This invitation is invalid, used or expired.")
	case errors.Is(err, accounts.ErrLastAdmin):
		return newErr(http.StatusConflict, "last_admin", "The last administrator cannot be removed.")
	case errors.Is(err, accounts.ErrSelfModification):
		return newErr(http.StatusConflict, "self_modification", "You cannot demote, disable or delete your own account.")
	case errors.Is(err, accounts.ErrOwnsGroups):
		return newErr(http.StatusConflict, "owns_groups", "This user still owns groups. Transfer or delete them first.")
	case errors.Is(err, repo.ErrNotFound):
		return errNotFound
	case errors.Is(err, repo.ErrConflict):
		return newErr(http.StatusConflict, "conflict", "The request conflicts with the current state.")
	case errors.Is(err, repo.ErrBusy):
		return &apiError{
			Status: http.StatusServiceUnavailable, Code: "busy", Message: "The server is busy, try again.",
			Header: map[string]string{"Retry-After": "2"},
		}
	case errors.Is(err, context.Canceled):
		return newErr(499, "canceled", "Request canceled.")
	}
	a.Log.ErrorContext(c.Request.Context(), "admin api error", "path", c.Request.URL.Path, "err", err)
	return newErr(http.StatusInternalServerError, "internal", "Something went wrong.")
}
