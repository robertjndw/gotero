package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/importer"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/server"
)

const importUsage = "  gotero import --user <username> --api-key-stdin [--no-files] [--no-groups]"

// runImport queues an import for a user and runs it in the foreground.
func runImport(ctx context.Context, cfg *config.Config, log *slog.Logger, store repo.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	username := fs.String("user", "", "gotero username that receives the library")
	keyStdin := fs.Bool("api-key-stdin", false, "read the zotero.org API key from stdin")
	noFiles := fs.Bool("no-files", false, "do not import attachment files")
	noGroups := fs.Bool("no-groups", false, "do not import group libraries")
	baseURL := fs.String("base-url", "", "zotero.org API root (for testing)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" || !*keyStdin {
		return fmt.Errorf("usage:\n%s", importUsage)
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return errors.New("no API key on stdin")
	}
	user, err := store.Users().GetByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("user %q: %w", *username, err)
	}

	backend, err := server.NewBackend(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	w := importer.New(importer.Deps{
		Store: store, Files: files.NewService(backend, store.Files(), cfg.Secret), Log: log, BaseURL: *baseURL,
	})
	check, err := w.ValidateImportKey(ctx, key)
	if err != nil {
		return fmt.Errorf("could not reach zotero.org: %w", err)
	}
	if !check.Valid {
		return errors.New("zotero.org rejected the API key")
	}
	if !check.Library {
		return errors.New("the API key cannot read your library; create one with library access")
	}
	job, err := store.Imports().Create(ctx, repo.ImportJob{
		UserID: user.ID, SourceAPIKey: key, IncludeGroups: !*noGroups, IncludeFiles: !*noFiles,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Import %d started for %s\n", job.ID, user.Username)
	return w.RunForeground(ctx, job.ID, stdout)
}
