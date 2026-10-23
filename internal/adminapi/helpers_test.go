package adminapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/adminapi"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/jobs"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/internal/sso"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const adminPass = "correct horse battery"

// clock is a movable time source shared by the API and the account service.
type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

type notifier struct {
	mu     sync.Mutex
	tokens []string
	access []int64
}

func (n *notifier) AccessChanged(id int64) {
	n.mu.Lock()
	n.access = append(n.access, id)
	n.mu.Unlock()
}

func (n *notifier) accessed() []int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]int64(nil), n.access...)
}

func (n *notifier) LoginCompleted(t string) {
	n.mu.Lock()
	n.tokens = append(n.tokens, t)
	n.mu.Unlock()
}

type fakeGC struct{ dry []bool }

func (g *fakeGC) GC(_ context.Context, dryRun bool) (jobs.GCResult, error) {
	g.dry = append(g.dry, dryRun)
	return jobs.GCResult{OrphanBlobs: 3, OrphanBytes: 99, Deleted: !dryRun}, nil
}

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *pg.Store
	auth   *auth.Authenticator
	acc    *accounts.Service
	clock  *clock
	notify *notifier
	gc     *fakeGC
	srv    *httptest.Server
	cfg    *config.Config
	sso    *sso.Registry
	admin  repo.User
}

type envOpts struct {
	publicURL string
	validator adminapi.ImportValidator
	// cfg adjusts the config before the SSO registry is built. When it
	// configures a provider and publicURL is empty, the public URL becomes
	// the test server's address so callbacks and the ACS resolve to it.
	cfg func(*config.Config)
}

func newEnv(t *testing.T, opts ...envOpts) *env {
	t.Helper()
	pool := pgtest.New(t)
	store := pg.New(pool)
	cfg := &config.Config{
		Secret:     "0123456789abcdef0123456789abcdef",
		Storage:    config.Storage{Backend: "local"},
		LocalLogin: true,
	}
	var o envOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	// The listener exists before Start so the public URL is known up front.
	srv := httptest.NewUnstartedServer(nil)
	publicURL := o.publicURL
	if o.cfg != nil {
		o.cfg(cfg)
		if cfg.SSOEnabled() && publicURL == "" {
			publicURL = "http://" + srv.Listener.Addr().String()
		}
		cfg.PublicURL = publicURL
	}
	reg, err := sso.New(cfg, nil)
	require.NoError(t, err)
	e := &env{t: t, pool: pool, store: store, cfg: cfg, clock: &clock{}, notify: &notifier{}, gc: &fakeGC{}}
	e.auth = auth.NewAuthenticator(store.Keys(), store.Users())
	e.acc = accounts.New(store, e.auth, cfg, nil)
	e.acc.SetClock(e.clock.Now)

	api := adminapi.New(adminapi.Deps{
		Store: store, Auth: e.auth, Accounts: e.acc, Config: cfg, Now: e.clock.Now, Version: "test",
		GC: e.gc, Notifier: e.notify, ImportValidator: o.validator, SSO: reg,
	})
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api.Register(engine.Group("/admin/api"))
	api.RegisterSSO(engine.Group("/auth"))
	srv.Config.Handler = httpx.Chain(engine, httpx.ResolvePublicURL(publicURL, nil))
	srv.Start()
	e.srv, e.sso = srv, reg
	t.Cleanup(e.srv.Close)

	admin, err := e.acc.CreateUser(context.Background(), accounts.CreateUserInput{
		Username: "root", Password: adminPass, IsAdmin: true, Email: "root@example.com",
	})
	require.NoError(t, err)
	e.admin = admin
	return e
}

// user creates a plain account directly through the service.
func (e *env) user(name string) repo.User {
	e.t.Helper()
	u, err := e.acc.CreateUser(context.Background(), accounts.CreateUserInput{Username: name, Password: "password-" + name})
	require.NoError(e.t, err)
	return u
}

// client is a browser-like session: cookie jar plus CSRF echo and Origin.
type client struct {
	t      *testing.T
	e      *env
	hc     *http.Client
	csrf   string
	origin string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, e: e, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}, origin: e.srv.URL}
}

func (e *env) login(name, pass string) *client {
	e.t.Helper()
	c := e.client()
	res := c.do("POST", "/login", map[string]string{"username": name, "password": pass})
	require.Equal(e.t, 200, res.status, string(res.raw))
	return c
}

func (e *env) adminClient() *client { return e.login("root", adminPass) }

type opt func(*http.Request)

func noCSRF() opt { return func(r *http.Request) { r.Header.Del("X-CSRF-Token") } }
func withHeader(k, v string) opt {
	return func(r *http.Request) {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
}

type result struct {
	status int
	header http.Header
	raw    []byte
}

func (r result) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.raw, &m)
	return m
}

func (r result) errCode() string {
	e, _ := r.json()["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (r result) fields() map[string]any {
	e, _ := r.json()["error"].(map[string]any)
	f, _ := e["fields"].(map[string]any)
	return f
}

func (c *client) do(method, path string, body any, opts ...opt) result {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(c.t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.e.srv.URL+"/admin/api"+path, rd)
	require.NoError(c.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := c.hc.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if tok := resp.Header.Get("X-CSRF-Token"); tok != "" {
		c.csrf = tok
	}
	return result{status: resp.StatusCode, header: resp.Header, raw: raw}
}

func (c *client) mustOK(method, path string, body any) map[string]any {
	c.t.Helper()
	res := c.do(method, path, body)
	require.Less(c.t, res.status, 300, "%s %s: %s", method, path, res.raw)
	return res.json()
}

func num(m map[string]any, k string) int64 { return int64(m[k].(float64)) }

func items(m map[string]any) []any { return m["items"].([]any) }

func ctx() context.Context { return context.Background() }

func (r result) cookies(t *testing.T) []*http.Cookie {
	t.Helper()
	resp := http.Response{Header: r.header}
	return resp.Cookies()
}

func parseSeconds(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

func (e *env) poolQueryRow(sql string, args ...any) pgx.Row {
	return e.pool.QueryRow(ctx(), sql, args...)
}

// mustKey creates a key directly through the service.
func mustKey(t *testing.T, e *env, userID int64) (int64, string) {
	t.Helper()
	k, raw, err := e.acc.CreateKey(ctx(), userID, "test", accounts.FullAccess())
	require.NoError(t, err)
	return k.ID, raw
}

func importJob(userID int64, progress string) repo.ImportJob {
	return repo.ImportJob{UserID: userID, Status: "queued", Progress: json.RawMessage(progress)}
}
