package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/conformance/gotero"
	"github.com/robertjndw/gotero/conformance/syncsim"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

const simAdminPass = "syncsim admin password"

func simConfig(t *testing.T) *config.Config {
	return &config.Config{
		Listen: ":0", LocalLogin: true, Secret: strings.Repeat("s", 32), MaxRequestBytes: 1 << 20,
		MaxUploadBytes: 1 << 20,
		Storage:        config.Storage{Backend: "local", LocalPath: t.TempDir()},
		Admin:          config.AdminBootstrap{Username: "admin", Password: simAdminPass, Email: "admin@example.invalid"},
	}
}

func simProvisioner(t *testing.T, srv *httptest.Server) *gotero.Provisioner {
	t.Helper()
	prov, err := gotero.New(gotero.Options{AdminURL: srv.URL + "/admin/api", AdminUser: "admin", AdminPass: simAdminPass})
	if err != nil {
		t.Fatal(err)
	}
	return prov
}

// newSimServer starts the complete application, stream hub included, the way
// the binary wires it.
func newSimServer(t *testing.T, cfg *config.Config) (*httptest.Server, *gotero.Provisioner) {
	t.Helper()
	srv := serveApp(t, cfg, pgtest.New(t))
	return srv, simProvisioner(t, srv)
}

// crashableServer serves the application on a fixed address and can kill
// it: every connection is cut, websockets included, the database pool is
// closed, and a fresh instance with no in-memory state takes over the same
// database and file storage.
type crashableServer struct {
	cfg     *config.Config
	dsn     string
	handler atomic.Pointer[http.Handler]

	mu    sync.Mutex
	pool  *pgxpool.Pool
	conns map[net.Conn]struct{}
	down  bool
}

func newCrashableServer(t *testing.T) (*httptest.Server, *crashableServer) {
	t.Helper()
	cs := &crashableServer{cfg: simConfig(t), dsn: pgtest.NewDSN(t), conns: map[net.Conn]struct{}{}}
	if err := cs.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*cs.handler.Load()).ServeHTTP(w, r)
	}))
	srv.Listener = &crashListener{Listener: srv.Listener, cs: cs}
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		cs.mu.Lock()
		defer cs.mu.Unlock()
		cs.pool.Close()
	})
	return srv, cs
}

func (cs *crashableServer) start(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, cs.dsn)
	if err != nil {
		return err
	}
	s, err := New(ctx, cs.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pool)
	if err != nil {
		pool.Close()
		return err
	}
	h := s.Handler()
	cs.handler.Store(&h)
	cs.mu.Lock()
	cs.pool = pool
	cs.mu.Unlock()
	return nil
}

func (cs *crashableServer) restart(ctx context.Context) error {
	cs.mu.Lock()
	cs.down = true
	conns := make([]net.Conn, 0, len(cs.conns))
	for c := range cs.conns {
		conns = append(conns, c)
	}
	old := cs.pool
	cs.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	// Handlers that already sent COMMIT may still finish, exactly like a
	// crash after the commit reached the database: the client never hears.
	// Close waits for them, so two instances never run at once.
	old.Close()
	time.Sleep(10 * time.Millisecond)
	err := cs.start(ctx)
	cs.mu.Lock()
	cs.down = false
	cs.mu.Unlock()
	return err
}

type crashListener struct {
	net.Listener
	cs *crashableServer
}

func (l *crashListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.cs.mu.Lock()
		if l.cs.down {
			l.cs.mu.Unlock()
			_ = c.Close()
			continue
		}
		tc := &trackedConn{Conn: c, cs: l.cs}
		l.cs.conns[tc] = struct{}{}
		l.cs.mu.Unlock()
		return tc, nil
	}
}

type trackedConn struct {
	net.Conn
	cs   *crashableServer
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.cs.mu.Lock()
		delete(c.cs.conns, c)
		c.cs.mu.Unlock()
	})
	return c.Conn.Close()
}

// TestMultiClientSync runs simulated Zotero clients against one server and
// checks that they converge without losing acknowledged writes. Replay a
// failure with SYNCSIM_SEED, soak with SYNCSIM_RUNS=50.
func TestMultiClientSync(t *testing.T) {
	scenarios := []struct {
		name string
		cfg  syncsim.Config
		// lockTimeout, if set, replaces the server's 10s wait for the
		// library lock.
		lockTimeout time.Duration
	}{
		// Deterministic interleaving: a failure replays exactly from its seed.
		{"stepped/devices", syncsim.Config{Devices: 3, Steps: 60, Files: true}, 0},
		{"stepped/group", syncsim.Config{Users: 3, Devices: 1, Group: true, Steps: 60, Files: true}, 0},
		// Real concurrency, with streaming-triggered syncs.
		{"concurrent/devices", syncsim.Config{Devices: 4, Steps: 50, Concurrent: true, Files: true}, 0},
		{"concurrent/group", syncsim.Config{Users: 3, Devices: 2, Group: true, Steps: 40, Concurrent: true, Files: true}, 0},
		// Lost requests and lost responses: every write may have gone
		// through without the client knowing.
		{"faults/devices", syncsim.Config{Devices: 3, Steps: 50, Concurrent: true, Files: true, FaultRate: 0.15}, 0},
		{"faults/group", syncsim.Config{Users: 3, Devices: 1, Group: true, Steps: 50, FaultRate: 0.2, Files: true}, 0},
		// Many clients on one library with short bursts: maximal contention
		// on the library lock and the If-Unmodified-Since-Version checks.
		{"contention", syncsim.Config{Users: 4, Devices: 2, Group: true, Steps: 25, Concurrent: true}, 0},
		// The same with a lock timeout so short that writes are turned away
		// with 503 and Retry-After. None of them may have been half applied.
		{"busy", syncsim.Config{Users: 4, Devices: 2, Group: true, Steps: 25, Concurrent: true, Files: true}, 2 * time.Millisecond},
		// The server crashes and restarts mid-workload: between steps in the
		// stepped run, mid-request in the concurrent one.
		{"crash/stepped", syncsim.Config{Devices: 3, Steps: 50, Files: true, RestartRate: 0.04}, 0},
		{"crash/concurrent", syncsim.Config{Users: 2, Devices: 2, Group: true, Steps: 40, Concurrent: true, Files: true, RestartRate: 0.03}, 0},
	}
	for i, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			if testing.Short() && strings.HasPrefix(sc.name, "faults") {
				t.Skip("short mode")
			}
			for _, seed := range syncsim.Seeds(uint64(1000 * (i + 1))) {
				t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
					cfg := sc.cfg
					var srv *httptest.Server
					var prov *gotero.Provisioner
					if cfg.RestartRate > 0 {
						var cs *crashableServer
						srv, cs = newCrashableServer(t)
						prov = simProvisioner(t, srv)
						cfg.Restart = cs.restart
					} else {
						scfg := simConfig(t)
						if sc.lockTimeout > 0 {
							scfg.DBLockTimeout = sc.lockTimeout
						}
						srv, prov = newSimServer(t, scfg)
					}
					cfg.Seed = seed
					cfg.BaseURL = srv.URL + "/api/"
					cfg.StreamURL = "ws" + strings.TrimPrefix(srv.URL, "http") + "/stream"
					cfg.Provisioner = prov
					syncsim.Test(t, cfg)
				})
			}
		})
	}
}
