package importer

import (
	"archive/zip"
	"bytes"
	"crypto/md5" //nolint:gosec // test fixture
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertjndw/gotero/zotero"
)

const (
	fakeAPIKey  = "AbCdEfGhIjKlMnOpQrStUvWx"
	fakeUserID  = 4242
	fakeGroupID = 777
)

// Fixture keys. They only use the characters object keys may contain.
const (
	colRoot   = "CCCCCCC2"
	colChild  = "CCCCCCC3"
	search1   = "SSSSSSS2"
	itBook    = "IIIIIII2"
	itPDF     = "IIIIIII3"
	itNote    = "IIIIIII4"
	itSnap    = "IIIIIII5"
	itTrashed = "IIIIIII6"
	itLinked  = "IIIIIII7"
	itMissing = "IIIIIII8" // has an md5 but zotero.org has no file
	gItBook   = "GGGGGGG2"
	gItNote   = "GGGGGGG3"
	gCol      = "GGGGGGC2"
)

var (
	pdfBytes   = bytes.Repeat([]byte("%PDF-1.4 fake pdf body\n"), 200)
	htmlBytes  = []byte("<html><body>snapshot</body></html>")
	missingMD5 = md5hex([]byte("never uploaded"))
)

func md5hex(b []byte) string {
	h := md5.Sum(b) //nolint:gosec // test fixture
	return hex.EncodeToString(h[:])
}

func zipOf(t testing.TB, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeObj struct {
	version int64
	data    map[string]any // without key and version
}

type fakeFile struct {
	body       []byte
	md5        string // value of Zotero-File-MD5
	compressed bool
	mtime      int64
}

// fakeLibrary is one library on the fake zotero.org.
type fakeLibrary struct {
	group       bool
	id          int64
	version     int64
	collections map[string]fakeObj
	searches    map[string]fakeObj
	items       map[string]fakeObj
	settings    map[string]zotero.OrderedMap
	setVersions map[string]int64
	fulltext    map[string]zotero.FulltextContent
	ftVersions  map[string]int64
	files       map[string]fakeFile
}

// fakeZotero imitates api.zotero.org closely enough for the importer: the
// version endpoints, key lists, files via redirect, Backoff and 429.
type fakeZotero struct {
	t   testing.TB
	srv *httptest.Server

	libs      map[string]*fakeLibrary // "users/ID" or "groups/ID"
	groupMeta map[int64]zotero.GroupData
	access    zotero.Access

	// hook sees every request before it is handled; returning true means it
	// wrote the response itself.
	hook func(w http.ResponseWriter, r *http.Request) bool

	itemFetches  atomic.Int64
	fileRequests atomic.Int64
	sentBackoff  atomic.Bool
	sent429      atomic.Bool
	noFile404    atomic.Int64
}

func (f *fakeZotero) lib(kind string, id int64) *fakeLibrary {
	return f.libs[fmt.Sprintf("%s/%d", kind, id)]
}

func newFakeZotero(t testing.TB) *fakeZotero {
	t.Helper()
	zipped := zipOf(t, "index.html", htmlBytes)
	u := &fakeLibrary{
		id: fakeUserID, version: 50,
		collections: map[string]fakeObj{
			colRoot:  {10, map[string]any{"name": "Root", "parentCollection": false, "relations": map[string]any{}}},
			colChild: {12, map[string]any{"name": "Child", "parentCollection": colRoot, "relations": map[string]any{}}},
		},
		searches: map[string]fakeObj{
			search1: {14, map[string]any{"name": "Unread", "conditions": []any{map[string]any{"condition": "tag", "operator": "is", "value": "unread"}}}},
		},
		items: map[string]fakeObj{
			itBook: {20, map[string]any{
				"itemType": "book", "title": "A Book", "collections": []string{colChild},
				"tags":      []any{map[string]any{"tag": "unread"}, map[string]any{"tag": "auto", "type": 1}},
				"relations": map[string]any{"dc:relation": "http://zotero.org/users/4242/items/" + itNote},
				"dateAdded": "2024-01-01T00:00:00Z", "dateModified": "2024-02-01T00:00:00Z",
			}},
			itPDF: {41, map[string]any{
				"itemType": "attachment", "parentItem": itBook, "linkMode": "imported_file", "title": "paper.pdf",
				"contentType": "application/pdf", "filename": "paper.pdf", "md5": md5hex(pdfBytes), "mtime": 1700000000000,
				"tags": []any{}, "collections": []string{}, "relations": map[string]any{},
				"dateAdded": "2024-01-02T00:00:00Z", "dateModified": "2024-01-02T00:00:00Z",
			}},
			itNote: {22, map[string]any{
				"itemType": "note", "parentItem": itBook, "note": "<p>hi</p>",
				"tags": []any{map[string]any{"tag": "unread"}}, "collections": []string{}, "relations": []any{},
				"dateAdded": "2024-01-03T00:00:00Z", "dateModified": "2024-01-03T00:00:00Z",
			}},
			itSnap: {45, map[string]any{
				"itemType": "attachment", "linkMode": "imported_url", "title": "Snapshot", "url": "https://example.org/",
				"contentType": "text/html", "filename": "index.html", "md5": md5hex(htmlBytes), "mtime": 1700000001000,
				"tags": []any{}, "collections": []string{colRoot}, "relations": map[string]any{},
				"dateAdded": "2024-01-04T00:00:00Z", "dateModified": "2024-01-04T00:00:00Z",
			}},
			itTrashed: {30, map[string]any{
				"itemType": "book", "title": "Gone", "deleted": 1,
				"tags": []any{map[string]any{"tag": "old"}}, "collections": []string{}, "relations": map[string]any{},
				"dateAdded": "2024-01-05T00:00:00Z", "dateModified": "2024-01-05T00:00:00Z",
			}},
			itLinked: {31, map[string]any{
				"itemType": "attachment", "linkMode": "linked_url", "title": "Link", "url": "https://example.org/x",
				"tags": []any{}, "collections": []string{}, "relations": map[string]any{},
				"dateAdded": "2024-01-06T00:00:00Z", "dateModified": "2024-01-06T00:00:00Z",
			}},
			itMissing: {32, map[string]any{
				"itemType": "attachment", "linkMode": "imported_file", "title": "lost.pdf", "contentType": "application/pdf",
				"filename": "lost.pdf", "md5": missingMD5, "mtime": 1700000002000,
				"tags": []any{}, "collections": []string{}, "relations": map[string]any{},
				"dateAdded": "2024-01-07T00:00:00Z", "dateModified": "2024-01-07T00:00:00Z",
			}},
		},
		settings:    map[string]zotero.OrderedMap{},
		setVersions: map[string]int64{"tagColors": 33},
		fulltext: map[string]zotero.FulltextContent{
			itPDF:  {Content: "pdf words", IndexedPages: 3, TotalPages: 3},
			itSnap: {Content: "snapshot words", IndexedChars: 14, TotalChars: 14},
		},
		ftVersions: map[string]int64{itPDF: 41, itSnap: 45},
		files: map[string]fakeFile{
			itPDF:  {body: pdfBytes, md5: md5hex(pdfBytes), mtime: 1700000000000},
			itSnap: {body: zipped, md5: md5hex(htmlBytes), compressed: true, mtime: 1700000001000},
		},
	}
	var tc zotero.OrderedMap
	_ = tc.UnmarshalJSON([]byte(`[{"name":"unread","color":"#ff0000"}]`))
	u.settings["tagColors"] = tc

	g := &fakeLibrary{
		group: true, id: fakeGroupID, version: 90,
		collections: map[string]fakeObj{gCol: {80, map[string]any{"name": "Shared", "parentCollection": false, "relations": map[string]any{}}}},
		searches:    map[string]fakeObj{},
		items: map[string]fakeObj{
			gItBook: {85, map[string]any{
				"itemType": "journalArticle", "title": "Shared paper", "collections": []string{gCol},
				"tags": []any{map[string]any{"tag": "team"}}, "relations": map[string]any{},
				"dateAdded": "2024-03-01T00:00:00Z", "dateModified": "2024-03-01T00:00:00Z",
			}},
			gItNote: {88, map[string]any{
				"itemType": "note", "parentItem": gItBook, "note": "<p>team</p>", "tags": []any{},
				"collections": []string{}, "relations": map[string]any{},
				"dateAdded": "2024-03-02T00:00:00Z", "dateModified": "2024-03-02T00:00:00Z",
			}},
		},
		settings: map[string]zotero.OrderedMap{}, setVersions: map[string]int64{},
		fulltext: map[string]zotero.FulltextContent{}, ftVersions: map[string]int64{},
		files: map[string]fakeFile{},
	}

	f := &fakeZotero{
		t: t,
		libs: map[string]*fakeLibrary{
			fmt.Sprintf("users/%d", fakeUserID):   u,
			fmt.Sprintf("groups/%d", fakeGroupID): g,
		},
		groupMeta: map[int64]zotero.GroupData{
			fakeGroupID: {
				ID: fakeGroupID, Version: 5, Name: "Lab Group", Owner: 999, Type: "Private", Description: "Our lab",
				URL: "https://example.org/lab", LibraryEditing: "admins", LibraryReading: "members", FileEditing: "admins",
				Admins: []int64{fakeUserID}, Members: []int64{1, 2},
			},
		},
		access: zotero.Access{
			User:   zotero.UserAccess{Library: true, Files: true, Notes: true},
			Groups: map[string]zotero.GroupAccess{zotero.AllGroups: {Library: true}},
		},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeZotero) URL() string { return f.srv.URL + "/" }

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeZotero) serve(w http.ResponseWriter, r *http.Request) {
	if f.hook != nil && f.hook(w, r) {
		return
	}
	// Downloads are self-authenticating, like the signed S3 URLs.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/dl/"); ok {
		f.download(w, r, rest)
		return
	}
	if r.Header.Get("Zotero-API-Key") != fakeAPIKey {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "keys" && parts[1] == "current":
		jsonOut(w, 200, zotero.KeyInfo{Key: fakeAPIKey, UserID: fakeUserID, Username: "zoteroer", DisplayName: "Zoteroer", Access: f.access})
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "groups":
		out := map[string]int64{}
		for id, g := range f.groupMeta {
			out[strconv.FormatInt(id, 10)] = g.Version
		}
		jsonOut(w, 200, out)
	case len(parts) == 2 && parts[0] == "groups":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		g, ok := f.groupMeta[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		jsonOut(w, 200, zotero.Group{ID: id, Version: g.Version, Data: g})
	case len(parts) >= 3 && (parts[0] == "users" || parts[0] == "groups"):
		l := f.libs[parts[0]+"/"+parts[1]]
		if l == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Last-Modified-Version", strconv.FormatInt(l.version, 10))
		f.library(w, r, l, parts[2:])
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeZotero) library(w http.ResponseWriter, r *http.Request, l *fakeLibrary, parts []string) {
	q := r.URL.Query()
	if !f.sentBackoff.Swap(true) {
		// Ask the client to pause once; zoteroclient must honor it.
		w.Header().Set("Backoff", "1")
	}
	switch parts[0] {
	case "collections", "searches", "items":
		var set map[string]fakeObj
		typ := zotero.ObjectType(strings.TrimSuffix(parts[0], "s"))
		switch parts[0] {
		case "collections":
			set = l.collections
		case "searches":
			set = l.searches
			typ = zotero.TypeSearch
		default:
			set = l.items
		}
		if len(parts) == 3 && parts[2] == "fulltext" {
			f.itemFulltext(w, r, l, parts[1])
			return
		}
		if len(parts) == 3 && parts[2] == "file" {
			f.fileRedirect(w, r, l, parts[1])
			return
		}
		top := len(parts) == 2 && parts[1] == "top"
		if q.Get("format") == "versions" {
			out := map[string]int64{}
			for k, o := range set {
				if o.data["deleted"] == 1 && q.Get("includeTrashed") != "1" {
					continue
				}
				if top && o.data["parentItem"] != nil {
					continue
				}
				out[k] = o.version
			}
			jsonOut(w, 200, out)
			return
		}
		keys := strings.Split(q.Get(typ.KeyParam()), ",")
		if typ == zotero.TypeItem {
			if f.itemFetches.Add(1) == 1 && !f.sent429.Swap(true) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "slow down", http.StatusTooManyRequests)
				return
			}
		}
		out := []zotero.Object{}
		for _, k := range keys {
			if o, ok := set[k]; ok {
				out = append(out, f.object(l, typ, k, o))
			}
		}
		jsonOut(w, 200, out)
	case "settings":
		out := map[string]map[string]any{}
		for k, v := range l.settings {
			out[k] = map[string]any{"value": v, "version": l.setVersions[k]}
		}
		jsonOut(w, 200, out)
	case "fulltext":
		jsonOut(w, 200, l.ftVersions)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeZotero) object(l *fakeLibrary, typ zotero.ObjectType, key string, o fakeObj) zotero.Object {
	data := map[string]any{"key": key, "version": o.version}
	for k, v := range o.data {
		data[k] = v
	}
	raw, _ := json.Marshal(data)
	kind := "user"
	if l.group {
		kind = "group"
	}
	return zotero.Object{Key: key, Version: o.version, Library: zotero.Library{Type: kind, ID: l.id}, Data: raw}
}

func (f *fakeZotero) itemFulltext(w http.ResponseWriter, r *http.Request, l *fakeLibrary, key string) {
	c, ok := l.fulltext[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	jsonOut(w, 200, c)
}

func (f *fakeZotero) fileRedirect(w http.ResponseWriter, r *http.Request, l *fakeLibrary, key string) {
	f.fileRequests.Add(1)
	file, ok := l.files[key]
	if !ok {
		f.noFile404.Add(1)
		http.NotFound(w, r)
		return
	}
	compressed := "No"
	if file.compressed {
		compressed = "Yes"
	}
	w.Header().Set("Zotero-File-MD5", file.md5)
	w.Header().Set("Zotero-File-Compressed", compressed)
	w.Header().Set("Zotero-File-Modification-Time", strconv.FormatInt(file.mtime, 10))
	w.Header().Set("Location", fmt.Sprintf("/dl/%d/%s", l.id, key))
	w.WriteHeader(http.StatusFound)
}

func (f *fakeZotero) download(w http.ResponseWriter, r *http.Request, rest string) {
	idStr, key, _ := strings.Cut(rest, "/")
	if r.Header.Get("Zotero-API-Key") != "" {
		f.t.Errorf("API key leaked to the download host")
	}
	id, _ := strconv.ParseInt(idStr, 10, 64)
	for _, l := range f.libs {
		if l.id == id {
			if file, ok := l.files[key]; ok {
				w.Header().Set("Content-Length", strconv.Itoa(len(file.body)))
				_, _ = w.Write(file.body)
				return
			}
		}
	}
	http.NotFound(w, r)
}

// versionsOf lists key -> version of a fixture type, trashed included.
func (l *fakeLibrary) versionsOf(set map[string]fakeObj) map[string]int64 {
	out := map[string]int64{}
	for k, o := range set {
		out[k] = o.version
	}
	return out
}
