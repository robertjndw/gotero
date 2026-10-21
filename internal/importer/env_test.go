package importer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/storage/local"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func bg() context.Context { return context.Background() }

// env is a gotero database plus a fake zotero.org and a worker wired to both.
type env struct {
	t     testing.TB
	store *pg.Store
	files *files.Service
	user  repo.User
	fz    *fakeZotero
	w     *Worker
}

func newEnv(t testing.TB) *env {
	t.Helper()
	pool := pgtest.New(t)
	store := pg.New(pool)
	backend, err := local.New(t.TempDir())
	require.NoError(t, err)
	fs := files.NewService(backend, store.Files(), "0123456789abcdef0123456789abcdef")
	// The same ID as on zotero.org, so a client can switch servers without a resync.
	u, err := store.Users().Create(bg(), repo.User{ID: fakeUserID, Username: "alice", PasswordHash: "x"})
	require.NoError(t, err)
	fz := newFakeZotero(t)
	// Rate limit simulation costs seconds of real waiting; tests that care opt in.
	fz.sentBackoff.Store(true)
	fz.sent429.Store(true)
	w := New(Deps{
		Store: store, Files: fs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BaseURL: fz.URL(), ProgressInterval: time.Millisecond, PollInterval: 20 * time.Millisecond,
	})
	w.retryDelay = time.Millisecond
	return &env{t: t, store: store, files: fs, user: u, fz: fz, w: w}
}

type jobOpts struct{ groups, files bool }

func (e *env) enqueue(o jobOpts) repo.ImportJob {
	e.t.Helper()
	j, err := e.store.Imports().Create(bg(), repo.ImportJob{
		UserID: e.user.ID, SourceAPIKey: fakeAPIKey, IncludeGroups: o.groups, IncludeFiles: o.files,
	})
	require.NoError(e.t, err)
	return j
}

// runJob processes the next queued job and returns it as stored afterwards.
func (e *env) runJob(ctx context.Context, id int64) repo.ImportJob {
	e.t.Helper()
	did, err := e.w.RunOnce(ctx)
	require.NoError(e.t, err)
	require.True(e.t, did, "a job was queued")
	j, err := e.store.Imports().Get(bg(), id)
	require.NoError(e.t, err)
	return j
}

func (e *env) reader(libID int64) repo.LibraryReader {
	e.t.Helper()
	r, err := e.store.Library(bg(), libID)
	require.NoError(e.t, err)
	return r
}

func (e *env) versions(libID int64, t repo.ObjectType) map[string]int64 {
	e.t.Helper()
	v, err := (&run{w: e.w}).localVersions(bg(), libID, t)
	require.NoError(e.t, err)
	return v
}

type progressView struct {
	Libraries []struct {
		Name  string `json:"name"`
		Items int64  `json:"items"`
		Files int64  `json:"files"`
		Done  bool   `json:"done"`
		Error string `json:"error"`
		// Conflicts counts objects kept as edited in gotero.
		Conflicts int64 `json:"conflicts"`
	} `json:"libraries"`
	Objects  int64 `json:"objects"`
	Total    int64 `json:"objectsTotal"`
	Files    int64 `json:"files"`
	FilesAll int64 `json:"filesTotal"`
	Bytes    int64 `json:"bytes"`
}

func progressOf(t testing.TB, j repo.ImportJob) progressView {
	t.Helper()
	var p progressView
	require.NoError(t, json.Unmarshal(j.Progress, &p))
	return p
}

// zapiServer serves the gotero database through the real Zotero API.
type zapiServer struct {
	URL string
	Key string
}

func (e *env) serveAPI() zapiServer {
	e.t.Helper()
	a := auth.NewAuthenticator(e.store.Keys(), e.store.Users())
	api := zapi.New(zapi.Deps{
		Store: e.store, Auth: a, Schema: schema.Default(), Files: e.files,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxRequestBytes: 1 << 20,
	})
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.ForwardedByClientIP = false
	api.Register(engine.Group("/api"))
	api.RegisterFileRoutes(engine.Group("/files"))
	srv := httptest.NewServer(httpx.Chain(engine, httpx.ResolvePublicURL("", nil)))
	e.t.Cleanup(srv.Close)

	key := zotero.NewAPIKey()
	_, err := e.store.Keys().Create(bg(), repo.APIKey{
		UserID: e.user.ID, Hash: auth.HashToken(key), Prefix: key[:4], Name: "t",
		Access: repo.KeyAccess{Library: true, Notes: true, Files: true, Groups: "all"},
	})
	require.NoError(e.t, err)
	return zapiServer{URL: srv.URL, Key: key}
}

// apiResp is the part of a response the tests look at. Returning it instead
// of *http.Response keeps the body handling in one place.
type apiResp struct {
	StatusCode int
	Header     http.Header
}

func (s zapiServer) get(t testing.TB, path string) (apiResp, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(bg(), http.MethodGet, s.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Zotero-API-Key", s.Key)
	req.Header.Set("Zotero-API-Version", "3")
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return apiResp{StatusCode: resp.StatusCode, Header: resp.Header}, b
}
