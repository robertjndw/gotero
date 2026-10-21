package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertjndw/gotero/internal/adminapi"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

// errCancelled means the user cancelled the job; its status is already set.
var errCancelled = errors.New("importer: job cancelled")

// Phases of one library, in order. A saved cursor resumes at its phase; the
// phases themselves are idempotent, so repeating a half-done one is cheap.
const (
	phaseCollections = "collections"
	phaseSearches    = "searches"
	phaseItems       = "items"
	phaseSettings    = "settings"
	phaseFulltext    = "fulltext"
	phaseFiles       = "files"
)

var phaseOrder = []string{phaseCollections, phaseSearches, phaseItems, phaseSettings, phaseFulltext, phaseFiles}

// Import modes. A fresh import adopts zotero.org's keys and versions as they
// are. A merge import writes with new library versions and never replaces
// edits made in gotero.
const (
	modeFresh = "fresh"
	modeMerge = "merge"
)

// maxPasses bounds how often a library is re-synced because zotero.org
// changed while we were reading it.
const maxPasses = 3

// libState is the resume point and the progress numbers of one library.
type libState struct {
	ID      string `json:"id"` // "user" or "group:<id>"
	GroupID int64  `json:"groupId,omitempty"`
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Pass    int    `json:"pass"`
	// LMV is the highest Last-Modified-Version seen on zotero.org. The gotero
	// library is raised to it when the library is done.
	LMV   int64  `json:"lmv"`
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
	// Mode is decided once per library and kept in the cursor, so a crash in
	// the middle of a fresh import resumes as a fresh import even though the
	// library is no longer empty by then.
	Mode string `json:"mode,omitempty"`
	// Conflicts counts, per phase, objects left alone because they changed in
	// gotero after the importer wrote them.
	Conflicts map[string]int64 `json:"conflicts,omitempty"`

	// Total and Finished are per phase so a repeated pass overwrites instead of
	// double counting. Objects already up to date count as done.
	Total      map[string]int64 `json:"total,omitempty"`
	Finished   map[string]int64 `json:"finished,omitempty"`
	Files      int64            `json:"files"`
	FilesTotal int64            `json:"filesTotal"`
	Bytes      int64            `json:"bytes"`
}

// cursor is stored in import_jobs.cursor.
type cursor struct {
	Libs       []*libState `json:"libs"`
	Discovered bool        `json:"discovered"`
}

// run is the state of one job being processed.
type run struct {
	w   *Worker
	job repo.ImportJob
	log *slog.Logger

	client     *zoteroclient.Client
	user       repo.User
	userID     int64 // zotero.org user ID
	fileAccess bool
	// keyFiles remembers what the queueing step noted, kept in progress.
	keyFiles *bool

	mu       sync.Mutex // guards cur, lastSave, warnings, failed
	cur      cursor
	lastSave time.Time
	warnings []string
	failed   []string
	skipped  int

	quotaHit atomic.Bool
	qmu      sync.Mutex
	inflight int64
	quota    *int64
	quotaSet bool
}

func newRun(w *Worker, job repo.ImportJob, log *slog.Logger) *run {
	return &run{w: w, job: job, log: log}
}

// update mutates shared state under the lock.
func (r *run) update(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

func (r *run) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.update(func() {
		if !slices.Contains(r.warnings, msg) {
			r.warnings = append(r.warnings, msg)
		}
	})
}

// execute runs the whole job. The string is a warning for a job that
// completed but did not get everything.
func (r *run) execute(ctx context.Context) (string, error) {
	if r.job.SourceAPIKey == "" {
		return "", errors.New("the zotero.org API key is missing")
	}
	user, err := r.w.Store.Users().Get(ctx, r.job.UserID)
	if err != nil {
		return "", fmt.Errorf("load user: %w", err)
	}
	r.user = user
	r.client = r.w.newClient(r.job.SourceAPIKey)
	info, err := r.client.CurrentKey(ctx)
	if err != nil {
		return "", err
	}
	if !info.Access.User.Library {
		return "", errors.New("the API key cannot read your library. Create a key with library access")
	}
	r.userID = info.UserID
	r.fileAccess = r.job.IncludeFiles
	if r.job.IncludeFiles && !info.Access.User.Files {
		r.fileAccess = false
		r.warn("The API key has no file access, so no files were imported.")
	}
	if r.fileAccess && r.w.Files == nil {
		r.fileAccess = false
		r.warn("File storage is not enabled on this server, so no files were imported.")
	}
	keyFiles := info.Access.User.Files
	r.keyFiles = &keyFiles

	if len(r.job.Cursor) > 0 {
		// An unreadable cursor only costs a re-diff of the libraries.
		_ = json.Unmarshal(r.job.Cursor, &r.cur)
	}
	if len(r.cur.Libs) == 0 {
		r.cur.Libs = []*libState{{ID: "user", Name: "My Library"}}
		r.cur.Discovered = false
	}
	if r.job.IncludeGroups && !r.cur.Discovered {
		if err := r.discoverGroups(ctx, info); err != nil {
			return "", err
		}
		r.cur.Discovered = true
	}
	if err := r.checkpoint(ctx, true); err != nil {
		return "", err
	}

	for _, st := range r.cur.Libs {
		if st.Done {
			continue
		}
		if err := r.importLibrary(ctx, st); err != nil {
			return "", err
		}
		if err := r.checkpoint(ctx, true); err != nil {
			return "", err
		}
	}
	return r.summary(), nil
}

func (r *run) discoverGroups(ctx context.Context, info *zotero.KeyInfo) error {
	acc := info.Access.Groups
	readAll := acc[zotero.AllGroups].Library
	if !readAll && len(acc) == 0 {
		r.warn("The API key has no group access, so no groups were imported.")
		return nil
	}
	versions, err := r.client.UserGroupVersions(ctx, info.UserID)
	if zoteroclient.IsForbidden(err) {
		r.warn("The API key has no group access, so no groups were imported.")
		return nil
	}
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(versions))
	for id := range versions {
		if readAll || acc[strconv.FormatInt(id, 10)].Library {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		r.cur.Libs = append(r.cur.Libs, &libState{
			ID: "group:" + strconv.FormatInt(id, 10), GroupID: id, Name: "Group " + strconv.FormatInt(id, 10),
		})
	}
	return nil
}

// summary is the warning shown on a completed job.
func (r *run) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := slices.Clone(r.warnings)
	if len(r.failed) > 0 {
		shown := r.failed[:min(len(r.failed), 5)]
		parts = append(parts, fmt.Sprintf("%d files could not be downloaded (items %s). Run the import again to retry them.",
			len(r.failed), strings.Join(shown, ", ")))
	}
	if r.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d objects from zotero.org were skipped because their data was invalid.", r.skipped))
	}
	for _, st := range r.cur.Libs {
		if n := sum(st.Conflicts); n > 0 {
			parts = append(parts, st.Name+": "+conflictMessage(n))
		}
		if st.Error != "" && st.Error != libErrFilesFailed && st.Error != libErrQuota && st.Error != conflictMessage(sum(st.Conflicts)) {
			parts = append(parts, st.Name+": "+st.Error)
		}
	}
	return strings.Join(parts, " ")
}

// conflictMessage is the per-library warning for objects the importer kept.
func conflictMessage(n int64) string {
	return fmt.Sprintf("%d objects were changed in gotero after the last import and were not overwritten.", n)
}

func sum(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// snapshot renders cursor and progress. Callers hold r.mu.
func (r *run) snapshot() (cur, prog json.RawMessage) {
	p := adminapi.ImportProgress{Libraries: []adminapi.ImportLibrary{}, FileAccess: r.keyFiles}
	for _, st := range r.cur.Libs {
		p.Libraries = append(p.Libraries, adminapi.ImportLibrary{
			Name: st.Name, Items: st.Finished[phaseItems], Files: st.Files, Done: st.Done, Error: st.Error,
			Conflicts: sum(st.Conflicts),
		})
		p.Objects += sum(st.Finished)
		p.Total += sum(st.Total)
		p.Files += st.Files
		p.FilesAll += st.FilesTotal
		p.Bytes += st.Bytes
	}
	cur, _ = json.Marshal(r.cur)
	prog, _ = json.Marshal(p)
	return cur, prog
}

// checkpoint saves cursor and progress, at most once per ProgressInterval
// unless forced. It doubles as the cancellation check: the save is refused
// once the job is no longer running.
func (r *run) checkpoint(ctx context.Context, force bool) error {
	r.mu.Lock()
	if !force && time.Since(r.lastSave) < r.w.ProgressInterval {
		r.mu.Unlock()
		return nil
	}
	r.lastSave = time.Now()
	cur, prog := r.snapshot()
	r.mu.Unlock()

	ok, err := r.w.Store.Imports().SaveProgress(ctx, r.job.ID, cur, prog)
	if err != nil {
		return fmt.Errorf("save import progress: %w", err)
	}
	if !ok {
		return errCancelled
	}
	return nil
}

// checkpointQuiet is the last save before a job ends; failing to write
// progress must not change the outcome.
func (r *run) checkpointQuiet(ctx context.Context) {
	if r.client == nil {
		return
	}
	if err := r.checkpoint(ctx, true); err != nil && !errors.Is(err, errCancelled) {
		r.log.WarnContext(ctx, "importer: final progress save", "err", err)
	}
}
