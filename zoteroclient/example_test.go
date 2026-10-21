package zoteroclient_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

func fakeServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "42")
		switch {
		case r.URL.Path == "/users/1/items" && r.URL.Query().Get("format") == "versions":
			writeJSON(w, 200, `{"AAAA2222":40,"BBBB3333":42}`)
		case r.URL.Path == "/users/1/items":
			writeJSON(w, 200, `[{"key":"AAAA2222","version":40,"data":{"key":"AAAA2222","itemType":"book"}},{"key":"BBBB3333","version":42,"data":{"key":"BBBB3333","itemType":"note"}}]`)
		case r.URL.Path == "/users/1/deleted":
			writeJSON(w, 200, `{"collections":[],"searches":[],"items":["CCCC4444"],"tags":[],"settings":[]}`)
		case r.URL.Path == "/users/1/items/AAAA2222/file" && r.Method == http.MethodGet:
			w.Header().Set("Location", "http://"+r.Host+"/blob")
			w.Header().Set("Zotero-File-MD5", "0123456789abcdef0123456789abcdef")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/blob":
			_, _ = io.WriteString(w, "file bytes")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func Example() {
	srv := fakeServer()
	defer srv.Close()

	ctx := context.Background()
	client := zoteroclient.New("API-KEY", zoteroclient.WithBaseURL(srv.URL))
	lib := client.User(1)

	// Pull sync: versions, then objects, then deletions.
	versions, ver, err := lib.Versions(ctx, zotero.TypeItem, zoteroclient.VersionsQuery{IncludeTrashed: true})
	if err != nil {
		panic(err)
	}
	keys := make([]string, 0, len(versions))
	for k := range versions {
		keys = append(keys, k)
	}
	objs, _, err := lib.Objects(ctx, zotero.TypeItem, keys)
	if err != nil {
		panic(err)
	}
	deleted, _, err := lib.Deleted(ctx, 0)
	if err != nil {
		panic(err)
	}
	fmt.Println(ver, len(objs), deleted.Items)
	// Output: 42 2 [CCCC4444]
}

func ExampleLibrary_DownloadFile() {
	srv := fakeServer()
	defer srv.Close()

	lib := zoteroclient.New("API-KEY", zoteroclient.WithBaseURL(srv.URL)).User(1)
	d, err := lib.DownloadFile(context.Background(), "AAAA2222")
	if err != nil {
		panic(err)
	}
	defer func() { _ = d.Body.Close() }()
	b, _ := io.ReadAll(d.Body)
	fmt.Println(d.MD5, string(b))
	// Output: 0123456789abcdef0123456789abcdef file bytes
}

func ExampleLibrary_UploadFile() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"exists":1}`)
	}))
	defer srv.Close()

	lib := zoteroclient.New("API-KEY", zoteroclient.WithBaseURL(srv.URL)).User(1)
	content := "hello"
	res, err := lib.UploadFile(context.Background(), "AAAA2222", zoteroclient.FileInfo{
		MD5:      "5d41402abc4b2a76b9719d911017c592",
		Filename: "hello.txt",
		Size:     int64(len(content)),
		MtimeMS:  1700000000000,
		Body:     strings.NewReader(content),
	}, "") // "" means the attachment has no file yet
	fmt.Println(res.Exists, err)
	// Output: true <nil>
}

func ExampleIsPreconditionFailed() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "50")
		writeJSON(w, 412, "Library has been modified")
	}))
	defer srv.Close()

	lib := zoteroclient.New("API-KEY", zoteroclient.WithBaseURL(srv.URL)).User(1)
	since := int64(10)
	_, ver, err := lib.Write(context.Background(), zotero.TypeItem,
		[]zotero.OrderedMap{{}}, zoteroclient.WriteOptions{IfUnmodifiedSinceVersion: &since})
	if zoteroclient.IsPreconditionFailed(err) {
		fmt.Println("someone else wrote first, library is at", ver)
	}
	// Output: someone else wrote first, library is at 50
}

func ExampleLibrary_ListObjects() {
	srv := fakeServer()
	defer srv.Close()

	lib := zoteroclient.New("API-KEY", zoteroclient.WithBaseURL(srv.URL)).User(1)
	for obj, err := range lib.ListObjects(context.Background(), zotero.TypeItem, zoteroclient.ListQuery{}) {
		if err != nil {
			panic(err)
		}
		fmt.Println(obj.Key)
	}
	// Output:
	// AAAA2222
	// BBBB3333
}
