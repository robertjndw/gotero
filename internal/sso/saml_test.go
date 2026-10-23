package sso_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/sso"
	"github.com/robertjndw/gotero/internal/sso/ssotest"
)

type samlEnv struct {
	idp *ssotest.SAMLIdP
	p   sso.SAML
}

func newSAMLEnv(t *testing.T, mod func(*config.SAMLProvider)) samlEnv {
	t.Helper()
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	pc := config.SAMLProvider{
		Name: "okta", IDPMetadataFile: idp.WriteMetadataFile(t), DisplayName: "Okta",
		EmailAttribute: "email", UsernameAttribute: "uid", NameAttribute: "displayName",
	}
	if mod != nil {
		mod(&pc)
	}
	reg, err := sso.New(&config.Config{
		PublicURL: publicURL, SAML: []config.SAMLProvider{pc},
		SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile},
	}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	prov, _ := reg.Get("okta")
	p := prov.(sso.SAML)
	md, err := p.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	idp.TrustSP(t, md)
	return samlEnv{idp, p}
}

var ada = ssotest.SAMLUser{NameID: "ada-id", Email: "ada@example.org", Username: "ada", DisplayName: "Ada Lovelace"}

// start performs the SP-side start and returns the redirect URL and request ID.
func (e samlEnv) start(t *testing.T) (string, string) {
	t.Helper()
	u, id, err := e.p.AuthnRequest(context.Background(), "relay-1")
	if err != nil {
		t.Fatal(err)
	}
	return u, id
}

func TestSAMLValid(t *testing.T) {
	e := newSAMLEnv(t, nil)
	redirect, reqID := e.start(t)
	if !strings.HasPrefix(redirect, "https://idp.example.test/sso?") || !strings.Contains(redirect, "RelayState=relay-1") {
		t.Fatalf("redirect = %q", redirect)
	}
	post := e.idp.Respond(t, redirect, ada)
	if post.RelayState != "relay-1" {
		t.Fatalf("relay state = %q", post.RelayState)
	}
	id, err := e.p.ParseResponse(post.Request(), reqID)
	if err != nil {
		t.Fatal(err)
	}
	want := sso.Identity{Subject: "ada-id", Email: "ada@example.org", EmailVerified: true, Username: "ada", DisplayName: "Ada Lovelace"}
	if id != want {
		t.Fatalf("got %+v, want %+v", id, want)
	}
}

func TestSAMLPKCS8Key(t *testing.T) {
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, true)
	_, err := sso.New(&config.Config{
		PublicURL: publicURL,
		SAML:      []config.SAMLProvider{{Name: "okta", IDPMetadataFile: idp.WriteMetadataFile(t)}},
		SAMLSP:    config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile},
	}, quiet())
	if err != nil {
		t.Fatal(err)
	}
}

func TestSAMLNewFailsOnUnreadableInputs(t *testing.T) {
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	good := config.SAMLProvider{Name: "okta", IDPMetadataFile: idp.WriteMetadataFile(t)}
	cases := map[string]config.Config{
		"missing key":      {SAML: []config.SAMLProvider{good}, SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: "/nonexistent"}},
		"missing metadata": {SAML: []config.SAMLProvider{{Name: "okta", IDPMetadataFile: "/nonexistent"}}, SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.PublicURL = publicURL
			if _, err := sso.New(&cfg, quiet()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSAMLRejectsBadResponses(t *testing.T) {
	cases := map[string]func(t *testing.T, e samlEnv, redirect string) ssotest.SAMLPost{
		"wrong audience": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.RespondWith(t, r, ada, ssotest.SAMLOptions{Audience: "https://other-sp.example"})
		},
		"wrong InResponseTo": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.RespondWith(t, r, ada, ssotest.SAMLOptions{InResponseTo: "id-someone-elses"})
		},
		"unsolicited": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.RespondWith(t, r, ada, ssotest.SAMLOptions{Unsolicited: true})
		},
		"expired": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.RespondWith(t, r, ada, ssotest.SAMLOptions{Now: time.Now().Add(-time.Hour)})
		},
		"tampered signature": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.Respond(t, r, ada).TamperSignature(t)
		},
		"tampered content": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			return e.idp.Respond(t, r, ada).Replace(t, `Version="2.0"`, `Version="2.0" Foo="bar"`)
		},
		"transient nameid": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			u := ada
			u.NameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
			return e.idp.Respond(t, r, u)
		},
		"empty nameid": func(t *testing.T, e samlEnv, r string) ssotest.SAMLPost {
			u := ada
			u.NameID = ""
			return e.idp.Respond(t, r, u)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSAMLEnv(t, nil)
			redirect, reqID := e.start(t)
			post := mk(t, e, redirect)
			id, err := e.p.ParseResponse(post.Request(), reqID)
			if err == nil {
				t.Fatalf("accepted: %+v", id)
			}
			t.Log(err)
		})
	}
}

func TestSAMLResponseIsSingleRequestBound(t *testing.T) {
	e := newSAMLEnv(t, nil)
	redirect, _ := e.start(t)
	post := e.idp.Respond(t, redirect, ada)
	if _, err := e.p.ParseResponse(post.Request(), "id-other"); err == nil {
		t.Fatal("response accepted for a different request")
	}
	if _, err := e.p.ParseResponse(post.Request(), ""); err == nil {
		t.Fatal("empty request id accepted")
	}
}

func TestSAMLTransientAllowedWithSubjectAttribute(t *testing.T) {
	e := newSAMLEnv(t, func(pc *config.SAMLProvider) { pc.SubjectAttribute = "employeeId" })
	redirect, reqID := e.start(t)
	u := ada
	u.NameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
	u.Attributes = map[string]string{"employeeId": "E-42"}
	id, err := e.p.ParseResponse(e.idp.Respond(t, redirect, u).Request(), reqID)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "E-42" {
		t.Fatalf("subject = %q", id.Subject)
	}
}

func TestSAMLSubjectAttributeMissing(t *testing.T) {
	e := newSAMLEnv(t, func(pc *config.SAMLProvider) { pc.SubjectAttribute = "employeeId" })
	redirect, reqID := e.start(t)
	if _, err := e.p.ParseResponse(e.idp.Respond(t, redirect, ada).Request(), reqID); err == nil {
		t.Fatal("expected error when the configured subject attribute is absent")
	}
}

func TestSAMLEmailAttributeFallback(t *testing.T) {
	e := newSAMLEnv(t, func(pc *config.SAMLProvider) { pc.EmailAttribute = "nonexistent" })
	redirect, reqID := e.start(t)
	// The test IdP emits mail as urn:oid:0.9.2342.19200300.100.1.3.
	id, err := e.p.ParseResponse(e.idp.Respond(t, redirect, ada).Request(), reqID)
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "ada@example.org" {
		t.Fatalf("email = %q", id.Email)
	}
}

func TestSAMLRejectsNonPostAndArtifact(t *testing.T) {
	e := newSAMLEnv(t, nil)
	redirect, reqID := e.start(t)
	post := e.idp.Respond(t, redirect, ada)

	get := httptest.NewRequest(http.MethodGet, "/auth/saml/okta/acs?SAMLResponse="+url.QueryEscape(post.SAMLResponse), nil)
	if _, err := e.p.ParseResponse(get, reqID); err == nil {
		t.Fatal("GET accepted")
	}
	art := post.Request()
	art.URL.RawQuery = "SAMLart=abc"
	if _, err := e.p.ParseResponse(art, reqID); err == nil {
		t.Fatal("artifact accepted")
	}
}

func TestSAMLBodyCap(t *testing.T) {
	e := newSAMLEnv(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/auth/saml/okta/acs", strings.NewReader("SAMLResponse="+strings.Repeat("A", 2<<20)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := e.p.ParseResponse(r, "id-1"); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestSAMLMetadata(t *testing.T) {
	e := newSAMLEnv(t, nil)
	md, err := e.p.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := string(md)
	for _, want := range []string{
		publicURL + "/auth/saml/okta/acs",
		`entityID="` + publicURL + `/auth/saml/okta/metadata"`,
		"nameid-format:persistent",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("metadata lacks %q", want)
		}
	}
	if strings.Contains(s, "HTTP-Artifact") {
		t.Error("metadata advertises the unsupported artifact binding")
	}
}

func TestSAMLCustomEntityID(t *testing.T) {
	e := newSAMLEnv(t, func(pc *config.SAMLProvider) { pc.EntityID = "urn:gotero:custom" })
	md, _ := e.p.Metadata(context.Background())
	if !strings.Contains(string(md), `entityID="urn:gotero:custom"`) {
		t.Fatal("custom entity ID not used")
	}
	redirect, reqID := e.start(t)
	// The IdP issues the audience from the SP metadata it was given, so a
	// matching round trip proves the SP validates against its custom ID.
	if _, err := e.p.ParseResponse(e.idp.Respond(t, redirect, ada).Request(), reqID); err != nil {
		t.Fatal(err)
	}
}

func TestSAMLMetadataURLLazyAndRetried(t *testing.T) {
	idp := ssotest.NewSAMLIdP(t)
	raw, err := os.ReadFile(idp.WriteMetadataFile(t))
	if err != nil {
		t.Fatal(err)
	}
	up := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)

	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	reg, err := sso.New(&config.Config{
		PublicURL: publicURL,
		SAML: []config.SAMLProvider{{
			Name: "okta", IDPMetadataURL: srv.URL, EmailAttribute: "email",
			UsernameAttribute: "uid", NameAttribute: "displayName",
		}},
		SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile},
	}, quiet())
	if err != nil {
		t.Fatalf("New must not fetch metadata: %v", err)
	}
	prov, _ := reg.Get("okta")
	p := prov.(sso.SAML)
	if _, _, err := p.AuthnRequest(context.Background(), "r"); err == nil {
		t.Fatal("expected failure while metadata is unreachable")
	}
	up = true
	redirect, reqID, err := p.AuthnRequest(context.Background(), "r")
	if err != nil {
		t.Fatalf("no recovery: %v", err)
	}
	md, _ := p.Metadata(context.Background())
	idp.TrustSP(t, md)
	if _, err := p.ParseResponse(idp.Respond(t, redirect, ada).Request(), reqID); err != nil {
		t.Fatal(err)
	}
}
