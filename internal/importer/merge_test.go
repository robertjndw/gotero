package importer

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero"
)

// editInGotero changes an item the way a client write would: new content,
// new hash and a fresh library version.
func (e *env) editInGotero(libID int64, key string, mutate func(m *zotero.OrderedMap)) int64 {
	e.t.Helper()
	got, err := e.reader(libID).Get(bg(), repo.TypeItem, []string{key})
	require.NoError(e.t, err)
	var m zotero.OrderedMap
	require.NoError(e.t, json.Unmarshal(got[key].Data, &m))
	mutate(&m)
	obj, err := zapi.ObjectFromData(repo.TypeItem, key, m)
	require.NoError(e.t, err)
	v, err := e.store.InLibrary(bg(), libID, func(tx repo.LibraryTx) error {
		v, err := tx.NextVersion(bg())
		if err != nil {
			return err
		}
		obj.Version = v
		return tx.PutObjects(bg(), repo.TypeItem, []repo.Object{obj})
	})
	require.NoError(e.t, err)
	return v
}

func (e *env) item(libID int64, key string) repo.Object {
	e.t.Helper()
	got, err := e.reader(libID).Get(bg(), repo.TypeItem, []string{key})
	require.NoError(e.t, err)
	return got[key]
}

func TestFreshImportRecordsWhatItWrote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	var cur cursor
	require.NoError(t, json.Unmarshal(done.Cursor, &cur))
	assert.Equal(t, modeFresh, cur.Libs[0].Mode)

	recs, err := e.reader(e.user.LibraryID).ImportedObjects(bg(), repo.TypeItem)
	require.NoError(t, err)
	assert.Len(t, recs, len(e.fz.lib("users", fakeUserID).items))
	assert.EqualValues(t, 20, recs[itBook].SourceVersion)
}

// A crash in the middle of a fresh import leaves a non-empty library behind.
// The resume must still keep zotero.org's versions.
func TestFreshImportResumesAsFresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 250)

	ctx, cancel := context.WithCancel(bg())
	defer cancel()
	var batches atomic.Int64
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("itemKey") != "" && batches.Add(1) == 3 {
			cancel()
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}
	job := e.enqueue(jobOpts{})
	interrupted := e.runJob(ctx, job.ID)
	require.Equal(t, "queued", interrupted.Status)
	require.NotZero(t, e.reader(e.user.LibraryID).Version(), "the library is no longer empty")
	var cur cursor
	require.NoError(t, json.Unmarshal(interrupted.Cursor, &cur))
	assert.Equal(t, modeFresh, cur.Libs[0].Mode)

	e.fz.hook = nil
	done := e.runJob(bg(), job.ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Equal(t, e.sourceVersions(src, repo.TypeItem), e.versions(e.user.LibraryID, repo.TypeItem))
	assert.EqualValues(t, src.version, e.reader(e.user.LibraryID).Version())
}

func TestRerunKeepsEditsMadeInGotero(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	e.runJob(bg(), e.enqueue(jobOpts{}).ID)

	// Edited in gotero, and also changed on zotero.org afterwards.
	e.editInGotero(lib, itBook, func(m *zotero.OrderedMap) { m.Set("title", json.RawMessage(`"Mine"`)) })
	// Only changed on zotero.org.
	src := e.fz.lib("users", fakeUserID)
	for _, k := range []string{itBook, itNote} {
		o := src.items[k]
		o.version = 70
		if k == itBook {
			o.data["title"] = "Theirs"
		} else {
			o.data["note"] = "<p>newer</p>"
		}
		src.items[k] = o
	}
	src.version = 70
	before := e.reader(lib).Version()
	noteBefore := e.item(lib, itNote).Version

	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)

	assert.Contains(t, string(e.item(lib, itBook).Data), "Mine", "the gotero edit survives")
	note := e.item(lib, itNote)
	assert.Contains(t, string(note.Data), "newer", "untouched objects still follow zotero.org")
	assert.Greater(t, note.Version, noteBefore)
	assert.Greater(t, note.Version, before, "the new version is above everything a client has seen")
	assert.Equal(t, e.reader(lib).Version(), note.Version)

	p := progressOf(t, done)
	assert.EqualValues(t, 1, p.Libraries[0].Conflicts)
	assert.Contains(t, p.Libraries[0].Error, "1 objects were changed in gotero")
	assert.Contains(t, done.Error, "changed in gotero")

	// Running again does not turn the conflict into an overwrite.
	again := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", again.Status, again.Error)
	assert.Contains(t, string(e.item(lib, itBook).Data), "Mine")
}

// Unchanged objects are not rewritten, so a rerun does not move versions.
func TestRerunWithoutChangesWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	// Make the second run a merge one: gotero moves ahead of the source.
	e.editInGotero(lib, itNote, func(m *zotero.OrderedMap) { m.Set("note", json.RawMessage(`"<p>mine</p>"`)) })
	before := e.versions(lib, repo.TypeItem)
	v := e.reader(lib).Version()

	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Equal(t, before, e.versions(lib, repo.TypeItem))
	assert.Equal(t, v, e.reader(lib).Version())
}

// An object deleted in gotero stays deleted when zotero.org later changes it.
func TestRerunDoesNotResurrectDeletedObject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	_, err := e.store.InLibrary(bg(), lib, func(tx repo.LibraryTx) error {
		v, err := tx.NextVersion(bg())
		if err != nil {
			return err
		}
		_, err = tx.DeleteObjects(bg(), repo.TypeItem, []string{itNote}, v)
		return err
	})
	require.NoError(t, err)
	src := e.fz.lib("users", fakeUserID)
	o := src.items[itNote]
	o.version, o.data["note"] = 80, "<p>changed</p>"
	src.items[itNote], src.version = o, 80

	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.NotContains(t, e.versions(lib, repo.TypeItem), itNote)
	assert.EqualValues(t, 1, progressOf(t, done).Libraries[0].Conflicts)
}

func TestMergeKeepsSettingsAndTakesNewOnes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	_, err := e.store.InLibrary(bg(), lib, func(tx repo.LibraryTx) error {
		v, err := tx.NextVersion(bg())
		if err != nil {
			return err
		}
		return tx.PutSettings(bg(), []repo.Setting{{Key: "tagColors", Value: json.RawMessage(`[]`), Version: v}})
	})
	require.NoError(t, err)
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	set, err := e.reader(lib).Settings(bg(), 0)
	require.NoError(t, err)
	require.Len(t, set, 1)
	assert.JSONEq(t, `[]`, string(set[0].Value), "a setting that exists is not replaced")
}

func TestMergeFulltextGetsNewVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	_, err := e.store.InLibrary(bg(), lib, func(tx repo.LibraryTx) error { return tx.RaiseVersion(bg(), 300) })
	require.NoError(t, err)
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	ft, err := e.reader(lib).FulltextVersions(bg(), 0)
	require.NoError(t, err)
	require.Len(t, ft, 2)
	for k, v := range ft {
		assert.Greater(t, v, int64(300), k)
	}
}

// A client uploads a new file while the importer downloads the old one. The
// attachment must keep following the client's file.
func TestFileRegistrationSkipsItemChangedMeanwhile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	lib := e.user.LibraryID
	var once atomic.Bool
	e.fz.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/items/"+itPDF+"/file") && once.CompareAndSwap(false, true) {
			e.editInGotero(lib, itPDF, func(m *zotero.OrderedMap) {
				m.Set("md5", json.RawMessage(`"`+md5hex([]byte("new bytes"))+`"`))
				m.Set("mtime", json.RawMessage(`1800000000000`))
			})
		}
		return false
	}
	done := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	require.True(t, once.Load())

	_, err := e.reader(lib).AttachmentFile(bg(), itPDF)
	assert.ErrorIs(t, err, repo.ErrNotFound, "the stale blob was not registered")
	assert.GreaterOrEqual(t, progressOf(t, done).Libraries[0].Conflicts, int64(1))
	_, err = e.reader(lib).AttachmentFile(bg(), itSnap)
	assert.NoError(t, err, "other files are unaffected")
}

type recordingNotifier struct{ users []int64 }

func (n *recordingNotifier) AccessChanged(id int64) { n.users = append(n.users, id) }

func TestImportNotifiesAboutGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := &recordingNotifier{}
	e.w.Notifier = n
	e.runJob(bg(), e.enqueue(jobOpts{groups: true}).ID)
	assert.Equal(t, []int64{e.user.ID}, n.users)

	// Without groups there is nothing to announce, and a nil Notifier is fine.
	e2 := newEnv(t)
	e2.runJob(bg(), e2.enqueue(jobOpts{groups: true}).ID)
}
