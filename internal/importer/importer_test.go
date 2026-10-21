package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

func (e *env) sourceVersions(l *fakeLibrary, t repo.ObjectType) map[string]int64 {
	switch t {
	case repo.TypeCollection:
		return l.versionsOf(l.collections)
	case repo.TypeSearch:
		return l.versionsOf(l.searches)
	}
	return l.versionsOf(l.items)
}

func TestImportEverything(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fz.sentBackoff.Store(false)
	e.fz.sent429.Store(false)
	src := e.fz.lib("users", fakeUserID)
	job := e.enqueue(jobOpts{groups: true, files: true})
	started := time.Now()
	done := e.runJob(bg(), job.ID)
	assert.GreaterOrEqual(t, time.Since(started), time.Second, "Backoff and Retry-After were waited out")

	require.Equal(t, "completed", done.Status, done.Error)
	assert.Empty(t, done.Error)
	assert.Empty(t, done.SourceAPIKey, "the zotero.org key is wiped")
	assert.NotNil(t, done.FinishedAt)

	// Objects keep keys and versions.
	lib := e.user.LibraryID
	for _, typ := range []repo.ObjectType{repo.TypeCollection, repo.TypeSearch, repo.TypeItem} {
		assert.Equal(t, e.sourceVersions(src, typ), e.versions(lib, typ), typ)
	}
	r := e.reader(lib)
	assert.EqualValues(t, 50, r.Version(), "library version follows zotero.org")

	// Item columns match what an API write would have stored.
	got, err := r.Get(bg(), repo.TypeItem, []string{itBook, itPDF, itTrashed, itSnap})
	require.NoError(t, err)
	assert.Equal(t, "book", got[itBook].ItemType)
	assert.Empty(t, got[itBook].ParentKey)
	assert.False(t, got[itBook].InTrash)
	tags, err := r.Tags(bg(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, Keys: []string{itBook}}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []repo.ItemTag{{Tag: "unread", NumItems: 1}, {Tag: "auto", Type: 1, NumItems: 1}}, tags)
	assert.Equal(t, itBook, got[itPDF].ParentKey)
	assert.True(t, got[itTrashed].InTrash)
	assert.EqualValues(t, 41, got[itPDF].Version)
	assert.NotEmpty(t, got[itBook].ContentHash)
	var data zotero.OrderedMap
	require.NoError(t, json.Unmarshal(got[itBook].Data, &data))
	assert.False(t, data.Has("key"))
	assert.False(t, data.Has("version"))
	rel, _ := data.Get("relations")
	assert.JSONEq(t, `{"dc:relation":"http://zotero.org/users/4242/items/`+itNote+`"}`, string(rel))
	note, err := r.Get(bg(), repo.TypeItem, []string{itNote})
	require.NoError(t, err)
	var nd zotero.OrderedMap
	require.NoError(t, json.Unmarshal(note[itNote].Data, &nd))
	rel, _ = nd.Get("relations")
	assert.JSONEq(t, `{}`, string(rel), "empty relation arrays become objects")

	// Settings and full text.
	settings, err := r.Settings(bg(), 0)
	require.NoError(t, err)
	require.Len(t, settings, 1)
	assert.Equal(t, "tagColors", settings[0].Key)
	assert.EqualValues(t, 33, settings[0].Version)
	ft, err := r.FulltextVersions(bg(), 0)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{itPDF: 41, itSnap: 45}, ft)
	pdfFT, err := r.Fulltext(bg(), itPDF)
	require.NoError(t, err)
	assert.Equal(t, "pdf words", pdfFT.Content)
	require.NotNil(t, pdfFT.TotalPages)
	assert.Equal(t, 3, *pdfFT.TotalPages)
	assert.Nil(t, pdfFT.TotalChars)

	// Files: plain PDF and zipped snapshot.
	af, err := r.AttachmentFile(bg(), itPDF)
	require.NoError(t, err)
	assert.Equal(t, md5hex(pdfBytes), af.MD5)
	assert.EqualValues(t, len(pdfBytes), af.Size)
	assert.False(t, af.Compressed)
	assert.Equal(t, "paper.pdf", af.Filename)
	assert.EqualValues(t, 1700000000000, af.MtimeMS)
	assert.Equal(t, pdfBytes, e.blob(af.MD5))

	snap, err := r.AttachmentFile(bg(), itSnap)
	require.NoError(t, err)
	zipped := zipOf(t, "index.html", htmlBytes)
	assert.True(t, snap.Compressed)
	assert.Equal(t, md5hex(zipped), snap.MD5, "the blob is addressed by the zip's md5")
	assert.Equal(t, zipped, e.blob(snap.MD5))
	var sd zotero.OrderedMap
	require.NoError(t, json.Unmarshal(got[itSnap].Data, &sd))
	itemMD5, _ := sd.GetString("md5")
	assert.Equal(t, md5hex(htmlBytes), itemMD5, "the item keeps the file md5")

	_, err = r.AttachmentFile(bg(), itMissing)
	assert.ErrorIs(t, err, repo.ErrNotFound, "no file on zotero.org, no file here")
	assert.EqualValues(t, len(pdfBytes)+len(zipped), r.StorageUsed())

	// The group became a gotero group with the same ID.
	g, err := e.store.Groups().Get(bg(), fakeGroupID)
	require.NoError(t, err)
	assert.Equal(t, e.user.ID, g.OwnerID)
	assert.Equal(t, "Lab Group", g.Name)
	assert.Equal(t, "Our lab", g.Description)
	assert.Equal(t, "https://example.org/lab", g.URL)
	assert.Equal(t, "admins", g.LibraryEditing)
	assert.Equal(t, "members", g.LibraryReading)
	assert.Equal(t, "admins", g.FileEditing)
	members, err := e.store.Groups().Members(bg(), fakeGroupID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "other zotero.org members are not created")
	gsrc := e.fz.lib("groups", fakeGroupID)
	for _, typ := range []repo.ObjectType{repo.TypeCollection, repo.TypeItem} {
		assert.Equal(t, e.sourceVersions(gsrc, typ), e.versions(g.LibraryID, typ), typ)
	}
	assert.EqualValues(t, 90, e.reader(g.LibraryID).Version())

	// Progress has the shape the UI reads.
	p := progressOf(t, done)
	require.Len(t, p.Libraries, 2)
	assert.Equal(t, "My Library", p.Libraries[0].Name)
	assert.Equal(t, "Lab Group", p.Libraries[1].Name)
	assert.True(t, p.Libraries[0].Done && p.Libraries[1].Done)
	assert.EqualValues(t, 7, p.Libraries[0].Items)
	assert.EqualValues(t, 2, p.Libraries[1].Items)
	assert.EqualValues(t, (2+1+7)+(1+2), p.Objects)
	assert.Equal(t, p.Objects, p.Total)
	assert.EqualValues(t, 3, p.Files, "pdf, snapshot and the missing one are all accounted for")
	assert.Equal(t, p.Files, p.FilesAll)
	assert.EqualValues(t, len(pdfBytes)+len(zipped), p.Bytes)

	// The 429 and the Backoff header were survived on the way.
	assert.GreaterOrEqual(t, e.fz.itemFetches.Load(), int64(2))
}

func (e *env) blob(md5 string) []byte {
	e.t.Helper()
	rd, _, err := e.files.Open(bg(), md5)
	require.NoError(e.t, err)
	defer func() { _ = rd.Close() }()
	b, err := io.ReadAll(rd)
	require.NoError(e.t, err)
	return b
}

// TestImportServesThroughAPI proves a client switching servers sees the same
// versions: the imported data goes out through the real zapi.
func TestImportServesThroughAPI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	e.runJob(bg(), e.enqueue(jobOpts{groups: true, files: true}).ID)
	api := e.serveAPI()
	base := fmt.Sprintf("/api/users/%d", fakeUserID)

	for _, tc := range []struct {
		path string
		want map[string]int64
	}{
		{"/items?format=versions&includeTrashed=1", src.versionsOf(src.items)},
		{"/items/top?format=versions&includeTrashed=1", map[string]int64{
			itBook: 20, itSnap: 45, itTrashed: 30, itLinked: 31, itMissing: 32,
		}},
		{"/collections?format=versions", src.versionsOf(src.collections)},
		{"/searches?format=versions", src.versionsOf(src.searches)},
		{"/fulltext", src.ftVersions},
	} {
		resp, body := api.get(t, base+tc.path)
		require.Equal(t, 200, resp.StatusCode, "%s: %s", tc.path, body)
		var got map[string]int64
		require.NoError(t, json.Unmarshal(body, &got))
		assert.Equal(t, tc.want, got, tc.path)
		assert.Equal(t, "50", resp.Header.Get("Last-Modified-Version"), tc.path)
	}
	// Without includeTrashed the trashed item stays out, as on zotero.org.
	_, body := api.get(t, base+"/items?format=versions")
	var visible map[string]int64
	require.NoError(t, json.Unmarshal(body, &visible))
	assert.NotContains(t, visible, itTrashed)
	assert.Contains(t, visible, itBook)

	// A client that is at the library version has nothing left to fetch.
	assert.Empty(t, mustDecode(t, api, base+"/items?format=versions&includeTrashed=1&since=50"))

	// Full objects round trip.
	resp, body := api.get(t, base+"/items/"+itBook)
	require.Equal(t, 200, resp.StatusCode, string(body))
	var obj struct {
		Key     string
		Version int64
		Data    struct {
			Title     string `json:"title"`
			ItemType  string `json:"itemType"`
			Tags      []struct{ Tag string }
			Relations map[string]string
		}
	}
	require.NoError(t, json.Unmarshal(body, &obj))
	assert.EqualValues(t, 20, obj.Version)
	assert.Equal(t, "A Book", obj.Data.Title)
	assert.Len(t, obj.Data.Tags, 2)
	assert.Contains(t, obj.Data.Relations, "dc:relation")

	// And the file is downloadable.
	resp, _ = api.get(t, base+"/items/"+itPDF+"/file")
	require.Equal(t, 302, resp.StatusCode)
	assert.Equal(t, md5hex(pdfBytes), resp.Header.Get("Zotero-File-MD5"))
	assert.Equal(t, "No", resp.Header.Get("Zotero-File-Compressed"))
	resp, _ = api.get(t, base+"/items/"+itSnap+"/file")
	require.Equal(t, 302, resp.StatusCode)
	assert.Equal(t, md5hex(htmlBytes), resp.Header.Get("Zotero-File-MD5"))
	assert.Equal(t, "Yes", resp.Header.Get("Zotero-File-Compressed"))

	// Group library.
	gsrc := e.fz.lib("groups", fakeGroupID)
	resp, body = api.get(t, fmt.Sprintf("/api/groups/%d/items?format=versions&includeTrashed=1", fakeGroupID))
	require.Equal(t, 200, resp.StatusCode, string(body))
	var gv map[string]int64
	require.NoError(t, json.Unmarshal(body, &gv))
	assert.Equal(t, gsrc.versionsOf(gsrc.items), gv)
	assert.Equal(t, "90", resp.Header.Get("Last-Modified-Version"))
	resp, body = api.get(t, fmt.Sprintf("/api/users/%d/groups?format=versions", fakeUserID))
	require.Equal(t, 200, resp.StatusCode, string(body))
	assert.Contains(t, string(body), fmt.Sprintf("%q", fmt.Sprint(fakeGroupID)))
}

func mustDecode(t testing.TB, api zapiServer, path string) map[string]int64 {
	t.Helper()
	_, body := api.get(t, path)
	var m map[string]int64
	require.NoError(t, json.Unmarshal(body, &m))
	return m
}

func TestImportIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.runJob(bg(), e.enqueue(jobOpts{groups: true, files: true}).ID)
	lib := e.user.LibraryID
	before := e.versions(lib, repo.TypeItem)
	fetches := e.fz.itemFetches.Load()
	fileReqs := e.fz.fileRequests.Load() - e.fz.noFile404.Load()

	// A gotero-only object must survive.
	_, err := e.store.InLibrary(bg(), lib, func(tx repo.LibraryTx) error {
		return tx.PutObjects(bg(), repo.TypeItem, []repo.Object{{
			Key: "LLLLLLL2", Version: 49, ItemType: "book", Data: json.RawMessage(`{"itemType":"book","title":"Mine"}`), ContentHash: []byte("x"),
		}})
	})
	require.NoError(t, err)

	second := e.runJob(bg(), e.enqueue(jobOpts{groups: true, files: true}).ID)
	require.Equal(t, "completed", second.Status, second.Error)
	assert.Equal(t, fetches, e.fz.itemFetches.Load(), "nothing is fetched again")
	// The item without a file on zotero.org is asked about again; that is all.
	assert.Equal(t, fileReqs, e.fz.fileRequests.Load()-e.fz.noFile404.Load(), "files are not downloaded twice")

	after := e.versions(lib, repo.TypeItem)
	delete(after, "LLLLLLL2")
	assert.Equal(t, before, after)
	assert.Contains(t, e.versions(lib, repo.TypeItem), "LLLLLLL2", "objects only gotero has are left alone")

	// A change on zotero.org arrives on the next run, and only that change.
	src := e.fz.lib("users", fakeUserID)
	book := src.items[itBook]
	book.version = 60
	book.data["title"] = "A Book, revised"
	src.items[itBook] = book
	src.version = 60
	third := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", third.Status, third.Error)
	got, err := e.reader(lib).Get(bg(), repo.TypeItem, []string{itBook})
	require.NoError(t, err)
	// The first run was a fresh import, so this one allocates a new version
	// above the library's instead of reusing zotero.org's 60.
	assert.Equal(t, e.reader(lib).Version(), got[itBook].Version)
	assert.Greater(t, got[itBook].Version, before[itBook])
	assert.Contains(t, string(got[itBook].Data), "revised")
	assert.Equal(t, fetches+1, e.fz.itemFetches.Load(), "one item batch for the one change")
}

func TestImportNeverLowersLibraryVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.store.InLibrary(bg(), e.user.LibraryID, func(tx repo.LibraryTx) error { return tx.RaiseVersion(bg(), 500) })
	require.NoError(t, err)
	e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	lib := e.reader(e.user.LibraryID)
	assert.Greater(t, lib.Version(), int64(500))
	// Every imported object is newer than 500, so a client that synced at 500
	// sees all of them.
	for k, v := range e.versions(e.user.LibraryID, repo.TypeItem) {
		assert.Greater(t, v, int64(500), k)
		assert.LessOrEqual(t, v, lib.Version(), k)
	}
	for k, v := range e.versions(e.user.LibraryID, repo.TypeCollection) {
		assert.Greater(t, v, int64(500), k)
	}
}

func TestImportWithoutFilesOrGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.EqualValues(t, 0, e.fz.fileRequests.Load())
	_, err := e.store.Groups().Get(bg(), fakeGroupID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	assert.Len(t, progressOf(t, done).Libraries, 1)
}

func TestImportKeyWithoutFileAccessWarns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fz.access.User.Files = false
	done := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Contains(t, done.Error, "no file access")
	assert.EqualValues(t, 0, e.fz.fileRequests.Load())
}

func TestImportRejectedKeyFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job, err := e.store.Imports().Create(bg(), repo.ImportJob{UserID: e.user.ID, SourceAPIKey: "wrongwrongwrongwrongwrong"})
	require.NoError(t, err)
	done := e.runJob(bg(), job.ID)
	assert.Equal(t, "failed", done.Status)
	assert.Contains(t, done.Error, "refused access")
	assert.Empty(t, done.SourceAPIKey)
}

// A group ID that belongs to someone else is skipped, the rest imports.
func TestImportGroupIDConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	other, err := e.store.Users().Create(bg(), repo.User{Username: "bob", PasswordHash: "x"})
	require.NoError(t, err)
	_, err = e.store.Groups().Create(bg(), repo.Group{ID: fakeGroupID, OwnerID: other.ID, Name: "Bob's"})
	require.NoError(t, err)

	done := e.runJob(bg(), e.enqueue(jobOpts{groups: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	p := progressOf(t, done)
	require.Len(t, p.Libraries, 2)
	assert.True(t, p.Libraries[0].Done)
	assert.Contains(t, p.Libraries[1].Error, "already used by a group you do not own")
	assert.Contains(t, done.Error, "already used")
	g, err := e.store.Groups().Get(bg(), fakeGroupID)
	require.NoError(t, err)
	assert.Equal(t, other.ID, g.OwnerID)
	assert.Equal(t, "Bob's", g.Name)
	assert.Empty(t, e.versions(g.LibraryID, repo.TypeItem), "nothing leaked into the other group")
	assert.NotEmpty(t, e.versions(e.user.LibraryID, repo.TypeItem))
}

// A group the user already owns takes the import.
func TestImportIntoOwnGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	g, err := e.store.Groups().Create(bg(), repo.Group{ID: fakeGroupID, OwnerID: e.user.ID, Name: "Existing"})
	require.NoError(t, err)
	done := e.runJob(bg(), e.enqueue(jobOpts{groups: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Empty(t, done.Error)
	assert.Len(t, e.versions(g.LibraryID, repo.TypeItem), 2)
	got, _ := e.store.Groups().Get(bg(), fakeGroupID)
	assert.Equal(t, "Existing", got.Name, "an existing group is not renamed")
}

func TestImportGroupsNeedGroupAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fz.access.Groups = nil
	done := e.runJob(bg(), e.enqueue(jobOpts{groups: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Contains(t, done.Error, "no group access")
	_, err := e.store.Groups().Get(bg(), fakeGroupID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

// addItems puts n simple extra items into the source user library.
func addItems(l *fakeLibrary, n int) []string {
	const alpha = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"
	var keys []string
	for i := range n {
		key := "X" + string(alpha[i/len(alpha)/len(alpha)%len(alpha)]) + string(alpha[i/len(alpha)%len(alpha)]) + string(alpha[i%len(alpha)]) + "AAAA"
		l.items[key] = fakeObj{int64(100 + i), map[string]any{
			"itemType": "book", "title": fmt.Sprintf("Extra %d", i), "tags": []any{}, "collections": []string{}, "relations": map[string]any{},
			"dateAdded": "2024-04-01T00:00:00Z", "dateModified": "2024-04-01T00:00:00Z",
		}}
		keys = append(keys, key)
	}
	l.version = int64(100 + n)
	return keys
}

// objectFetchKeys records which item keys the importer asked for.
type objectFetchKeys struct {
	mu   sync.Mutex
	keys []string
}

func (o *objectFetchKeys) add(q string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keys = append(o.keys, strings.Split(q, ",")...)
}

func TestImportResumesAfterInterruption(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 250)

	ctx, cancel := context.WithCancel(bg())
	defer cancel()
	var first objectFetchKeys
	var batches int
	var mu sync.Mutex
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query().Get("itemKey")
		if q == "" {
			return false
		}
		mu.Lock()
		batches++
		n := batches
		mu.Unlock()
		if n == 3 {
			// The process dies while the third batch is in flight.
			cancel()
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		first.add(q)
		return false
	}
	job := e.enqueue(jobOpts{})
	interrupted := e.runJob(ctx, job.ID)
	require.Equal(t, "queued", interrupted.Status, "an interrupted job goes back in the queue")
	assert.Equal(t, fakeAPIKey, interrupted.SourceAPIKey, "the key is kept for the resume")
	var cur cursor
	require.NoError(t, json.Unmarshal(interrupted.Cursor, &cur))
	require.NotEmpty(t, cur.Libs)
	assert.Equal(t, phaseItems, cur.Libs[0].Phase)

	written := e.versions(e.user.LibraryID, repo.TypeItem)
	require.NotEmpty(t, written)
	require.Less(t, len(written), len(src.items))

	var second objectFetchKeys
	e.fz.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if q := r.URL.Query().Get("itemKey"); q != "" {
			second.add(q)
		}
		return false
	}
	done := e.runJob(bg(), job.ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Equal(t, e.sourceVersions(src, repo.TypeItem), e.versions(e.user.LibraryID, repo.TypeItem))
	for _, k := range second.keys {
		assert.NotContains(t, written, k, "batches written before the crash are not fetched again")
	}
	assert.EqualValues(t, src.version, e.reader(e.user.LibraryID).Version())
}

// A crash leaves the job running; the next Run picks it up again.
func TestRunRequeuesOrphanedJobs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job := e.enqueue(jobOpts{})
	claimed, err := e.store.Imports().ClaimNext(bg())
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	ctx, cancel := context.WithTimeout(bg(), 30*time.Second)
	defer cancel()
	go e.w.Run(ctx)
	require.Eventually(t, func() bool {
		j, err := e.store.Imports().Get(bg(), job.ID)
		return err == nil && j.Status == "completed"
	}, 20*time.Second, 50*time.Millisecond)
}

func TestImportCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 400)
	job := e.enqueue(jobOpts{files: true})

	var once sync.Once
	var afterCancel int
	var cancelled bool
	var mu sync.Mutex
	e.fz.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("itemKey") == "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if cancelled {
			afterCancel++
			return false
		}
		once.Do(func() {
			// What POST /imports/{id}/cancel does.
			j, err := e.store.Imports().Get(bg(), job.ID)
			require.NoError(t, err)
			now := time.Now()
			j.Status, j.FinishedAt, j.SourceAPIKey = "cancelled", &now, ""
			require.NoError(t, e.store.Imports().Update(bg(), j))
			cancelled = true
		})
		return false
	}
	got := e.runJob(bg(), job.ID)
	assert.Equal(t, "cancelled", got.Status, "the worker does not overwrite the user's cancel")
	assert.Empty(t, got.SourceAPIKey)
	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, afterCancel, 1, "the worker stops at the next checkpoint")
	assert.Less(t, len(e.versions(e.user.LibraryID, repo.TypeItem)), len(src.items))
}

func TestImportStopsAtQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	zipped := zipOf(t, "index.html", htmlBytes)
	// Room for the snapshot but not for the PDF as well.
	quota := int64(len(pdfBytes) + len(zipped) - 1)
	_, err := e.store.Users().Update(bg(), withQuota(e.user, quota))
	require.NoError(t, err)

	done := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, "a quota stop is a warning, not a failure")
	assert.Contains(t, done.Error, "Storage quota reached")
	r := e.reader(e.user.LibraryID)
	assert.LessOrEqual(t, r.StorageUsed(), quota)
	assert.Less(t, r.StorageUsed(), int64(len(pdfBytes)+len(zipped)))
	p := progressOf(t, done)
	assert.Less(t, p.Files, p.FilesAll, "progress shows files were left out")
	assert.Equal(t, e.sourceVersions(e.fz.lib("users", fakeUserID), repo.TypeItem), e.versions(e.user.LibraryID, repo.TypeItem),
		"metadata is complete regardless")

	// With more room the same import finishes the rest.
	_, err = e.store.Users().Update(bg(), withQuota(e.user, 1<<30))
	require.NoError(t, err)
	again := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", again.Status, again.Error)
	assert.Empty(t, again.Error)
	assert.EqualValues(t, len(pdfBytes)+len(zipped), e.reader(e.user.LibraryID).StorageUsed())
}

func withQuota(u repo.User, q int64) repo.User {
	u.QuotaBytes = &q
	return u
}

func TestImportFileFailureIsRetriedThenReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var mu sync.Mutex
	attempts := map[string]int{}
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/items/"+itPDF+"/file") {
			mu.Lock()
			attempts[itPDF]++
			n := attempts[itPDF]
			mu.Unlock()
			if n == 1 {
				// Not retried by zoteroclient (4xx), so only our retry helps.
				http.Error(w, "nope", http.StatusForbidden)
				return true
			}
		}
		return false
	}
	done := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Empty(t, done.Error, "one retry fixed it")
	_, err := e.reader(e.user.LibraryID).AttachmentFile(bg(), itPDF)
	require.NoError(t, err)

	// A permanent failure is recorded and does not fail the job.
	e2 := newEnv(t)
	e2.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/items/"+itPDF+"/file") {
			http.Error(w, "nope", http.StatusForbidden)
			return true
		}
		return false
	}
	done = e2.runJob(bg(), e2.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Contains(t, done.Error, "1 files could not be downloaded")
	assert.Contains(t, done.Error, itPDF)
	_, err = e2.reader(e2.user.LibraryID).AttachmentFile(bg(), itSnap)
	assert.NoError(t, err, "other files still arrive")
}

func TestValidateImportKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	got, err := e.w.ValidateImportKey(bg(), fakeAPIKey)
	require.NoError(t, err)
	assert.True(t, got.Valid)
	assert.True(t, got.Library)
	assert.True(t, got.Files)

	got, err = e.w.ValidateImportKey(bg(), "bad")
	require.NoError(t, err, "a rejected key is an answer, not an error")
	assert.False(t, got.Valid)

	e.fz.access.User.Files = false
	got, err = e.w.ValidateImportKey(bg(), fakeAPIKey)
	require.NoError(t, err)
	assert.True(t, got.Valid)
	assert.False(t, got.Files)

	e.fz.srv.Close()
	_, err = e.w.ValidateImportKey(bg(), fakeAPIKey)
	assert.Error(t, err, "network trouble is an error")
}

func TestRunForegroundPrintsProgress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job := e.enqueue(jobOpts{groups: true, files: true})
	var out strings.Builder
	require.NoError(t, e.w.RunForeground(bg(), job.ID, &out))
	assert.Contains(t, out.String(), "completed: objects 13/13, files 3/3")
	assert.Contains(t, out.String(), "libraries 2/2")
	got, err := e.store.Imports().Get(bg(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)

	failing := e.enqueue(jobOpts{})
	e.fz.access.User.Library = false
	err = e.w.RunForeground(bg(), failing.ID, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "import failed")
}
