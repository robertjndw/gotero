// Package ssotest provides in-process fake identity providers for testing
// the sso package and anything built on it.
package ssotest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDCClientID is the only client_id the fake IdP accepts.
const OIDCClientID = "gotero-test-client"

// OIDCServer is a minimal OIDC provider: discovery, JWKS, an authorize
// endpoint that approves immediately, a token endpoint that enforces PKCE
// S256, and an optional userinfo endpoint.
type OIDCServer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	// otherKey signs tokens when SignWithWrongKey is set; it is never
	// published in the JWKS.
	otherKey *rsa.PrivateKey

	mu          sync.Mutex
	claims      map[string]any
	userinfo    map[string]any
	wrongKey    bool
	down        bool
	codes       map[string]oidcGrant
	accessToken map[string]string // access token -> subject
}

type oidcGrant struct {
	challenge string
	nonce     string
	claims    map[string]any
	wrongKey  bool
}

func NewOIDCServer(t testing.TB) *OIDCServer {
	t.Helper()
	gen := func() *rsa.PrivateKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	s := &OIDCServer{
		key:         gen(),
		otherKey:    gen(),
		claims:      map[string]any{},
		codes:       map[string]oidcGrant{},
		accessToken: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/jwks", s.jwks)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)
	mux.HandleFunc("/userinfo", s.userinfoHandler)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		down := s.down
		s.mu.Unlock()
		if down {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *OIDCServer) Issuer() string { return s.srv.URL }

// SetClaims sets claims for subsequent logins, merged over the defaults
// (iss, aud, sub "user-1", iat, exp, and the nonce from the authorize
// request). Overriding a default, e.g. "aud" or "nonce", is how tests
// produce invalid tokens. Passing nil restores the defaults.
func (s *OIDCServer) SetClaims(c map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = c
}

// SetUserInfo enables the userinfo endpoint with these claims. "sub"
// defaults to the ID token's subject.
func (s *OIDCServer) SetUserInfo(c map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userinfo = c
}

// SignWithWrongKey signs ID tokens of subsequent logins with a key that is
// not in the JWKS.
func (s *OIDCServer) SignWithWrongKey(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wrongKey = v
}

// SetDown makes every endpoint answer 503, simulating an outage.
func (s *OIDCServer) SetDown(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = v
}

// Authorize plays the browser: it requests authURL and returns the code and
// state from the redirect back to the client. It fails the test if the IdP
// rejects the request.
func (s *OIDCServer) Authorize(t testing.TB, authURL string) (code, state string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("code"), loc.Query().Get("state")
}

func (s *OIDCServer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                s.Issuer(),
		"authorization_endpoint":                s.Issuer() + "/authorize",
		"token_endpoint":                        s.Issuer() + "/token",
		"jwks_uri":                              s.Issuer() + "/jwks",
		"userinfo_endpoint":                     s.Issuer() + "/userinfo",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (s *OIDCServer) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &s.key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig",
	}}})
}

func (s *OIDCServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if q.Get("client_id") != OIDCClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || redirect == "" {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	code := randString()
	s.mu.Lock()
	claims := map[string]any{}
	for k, v := range s.claims {
		claims[k] = v
	}
	s.codes[code] = oidcGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), claims: claims, wrongKey: s.wrongKey}
	s.mu.Unlock()

	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	rq := u.Query()
	rq.Set("code", code)
	rq.Set("state", q.Get("state"))
	u.RawQuery = rq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound) //nolint:gosec // a test IdP redirecting to the client's redirect_uri is the point
}

func (s *OIDCServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid_request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	g, ok := s.codes[r.PostForm.Get("code")]
	delete(s.codes, r.PostForm.Get("code")) // single use
	s.mu.Unlock()

	clientID := r.PostForm.Get("client_id")
	if user, _, hasBasic := r.BasicAuth(); hasBasic {
		clientID = user
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || clientID != OIDCClientID || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}

	now := time.Now()
	claims := map[string]any{
		"iss":   s.Issuer(),
		"aud":   OIDCClientID,
		"sub":   "user-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": g.nonce,
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	key := s.key
	if g.wrongKey {
		key = s.otherKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "test"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := randString()
	sub, _ := claims["sub"].(string)
	s.mu.Lock()
	s.accessToken[access] = sub
	s.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (s *OIDCServer) userinfoHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	sub, ok := "", false
	if len(h) > len(prefix) {
		sub, ok = s.accessToken[h[len(prefix):]]
	}
	if !ok || s.userinfo == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	out := map[string]any{"sub": sub}
	for k, v := range s.userinfo {
		out[k] = v
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
