// Command gotero is a self-hosted sync server compatible with Zotero.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/server"
	"github.com/robertjndw/gotero/internal/version"
)

const usageHead = `gotero - self-hosted sync server compatible with Zotero

Usage:
  gotero serve            run the server (applies migrations first)
  gotero migrate [status] apply migrations, or show their status
  gotero healthcheck      probe the local server (for container health checks)
  gotero version          print version

Accounts (these need the database and a migrated schema):
`

const usageTail = `

Configuration is read from GOTERO_* environment variables, see docs/configuration.md.
`

var usage = usageHead + accounts.CLIUsage + "\n\nImport from zotero.org:\n" + importUsage + usageTail

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("missing command")
	}
	if args[0] == "version" {
		fmt.Printf("gotero %s (%s)\n", version.Version, version.Commit)
		return nil
	}
	if args[0] == "healthcheck" {
		return healthcheck()
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pg.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "user", "invite":
		// The CLI has no authenticator: it cannot evict keys cached by a
		// running server, which the commands tell the operator about.
		svc := accounts.New(pg.New(pool), nil, cfg, log)
		return svc.RunCLI(ctx, args, os.Stdin, os.Stdout, os.Stderr)
	case "import":
		return runImport(ctx, cfg, log, pg.New(pool), args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "serve":
		if err := pg.Migrate(ctx, pool, log); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		srv, err := server.New(ctx, cfg, log, pool)
		if err != nil {
			return err
		}
		return srv.Run(ctx)
	case "migrate":
		if len(args) > 1 && args[1] == "status" {
			st, err := pg.MigrationStatus(ctx, pool)
			if err != nil {
				return err
			}
			for _, m := range st {
				fmt.Printf("%-8s %05d %s\n", m.State, m.Source.Version, m.Source.Path)
			}
			return nil
		}
		return pg.Migrate(ctx, pool, log)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// healthcheck exists because the distroless image has no curl or wget.
func healthcheck() error {
	addr := os.Getenv("GOTERO_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	client := http.Client{Timeout: 3 * time.Second}
	// The target is always loopback; only the port comes from config.
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz") //nolint:gosec,noctx
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}
