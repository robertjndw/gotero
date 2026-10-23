package adminapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
)

const (
	cookieName      = "gotero_session"
	sessionLifetime = 14 * 24 * time.Hour
	// A refresh writes to the database, so do it at most once per hour.
	sessionRefreshAfter = time.Hour

	ctxUser    = "gotero.user"
	ctxSession = "gotero.session"
)

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// checkOrigin requires unsafe requests to come from our own origin. Browsers
// always send Origin on cross-site POSTs, with Referer as the fallback for
// old ones; a request with neither is not from a browser page we served.
func (a *API) checkOrigin(c *gin.Context) {
	if isSafeMethod(c.Request.Method) {
		return
	}
	base := httpx.BaseURL(c.Request.Context())
	origin := c.GetHeader("Origin")
	if origin == "" || origin == "null" {
		if ref := c.GetHeader("Referer"); ref != "" {
			if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
				origin = u.Scheme + "://" + u.Host
			}
		} else {
			origin = ""
		}
	}
	if base == "" || !strings.EqualFold(origin, base) {
		a.fail(c, errOrigin)
	}
}

func (a *API) secureCookies(c *gin.Context) bool {
	return strings.HasPrefix(httpx.BaseURL(c.Request.Context()), "https://")
}

func (a *API) setCookie(c *gin.Context, token string, expires time.Time) {
	a.writeCookie(c, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", Expires: expires, MaxAge: int(sessionLifetime / time.Second),
	})
}

func (a *API) clearCookie(c *gin.Context) {
	a.writeCookie(c, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
}

// writeCookie sets the flags every cookie of this API shares.
func (a *API) writeCookie(c *gin.Context, ck *http.Cookie) {
	ck.HttpOnly, ck.Secure, ck.SameSite = true, a.secureCookies(c), http.SameSiteLaxMode
	//nolint:gosec // Secure follows the public URL scheme; plain http is allowed for local setups.
	http.SetCookie(c.Writer, ck)
}

// startSession creates a session for u, sets the cookie and the CSRF header.
func (a *API) startSession(c *gin.Context, u repo.User) (string, error) {
	token, err := randomB64(32)
	if err != nil {
		return "", err
	}
	csrf, err := randomB64(32)
	if err != nil {
		return "", err
	}
	exp := a.Now().Add(sessionLifetime)
	if err := a.Store.WebSessions().Create(c.Request.Context(), repo.WebSession{
		IDHash: auth.HashToken(token), UserID: u.ID, CSRFToken: csrf, ExpiresAt: exp,
	}); err != nil {
		return "", err
	}
	a.setCookie(c, token, exp)
	c.Header("X-CSRF-Token", csrf)
	return csrf, nil
}

// loadSession resolves the cookie to a live session and an enabled user.
// It returns errUnauthenticated for anything short of that.
func (a *API) loadSession(c *gin.Context) (repo.WebSession, repo.User, error) {
	cookie, err := c.Cookie(cookieName)
	if err != nil || cookie == "" {
		return repo.WebSession{}, repo.User{}, errUnauthenticated
	}
	ctx := c.Request.Context()
	idHash := auth.HashToken(cookie)
	sess, err := a.Store.WebSessions().Get(ctx, idHash)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.WebSession{}, repo.User{}, errUnauthenticated
	}
	if err != nil {
		return repo.WebSession{}, repo.User{}, err
	}
	now := a.Now()
	if !sess.ExpiresAt.After(now) {
		_ = a.Store.WebSessions().Delete(ctx, idHash)
		return repo.WebSession{}, repo.User{}, errUnauthenticated
	}
	u, err := a.Store.Users().Get(ctx, sess.UserID)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && u.Disabled) {
		_ = a.Store.WebSessions().Delete(ctx, idHash)
		return repo.WebSession{}, repo.User{}, errUnauthenticated
	}
	if err != nil {
		return repo.WebSession{}, repo.User{}, err
	}
	// Sliding expiry: only write when at least refresh-after has passed since
	// the last extension, which is when the stored expiry fell that far
	// below a full lifetime.
	if want := now.Add(sessionLifetime); want.Sub(sess.ExpiresAt) >= sessionRefreshAfter {
		if err := a.Store.WebSessions().Extend(ctx, idHash, want); err != nil && !errors.Is(err, repo.ErrNotFound) {
			a.Log.WarnContext(ctx, "could not extend web session", "err", err)
		} else {
			sess.ExpiresAt = want
			a.setCookie(c, cookie, want)
		}
	}
	return sess, u, nil
}

// requireUser authenticates the cookie and enforces CSRF on unsafe methods.
// Login state is checked first so anonymous callers get 401, which the UI
// turns into a redirect, rather than a confusing 403.
func (a *API) requireUser(c *gin.Context) {
	sess, u, err := a.loadSession(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.Header("X-CSRF-Token", sess.CSRFToken)
	if !isSafeMethod(c.Request.Method) {
		if a.checkOrigin(c); c.IsAborted() {
			return
		}
		got := c.GetHeader("X-CSRF-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
			a.fail(c, errCSRF)
			return
		}
	}
	c.Set(ctxUser, u)
	c.Set(ctxSession, sess)
	c.Next()
}

func requireAdmin(c *gin.Context) {
	if !currentUser(c).IsAdmin {
		// fail() is a method, but the shape is fixed, so write it directly.
		c.AbortWithStatusJSON(errAdminOnly.Status, gin.H{"error": gin.H{"code": errAdminOnly.Code, "message": errAdminOnly.Message}})
		return
	}
	c.Next()
}

func currentUser(c *gin.Context) repo.User {
	u, _ := c.Get(ctxUser)
	return u.(repo.User)
}

func currentSession(c *gin.Context) repo.WebSession {
	s, _ := c.Get(ctxSession)
	return s.(repo.WebSession)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *API) login(c *gin.Context) error {
	var in loginRequest
	if err := bind(c, &in); err != nil {
		return err
	}
	u, err := a.Accounts.Login(c.Request.Context(), in.Username, in.Password, httpx.ClientIP(c.Request.Context()))
	var limited *accounts.RateLimitedError
	switch {
	case errors.As(err, &limited):
		return &apiError{
			Status: http.StatusTooManyRequests, Code: "rate_limited",
			Message: "Too many failed attempts. Try again later.",
			Header:  map[string]string{"Retry-After": httpx.RetryAfter(limited.RetryAfter)},
		}
	case errors.Is(err, accounts.ErrBadCredentials):
		return newErr(http.StatusUnauthorized, "invalid_credentials", "Invalid username or password.")
	case errors.Is(err, accounts.ErrDisabled):
		// The password was right, so telling the user why is not a leak.
		return newErr(http.StatusForbidden, "disabled", "This account is disabled.")
	case err != nil:
		return err
	}
	csrf, err := a.startSession(c, u)
	if err != nil {
		return err
	}
	me, err := a.meDTO(c.Request.Context(), u, csrf)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, me)
	return nil
}

func (a *API) logout(c *gin.Context) error {
	cookie, err := c.Cookie(cookieName)
	if err == nil && cookie != "" {
		idHash := auth.HashToken(cookie)
		sess, gerr := a.Store.WebSessions().Get(c.Request.Context(), idHash)
		if gerr == nil {
			// A logout with a wrong CSRF token must not end someone's session
			// from a third-party page.
			got := c.GetHeader("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
				return errCSRF
			}
			if derr := a.Store.WebSessions().Delete(c.Request.Context(), idHash); derr != nil {
				return derr
			}
		}
	}
	a.clearCookie(c)
	c.Status(http.StatusNoContent)
	return nil
}

func (a *API) getMe(c *gin.Context) error {
	me, err := a.meDTO(c.Request.Context(), currentUser(c), currentSession(c).CSRFToken)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, me)
	return nil
}
