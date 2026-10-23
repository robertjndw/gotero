// Package pg is the PostgreSQL implementation of the repo interfaces.
package pg

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open connects to Postgres and verifies the connection. maxConns sizes the
// pool unless the DSN already carries pool_max_conns, which wins so an
// operator's explicit URL is never silently overridden; zero keeps pgx's
// default.
func Open(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 && !strings.Contains(dsn, "pool_max_conns") {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

func provider(pool *pgxpool.Pool) (*goose.Provider, *sql.DB, error) {
	db := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, db, mustSub(migrations, "migrations"))
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return p, db, nil
}

// Migrate applies all pending migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	p, db, err := provider(pool)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	results, err := p.Up(ctx)
	for _, r := range results {
		log.Info("applied migration", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
	}
	return err
}

// MigrationStatus reports each known migration and whether it is applied.
func MigrationStatus(ctx context.Context, pool *pgxpool.Pool) ([]*goose.MigrationStatus, error) {
	p, db, err := provider(pool)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return p.Status(ctx)
}
