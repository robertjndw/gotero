package sso_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/sso"
	"github.com/robertjndw/gotero/internal/sso/ssotest"
)

const publicURL = "https://zotero.example.org"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func oidcProvider(t *testing.T, srv *ssotest.OIDCServer, mod func(*config.OIDCProvider)) sso.OIDC {
	t.Helper()
	pc := config.OIDCProvider{
		Name: "corp", Issuer: srv.Issuer(), ClientID: ssotest.OIDCClientID, ClientSecret: "s3cret",
		DisplayName: "Corp", Scopes: []string{"openid", "email", "profile"}, UsernameClaim: "preferred_username",
	}
	if mod != nil {
		mod(&pc)
	}
	reg, err := sso.New(&config.Config{PublicURL: publicURL, OIDC: []config.OIDCProvider{pc}}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reg.Get("corp")
	if !ok {
		t.Fatal("provider missing")
	}
	return p.(sso.OIDC)
}

// login runs the full authorize and exchange round trip. exchangeNonce and
// exchangeVerifier let tests deviate from what AuthURL was given.
func login(t *testing.T, srv *ssotest.OIDCServer, p sso.OIDC, exchangeNonce, exchangeVerifier string) (sso.Identity, error) {
	t.Helper()
	ctx := context.Background()
	nonce, verifier := sso.NewPKCEVerifier(), sso.NewPKCEVerifier()
	u, err := p.AuthURL(ctx, "state-1", nonce, verifier)
	if err != nil {
		t.Fatal(err)
	}
	code, state := srv.Authorize(t, u)
	if state != "state-1" {
		t.Fatalf("state = %q", state)
	}
	if exchangeNonce == "" {
		exchangeNonce = nonce
	}
	if exchangeVerifier == "" {
		exchangeVerifier = verifier
	}
	return p.Exchange(ctx, code, exchangeNonce, exchangeVerifier)
}

func TestOIDCHappyPath(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	srv.SetClaims(map[string]any{
		"email": "ada@example.org", "email_verified": true,
		"preferred_username": "ada", "name": "Ada Lovelace",
	})
	p := oidcProvider(t, srv, nil)
	id, err := login(t, srv, p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := sso.Identity{Subject: "user-1", Email: "ada@example.org", EmailVerified: true, Username: "ada", DisplayName: "Ada Lovelace"}
	if id != want {
		t.Fatalf("got %+v, want %+v", id, want)
	}
	if p.Kind() != sso.KindOIDC || p.Name() != "corp" || p.DisplayName() != "Corp" {
		t.Fatal("provider metadata wrong")
	}
}

func TestOIDCAuthURL(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	p := oidcProvider(t, srv, nil)
	u, err := p.AuthURL(context.Background(), "st", "nn", sso.NewPKCEVerifier())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"redirect_uri=https%3A%2F%2Fzotero.example.org%2Fauth%2Foidc%2Fcorp%2Fcallback",
		"code_challenge_method=S256", "nonce=nn", "state=st", "scope=openid+email+profile",
	} {
		if !contains(u, want) {
			t.Errorf("auth url %q lacks %q", u, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	cases := map[string]struct {
		claims   map[string]any
		wrongKey bool
	}{
		"wrong nonce":    {claims: map[string]any{"nonce": "attacker"}},
		"wrong audience": {claims: map[string]any{"aud": "someone-else"}},
		"wrong issuer":   {claims: map[string]any{"iss": "https://evil.example"}},
		"expired":        {claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}},
		"bad signature":  {wrongKey: true},
		"empty subject":  {claims: map[string]any{"sub": ""}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := ssotest.NewOIDCServer(t)
			srv.SetClaims(c.claims)
			srv.SignWithWrongKey(c.wrongKey)
			p := oidcProvider(t, srv, nil)
			id, err := login(t, srv, p, "", "")
			if err == nil {
				t.Fatalf("accepted: %+v", id)
			}
			t.Log(err)
		})
	}
}

func TestOIDCNonceMismatchOnOurSide(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	p := oidcProvider(t, srv, nil)
	if _, err := login(t, srv, p, "not-the-stored-nonce", ""); err == nil {
		t.Fatal("expected nonce mismatch")
	}
}

func TestOIDCPKCEMismatch(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	p := oidcProvider(t, srv, nil)
	if _, err := login(t, srv, p, "", sso.NewPKCEVerifier()); err == nil {
		t.Fatal("expected token endpoint to reject the verifier")
	}
}

func TestOIDCEmailVerifiedHandling(t *testing.T) {
	cases := []struct {
		name  string
		claim any
		trust bool
		want  bool
	}{
		{"string true", "true", false, true},
		{"string false", "false", false, false},
		{"bool false", false, false, false},
		{"absent", nil, false, false},
		{"absent but trusted", nil, true, true},
		{"false but trusted", false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := ssotest.NewOIDCServer(t)
			claims := map[string]any{"email": "a@example.org"}
			if c.claim != nil {
				claims["email_verified"] = c.claim
			}
			srv.SetClaims(claims)
			p := oidcProvider(t, srv, func(pc *config.OIDCProvider) { pc.TrustEmail = c.trust })
			id, err := login(t, srv, p, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if id.EmailVerified != c.want {
				t.Fatalf("EmailVerified = %v, want %v", id.EmailVerified, c.want)
			}
		})
	}
}

func TestOIDCTrustEmailNeedsAnEmail(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	p := oidcProvider(t, srv, func(pc *config.OIDCProvider) { pc.TrustEmail = true })
	id, err := login(t, srv, p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.EmailVerified {
		t.Fatal("no email, cannot be verified")
	}
}

func TestOIDCUserInfoFallback(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	srv.SetUserInfo(map[string]any{"email": "info@example.org", "email_verified": true, "name": "Info Person"})
	p := oidcProvider(t, srv, nil)
	id, err := login(t, srv, p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "info@example.org" || !id.EmailVerified || id.DisplayName != "Info Person" {
		t.Fatalf("got %+v", id)
	}
}

func TestOIDCUserInfoSubjectMismatchIgnored(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	srv.SetUserInfo(map[string]any{"sub": "someone-else", "email": "victim@example.org", "email_verified": true})
	p := oidcProvider(t, srv, nil)
	id, err := login(t, srv, p, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "" {
		t.Fatalf("took email from a userinfo response for another subject: %+v", id)
	}
}

func TestOIDCIdPDownAtStartupThenRecovers(t *testing.T) {
	srv := ssotest.NewOIDCServer(t)
	srv.SetDown(true)
	p := oidcProvider(t, srv, nil) // New must not need the IdP
	if _, err := p.AuthURL(context.Background(), "s", "n", sso.NewPKCEVerifier()); err == nil {
		t.Fatal("expected discovery failure")
	}
	srv.SetDown(false)
	if _, err := login(t, srv, p, "", ""); err != nil {
		t.Fatalf("no recovery after IdP came back: %v", err)
	}
}
