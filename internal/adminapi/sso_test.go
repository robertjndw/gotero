package adminapi_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/sso"
	"github.com/robertjndw/gotero/internal/sso/ssotest"
)

// ssoEnv is an env with one OIDC provider ("corp") and one SAML provider
// ("okta") pointed at the in-process fake IdPs.
type ssoEnv struct {
	*env
	oidc *ssotest.OIDCServer
	idp  *ssotest.SAMLIdP
}

func newSSOEnv(t *testing.T, mod func(*config.Config)) *ssoEnv {
	t.Helper()
	oidc := ssotest.NewOIDCServer(t)
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	metadata := idp.WriteMetadataFile(t)
	e := newEnv(t, envOpts{cfg: func(c *config.Config) {
		c.OIDC = []config.OIDCProvider{{
			Name: "corp", Issuer: oidc.Issuer(), ClientID: ssotest.OIDCClientID, ClientSecret: "s3cret",
			DisplayName: "Corp SSO", Scopes: []string{"openid", "email", "profile"}, UsernameClaim: "preferred_username",
			AutoProvision: true,
		}}
		c.SAML = []config.SAMLProvider{{
			Name: "okta", IDPMetadataFile: metadata, DisplayName: "Okta", AutoProvision: true,
			EmailAttribute: "email", UsernameAttribute: "uid", NameAttribute: "displayName",
		}}
		c.SAMLSP = config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile}
		if mod != nil {
			mod(c)
		}
	}})
	p, ok := e.sso.Get("okta")
	require.True(t, ok)
	md, err := p.(sso.SAML).Metadata(context.Background())
	require.NoError(t, err)
	idp.TrustSP(t, md)
	oidc.SetClaims(adaClaims())
	return &ssoEnv{env: e, oidc: oidc, idp: idp}
}

func adaClaims() map[string]any {
	return map[string]any{
		"sub": "user-1", "email": "ada@example.org", "email_verified": true,
		"preferred_username": "ada", "name": "Ada Lovelace",
	}
}

var adaSAML = ssotest.SAMLUser{NameID: "ada-id", Email: "ada@example.org", Username: "ada", DisplayName: "Ada Lovelace"}

// browser returns a cookie-jar client that never follows redirects, so tests
// can inspect each hop.
func (e *env) browser() *client {
	c := e.client()
	c.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

type hop struct {
	status int
	header http.Header
	body   string
}

func (h hop) location() string { return h.header.Get("Location") }

func (c *client) rawDo(method, target string, form url.Values, hdr map[string]string) hop {
	c.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = c.e.srv.URL + target
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	require.NoError(c.t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return hop{status: resp.StatusCode, header: resp.Header, body: string(raw)}
}

func (c *client) get(target string) hop { return c.rawDo("GET", target, nil, nil) }

func (c *client) cookie(name string) string {
	u, _ := url.Parse(c.e.srv.URL + "/auth/")
	for _, ck := range c.hc.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// oidcStart runs start and the IdP hop and returns the callback path.
func (e *ssoEnv) oidcStart(t *testing.T, b *client, query string) (callback string) {
	t.Helper()
	h := b.get("/auth/oidc/corp/start" + query)
	require.Equal(t, http.StatusFound, h.status, h.body)
	require.Equal(t, "no-store", h.header.Get("Cache-Control"))
	code, state := e.oidc.Authorize(t, h.location())
	return "/auth/oidc/corp/callback?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
}

func (e *ssoEnv) oidcLogin(t *testing.T, b *client, query string) hop {
	t.Helper()
	return b.get(e.oidcStart(t, b, query))
}

func TestSSOOIDCRoundTrip(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	h := e.oidcLogin(t, b, "")
	require.Equal(t, http.StatusSeeOther, h.status)
	assert.Equal(t, "/", h.location())
	assert.Empty(t, b.cookie("gotero_sso"), "binding cookie is cleared after login")

	me := b.mustOK("GET", "/me", nil)
	assert.Equal(t, "ada", me["username"])
	assert.Equal(t, false, me["isAdmin"])
	assert.Equal(t, false, me["hasPassword"])
	assert.Equal(t, []any{"corp"}, me["providers"])
	ids := me["identities"].([]any)
	require.Len(t, ids, 1)
	id := ids[0].(map[string]any)
	assert.Equal(t, "Corp SSO", id["providerName"])
	assert.Equal(t, "oidc", id["kind"])

	// A second login finds the existing link instead of provisioning again.
	b2 := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b2, "").location())
	assert.Equal(t, me["id"], b2.mustOK("GET", "/me", nil)["id"])
}

func TestSSOBindingCookieFlags(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	h := b.get("/auth/oidc/corp/start")
	var found bool
	for _, ck := range (&http.Response{Header: h.header}).Cookies() {
		if ck.Name != "gotero_sso" {
			continue
		}
		found = true
		assert.True(t, ck.HttpOnly)
		assert.Equal(t, http.SameSiteLaxMode, ck.SameSite)
		assert.Equal(t, "/auth", ck.Path)
		assert.Equal(t, 600, ck.MaxAge)
		assert.False(t, ck.Secure, "test server is plain http")
	}
	assert.True(t, found)
	first := b.cookie("gotero_sso")
	b.get("/auth/oidc/corp/start")
	assert.Equal(t, first, b.cookie("gotero_sso"), "binding cookie is reused for parallel tabs")
}

func TestSSOOIDCNoAutoProvision(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.OIDC[0].AutoProvision = false })
	b := e.browser()
	h := e.oidcLogin(t, b, "")
	assert.Equal(t, "/login?error=no_account", h.location())
	assert.Equal(t, 401, b.do("GET", "/me", nil).status)
}

func TestSSOReturnPaths(t *testing.T) {
	e := newSSOEnv(t, nil)
	tests := []struct{ name, query, want string }{
		{"session", "?session=abc123DEF-_", "/login?session=abc123DEF-_"},
		{"redirect", "?redirect=" + url.QueryEscape("/groups/12?x=1"), "/groups/12?x=1"},
		{"protocol relative", "?redirect=" + url.QueryEscape("//evil.example"), "/"},
		{"backslash", "?redirect=" + url.QueryEscape("/\\evil.example"), "/"},
		{"absolute", "?redirect=" + url.QueryEscape("https://evil.example"), "/"},
		{"bad session token", "?session=" + url.QueryEscape("x&redirect=//evil"), "/"},
		{"session wins", "?session=tok&redirect=/groups", "/login?session=tok"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.oidc.SetClaims(map[string]any{
				"sub": "sub-" + string(rune('a'+i)), "email": "u" + string(rune('a'+i)) + "@example.org",
				"email_verified": true, "preferred_username": "user" + string(rune('a'+i)),
			})
			h := e.oidcLogin(t, e.browser(), tc.query)
			assert.Equal(t, tc.want, h.location())
		})
	}
}

func TestSSOStateChecks(t *testing.T) {
	e := newSSOEnv(t, nil)

	t.Run("no cookie", func(t *testing.T) {
		b := e.browser()
		cb := e.oidcStart(t, b, "")
		other := e.browser()
		assert.Equal(t, "/login?error=sso_state", other.get(cb).location())
		assert.Equal(t, 401, other.do("GET", "/me", nil).status)
	})
	t.Run("wrong cookie", func(t *testing.T) {
		b := e.browser()
		cb := e.oidcStart(t, b, "")
		other := e.browser()
		other.get("/auth/oidc/corp/start") // gets its own binding cookie
		assert.Equal(t, "/login?error=sso_state", other.get(cb).location())
	})
	t.Run("replay", func(t *testing.T) {
		b := e.browser()
		cb := e.oidcStart(t, b, "")
		keep := b.cookie("gotero_sso")
		require.Equal(t, "/", b.get(cb).location())
		// Put the binding cookie back as an attacker replaying the URL with
		// the victim's cookie would have it.
		b.hc.Jar.SetCookies(mustURL(e.srv.URL+"/auth/"), []*http.Cookie{{Name: "gotero_sso", Value: keep, Path: "/auth"}})
		assert.Equal(t, "/login?error=sso_state", b.get(cb).location())
	})
	t.Run("unknown state", func(t *testing.T) {
		b := e.browser()
		assert.Equal(t, "/login?error=sso_state", b.get("/auth/oidc/corp/callback?code=x&state=nope").location())
		assert.Equal(t, "/login?error=sso_state", b.get("/auth/oidc/corp/callback").location())
	})
	t.Run("expired", func(t *testing.T) {
		b := e.browser()
		cb := e.oidcStart(t, b, "")
		e.clock.Advance(11 * time.Minute)
		defer e.clock.Advance(-11 * time.Minute)
		assert.Equal(t, "/login?error=sso_state", b.get(cb).location())
	})
	t.Run("provider mismatch", func(t *testing.T) {
		b := e.browser()
		cb := e.oidcStart(t, b, "")
		cb = strings.Replace(cb, "/oidc/corp/", "/oidc/other/", 1)
		assert.Equal(t, "/login?error=unknown_provider", b.get(cb).location())
	})
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestSSOIdPErrorsDoNotLeak(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	h := b.get("/auth/oidc/corp/start")
	_, state := e.oidc.Authorize(t, h.location())
	got := b.get("/auth/oidc/corp/callback?error=access_denied&error_description=" + url.QueryEscape("secret <script>") + "&state=" + url.QueryEscape(state))
	assert.Equal(t, "/login?error=sso_failed", got.location())
}

func TestSSOIdPDownAtStart(t *testing.T) {
	e := newSSOEnv(t, nil)
	e.oidc.SetDown(true)
	// Discovery is lazy, so the first start is what notices the outage.
	h := e.browser().get("/auth/oidc/corp/start")
	assert.Equal(t, "/login?error=sso_failed", h.location())
}

func TestSSOUnknownProvider(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	assert.Equal(t, "/login?error=unknown_provider", b.get("/auth/oidc/nope/start").location())
	// A SAML provider is not reachable through the OIDC routes.
	assert.Equal(t, "/login?error=unknown_provider", b.get("/auth/oidc/okta/start").location())
	assert.Equal(t, 404, b.get("/auth/saml/nope/metadata").status)
}

func TestSSOInviteSignup(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.OIDC[0].AutoProvision = false })
	_, tok, err := e.acc.CreateInvite(ctx(), accounts.CreateInviteInput{IsAdmin: true, TTL: time.Hour})
	require.NoError(t, err)

	b := e.browser()
	h := e.oidcLogin(t, b, "?invite="+url.QueryEscape(tok))
	assert.Equal(t, "/", h.location())
	me := b.mustOK("GET", "/me", nil)
	assert.Equal(t, "ada", me["username"])
	assert.Equal(t, true, me["isAdmin"], "the invite's admin flag is honoured")

	// The invite is single use.
	other := e.browser()
	e.oidc.SetClaims(map[string]any{"sub": "user-2", "email": "grace@example.org", "email_verified": true, "preferred_username": "grace"})
	h = other.get("/auth/oidc/corp/start?invite=" + url.QueryEscape(tok))
	assert.Equal(t, "/register?invite="+url.QueryEscape(tok)+"&error=invalid_invite", h.location())
}

func TestSSOInviteErrorsReturnToRegister(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.OIDC[0].AutoProvision = false })
	_, tok, err := e.acc.CreateInvite(ctx(), accounts.CreateInviteInput{Email: "other@example.org", TTL: time.Hour})
	require.NoError(t, err)

	b := e.browser()
	h := e.oidcLogin(t, b, "?invite="+url.QueryEscape(tok))
	assert.Equal(t, "/register?invite="+url.QueryEscape(tok)+"&error=invite_email_mismatch", h.location())

	// Without the matching cookie the invite token is not echoed back.
	b2 := e.browser()
	cb := e.oidcStart(t, b2, "?invite="+url.QueryEscape(tok))
	b2.hc.Jar.SetCookies(mustURL(e.srv.URL+"/auth/"), []*http.Cookie{{Name: "gotero_sso_invite", Value: "wrong", Path: "/auth"}})
	assert.Equal(t, "/login?error=invite_email_mismatch", b2.get(cb).location())
}

func TestSSOLinkByEmailSkipsAdmins(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.OIDC[0].LinkByEmail = true })
	e.oidc.SetClaims(map[string]any{"sub": "root-sub", "email": "root@example.com", "email_verified": true, "preferred_username": "root"})
	h := e.oidcLogin(t, e.browser(), "")
	assert.Equal(t, "/login?error=link_required", h.location())

	// A regular user with a matching email is linked.
	e.user("bob")
	_, err := e.pool.Exec(ctx(), "UPDATE users SET email = 'bob@example.org', email_verified = true WHERE username = 'bob'")
	require.NoError(t, err)
	e.oidc.SetClaims(map[string]any{"sub": "bob-sub", "email": "bob@example.org", "email_verified": true, "preferred_username": "bob"})
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())
	assert.Equal(t, "bob", b.mustOK("GET", "/me", nil)["username"])
}

// Regression: a user could put someone else's address on their own account
// and so receive that person's first SSO login.
func TestSSOLinkByEmailNeedsVouchedEmail(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.OIDC[0].LinkByEmail = true })
	e.user("mallory")
	m := e.login("mallory", "password-mallory")
	m.mustOK("PATCH", "/me", map[string]any{"email": "alice@corp.example"})

	e.oidc.SetClaims(map[string]any{"sub": "alice-sub", "email": "alice@corp.example", "email_verified": true, "preferred_username": "alice"})
	assert.Equal(t, "/login?error=link_required", e.oidcLogin(t, e.browser(), "").location())

	// Once an admin sets the address, gotero vouches for it.
	admin := e.adminClient()
	mal, err := e.store.Users().GetByUsername(ctx(), "mallory")
	require.NoError(t, err)
	admin.mustOK("PATCH", fmt.Sprintf("/admin/users/%d", mal.ID), map[string]any{"email": "alice@corp.example"})
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())
	assert.Equal(t, "mallory", b.mustOK("GET", "/me", nil)["username"])

	// Changing it again by themselves drops the vouching.
	m.mustOK("PATCH", "/me", map[string]any{"email": "carol@corp.example"})
	got, err := e.store.Users().GetByUsername(ctx(), "mallory")
	require.NoError(t, err)
	assert.False(t, got.EmailVerified)
}

func TestSSOLinkAsLoggedInAdmin(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	res := b.do("POST", "/login", map[string]string{"username": "root", "password": adminPass})
	require.Equal(t, 200, res.status, string(res.raw))

	e.oidc.SetClaims(map[string]any{"sub": "root-sub", "email": "root@corp.example", "email_verified": true, "preferred_username": "root"})
	h := e.oidcLogin(t, b, "?link=1")
	assert.Equal(t, "/account", h.location())
	me := b.mustOK("GET", "/me", nil)
	assert.Equal(t, "root", me["username"], "the session is kept")
	assert.Equal(t, []any{"corp"}, me["providers"])

	// Later SSO logins reach the admin through the link.
	b2 := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b2, "").location())
	me2 := b2.mustOK("GET", "/me", nil)
	assert.Equal(t, "root", me2["username"])
	assert.Equal(t, true, me2["isAdmin"])
}

func TestSSOLinkNeedsSession(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	assert.Equal(t, "/login?error=sso_state", b.get("/auth/oidc/corp/start?link=1").location())

	t.Run("session switched mid flow", func(t *testing.T) {
		b := e.adminBrowser()
		cb := e.oidcStart(t, b, "?link=1")
		res := b.do("POST", "/logout", nil)
		require.Equal(t, 204, res.status)
		h := b.get(cb)
		// Logout cleared only the session cookie, so the binding check passes
		// and the failure is the session check.
		assert.Equal(t, "/account?error=sso_state", h.location())
	})
	t.Run("identity in use", func(t *testing.T) {
		other := e.browser()
		e.oidc.SetClaims(map[string]any{"sub": "shared", "email": "s@example.org", "email_verified": true, "preferred_username": "shared"})
		require.Equal(t, "/", e.oidcLogin(t, other, "").location())

		b := e.adminBrowser()
		h := e.oidcLogin(t, b, "?link=1")
		assert.Equal(t, "/account?error=identity_in_use", h.location())
	})
}

func (e *env) adminBrowser() *client {
	b := e.browser()
	res := b.do("POST", "/login", map[string]string{"username": "root", "password": adminPass})
	require.Equal(e.t, 200, res.status, string(res.raw))
	return b
}

func TestSSOExistingSessionIsReplaced(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.adminBrowser()
	var oldTok string
	for _, ck := range b.hc.Jar.Cookies(mustURL(e.srv.URL)) {
		if ck.Name == "gotero_session" {
			oldTok = ck.Value
		}
	}
	require.NotEmpty(t, oldTok)
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())

	var n int
	require.NoError(t, e.poolQueryRow("SELECT count(*) FROM web_sessions WHERE user_id = $1", e.admin.ID).Scan(&n))
	assert.Equal(t, 0, n, "the previous session row is deleted")
	assert.Equal(t, "ada", b.mustOK("GET", "/me", nil)["username"])
}

// samlStart returns the IdP redirect URL for a SAML flow.
func (e *ssoEnv) samlStart(t *testing.T, b *client, query string) string {
	t.Helper()
	h := b.get("/auth/saml/okta/start" + query)
	require.Equal(t, http.StatusFound, h.status, h.body)
	return h.location()
}

func (e *ssoEnv) acs(t *testing.T, b *client, post ssotest.SAMLPost, hdr map[string]string) hop {
	t.Helper()
	u := mustURL(post.ACSURL)
	return b.rawDo("POST", u.Path, url.Values{"SAMLResponse": {post.SAMLResponse}, "RelayState": {post.RelayState}}, hdr)
}

func TestSSOSAMLRoundTrip(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	post := e.idp.Respond(t, e.samlStart(t, b, "?session=tok123"), adaSAML)
	assert.Equal(t, e.srv.URL+"/auth/saml/okta/acs", post.ACSURL)

	// The cross-site POST arrives without cookies and with a foreign Origin.
	poster := e.browser()
	h := e.acs(t, poster, post, map[string]string{"Origin": "https://idp.example.test"})
	require.Equal(t, http.StatusSeeOther, h.status, h.body)
	assert.True(t, strings.HasPrefix(h.location(), "/auth/saml/okta/finish?state="), h.location())
	assert.Equal(t, 401, poster.do("GET", "/me", nil).status, "the ACS alone does not log in")

	fin := b.get(h.location())
	require.Equal(t, http.StatusSeeOther, fin.status)
	assert.Equal(t, "/login?session=tok123", fin.location())
	me := b.mustOK("GET", "/me", nil)
	assert.Equal(t, "ada", me["username"])
	assert.Equal(t, "saml", me["identities"].([]any)[0].(map[string]any)["kind"])

	// Finish is single use.
	b.hc.Jar.SetCookies(mustURL(e.srv.URL+"/auth/"), []*http.Cookie{{Name: "gotero_sso", Value: "x", Path: "/auth"}})
	assert.Equal(t, "/login?error=sso_state", b.get(h.location()).location())
}

func TestSSOSAMLFinishNeedsBindingCookie(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	post := e.idp.Respond(t, e.samlStart(t, b, ""), adaSAML)
	h := e.acs(t, e.browser(), post, nil)
	require.Equal(t, http.StatusSeeOther, h.status)

	other := e.browser()
	assert.Equal(t, "/login?error=sso_state", other.get(h.location()).location())
	assert.Equal(t, 401, other.do("GET", "/me", nil).status)

	// The attempt was consumed, so even the right browser cannot retry.
	assert.Equal(t, "/login?error=sso_state", b.get(h.location()).location())
}

func TestSSOSAMLACSRejections(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	post := e.idp.Respond(t, e.samlStart(t, b, ""), adaSAML)

	t.Run("no relay state", func(t *testing.T) {
		p := post
		p.RelayState = ""
		assert.Equal(t, "/login?error=sso_state", e.acs(t, e.browser(), p, nil).location())
	})
	t.Run("unknown relay state", func(t *testing.T) {
		p := post
		p.RelayState = "nope"
		assert.Equal(t, "/login?error=sso_state", e.acs(t, e.browser(), p, nil).location())
	})
	t.Run("tampered", func(t *testing.T) {
		h := e.acs(t, e.browser(), post.TamperSignature(t), nil)
		assert.Equal(t, "/login?error=sso_failed", h.location())
	})
	t.Run("second post for same state", func(t *testing.T) {
		require.Equal(t, http.StatusSeeOther, e.acs(t, e.browser(), post, nil).status)
		assert.Equal(t, "/login?error=sso_state", e.acs(t, e.browser(), post, nil).location())
	})
}

func TestSSOSAMLMetadata(t *testing.T) {
	e := newSSOEnv(t, nil)
	h := e.browser().get("/auth/saml/okta/metadata")
	assert.Equal(t, 200, h.status)
	assert.Equal(t, "application/samlmetadata+xml", h.header.Get("Content-Type"))
	assert.Contains(t, h.body, e.srv.URL+"/auth/saml/okta/acs")
}

func TestSSOSetup(t *testing.T) {
	e := newSSOEnv(t, nil)
	got := e.client().mustOK("GET", "/setup", nil)
	assert.Equal(t, true, got["localLogin"])
	provs := got["providers"].([]any)
	require.Len(t, provs, 2)
	assert.Equal(t, map[string]any{"id": "corp", "kind": "oidc", "displayName": "Corp SSO", "startUrl": "/auth/oidc/corp/start"}, provs[0])
	assert.Equal(t, map[string]any{"id": "okta", "kind": "saml", "displayName": "Okta", "startUrl": "/auth/saml/okta/start"}, provs[1])

	plain := newEnv(t)
	got = plain.client().mustOK("GET", "/setup", nil)
	assert.Equal(t, true, got["localLogin"])
	assert.Equal(t, []any{}, got["providers"])
}

func TestSSOLocalLoginOff(t *testing.T) {
	e := newSSOEnv(t, func(c *config.Config) { c.LocalLogin = false })
	e.user("bob")

	assert.Equal(t, false, e.client().mustOK("GET", "/setup", nil)["localLogin"])

	c := e.client()
	res := c.do("POST", "/login", map[string]string{"username": "bob", "password": "password-bob"})
	assert.Equal(t, 401, res.status)
	assert.Equal(t, "invalid_credentials", res.errCode())

	res = c.do("POST", "/login", map[string]string{"username": "root", "password": adminPass})
	assert.Equal(t, 200, res.status, string(res.raw))

	res = e.client().do("POST", "/register", map[string]string{"username": "new", "password": "long enough pw", "email": "n@example.org"})
	assert.Equal(t, 403, res.status)
	assert.Equal(t, "local_login_disabled", res.errCode())

	// Bob signs in through SSO and still cannot set a password.
	e.oidc.SetClaims(map[string]any{"sub": "b", "email": "b@example.org", "email_verified": true, "preferred_username": "bobsso"})
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())
	b.mustOK("GET", "/me", nil) // picks up the CSRF token
	res = b.do("POST", "/me/password", map[string]string{"newPassword": "new password 123"})
	assert.Equal(t, 403, res.status)
	assert.Equal(t, "local_login_disabled", res.errCode())

	// Admins keep the password change.
	admin := e.adminClient()
	res = admin.do("POST", "/me/password", map[string]string{"currentPassword": adminPass, "newPassword": "another long pass"})
	assert.Equal(t, 204, res.status, string(res.raw))
}

func TestSSOSetFirstPassword(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())
	require.Equal(t, false, b.mustOK("GET", "/me", nil)["hasPassword"])

	b.mustOK("GET", "/me", nil)
	res := b.do("POST", "/me/password", map[string]string{"newPassword": "first password 1"})
	require.Equal(t, 204, res.status, string(res.raw))
	assert.Equal(t, true, b.mustOK("GET", "/me", nil)["hasPassword"])

	// Once a password exists the current one is required again.
	res = b.do("POST", "/me/password", map[string]string{"newPassword": "second password 2"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "currentPassword")

	// And it can be used to log in.
	assert.Equal(t, 200, e.client().do("POST", "/login", map[string]string{"username": "ada", "password": "first password 1"}).status)
}

func TestSSOUnlinkIdentity(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())

	b.mustOK("GET", "/me", nil) // picks up the CSRF token
	res := b.do("DELETE", "/me/identities/corp", nil)
	assert.Equal(t, 409, res.status)
	assert.Equal(t, "last_login_method", res.errCode())

	assert.Equal(t, 204, b.do("POST", "/me/password", map[string]string{"newPassword": "first password 1"}).status)
	assert.Equal(t, 404, b.do("DELETE", "/me/identities/okta", nil).status)
	assert.Equal(t, 204, b.do("DELETE", "/me/identities/corp", nil).status)
	assert.Equal(t, []any{}, b.mustOK("GET", "/me", nil)["providers"])
	assert.Equal(t, []any{}, b.mustOK("GET", "/me", nil)["identities"])
}

func TestSSOAdminUnlinkAndListProviders(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.browser()
	require.Equal(t, "/", e.oidcLogin(t, b, "").location())
	id := num(b.mustOK("GET", "/me", nil), "id")
	e.user("plain")

	admin := e.adminClient()
	providers := map[string]any{}
	for _, it := range items(admin.mustOK("GET", "/admin/users", nil)) {
		m := it.(map[string]any)
		providers[m["username"].(string)] = m["providers"]
	}
	assert.Equal(t, []any{"corp"}, providers["ada"])
	assert.Equal(t, []any{}, providers["plain"])

	path := "/admin/users/" + itoa(id) + "/identities/corp"
	res := admin.do("DELETE", path, nil)
	assert.Equal(t, 409, res.status, "the lockout guard applies to admins too")
	assert.Equal(t, "last_login_method", res.errCode())

	admin.mustOK("PATCH", "/admin/users/"+itoa(id), map[string]any{"password": "admin set password"})
	assert.Equal(t, 204, admin.do("DELETE", path, nil).status)
	assert.Equal(t, 404, admin.do("DELETE", path, nil).status)

	// Not an admin: forbidden.
	assert.Equal(t, 403, e.login("plain", "password-plain").do("DELETE", path, nil).status)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestSSOGetAuthUnknownIsNotSPA(t *testing.T) {
	// The webui reserved list is exercised in the server tests; here we only
	// check that the adminapi group answers /auth routes itself and leaves
	// unknown ones to the engine's 404.
	e := newSSOEnv(t, nil)
	h := e.browser().get("/auth/unknown")
	assert.Equal(t, 404, h.status)
	assert.NotContains(t, h.body, "<html")
}
