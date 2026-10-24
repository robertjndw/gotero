package zapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/storage/local"
	"github.com/robertjndw/gotero/zotero/schema"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// recordingPublisher remembers events for assertions.
type recordingPublisher struct {
	mu     sync.Mutex
	events []zapi.Event
}

func (p *recordingPublisher) Publish(ev zapi.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *recordingPublisher) all() []zapi.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]zapi.Event(nil), p.events...)
}

// testEnv is a running API on a fresh database.
type testEnv struct {
	URL   string
	Store *pg.Store
	Pool  *pgxpool.Pool
	Auth  *auth.Authenticator
	Pub   *recordingPublisher
	Files *files.Service
}

type envConfig struct {
	deps     func(*zapi.Deps)
	pgOpts   []pg.Option
	maxConns int32
}

type envOpt func(*envConfig)

func withDeps(f func(*zapi.Deps)) envOpt { return func(c *envConfig) { c.deps = f } }
func withPG(o ...pg.Option) envOpt       { return func(c *envConfig) { c.pgOpts = o } }
func withMaxConns(n int32) envOpt        { return func(c *envConfig) { c.maxConns = n } }

func newEnv(tb testing.TB, opts ...envOpt) *testEnv {
	tb.Helper()
	var cfg envConfig
	for _, o := range opts {
		o(&cfg)
	}
	pool := pgtest.NewSized(tb, cfg.maxConns)
	store := pg.New(pool, cfg.pgOpts...)
	a := auth.NewAuthenticator(store.Keys(), store.Users())
	pub := &recordingPublisher{}
	backend, err := local.New(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	fs := files.NewService(backend, store.Files(), "0123456789abcdef0123456789abcdef")
	// The same guarded login the server wires in, so legacy POST /keys is
	// rate limited here exactly as in production.
	acc := accounts.New(store, a, &config.Config{Secret: "0123456789abcdef0123456789abcdef", LocalLogin: true}, nil)
	deps := zapi.Deps{
		Login: acc.Login, DefaultQuota: acc.DefaultQuota,
		Store: store, Auth: a, Schema: schema.Default(), Publisher: pub,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxRequestBytes: 1 << 20,
		Files: fs,
	}
	if cfg.deps != nil {
		cfg.deps(&deps)
	}
	api := zapi.New(deps)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.ForwardedByClientIP = false
	api.Register(engine.Group("/api"))
	api.RegisterFileRoutes(engine.Group("/files"))
	srv := httptest.NewServer(httpx.Chain(engine, httpx.ResolvePublicURL("", nil)))
	tb.Cleanup(srv.Close)
	return &testEnv{URL: srv.URL, Store: store, Pool: pool, Auth: a, Pub: pub, Files: fs}
}

// client does not follow redirects: file downloads answer 302 and the tests
// look at that response and then fetch the target themselves.
func (e *testEnv) client() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func ctx() context.Context { return context.Background() }

var _ repo.Store = (*pg.Store)(nil)
