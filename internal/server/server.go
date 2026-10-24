// Package server wires configuration, storage and handlers into an
// http.Server.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/adminapi"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/importer"
	"github.com/robertjndw/gotero/internal/jobs"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/sso"
	"github.com/robertjndw/gotero/internal/stream"
	"github.com/robertjndw/gotero/internal/version"
	"github.com/robertjndw/gotero/internal/webui"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero/schema"
)

// uploadIdleTimeout is how long an upload body, or a streamed listing's
// response, may stall before the connection is cut.
const uploadIdleTimeout = time.Minute

type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	http    *http.Server
	jobs    *jobs.Runner
	imports *importer.Worker
	hub     *stream.Hub
}

// New builds the complete application. ctx is only used for setup work
// such as connecting to the storage backend.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, pool *pgxpool.Pool) (*Server, error) {
	trusted, err := httpx.ParsePrefixes(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	backend, err := NewBackend(ctx, cfg.Storage)
	if err != nil {
		return nil, err
	}

	store := pg.New(pool, pg.WithAcquireTimeout(cfg.DBAcquireTimeout), pg.WithLockTimeout(cfg.DBLockTimeout))
	authn := auth.NewAuthenticator(store.Keys(), store.Users())
	acct := accounts.New(store, authn, cfg, log)
	if err := acct.EnsureBootstrapAdmin(ctx, cfg.Admin); err != nil {
		return nil, err
	}
	fileSvc := files.NewService(backend, store.Files(), cfg.Secret)
	runner := jobs.New(store, fileSvc, log, cfg.DeletedLogRetention)
	hub := stream.NewHub(topicResolver{auth: authn, groups: store.Groups()}, log)

	imports := importer.New(importer.Deps{
		Store:        store,
		Files:        fileSvc,
		Log:          log,
		DefaultQuota: acct.DefaultQuota,
		Publisher:    hubPublisher{hub},
		Notifier:     hub,
	})

	s := &Server{cfg: cfg, log: log, pool: pool, jobs: runner, imports: imports, hub: hub}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// Proxy headers are handled by httpx.ResolvePublicURL with our own
	// trusted-proxy list; gin must not second-guess the client IP.
	engine.ForwardedByClientIP = false
	engine.HandleMethodNotAllowed = true
	engine.GET("/healthz", s.handleHealth)
	engine.GET("/stream", hub.Handler())

	api := zapi.New(zapi.Deps{
		Store:           store,
		Auth:            authn,
		Schema:          schema.Default(),
		Publisher:       hubPublisher{hub},
		Log:             log,
		MaxRequestBytes: cfg.MaxRequestBytes,
		Files:           fileSvc,
		MaxUploadBytes:  cfg.MaxUploadBytes,
		DefaultQuota:    acct.DefaultQuota,
		Notifier:        hub,
		// Shares the web UI's login guard, so both password paths draw from
		// one attempt budget.
		Login:            acct.Login,
		WriteIdleTimeout: uploadIdleTimeout,
	})
	api.Register(engine.Group("/api"))
	// Upload and download targets authenticate with their own tokens, not
	// API keys: Zotero follows download redirects without the key.
	api.RegisterFileRoutes(engine.Group("/files"))

	ssoReg, err := sso.New(cfg, log)
	if err != nil {
		return nil, err
	}
	admin := adminapi.New(adminapi.Deps{
		Store:           store,
		Auth:            authn,
		Accounts:        acct,
		Log:             log,
		Config:          cfg,
		Version:         version.Version,
		GC:              runner,
		Notifier:        hub,
		ImportValidator: imports,
		SSO:             ssoReg,
	})
	admin.Register(engine.Group("/admin/api"))
	admin.RegisterSSO(engine.Group("/auth"))

	engine.NoRoute(webui.Handler())

	// Logging (with panic recovery) and public URL resolution wrap the whole
	// engine so they also cover gin's own 404/405 responses.
	handler := httpx.Chain(engine,
		httpx.Log(log),
		httpx.ResolvePublicURL(cfg.PublicURL, trusted),
		// Two minutes covers a 128 MiB full-text upload on a slow link;
		// attachment uploads only need to keep moving.
		httpx.BodyDeadlines(2*time.Minute, uploadIdleTimeout),
	)
	s.http = &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// No WriteTimeout: file downloads and the streaming websocket are
		// long-lived. Slow-client protection comes from the header and idle
		// timeouts plus per-handler deadlines.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler exposes the root handler, for tests that serve it themselves.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.log.Info("listening", "addr", ln.Addr().String(), "version", version.Version, "storage", s.cfg.Storage.Backend)

	jobsCtx, stopJobs := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { s.jobs.Run(jobsCtx) })
	wg.Go(func() { s.imports.Run(jobsCtx) })
	wg.Go(func() { s.revalidateStreams(jobsCtx) })

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(ln) }()
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if serr := s.http.Shutdown(shutdownCtx); serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
		err = serr
	}
	stopJobs()
	wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// streamRevalidateInterval bounds how long an open stream keeps hearing
// about a library after access to it was removed from outside the server.
const streamRevalidateInterval = time.Minute

func (s *Server) revalidateStreams(ctx context.Context) {
	t := time.NewTicker(streamRevalidateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.hub.Revalidate(ctx)
		}
	}
}

func (s *Server) handleHealth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	if err := s.pool.Ping(ctx); err != nil {
		status, code = "database unavailable", http.StatusServiceUnavailable
	}
	c.JSON(code, gin.H{"status": status, "version": version.Version})
}

// hubPublisher adapts the stream hub to the Zotero API's publisher.
type hubPublisher struct{ hub *stream.Hub }

func (p hubPublisher) Publish(ev zapi.Event) { p.hub.Publish(ev.Topic, ev.Version) }
