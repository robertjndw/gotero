package sso_test

import (
	"testing"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/sso"
	"github.com/robertjndw/gotero/internal/sso/ssotest"
)

func TestNilRegistry(t *testing.T) {
	var r *sso.Registry
	if r.List() != nil {
		t.Fatal("nil registry should list nothing")
	}
	if _, ok := r.Get("x"); ok {
		t.Fatal("nil registry should have no providers")
	}
}

func TestRegistryOrder(t *testing.T) {
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	meta := idp.WriteMetadataFile(t)
	reg, err := sso.New(&config.Config{
		PublicURL: publicURL,
		OIDC: []config.OIDCProvider{
			{Name: "corp", Issuer: "https://a.example", ClientID: "c"},
			{Name: "google", Issuer: "https://b.example", ClientID: "c"},
		},
		SAML: []config.SAMLProvider{
			{Name: "okta", IDPMetadataFile: meta},
			{Name: "azure", IDPMetadataFile: meta},
		},
		SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile},
	}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range reg.List() {
		got = append(got, p.Name())
	}
	want := []string{"corp", "google", "okta", "azure"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if p, ok := reg.Get("okta"); !ok || p.Kind() != sso.KindSAML {
		t.Fatal("okta lookup failed")
	}
	if p, _ := reg.Get("corp"); p.Kind() != sso.KindOIDC {
		t.Fatal("corp should be oidc")
	}
	if _, ok := reg.Get("missing"); ok {
		t.Fatal("unexpected provider")
	}
}

func TestNewPKCEVerifier(t *testing.T) {
	if sso.NewPKCEVerifier() == "" {
		t.Fatal("empty verifier")
	}
}
