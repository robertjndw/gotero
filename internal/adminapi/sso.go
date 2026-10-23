package adminapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/sso"
)

const (
	// ssoCookie binds a flow to the browser that started it, so a callback
	// URL planted in another browser (login CSRF) is refused.
	ssoCookie = "gotero_sso"
	// ssoInviteCookie lets a failed invite signup return to /register with
	// the invite token. Only the token's hash is stored server-side.
	ssoInviteCookie = "gotero_sso_invite"
	ssoCookiePath   = "/auth"

	ssoStateTTL = 10 * time.Minute
	// A stashed SAML identity only has to survive one redirect.
	ssoStashTTL = 2 * time.Minute
	// Zotero login session tokens are 64 alphanumeric characters.
	maxSSOTokenLen = 256
)

// RegisterSSO mounts the OIDC and SAML endpoints (the server uses /auth).
// They live outside /admin/api because the IdP-facing URLs answer with
// redirects and the SAML ACS is a cross-site POST that checkOrigin would
// reject.
func (a *API) RegisterSSO(r gin.IRouter) {
	r.Use(a.noStore)

	r.GET("/oidc/:provider/start", a.ssoHandle(func(c *gin.Context) error { return ssoStart[sso.OIDC](a, c) }))
	r.GET("/oidc/:provider/callback", a.ssoHandle(a.oidcCallback))
	r.GET("/saml/:provider/start", a.ssoHandle(func(c *gin.Context) error { return ssoStart[sso.SAML](a, c) }))
	r.POST("/saml/:provider/acs", a.ssoHandle(a.samlACS))
	r.GET("/saml/:provider/finish", a.ssoHandle(a.samlFinish))
	r.GET("/saml/:provider/metadata", a.handle(a.samlMetadata))
}

// ssoHandle turns unexpected errors into a redirect to the login page: the
// browser is mid-navigation and cannot show a JSON error body.
func (a *API) ssoHandle(fn func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			a.ssoFail(c, nil, "sso_failed", err)
		}
	}
}

// ssoFail redirects to the page the flow started from. Details only go to
// the log: IdP error text must not reach a URL.
func (a *API) ssoFail(c *gin.Context, st *repo.SSOState, code string, cause error) {
	args := []any{"code", code, "path", c.Request.URL.Path}
	if st != nil {
		args = append(args, "provider", st.Provider)
	}
	if cause != nil {
		args = append(args, "err", cause)
	}
	a.Log.WarnContext(c.Request.Context(), "sso login failed", args...)

	target := "/login?error=" + code
	switch {
	case st != nil && st.LinkUserID != nil:
		target = "/account?error=" + code
	case st != nil && len(st.InviteHash) > 0:
		if tok, ok := a.inviteFromCookie(c, st); ok {
			target = "/register?invite=" + url.QueryEscape(tok) + "&error=" + code
		}
	}
	c.Redirect(http.StatusSeeOther, target)
}

// providerAs looks up the provider named in the path and requires it to be
// a T, so an OIDC route never serves a SAML provider or the other way round.
func providerAs[T sso.Provider](a *API, c *gin.Context) (T, bool) {
	p, _ := a.SSO.Get(c.Param("provider"))
	t, ok := p.(T)
	return t, ok
}

// providerOrRedirect is providerAs for the browser-facing flow endpoints: an
// unknown name sends the user back to the login page.
func providerOrRedirect[T sso.Provider](a *API, c *gin.Context) (T, bool) {
	p, ok := providerAs[T](a, c)
	if !ok {
		c.Redirect(http.StatusSeeOther, "/login?error=unknown_provider")
	}
	return p, ok
}

// safeRedirect accepts only same-origin absolute paths. Backslashes and
// control characters are refused too because browsers treat "/\" like "//".
func safeRedirect(s string) bool {
	if len(s) == 0 || len(s) > 2048 || s[0] != '/' {
		return false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f || s[i] == '\\' {
			return false
		}
	}
	return true
}

func isTokenChars(s string) bool {
	if s == "" || len(s) > maxSSOTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

func (a *API) setSSOCookie(c *gin.Context, name, value string, maxAge int) {
	a.writeCookie(c, &http.Cookie{Name: name, Value: value, Path: ssoCookiePath, MaxAge: maxAge})
}

func (a *API) clearSSOCookies(c *gin.Context) {
	a.setSSOCookie(c, ssoCookie, "", -1)
	a.setSSOCookie(c, ssoInviteCookie, "", -1)
}

// boundToBrowser checks the binding cookie against the stored hash.
func boundToBrowser(c *gin.Context, st repo.SSOState) bool {
	v, err := c.Cookie(ssoCookie)
	if err != nil || v == "" {
		return false
	}
	return subtle.ConstantTimeCompare(auth.HashToken(v), st.BindingHash) == 1
}

// inviteFromCookie returns the raw invite token only if it still matches
// the hash stored with the flow.
func (a *API) inviteFromCookie(c *gin.Context, st *repo.SSOState) (string, bool) {
	v, err := c.Cookie(ssoInviteCookie)
	if err != nil || !isTokenChars(v) {
		return "", false
	}
	if subtle.ConstantTimeCompare(auth.HashToken(v), st.InviteHash) != 1 {
		return "", false
	}
	return v, true
}

func ssoStart[T sso.Provider](a *API, c *gin.Context) error {
	ctx := c.Request.Context()
	p, ok := providerOrRedirect[T](a, c)
	if !ok {
		return nil
	}

	st := repo.SSOState{Provider: p.Name(), ReturnTo: "/"}
	var invite string
	if c.Query("link") == "1" {
		_, u, err := a.loadSession(c)
		if err != nil {
			if errors.Is(err, errUnauthenticated) {
				c.Redirect(http.StatusSeeOther, "/login?error=sso_state")
				return nil
			}
			return err
		}
		st.LinkUserID = &u.ID
		st.ReturnTo = "/account"
	} else {
		if invite = c.Query("invite"); invite != "" {
			// Check now so a bad link fails before a round trip to the IdP.
			_, valid, err := a.Accounts.LookupInvite(ctx, invite)
			if err != nil {
				return err
			}
			if !valid || !isTokenChars(invite) {
				c.Redirect(http.StatusSeeOther, "/register?invite="+url.QueryEscape(invite)+"&error=invalid_invite")
				return nil
			}
			st.InviteHash = auth.HashToken(invite)
		}
		// The session token is validated and the path built here, so the
		// stored return path never contains caller-supplied URL syntax.
		if s := c.Query("session"); isTokenChars(s) {
			st.ReturnTo = "/login?session=" + url.QueryEscape(s)
		} else if r := c.Query("redirect"); safeRedirect(r) {
			st.ReturnTo = r
		}
	}

	binding, _ := c.Cookie(ssoCookie)
	if len(binding) < 32 || len(binding) > 128 {
		var err error
		if binding, err = randomB64(32); err != nil {
			return err
		}
	}
	st.BindingHash = auth.HashToken(binding)

	state, err := randomB64(32)
	if err != nil {
		return err
	}
	st.IDHash = auth.HashToken(state)
	st.ExpiresAt = a.Now().Add(ssoStateTTL)

	var dest string
	switch pv := any(p).(type) {
	case sso.OIDC:
		if st.Nonce, err = randomB64(32); err != nil {
			return err
		}
		st.PKCEVerifier = sso.NewPKCEVerifier()
		dest, err = pv.AuthURL(ctx, state, st.Nonce, st.PKCEVerifier)
	case sso.SAML:
		dest, st.RequestID, err = pv.AuthnRequest(ctx, state)
	default:
		err = errors.New("unsupported provider type")
	}
	if err != nil {
		return err
	}
	if err := a.Store.SSOStates().Create(ctx, st); err != nil {
		return err
	}

	a.setSSOCookie(c, ssoCookie, binding, int(ssoStateTTL/time.Second))
	inviteMaxAge := int(ssoStateTTL / time.Second)
	if invite == "" {
		inviteMaxAge = -1
	}
	a.setSSOCookie(c, ssoInviteCookie, invite, inviteMaxAge)
	c.Redirect(http.StatusFound, dest)
	return nil
}

// takeState redeems the flow state named by the request. A nil result means
// the failure redirect was already sent.
func (a *API) takeState(c *gin.Context, p sso.Provider) (*repo.SSOState, error) {
	stateParam := c.Query("state")
	if stateParam == "" {
		a.ssoFail(c, nil, "sso_state", errors.New("missing state"))
		return nil, nil
	}
	st, err := a.Store.SSOStates().Take(c.Request.Context(), auth.HashToken(stateParam), a.Now())
	if errors.Is(err, repo.ErrNotFound) {
		a.ssoFail(c, nil, "sso_state", errors.New("unknown, expired or replayed state"))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Provider != p.Name() || !boundToBrowser(c, st) {
		// Passing the state only picks the page to return to, so a link
		// attempt lands back on /account where the error is shown.
		a.ssoFail(c, &st, "sso_state", errors.New("state does not match provider or browser"))
		return nil, nil
	}
	return &st, nil
}

func (a *API) oidcCallback(c *gin.Context) error {
	p, ok := providerOrRedirect[sso.OIDC](a, c)
	if !ok {
		return nil
	}
	st, err := a.takeState(c, p)
	if st == nil {
		return err
	}
	if e := c.Query("error"); e != "" {
		a.ssoFail(c, st, "sso_failed", errors.New("idp returned "+e+": "+c.Query("error_description")))
		return nil
	}
	code := c.Query("code")
	if code == "" {
		a.ssoFail(c, st, "sso_failed", errors.New("missing code"))
		return nil
	}
	ident, err := p.Exchange(c.Request.Context(), code, st.Nonce, st.PKCEVerifier)
	if err != nil {
		a.ssoFail(c, st, "sso_failed", err)
		return nil
	}
	return a.completeSSO(c, p, *st, ident)
}

// samlACS runs on a cross-site POST, so the binding cookie is not sent. It
// validates the response and parks the identity on the flow state; the
// follow-up GET to finish carries the cookie and does the actual login.
func (a *API) samlACS(c *gin.Context) error {
	p, ok := providerOrRedirect[sso.SAML](a, c)
	if !ok {
		return nil
	}
	ctx := c.Request.Context()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, sso.MaxACSBody)
	if err := c.Request.ParseForm(); err != nil {
		a.ssoFail(c, nil, "sso_failed", err)
		return nil
	}
	relay := c.Request.PostForm.Get("RelayState")
	if relay == "" {
		a.ssoFail(c, nil, "sso_state", errors.New("missing relay state (idp-initiated login is not supported)"))
		return nil
	}
	hash := auth.HashToken(relay)
	now := a.Now()
	st, err := a.Store.SSOStates().Get(ctx, hash, now)
	if errors.Is(err, repo.ErrNotFound) {
		a.ssoFail(c, nil, "sso_state", errors.New("unknown or expired relay state"))
		return nil
	}
	if err != nil {
		return err
	}
	if st.Provider != p.Name() || st.Identity != nil {
		a.ssoFail(c, nil, "sso_state", errors.New("relay state does not match provider or was used"))
		return nil
	}
	ident, err := p.ParseResponse(c.Request, st.RequestID)
	if err != nil {
		a.ssoFail(c, &st, "sso_failed", err)
		return nil
	}
	raw, err := json.Marshal(ident)
	if err != nil {
		return err
	}
	if err := a.Store.SSOStates().Stash(ctx, hash, raw, now.Add(ssoStashTTL), now); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			a.ssoFail(c, nil, "sso_state", err)
			return nil
		}
		return err
	}
	c.Redirect(http.StatusSeeOther, "/auth/saml/"+url.PathEscape(p.Name())+"/finish?state="+url.QueryEscape(relay))
	return nil
}

func (a *API) samlFinish(c *gin.Context) error {
	p, ok := providerOrRedirect[sso.SAML](a, c)
	if !ok {
		return nil
	}
	st, err := a.takeState(c, p)
	if st == nil {
		return err
	}
	var ident sso.Identity
	if st.Identity == nil {
		a.ssoFail(c, st, "sso_state", errors.New("no stashed identity"))
		return nil
	}
	if err := json.Unmarshal(st.Identity, &ident); err != nil {
		a.ssoFail(c, st, "sso_state", err)
		return nil
	}
	return a.completeSSO(c, p, *st, ident)
}

func (a *API) samlMetadata(c *gin.Context) error {
	p, ok := providerAs[sso.SAML](a, c)
	if !ok {
		return errNotFound
	}
	md, err := p.Metadata(c.Request.Context())
	if err != nil {
		return err
	}
	c.Data(http.StatusOK, "application/samlmetadata+xml", md)
	return nil
}

// ssoErrorCode maps domain errors to the codes of the contract. Anything
// unexpected is reported as sso_failed.
func ssoErrorCode(err error) string {
	switch {
	case errors.Is(err, accounts.ErrNoAccount):
		return "no_account"
	case errors.Is(err, accounts.ErrDisabled):
		return "disabled"
	case errors.Is(err, accounts.ErrInvalidInvite):
		return "invalid_invite"
	case errors.Is(err, accounts.ErrInviteEmailMismatch):
		return "invite_email_mismatch"
	case errors.Is(err, accounts.ErrLinkRequired):
		return "link_required"
	case errors.Is(err, accounts.ErrIdentityInUse):
		return "identity_in_use"
	}
	return "sso_failed"
}

// completeSSO finishes a verified IdP login: either it connects the identity
// to the signed-in user (link=1) or it signs the user in.
func (a *API) completeSSO(c *gin.Context, p sso.Provider, st repo.SSOState, ident sso.Identity) error {
	ctx := c.Request.Context()
	ext := accounts.ExternalIdentity{Provider: p.Name(), Identity: ident}
	dest := st.ReturnTo
	if !safeRedirect(dest) {
		dest = "/"
	}

	if st.LinkUserID != nil {
		// The session may have ended or switched users while the browser was
		// at the IdP; linking to someone else's account would be an
		// account-takeover primitive.
		_, u, err := a.loadSession(c)
		if err != nil || u.ID != *st.LinkUserID {
			a.ssoFail(c, &st, "sso_state", err)
			return nil
		}
		if err := a.Accounts.LinkExternal(ctx, u.ID, ext); err != nil {
			a.ssoFail(c, &st, ssoErrorCode(err), err)
			return nil
		}
		a.clearSSOCookies(c)
		c.Redirect(http.StatusSeeOther, dest)
		return nil
	}

	u, err := a.Accounts.LoginExternal(ctx, accounts.ExternalLogin{
		Identity:   ext,
		Policy:     accounts.ExternalPolicy{AutoProvision: p.AutoProvision(), LinkByEmail: p.LinkByEmail()},
		InviteHash: st.InviteHash,
	})
	if err != nil {
		a.ssoFail(c, &st, ssoErrorCode(err), err)
		return nil
	}
	// Never keep a previous session alive across a login (session fixation).
	if old, cerr := c.Cookie(cookieName); cerr == nil && old != "" {
		if derr := a.Store.WebSessions().Delete(ctx, auth.HashToken(old)); derr != nil && !errors.Is(derr, repo.ErrNotFound) {
			a.Log.WarnContext(ctx, "could not delete replaced web session", "err", derr)
		}
	}
	if _, err := a.startSession(c, u); err != nil {
		return err
	}
	a.clearSSOCookies(c)
	c.Redirect(http.StatusSeeOther, dest)
	return nil
}
