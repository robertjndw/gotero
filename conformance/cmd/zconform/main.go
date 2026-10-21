// Command zconform runs the conformance suites outside `go test`.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/robertjndw/gotero/conformance"
	"github.com/robertjndw/gotero/conformance/gotero"
)

type options struct {
	url, streamURL, provisioner, adminURL, adminUser, adminPass string
	users, run, suites, skip                                    string
	verbose                                                     bool
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parseFlags(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("zconform", flag.ContinueOnError)
	fs.StringVar(&o.url, "url", envOr(gotero.EnvURL, ""), "API base URL, e.g. http://localhost:8080/api/")
	fs.StringVar(&o.streamURL, "stream-url", envOr(gotero.EnvStreamURL, ""), "WebSocket URL (stream suite skips without it)")
	fs.StringVar(&o.provisioner, "provisioner", "gotero", "gotero or static")
	fs.StringVar(&o.adminURL, "admin-url", envOr(gotero.EnvAdminURL, ""), "gotero admin API root (default: <origin>/admin/api)")
	fs.StringVar(&o.adminUser, "admin-user", envOr(gotero.EnvAdminUser, ""), "gotero admin username")
	fs.StringVar(&o.adminPass, "admin-pass", envOr(gotero.EnvAdminPass, ""), "gotero admin password")
	fs.StringVar(&o.users, "users", os.Getenv("ZCONFORM_USERS"), "static provisioner users: id:username:password:apikey,...")
	fs.StringVar(&o.run, "run", "", "regexp selecting tests, matched per level as suite/test")
	fs.StringVar(&o.suites, "suites", envOr(gotero.EnvSuites, ""), "comma separated suites (default all)")
	fs.StringVar(&o.skip, "skip", envOr(gotero.EnvSkip, ""), "comma separated tests to skip")
	fs.BoolVar(&o.verbose, "v", false, "verbose test output")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if o.url == "" {
		return o, fmt.Errorf("-url is required")
	}
	return o, nil
}

func buildConfig(o options) (conformance.Config, error) {
	cfg := conformance.Config{BaseURL: o.url, StreamURL: o.streamURL}
	for _, s := range strings.Split(o.suites, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.Suites = append(cfg.Suites, s)
		}
	}
	for _, s := range strings.Split(o.skip, ",") {
		if s = strings.TrimSpace(s); s != "" {
			cfg.Skip = append(cfg.Skip, s)
		}
	}
	switch o.provisioner {
	case "gotero":
		admin := o.adminURL
		if admin == "" {
			var err error
			if admin, err = gotero.DefaultAdminURL(o.url); err != nil {
				return cfg, err
			}
		}
		p, err := gotero.New(gotero.Options{AdminURL: admin, AdminUser: o.adminUser, AdminPass: o.adminPass})
		if err != nil {
			return cfg, err
		}
		cfg.Provisioner = p
	case "static":
		users, err := conformance.ParseStaticUsers(o.users)
		if err != nil {
			return cfg, err
		}
		cfg.Provisioner = conformance.NewStaticProvisioner(users)
	default:
		return cfg, fmt.Errorf("unknown provisioner %q", o.provisioner)
	}
	return cfg, nil
}

type tally struct {
	mu                  sync.Mutex
	pass, fail, skipped int
	failed              []string
}

func (s *tally) add(name string, failed, skipped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case failed:
		s.fail++
		s.failed = append(s.failed, name)
	case skipped:
		s.skipped++
	default:
		s.pass++
	}
}

func (s *tally) print() {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Strings(s.failed)
	fmt.Printf("\nconformance summary: %d passed, %d failed, %d skipped\n", s.pass, s.fail, s.skipped)
	for _, f := range s.failed {
		fmt.Println("  FAIL", f)
	}
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zconform:", err)
		os.Exit(2)
	}
	cfg, err := buildConfig(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zconform:", err)
		os.Exit(2)
	}
	var st tally
	cfg.OnResult = st.add

	// testing.Main drives the suites exactly as `go test` would. Its flags
	// must be registered and set before it parses os.Args.
	testing.Init()
	pattern := "^Conformance$"
	if o.run != "" {
		pattern += "/" + o.run
	}
	_ = flag.Set("test.run", pattern)
	_ = flag.Set("test.v", fmt.Sprint(o.verbose))
	os.Args = os.Args[:1]
	flag.Parse()

	tests := []testing.InternalTest{{
		Name: "Conformance",
		F: func(t *testing.T) {
			t.Cleanup(st.print)
			conformance.Run(t, cfg)
		},
	}}
	testing.Main(func(pat, str string) (bool, error) { return regexp.MatchString(pat, str) }, tests, nil, nil)
}
