package adminapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
)

func TestLoginLogoutAndCookie(t *testing.T) {
	e := newEnv(t)
	c := e.client()

	assert.Equal(t, 401, c.do("GET", "/me", nil).status, "logged out")
	assert.Equal(t, "unauthenticated", c.do("GET", "/me", nil).errCode())

	bad := c.do("POST", "/login", map[string]string{"username": "root", "password": "nope"})
	assert.Equal(t, 401, bad.status)
	assert.Equal(t, "invalid_credentials", bad.errCode())
	ghost := c.do("POST", "/login", map[string]string{"username": "ghost", "password": "nope"})
	assert.Equal(t, 401, ghost.status)
	assert.Equal(t, bad.errCode(), ghost.errCode(), "unknown users look like wrong passwords")

	res := c.do("POST", "/login", map[string]string{"username": "ROOT", "password": adminPass})
	require.Equal(t, 200, res.status, string(res.raw))
	me := res.json()
	assert.Equal(t, "root", me["username"])
	assert.Equal(t, true, me["isAdmin"])
	assert.NotEmpty(t, me["csrfToken"])
	assert.Equal(t, me["csrfToken"], res.header.Get("X-CSRF-Token"))

	var cookie *http.Cookie
	for _, ck := range res.cookies(t) {
		if ck.Name == "gotero_session" {
			cookie = ck
		}
	}
	require.NotNil(t, cookie)
	assert.True(t, cookie.HttpOnly)
	assert.False(t, cookie.Secure, "http public URL")
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.GreaterOrEqual(t, len(cookie.Value), 40)
	_, err := e.store.WebSessions().Get(ctx(), auth.HashToken(cookie.Value))
	require.NoError(t, err, "only the hash is stored under the cookie value")

	got := c.do("GET", "/me", nil)
	assert.Equal(t, 200, got.status)
	assert.Equal(t, me["csrfToken"], got.header.Get("X-CSRF-Token"))

	assert.Equal(t, 204, c.do("POST", "/logout", nil).status)
	assert.Equal(t, 401, c.do("GET", "/me", nil).status)
	_, err = e.store.WebSessions().Get(ctx(), auth.HashToken(cookie.Value))
	assert.Error(t, err, "session row removed")
	assert.Equal(t, 204, c.do("POST", "/logout", nil).status, "idempotent")
}

func TestSecureCookieOnHTTPSPublicURL(t *testing.T) {
	e := newEnv(t, envOpts{publicURL: "https://gotero.example"})
	c := e.client()
	c.origin = "https://gotero.example"
	res := c.do("POST", "/login", map[string]string{"username": "root", "password": adminPass})
	require.Equal(t, 200, res.status, string(res.raw))
	cookies := res.cookies(t)
	require.NotEmpty(t, cookies)
	assert.True(t, cookies[0].Secure)
}

func TestCSRFAndOriginChecks(t *testing.T) {
	e := newEnv(t)
	c := e.adminClient()
	body := map[string]any{"name": "k", "access": map[string]any{"library": true, "groups": "none"}}

	assert.Equal(t, 201, c.do("POST", "/me/keys", body).status)

	missing := c.do("POST", "/me/keys", body, noCSRF())
	assert.Equal(t, 403, missing.status)
	assert.Equal(t, "csrf", missing.errCode())
	assert.Equal(t, 403, c.do("POST", "/me/keys", body, withHeader("X-CSRF-Token", "wrong")).status)

	badOrigin := c.do("POST", "/me/keys", body, withHeader("Origin", "https://evil.example"))
	assert.Equal(t, 403, badOrigin.status)
	assert.Equal(t, "csrf", badOrigin.errCode())
	assert.Equal(t, 403, c.do("POST", "/me/keys", body, withHeader("Origin", "")).status, "no Origin and no Referer")
	assert.Equal(t, 403, c.do("POST", "/me/keys", body, withHeader("Origin", "null")).status)

	// Referer is the fallback when Origin is absent.
	ok := c.do("POST", "/me/keys", body, withHeader("Origin", ""), withHeader("Referer", e.srv.URL+"/account"))
	assert.Equal(t, 201, ok.status)
	evil := c.do("POST", "/me/keys", body, withHeader("Origin", ""), withHeader("Referer", "https://evil.example/x"))
	assert.Equal(t, 403, evil.status)

	// GET needs neither; and a logged-out unsafe request is 401, not 403.
	assert.Equal(t, 200, c.do("GET", "/me/keys", nil, noCSRF(), withHeader("Origin", "")).status)
	anon := e.client()
	assert.Equal(t, 401, anon.do("POST", "/me/keys", body).status)

	// Public POSTs check Origin but not CSRF.
	creds := map[string]string{"username": "root", "password": adminPass}
	assert.Equal(t, 403, anon.do("POST", "/login", creds, withHeader("Origin", "https://evil.example")).status)
	assert.Equal(t, 403, anon.do("POST", "/register", map[string]string{}, withHeader("Origin", "")).status)
	assert.Equal(t, 200, anon.do("POST", "/login", creds).status)
}

func TestSessionSlidingExpiry(t *testing.T) {
	e := newEnv(t)
	c := e.adminClient()
	expiry := func() time.Time {
		var exp time.Time
		require.NoError(t, e.poolQueryRow(`SELECT expires_at FROM web_sessions LIMIT 1`).Scan(&exp))
		return exp
	}
	first := expiry()
	assert.WithinDuration(t, time.Now().Add(14*24*time.Hour), first, time.Minute)

	e.clock.Advance(10 * time.Minute)
	require.Equal(t, 200, c.do("GET", "/me", nil).status)
	assert.True(t, first.Equal(expiry()), "no write within the hour")

	e.clock.Advance(2 * time.Hour)
	res := c.do("GET", "/me", nil)
	require.Equal(t, 200, res.status)
	assert.True(t, expiry().After(first), "expiry slid forward")
	assert.NotEmpty(t, res.header.Values("Set-Cookie"), "cookie lifetime refreshed too")

	e.clock.Advance(15 * 24 * time.Hour)
	assert.Equal(t, 401, c.do("GET", "/me", nil).status, "expired")
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	wrong := map[string]string{"username": "root", "password": "wrong"}
	for i := range 10 {
		res := c.do("POST", "/login", wrong)
		require.Equal(t, 401, res.status, "attempt %d", i)
	}
	res := c.do("POST", "/login", wrong)
	assert.Equal(t, 429, res.status)
	assert.Equal(t, "rate_limited", res.errCode())
	secs, err := parseSeconds(res.header.Get("Retry-After"))
	require.NoError(t, err)
	assert.InDelta(t, 15*60, secs, 5)

	good := c.do("POST", "/login", map[string]string{"username": "root", "password": adminPass})
	assert.Equal(t, 429, good.status, "the right password does not bypass the block")
	// Limits are per (username, IP) plus a looser per-IP budget, so other
	// accounts behind the same NAT keep working and nobody can lock a user
	// out from a different address.
	other := c.do("POST", "/login", map[string]string{"username": "someone-else", "password": "x"})
	assert.Equal(t, 401, other.status)

	e.clock.Advance(16 * time.Minute)
	assert.Equal(t, 200, c.do("POST", "/login", map[string]string{"username": "root", "password": adminPass}).status)
}

func TestSuccessfulLoginClearsUsernameFailures(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for range 9 {
		assert.Equal(t, 401, c.do("POST", "/login", map[string]string{"username": "root", "password": "x"}).status)
	}
	assert.Equal(t, 200, c.do("POST", "/login", map[string]string{"username": "root", "password": adminPass}).status)
	// Success forgives the (username, IP) attempts, so the next misses start
	// counting from zero again.
	for range 10 {
		assert.Equal(t, 401, c.do("POST", "/login", map[string]string{"username": "root", "password": "x"}).status)
	}
	assert.Equal(t, 429, c.do("POST", "/login", map[string]string{"username": "root", "password": "x"}).status)
}

func TestDisabledUserCannotLogin(t *testing.T) {
	e := newEnv(t)
	u := e.user("dave")
	dave := e.login("dave", "password-dave")
	adm := e.adminClient()
	adm.mustOK("PATCH", fmt.Sprintf("/admin/users/%d", u.ID), map[string]any{"disabled": true})
	assert.Equal(t, 401, dave.do("GET", "/me", nil).status, "existing sessions die")
	res := e.client().do("POST", "/login", map[string]string{"username": "dave", "password": "password-dave"})
	assert.Equal(t, 403, res.status)
	assert.Equal(t, "disabled", res.errCode())
	res = e.client().do("POST", "/login", map[string]string{"username": "dave", "password": "bad"})
	assert.Equal(t, 401, res.status, "wrong password does not reveal the disabled state")
}

func TestMalformedBodyIsNot500(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	res := c.do("POST", "/login", "not an object")
	assert.Equal(t, 400, res.status)
	assert.True(t, strings.Contains(res.errCode(), "bad_request"))
}
