package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/robertjndw/gotero/internal/accounts"
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
		// The same instance quota the server applies. The CLI has no
		// authenticator, which DefaultQuota does not need.
		DefaultQuota: accounts.New(store, nil, cfg, log).DefaultQuota,
		// Publisher and Notifier stay unset: they feed the stream hub,
		// which lives in the server process. Clients connected to a running
		// server learn about the import on their next regular sync.
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
	if !check.Notes {
		return errors.New("the API key cannot read your notes, so they would be left out; create one with notes access")
	}
	job, err := store.Imports().Create(ctx, repo.ImportJob{
		UserID: user.ID, SourceAPIKey: key, IncludeGroups: !*noGroups, IncludeFiles: !*noFiles,
	})
	if errors.Is(err, repo.ErrImportActive) {
		return resumeImport(ctx, w, store, user, stdout)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Import %d started for %s\n", job.ID, user.Username)
	return w.RunForeground(ctx, job.ID, stdout)
}

// resumeImport continues the user's unfinished import, for example one
// interrupted with Ctrl-C, instead of starting a second one into the same
// library. It keeps the options and key the job was queued with.
func resumeImport(ctx context.Context, w *importer.Worker, store repo.Store, user repo.User, stdout io.Writer) error {
	// A job a killed process left running counts as active until its lease
	// runs out; requeue it if it already has.
	if _, err := store.Imports().RequeueExpired(ctx); err != nil {
		return err
	}
	job, err := store.Imports().ActiveForUser(ctx, user.ID)
	if errors.Is(err, repo.ErrNotFound) {
		return errors.New("another import for this user just ended; run the command again")
	}
	if err != nil {
		return err
	}
	if job.Status == "running" {
		return fmt.Errorf("import %d for %s is already running in another process (the server or another gotero import); "+
			"wait for it or cancel it in the web UI. A process that was killed hands the job back after a few minutes", job.ID, user.Username)
	}
	_, _ = fmt.Fprintf(stdout, "Resuming import %d for %s, with the options it was started with\n", job.ID, user.Username)
	return w.RunForeground(ctx, job.ID, stdout)
}
