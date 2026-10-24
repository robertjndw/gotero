// Package pgtest hands out isolated, fully migrated Postgres databases for
// tests. One container serves the whole test binary; every test gets its own
// database cloned from a migrated template, which is far cheaper than running
// migrations per test and keeps tests safe to run in parallel.
package pgtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/robertjndw/gotero/internal/repo/pg"
)

const (
	image    = "postgres:18-alpine"
	template = "gotero_tpl"
)

var (
	once    sync.Once
	startOK error
	ctr     *tcpostgres.PostgresContainer
	baseURL *url.URL
	admin   *pgxpool.Pool
	counter atomic.Int64
)

// Main is meant to be called from TestMain. It runs the tests and then tears
// the container down; without it the container is reaped by testcontainers'
// reaper when the process exits.
func Main(m *testing.M) {
	code := m.Run()
	if admin != nil {
		admin.Close()
	}
	if ctr != nil {
		_ = testcontainers.TerminateContainer(ctr)
	}
	os.Exit(code)
}

func start(ctx context.Context) error {
	var err error
	ctr, err = tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	if baseURL, err = url.Parse(dsn); err != nil {
		return err
	}
	if admin, err = pgxpool.New(ctx, dsn); err != nil {
		return err
	}
	if _, err = admin.Exec(ctx, `CREATE DATABASE `+template); err != nil {
		return err
	}
	tpl, err := pgxpool.New(ctx, dsnFor(template))
	if err != nil {
		return err
	}
	// The template must have no open connections when it is cloned.
	defer tpl.Close()
	return pg.Migrate(ctx, tpl, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func dsnFor(db string) string {
	u := *baseURL
	u.Path = "/" + db
	return u.String()
}

// New returns a pool on a fresh database. The test is skipped in -short mode
// or when Docker is not available.
func New(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	pool, err := pgxpool.New(context.Background(), NewDSN(tb))
	if err != nil {
		tb.Fatalf("connect test database: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

// NewDSN creates a fresh database like New and returns its connection
// string, for tests that open and close their own pools, such as ones that
// restart the server.
func NewDSN(tb testing.TB) string {
	tb.Helper()
	if testing.Short() {
		tb.Skip("skipping database test in -short mode")
	}
	if err := dockerHealthy(); err != nil {
		tb.Skipf("docker unavailable: %v", err)
	}

	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		startOK = start(ctx)
	})
	if startOK != nil {
		tb.Fatalf("pgtest: %v", startOK)
	}

	name := fmt.Sprintf("test_%d", counter.Add(1))
	if _, err := admin.Exec(context.Background(), fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, template)); err != nil {
		tb.Fatalf("create test database: %v", err)
	}
	// Registered first, so it runs after the cleanups of whatever the test
	// connects; FORCE drops any connection that is still open anyway.
	tb.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return dsnFor(name)
}

func dockerHealthy() error {
	p, err := testcontainers.NewDockerProvider()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Health(ctx)
}
