// Package config loads gotero's runtime configuration from environment
// variables. Every variable is prefixed with GOTERO_ and documented in
// docs/configuration.md; keep the two in sync.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
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
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
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
	return errors.Join(errs...)
}
