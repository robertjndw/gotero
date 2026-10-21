// Package conformance is a server-agnostic protocol test kit for
// Zotero-compatible sync servers. It speaks raw HTTP (and WebSocket for the
// stream) so that status codes and headers are asserted exactly as a Zotero
// client would see them.
//
// Every assertion traces to docs/dev/protocol-notes.md or the public Zotero
// Web API v3 documentation.
package conformance

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// User is an account the suites can act as.
type User struct {
	ID       int64
	Username string
	Password string
	APIKey   string
}

// Group is a group library created by a Provisioner.
type Group struct {
	ID int64
}

// ErrUnsupported is returned by a Provisioner for operations it cannot do.
// Tests needing the operation are skipped instead of failed.
var ErrUnsupported = errors.New("conformance: operation not supported by provisioner")

// Provisioner creates the accounts the suites run against. Every test asks for
// its own fresh user so that tests can run in parallel.
type Provisioner interface {
	// NewUser returns a user with a full-access API key (user library, notes,
	// files, write, and all groups).
	NewUser(ctx context.Context) (User, error)
	// NewGroup creates a group owned by owner. members are plain members and
	// admins are group admins; the owner needs no listing.
	NewGroup(ctx context.Context, owner User, members, admins []User) (Group, error)
}

// MemberAdder is implemented by provisioners that can change membership of an
// existing group.
type MemberAdder interface {
	AddMember(ctx context.Context, g Group, member User, admin bool) error
}

// KeyRevoker is implemented by provisioners that can revoke a key out of band.
type KeyRevoker interface {
	RevokeKey(ctx context.Context, u User) error
}

// QuotaSetter is implemented by provisioners that can set a storage quota.
type QuotaSetter interface {
	SetQuota(ctx context.Context, u User, bytes int64) error
}

// SessionApprover is implemented by provisioners that can act as the user
// approving a browser login session (keys/sessions flow).
type SessionApprover interface {
	ApproveSession(ctx context.Context, u User, token string) error
}

// Config configures a conformance run.
type Config struct {
	// BaseURL is the API root, e.g. http://localhost:8080/api/
	BaseURL string
	// StreamURL is the optional WebSocket URL; stream tests skip without it.
	StreamURL   string
	Provisioner Provisioner
	HTTPClient  *http.Client
	// Suites limits the run to the named suites; empty means all.
	Suites []string
	// Skip lists tests to skip, as "name" or "suite/name". Use it to document
	// known deviations.
	Skip []string
	// OnResult, if set, is called once per finished test.
	OnResult func(name string, failed, skipped bool)
}

type env struct {
	cfg  Config
	base string
	hc   *http.Client
	prov Provisioner
}

type testCase struct {
	name string
	fn   func(*testing.T, *env)
}

type suite struct {
	name  string
	tests []testCase
}

func allSuites() []suite {
	return []suite{
		{"auth", authTests},
		{"groups", groupsTests},
		{"write", writeTests},
		{"read", readTests},
		{"deleted", deletedTests},
		{"settings", settingsTests},
		{"tags", tagsTests},
		{"fulltext", fulltextTests},
		{"files", filesTests},
		{"stream", streamTests},
		{"sessions", sessionsTests},
		{"quirks", quirksTests},
	}
}

// SuiteNames lists the available suites.
func SuiteNames() []string {
	var out []string
	for _, s := range allSuites() {
		out = append(out, s.name)
	}
	return out
}

// TestCount returns the number of registered tests.
func TestCount() int {
	n := 0
	for _, s := range allSuites() {
		n += len(s.tests)
	}
	return n
}

func newEnv(cfg Config) (*env, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("conformance: BaseURL must be an absolute URL")
	}
	base := strings.TrimRight(cfg.BaseURL, "/") + "/"
	var hc http.Client
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	if hc.Timeout == 0 {
		hc.Timeout = 2 * time.Minute
	}
	// Redirects must stay visible: file download answers 302 and the test
	// follows Location itself, without the API key.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &env{cfg: cfg, base: base, hc: &hc, prov: cfg.Provisioner}, nil
}

func contains(list []string, names ...string) bool {
	for _, l := range list {
		for _, n := range names {
			if l == n {
				return true
			}
		}
	}
	return false
}

// Run executes the configured suites as parallel subtests of t.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.BaseURL == "" {
		t.Skip("conformance: BaseURL not set")
	}
	if cfg.Provisioner == nil {
		t.Skip("conformance: no Provisioner configured")
	}
	e, err := newEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range allSuites() {
		if len(cfg.Suites) > 0 && !contains(cfg.Suites, s.name) {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range s.tests {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					if cfg.OnResult != nil {
						t.Cleanup(func() { cfg.OnResult(s.name+"/"+tc.name, t.Failed(), t.Skipped()) })
					}
					if contains(cfg.Skip, tc.name, s.name+"/"+tc.name) {
						t.Skip("listed in Config.Skip")
					}
					tc.fn(t, e)
				})
			}
		})
	}
}
