// Package config loads gotero's runtime configuration from environment
// variables. Every variable is prefixed with GOTERO_ and documented in
// docs/configuration.md; keep the two in sync.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Listen    string `env:"LISTEN" envDefault:":8080"`
	PublicURL string `env:"PUBLIC_URL"`
	// Secret signs download tokens. It must stay stable across restarts or
	// in-flight download links break.
	Secret string `env:"SECRET"`

	TrustedProxies []string `env:"TRUSTED_PROXIES" envSeparator:","`

	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://gotero:gotero@localhost:5432/gotero?sslmode=disable"`

	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// MaxRequestBytes limits JSON request bodies after gzip decoding.
	MaxRequestBytes int64 `env:"MAX_REQUEST_BYTES" envDefault:"134217728"`
	// MaxUploadBytes limits a single attachment upload. 0 means unlimited.
	MaxUploadBytes int64 `env:"MAX_UPLOAD_BYTES" envDefault:"0"`
	// DefaultQuotaMB applies to users without an explicit quota. 0 means unlimited.
	DefaultQuotaMB int64 `env:"DEFAULT_QUOTA_MB" envDefault:"0"`

	OpenRegistration bool `env:"OPEN_REGISTRATION" envDefault:"false"`

	// LocalLogin enables username/password sign-in for regular users.
	// Admins can always sign in with their password, so turning this off
	// can never lock the instance out.
	LocalLogin bool `env:"LOCAL_LOGIN" envDefault:"true"`

	// OIDCProviders and SAMLProviders name the configured SSO providers.
	// Each name's settings live under GOTERO_OIDC_<NAME>_* or
	// GOTERO_SAML_<NAME>_* and are parsed into OIDC/SAML in a second pass,
	// because env can't key nested structs by a runtime name.
	OIDCProviders []string            `env:"OIDC_PROVIDERS" envSeparator:","`
	SAMLProviders []string            `env:"SAML_PROVIDERS" envSeparator:","`
	SAMLSP        SAMLServiceProvider `envPrefix:"SAML_SP_"`

	OIDC []OIDCProvider `env:"-"`
	SAML []SAMLProvider `env:"-"`

	Admin   AdminBootstrap `envPrefix:"ADMIN_"`
	Storage Storage        `envPrefix:"STORAGE_"`

	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"30s"`
}

// AdminBootstrap creates the first admin on startup if no admin exists yet.
type AdminBootstrap struct {
	Username string `env:"USERNAME"`
	Password string `env:"PASSWORD"`
	Email    string `env:"EMAIL"`
}

// OIDCProvider configures one OpenID Connect identity provider.
type OIDCProvider struct {
	Name          string   `env:"-"`
	Issuer        string   `env:"ISSUER"`
	ClientID      string   `env:"CLIENT_ID"`
	ClientSecret  string   `env:"CLIENT_SECRET"`
	DisplayName   string   `env:"DISPLAY_NAME"`
	Scopes        []string `env:"SCOPES" envSeparator:"," envDefault:"openid,email,profile"`
	UsernameClaim string   `env:"USERNAME_CLAIM" envDefault:"preferred_username"`
	AutoProvision bool     `env:"AUTO_PROVISION"`
	LinkByEmail   bool     `env:"LINK_BY_EMAIL"`
	// TrustEmail treats the email claim as verified for IdPs that never
	// send email_verified (Entra ID).
	TrustEmail bool `env:"TRUST_EMAIL"`
}

// SAMLProvider configures one SAML 2.0 identity provider.
type SAMLProvider struct {
	Name              string `env:"-"`
	IDPMetadataURL    string `env:"IDP_METADATA_URL"`
	IDPMetadataFile   string `env:"IDP_METADATA_FILE"`
	EntityID          string `env:"ENTITY_ID"`
	DisplayName       string `env:"DISPLAY_NAME"`
	SubjectAttribute  string `env:"SUBJECT_ATTRIBUTE"`
	EmailAttribute    string `env:"EMAIL_ATTRIBUTE" envDefault:"email"`
	UsernameAttribute string `env:"USERNAME_ATTRIBUTE" envDefault:"uid"`
	NameAttribute     string `env:"NAME_ATTRIBUTE" envDefault:"displayName"`
	AutoProvision     bool   `env:"AUTO_PROVISION"`
	// LinkByEmail is the operator vouching that this IdP's emails are
	// verified; SAML has no email_verified equivalent.
	LinkByEmail bool `env:"LINK_BY_EMAIL"`
}

// SAMLServiceProvider is gotero's own signing key pair, shared by all SAML
// providers.
type SAMLServiceProvider struct {
	CertFile string `env:"CERT_FILE"`
	KeyFile  string `env:"KEY_FILE"`
}

// SSOEnabled reports whether any SSO provider is configured.
func (c *Config) SSOEnabled() bool { return len(c.OIDC)+len(c.SAML) > 0 }

type Storage struct {
	Backend string `env:"BACKEND" envDefault:"local"`

	LocalPath string `env:"LOCAL_PATH" envDefault:"/data/files"`

	S3Endpoint  string `env:"S3_ENDPOINT"`
	S3Region    string `env:"S3_REGION" envDefault:"us-east-1"`
	S3Bucket    string `env:"S3_BUCKET"`
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`
	S3Prefix    string `env:"S3_PREFIX"`
	S3UseSSL    bool   `env:"S3_USE_SSL" envDefault:"true"`

	WebDAVURL      string `env:"WEBDAV_URL"`
	WebDAVUsername string `env:"WEBDAV_USERNAME"`
	WebDAVPassword string `env:"WEBDAV_PASSWORD"`
}

// Load reads the environment and validates the result.
func Load() (*Config, error) {
	return parse(env.Options{Prefix: "GOTERO_"})
}

func parse(opts env.Options) (*Config, error) {
	var c Config
	if err := env.ParseWithOptions(&c, opts); err != nil {
		return nil, err
	}
	if err := c.parseProviders(opts); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) parseProviders(opts env.Options) error {
	base := opts.Prefix
	for _, name := range c.OIDCProviders {
		p := OIDCProvider{Name: strings.TrimSpace(name)}
		o := opts
		o.Prefix = base + "OIDC_" + strings.ToUpper(p.Name) + "_"
		if err := env.ParseWithOptions(&p, o); err != nil {
			return err
		}
		if p.DisplayName == "" {
			p.DisplayName = p.Name
		}
		if !slices.Contains(p.Scopes, "openid") {
			p.Scopes = append([]string{"openid"}, p.Scopes...)
		}
		c.OIDC = append(c.OIDC, p)
	}
	for _, name := range c.SAMLProviders {
		p := SAMLProvider{Name: strings.TrimSpace(name)}
		o := opts
		o.Prefix = base + "SAML_" + strings.ToUpper(p.Name) + "_"
		if err := env.ParseWithOptions(&p, o); err != nil {
			return err
		}
		if p.DisplayName == "" {
			p.DisplayName = p.Name
		}
		c.SAML = append(c.SAML, p)
	}
	return nil
}

// Provider names become env var segments and the user_identities.provider
// value, so they must be simple and unique across both protocols.
var providerName = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (c *Config) validateSSO() []error {
	var errs []error
	seen := map[string]bool{}
	checkName := func(kind, name string) {
		if !providerName.MatchString(name) {
			errs = append(errs, fmt.Errorf("GOTERO_%s_PROVIDERS: %q must match [a-z0-9]{1,32}", kind, name))
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("SSO provider name %q is used more than once", name))
		}
		seen[name] = true
	}
	for _, p := range c.OIDC {
		checkName("OIDC", p.Name)
		pre := "GOTERO_OIDC_" + strings.ToUpper(p.Name) + "_"
		if p.Issuer == "" {
			errs = append(errs, errors.New(pre+"ISSUER is required"))
		}
		if p.ClientID == "" {
			errs = append(errs, errors.New(pre+"CLIENT_ID is required"))
		}
	}
	for _, p := range c.SAML {
		checkName("SAML", p.Name)
		pre := "GOTERO_SAML_" + strings.ToUpper(p.Name) + "_"
		if (p.IDPMetadataURL == "") == (p.IDPMetadataFile == "") {
			errs = append(errs, errors.New("exactly one of "+pre+"IDP_METADATA_URL and "+pre+"IDP_METADATA_FILE must be set"))
		}
	}
	if len(c.SAML) > 0 {
		if c.SAMLSP.CertFile == "" || c.SAMLSP.KeyFile == "" {
			errs = append(errs, errors.New("GOTERO_SAML_SP_CERT_FILE and GOTERO_SAML_SP_KEY_FILE are required when a SAML provider is configured"))
		} else {
			for _, f := range []string{c.SAMLSP.CertFile, c.SAMLSP.KeyFile} {
				if _, err := os.Stat(f); err != nil {
					errs = append(errs, fmt.Errorf("SAML SP key pair: %w", err))
				}
			}
		}
	}
	// Callback, ACS and audience URLs are registered at the IdP and must
	// not depend on whatever Host or forwarded headers a request carries.
	if c.SSOEnabled() && c.PublicURL == "" {
		errs = append(errs, errors.New("GOTERO_PUBLIC_URL is required when an SSO provider is configured"))
	}
	if !c.LocalLogin && !c.SSOEnabled() {
		errs = append(errs, errors.New("GOTERO_LOCAL_LOGIN=false needs at least one SSO provider, otherwise only admins could sign in"))
	}
	return errs
}

func (c *Config) validate() error {
	var errs []error
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("GOTERO_PUBLIC_URL must be an absolute http(s) URL, got %q", c.PublicURL))
		}
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	}
	if len(c.Secret) < 32 {
		errs = append(errs, errors.New("GOTERO_SECRET must be at least 32 characters (generate one with `openssl rand -hex 32`)"))
	}
	for _, p := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				errs = append(errs, fmt.Errorf("GOTERO_TRUSTED_PROXIES: %q is neither an IP nor a CIDR", p))
			}
		}
	}
	switch c.Storage.Backend {
	case "local":
		if c.Storage.LocalPath == "" {
			errs = append(errs, errors.New("GOTERO_STORAGE_LOCAL_PATH is required for the local backend"))
		}
	case "s3":
		if c.Storage.S3Endpoint == "" || c.Storage.S3Bucket == "" {
			errs = append(errs, errors.New("GOTERO_STORAGE_S3_ENDPOINT and GOTERO_STORAGE_S3_BUCKET are required for the s3 backend"))
		}
	case "webdav":
		if c.Storage.WebDAVURL == "" {
			errs = append(errs, errors.New("GOTERO_STORAGE_WEBDAV_URL is required for the webdav backend"))
		}
	default:
		errs = append(errs, fmt.Errorf("GOTERO_STORAGE_BACKEND must be local, s3 or webdav, got %q", c.Storage.Backend))
	}
	if (c.Admin.Username == "") != (c.Admin.Password == "") {
		errs = append(errs, errors.New("GOTERO_ADMIN_USERNAME and GOTERO_ADMIN_PASSWORD must be set together"))
	}
	errs = append(errs, c.validateSSO()...)
	return errors.Join(errs...)
}
