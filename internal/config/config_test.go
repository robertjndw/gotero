package config

import (
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

func load(t *testing.T, vars map[string]string) (*Config, error) {
	t.Helper()
	return parse(env.Options{Prefix: "GOTERO_", Environment: vars})
}

func TestDefaults(t *testing.T) {
	c, err := load(t, map[string]string{"GOTERO_SECRET": strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.Storage.Backend != "local" || c.OpenRegistration {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.DBMaxConns != 20 || c.DBAcquireTimeout != 10*time.Second {
		t.Fatalf("unexpected pool defaults: %d, %s", c.DBMaxConns, c.DBAcquireTimeout)
	}
	if c.DeletedLogRetention != 0 {
		t.Fatalf("delete log must be kept forever by default, got %s", c.DeletedLogRetention)
	}
}

func TestDeletedLogRetention(t *testing.T) {
	c, err := load(t, map[string]string{"GOTERO_SECRET": strings.Repeat("x", 32), "GOTERO_DELETED_LOG_RETENTION": "720h"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DeletedLogRetention != 720*time.Hour {
		t.Fatalf("retention = %s", c.DeletedLogRetention)
	}
}

func TestValidation(t *testing.T) {
	secret := strings.Repeat("x", 32)
	cases := map[string]map[string]string{
		"short secret":    {"GOTERO_SECRET": "short"},
		"bad public url":  {"GOTERO_SECRET": secret, "GOTERO_PUBLIC_URL": "zotero.local"},
		"bad proxy":       {"GOTERO_SECRET": secret, "GOTERO_TRUSTED_PROXIES": "nope"},
		"bad backend":     {"GOTERO_SECRET": secret, "GOTERO_STORAGE_BACKEND": "ftp"},
		"s3 no bucket":    {"GOTERO_SECRET": secret, "GOTERO_STORAGE_BACKEND": "s3", "GOTERO_STORAGE_S3_ENDPOINT": "minio:9000"},
		"no connections":  {"GOTERO_SECRET": secret, "GOTERO_DB_MAX_CONNS": "0"},
		"negative wait":   {"GOTERO_SECRET": secret, "GOTERO_DB_ACQUIRE_TIMEOUT": "-1s"},
		"negative retain": {"GOTERO_SECRET": secret, "GOTERO_DELETED_LOG_RETENTION": "-1h"},
		"admin half":      {"GOTERO_SECRET": secret, "GOTERO_ADMIN_USERNAME": "root"},
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, vars); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPublicURLTrailingSlashTrimmed(t *testing.T) {
	c, err := load(t, map[string]string{
		"GOTERO_SECRET":          strings.Repeat("x", 32),
		"GOTERO_PUBLIC_URL":      "https://zotero.example.org/",
		"GOTERO_TRUSTED_PROXIES": "10.0.0.0/8,127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://zotero.example.org" {
		t.Fatalf("got %q", c.PublicURL)
	}
}

func TestSSOProviders(t *testing.T) {
	c, err := load(t, map[string]string{
		"GOTERO_SECRET":                   strings.Repeat("x", 32),
		"GOTERO_PUBLIC_URL":               "https://zotero.example.org",
		"GOTERO_LOCAL_LOGIN":              "false",
		"GOTERO_OIDC_PROVIDERS":           "corp,google",
		"GOTERO_OIDC_CORP_ISSUER":         "https://idp.example.org",
		"GOTERO_OIDC_CORP_CLIENT_ID":      "gotero",
		"GOTERO_OIDC_CORP_DISPLAY_NAME":   "Corp SSO",
		"GOTERO_OIDC_CORP_AUTO_PROVISION": "true",
		"GOTERO_OIDC_GOOGLE_ISSUER":       "https://accounts.google.com",
		"GOTERO_OIDC_GOOGLE_CLIENT_ID":    "abc",
		"GOTERO_OIDC_GOOGLE_SCOPES":       "email,profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalLogin || len(c.OIDC) != 2 || !c.SSOEnabled() {
		t.Fatalf("unexpected config: %+v", c)
	}
	corp, google := c.OIDC[0], c.OIDC[1]
	if corp.Name != "corp" || corp.DisplayName != "Corp SSO" || !corp.AutoProvision || corp.UsernameClaim != "preferred_username" {
		t.Fatalf("corp: %+v", corp)
	}
	if google.DisplayName != "google" || google.Scopes[0] != "openid" || len(google.Scopes) != 3 {
		t.Fatalf("google: %+v", google)
	}
}

func TestSSOValidation(t *testing.T) {
	secret := strings.Repeat("x", 32)
	oidc := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"GOTERO_SECRET":              secret,
			"GOTERO_PUBLIC_URL":          "https://zotero.example.org",
			"GOTERO_OIDC_PROVIDERS":      "corp",
			"GOTERO_OIDC_CORP_ISSUER":    "https://idp.example.org",
			"GOTERO_OIDC_CORP_CLIENT_ID": "gotero",
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if _, err := load(t, oidc(nil)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]map[string]string{
		"no providers, no local login": {"GOTERO_SECRET": secret, "GOTERO_LOCAL_LOGIN": "false"},
		"missing public url":           oidc(map[string]string{"GOTERO_PUBLIC_URL": ""}),
		"missing issuer":               oidc(map[string]string{"GOTERO_OIDC_CORP_ISSUER": ""}),
		"bad name":                     oidc(map[string]string{"GOTERO_OIDC_PROVIDERS": "Corp-1"}),
		"duplicate across kinds": oidc(map[string]string{
			"GOTERO_SAML_PROVIDERS":             "corp",
			"GOTERO_SAML_CORP_IDP_METADATA_URL": "https://idp.example.org/metadata",
			"GOTERO_SAML_SP_CERT_FILE":          "config_test.go",
			"GOTERO_SAML_SP_KEY_FILE":           "config_test.go",
		}),
		"saml without metadata": oidc(map[string]string{
			"GOTERO_SAML_PROVIDERS":    "okta",
			"GOTERO_SAML_SP_CERT_FILE": "config_test.go",
			"GOTERO_SAML_SP_KEY_FILE":  "config_test.go",
		}),
		"saml without sp key": oidc(map[string]string{
			"GOTERO_SAML_PROVIDERS":             "okta",
			"GOTERO_SAML_OKTA_IDP_METADATA_URL": "https://idp.example.org/metadata",
		}),
	}
	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, vars); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
