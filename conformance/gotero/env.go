package gotero

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/robertjndw/gotero/conformance"
)

// Environment variables read by ConfigFromEnv.
const (
	EnvURL       = "GOTERO_CONFORMANCE_URL"        // API base, e.g. http://localhost:8080/api/
	EnvStreamURL = "GOTERO_CONFORMANCE_STREAM_URL" // optional ws:// URL
	EnvAdminURL  = "GOTERO_CONFORMANCE_ADMIN_URL"  // default: <origin>/admin/api
	EnvAdminUser = "GOTERO_CONFORMANCE_ADMIN_USER"
	EnvAdminPass = "GOTERO_CONFORMANCE_ADMIN_PASS"
	EnvSkip      = "GOTERO_CONFORMANCE_SKIP"   // comma separated test names
	EnvSuites    = "GOTERO_CONFORMANCE_SUITES" // comma separated suite names
)

// DefaultAdminURL derives the admin API root from the Zotero API base URL.
func DefaultAdminURL(apiURL string) (string, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("gotero: %q is not an absolute URL", apiURL)
	}
	return u.Scheme + "://" + u.Host + "/admin/api", nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ConfigFromEnv builds a conformance.Config for a gotero server. ok is false
// when GOTERO_CONFORMANCE_URL is unset, so callers can skip cleanly.
func ConfigFromEnv() (cfg conformance.Config, ok bool, err error) {
	base := os.Getenv(EnvURL)
	if base == "" {
		return cfg, false, nil
	}
	adminURL := os.Getenv(EnvAdminURL)
	if adminURL == "" {
		if adminURL, err = DefaultAdminURL(base); err != nil {
			return cfg, true, err
		}
	}
	prov, err := New(Options{AdminURL: adminURL, AdminUser: os.Getenv(EnvAdminUser), AdminPass: os.Getenv(EnvAdminPass)})
	if err != nil {
		return cfg, true, fmt.Errorf("%w (set %s and %s)", err, EnvAdminUser, EnvAdminPass)
	}
	return conformance.Config{
		BaseURL:     base,
		StreamURL:   os.Getenv(EnvStreamURL),
		Provisioner: prov,
		Suites:      splitList(os.Getenv(EnvSuites)),
		Skip:        splitList(os.Getenv(EnvSkip)),
	}, true, nil
}
