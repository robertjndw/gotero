package zoteroclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

func keysN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("K%07d", i)
	}
	return out
}

func TestObjectsChunksAt100(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/1/items" {
			t.Errorf("path %q", r.URL.Path)
		}
		keys := strings.Split(r.URL.Query().Get("itemKey"), ",")
		mu.Lock()
		sizes = append(sizes, len(keys))
		mu.Unlock()
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf(`{"key":%q,"version":3,"data":{"key":%q}}`, k, k))
		}
		w.Header().Set("Last-Modified-Version", "9")
		writeJSON(w, 200, "["+strings.Join(parts, ",")+"]")
	})
	objs, ver, err := c.User(1).Objects(context.Background(), zotero.TypeItem, keysN(250))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 250 || ver != 9 || objs[249].Key != "K0000249" {
		t.Errorf("len=%d ver=%d", len(objs), ver)
	}
	if fmt.Sprint(sizes) != "[100 100 50]" {
		t.Errorf("chunk sizes = %v", sizes)
	}
}

func TestObjectsDetectsLibraryChange(t *testing.T) {
	var n int
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Last-Modified-Version", strconv.Itoa(10+n))
		writeJSON(w, 200, `[]`)
	})
	_, _, err := c.User(1).Objects(context.Background(), zotero.TypeCollection, keysN(150))
	if !errors.Is(err, zoteroclient.ErrLibraryChanged) {
		t.Errorf("err = %v", err)
	}
}

func okWrite(n int, offsetBase int) string {
	succ := map[string]string{}
	full := map[string]json.RawMessage{}
	for i := range n {
		succ[strconv.Itoa(i)] = fmt.Sprintf("OBJ%05d", offsetBase+i)
		full[strconv.Itoa(i)] = json.RawMessage(`{"key":"x"}`)
	}
	b, _ := json.Marshal(map[string]any{"successful": full, "success": succ, "unchanged": []any{}, "failed": map[string]any{
		"0": map[string]any{"code": 400, "message": "bad"},
	}})
	return string(b)
}

func mkObjs(n int) []zotero.OrderedMap {
	out := make([]zotero.OrderedMap, n)
	for i := range out {
		_ = out[i].SetValue("title", strconv.Itoa(i))
	}
	return out
}

func TestWriteChunksAt50AndRemapsIndexes(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	var conds []string
	version := 10
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(readBody(t, r)), &arr); err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		sizes = append(sizes, len(arr))
		conds = append(conds, r.Header.Get("If-Unmodified-Since-Version"))
		version++
		w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
		writeJSON(w, 200, okWrite(len(arr), 0))
	})
	cond := int64(10)
	resp, ver, err := c.User(1).Write(context.Background(), zotero.TypeItem, mkObjs(120), zoteroclient.WriteOptions{IfUnmodifiedSinceVersion: &cond})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "[50 50 20]" {
		t.Errorf("sizes = %v", sizes)
	}
	if fmt.Sprint(conds) != "[10 11 12]" {
		t.Errorf("conditions = %v", conds)
	}
	if ver != 13 || len(resp.Success) != 120 || len(resp.Successful) != 120 {
		t.Errorf("ver=%d success=%d", ver, len(resp.Success))
	}
	for _, idx := range []string{"0", "50", "100", "119"} {
		if _, ok := resp.Success[idx]; !ok {
			t.Errorf("missing index %s", idx)
		}
	}
	// Each chunk reported a failure at its local index 0.
	for _, idx := range []string{"0", "50", "100"} {
		if f, ok := resp.Failed[idx]; !ok || f.Code != 400 {
			t.Errorf("failed[%s] = %+v", idx, f)
		}
	}
	if len(resp.Unchanged) != 0 {
		t.Errorf("unchanged = %v", resp.Unchanged)
	}
}

func TestWriteStopsOnPreconditionFailed(t *testing.T) {
	var n int
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Last-Modified-Version", "20")
		if n == 2 {
			writeJSON(w, 412, "changed")
			return
		}
		writeJSON(w, 200, okWrite(50, 0))
	})
	resp, ver, err := c.User(1).Write(context.Background(), zotero.TypeItem, mkObjs(150), zoteroclient.WriteOptions{})
	if !zoteroclient.IsPreconditionFailed(err) {
		t.Fatalf("err = %v", err)
	}
	if n != 2 || len(resp.Success) != 50 || ver != 20 {
		t.Errorf("n=%d success=%d ver=%d", n, len(resp.Success), ver)
	}
}

func TestWriteToken(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-Write-Token") != "tok" {
			t.Error("token missing")
		}
		if r.Header.Get("If-Unmodified-Since-Version") != "" {
			t.Error("unexpected precondition")
		}
		writeJSON(w, 200, okWrite(1, 0))
	})
	if _, _, err := c.User(1).Write(context.Background(), zotero.TypeItem, mkObjs(1), zoteroclient.WriteOptions{WriteToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.User(1).Write(context.Background(), zotero.TypeItem, mkObjs(51), zoteroclient.WriteOptions{WriteToken: "tok"}); err == nil {
		t.Error("token with 51 objects should be rejected")
	}
}

func TestDeleteChunksAndTags(t *testing.T) {
	var mu sync.Mutex
	var got []string
	ver := 4
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		switch {
		case q.Has("itemKey"):
			got = append(got, fmt.Sprintf("items:%d:%s", len(strings.Split(q.Get("itemKey"), ",")), r.Header.Get("If-Unmodified-Since-Version")))
		case q.Has("tag"):
			got = append(got, "tag:"+q.Get("tag"))
		case q.Has("settingKey"):
			got = append(got, "setting:"+q.Get("settingKey"))
		}
		ver++
		w.Header().Set("Last-Modified-Version", strconv.Itoa(ver))
		w.WriteHeader(http.StatusNoContent)
	})
	lib := c.User(1)
	v, err := lib.Delete(context.Background(), zotero.TypeItem, keysN(120), 4)
	if err != nil || v != 7 {
		t.Fatalf("v=%d err=%v", v, err)
	}
	if _, err := lib.DeleteTags(context.Background(), []string{"a", "b c"}, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.DeleteSettings(context.Background(), []string{"x", "y"}, 8); err != nil {
		t.Fatal(err)
	}
	want := "[items:50:4 items:50:5 items:20:6 tag:a || b c setting:x,y]"
	if fmt.Sprint(got) != want {
		t.Errorf("got %v\nwant %s", got, want)
	}
}

func TestSettingsDeletedFulltext(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "8")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/groups/2/settings":
			writeJSON(w, 200, `{"tagColors":{"value":[{"name":"a","color":"#fff"}],"version":3}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/groups/2/settings":
			if body := readBody(t, r); body != `{"lastPageIndex":{"value":4}}` {
				t.Errorf("settings body %s", body)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/groups/2/deleted":
			if r.URL.Query().Get("since") != "3" {
				t.Error("since")
			}
			writeJSON(w, 200, `{"collections":[],"searches":[],"items":["AAAA2222"],"tags":[],"settings":[]}`)
		case r.URL.Path == "/groups/2/fulltext" && r.Method == http.MethodGet:
			writeJSON(w, 200, `{"AAAA2222":5}`)
		case r.URL.Path == "/groups/2/fulltext":
			var arr []map[string]any
			_ = json.Unmarshal([]byte(readBody(t, r)), &arr)
			if r.Header.Get("If-Unmodified-Since-Version") != "8" || arr[0]["key"] != "AAAA2222" {
				t.Errorf("fulltext write: %v", arr)
			}
			writeJSON(w, 200, `{"successful":{},"success":{"0":"AAAA2222"},"unchanged":[],"failed":[]}`)
		case r.URL.Path == "/groups/2/items/AAAA2222/fulltext" && r.Method == http.MethodGet:
			writeJSON(w, 200, `{"content":"hello","indexedChars":5,"totalChars":5}`)
		case r.URL.Path == "/groups/2/items/AAAA2222/fulltext" && r.Method == http.MethodPut:
			if strings.Contains(readBody(t, r), `"key"`) {
				t.Error("key should not be in PUT body")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ctx := context.Background()
	lib := c.Group(2)

	s, ver, err := lib.Settings(ctx, 0)
	if err != nil || ver != 8 || s["tagColors"].Version != 3 {
		t.Fatalf("settings: %v %d %v", s, ver, err)
	}
	if _, err := lib.WriteSettings(ctx, map[string]zoteroclient.Setting{"lastPageIndex": {Value: json.RawMessage("4")}}, nil); err != nil {
		t.Fatal(err)
	}
	d, _, err := lib.Deleted(ctx, 3)
	if err != nil || len(d.Items) != 1 {
		t.Fatalf("deleted: %+v %v", d, err)
	}
	fv, _, err := lib.FulltextVersions(ctx, 0)
	if err != nil || fv["AAAA2222"] != 5 {
		t.Fatalf("fulltext versions %v %v", fv, err)
	}
	wr, _, err := lib.WriteFulltext(ctx, []zotero.FulltextContent{{Key: "AAAA2222", Content: "hi", IndexedChars: 2, TotalChars: 2}}, 8)
	if err != nil || wr.Success["0"] != "AAAA2222" {
		t.Fatalf("write fulltext %+v %v", wr, err)
	}
	ft, _, err := lib.Fulltext(ctx, "AAAA2222")
	if err != nil || ft.Content != "hello" {
		t.Fatalf("fulltext %+v %v", ft, err)
	}
	if _, err := lib.SetFulltext(ctx, "AAAA2222", zotero.FulltextContent{Key: "zzz", Content: "hello", IndexedChars: 5, TotalChars: 5}); err != nil {
		t.Fatal(err)
	}
}

func TestListObjectsFollowsLinkHeader(t *testing.T) {
	var base string
	var versions []int64
	c, _, srv := setup(t, func(w http.ResponseWriter, r *http.Request) {
		start := r.URL.Query().Get("start")
		w.Header().Set("Last-Modified-Version", "5")
		switch start {
		case "":
			w.Header().Add("Link", fmt.Sprintf(`<%s/users/1/items?limit=2&start=2>; rel="next", <%s/users/1/items?limit=2&start=0>; rel="first"`, base, base))
			writeJSON(w, 200, `[{"key":"A"},{"key":"B"}]`)
		case "2":
			w.Header().Add("Link", `</users/1/items?limit=2&start=4>; rel="next"`)
			writeJSON(w, 200, `[{"key":"C"},{"key":"D"}]`)
		default:
			w.Header().Add("Link", `</users/1/items?limit=2&start=0>; rel="first"`)
			writeJSON(w, 200, `[{"key":"E"}]`)
		}
	})
	base = srv.URL
	var keys []string
	for o, err := range c.User(1).ListObjects(context.Background(), zotero.TypeItem, zoteroclient.ListQuery{
		Limit: 2, OnVersion: func(v int64) { versions = append(versions, v) },
	}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, "") != "ABCDE" || len(versions) != 3 {
		t.Errorf("keys=%v versions=%v", keys, versions)
	}
}

func TestListObjectsStartLimitFallbackAndEarlyStop(t *testing.T) {
	var reqs int
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		reqs++
		if r.URL.Query().Get("start") == "2" {
			writeJSON(w, 200, `[{"key":"C"}]`)
			return
		}
		writeJSON(w, 200, `[{"key":"A"},{"key":"B"}]`)
	})
	var keys []string
	for o, err := range c.User(1).ListObjects(context.Background(), zotero.TypeCollection, zoteroclient.ListQuery{Limit: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, "") != "ABC" || reqs != 2 {
		t.Errorf("keys=%v reqs=%d", keys, reqs)
	}
	reqs = 0
	for range c.User(1).ListObjects(context.Background(), zotero.TypeCollection, zoteroclient.ListQuery{Limit: 2}) {
		break
	}
	if reqs != 1 {
		t.Errorf("early stop fetched %d pages", reqs)
	}
}

func TestListObjectsRejectsForeignNextLink(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example/x>; rel="next"`)
		writeJSON(w, 200, `[{"key":"A"}]`)
	})
	var last error
	for _, err := range c.User(1).ListObjects(context.Background(), zotero.TypeItem, zoteroclient.ListQuery{}) {
		last = err
	}
	if last == nil {
		t.Error("expected an error for a next link on another host")
	}
}

func TestKeysAndGroups(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /keys":
			var b map[string]any
			_ = json.Unmarshal([]byte(readBody(t, r)), &b)
			if b["username"] != "u" || b["password"] != "p" {
				t.Errorf("body %v", b)
			}
			writeJSON(w, 201, `{"key":"NEWKEY","userID":1,"username":"u","displayName":"U","access":{"user":{"library":true},"groups":{}}}`)
		case "POST /keys/sessions":
			if body := readBody(t, r); body != `{"userID":9}` {
				t.Errorf("session body %s", body)
			}
			writeJSON(w, 201, `{"sessionToken":"T0K","loginURL":"https://x/login?session=T0K"}`)
		case "GET /keys/sessions/T0K":
			writeJSON(w, 200, `{"status":"completed","apiKey":"K","userID":9}`)
		case "DELETE /keys/sessions/T0K", "DELETE /keys/current":
			w.WriteHeader(http.StatusNoContent)
		case "GET /users/9/groups":
			if r.URL.Query().Get("format") != "versions" {
				t.Error("format")
			}
			writeJSON(w, 200, `{"11":3,"12":4}`)
		case "GET /groups/11":
			writeJSON(w, 200, `{"id":11,"version":3,"data":{"id":11,"name":"G","admins":[],"members":[]}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ctx := context.Background()
	ki, err := c.CreateKeyWithPassword(ctx, "u", "p", "n", zotero.Access{User: zotero.UserAccess{Library: true}})
	if err != nil || ki.Key != "NEWKEY" {
		t.Fatalf("%+v %v", ki, err)
	}
	uid := int64(9)
	s, err := c.CreateLoginSession(ctx, &uid)
	if err != nil || s.Token != "T0K" || !strings.Contains(s.LoginURL, "session=T0K") {
		t.Fatalf("%+v %v", s, err)
	}
	st, err := c.GetLoginSession(ctx, s.Token)
	if err != nil || st.Status != "completed" || st.APIKey != "K" {
		t.Fatalf("%+v %v", st, err)
	}
	if err := c.CancelLoginSession(ctx, s.Token); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCurrentKey(ctx); err != nil {
		t.Fatal(err)
	}
	gv, err := c.UserGroupVersions(ctx, 9)
	if err != nil || gv[11] != 3 || gv[12] != 4 {
		t.Fatalf("%v %v", gv, err)
	}
	g, err := c.GetGroup(ctx, 11)
	if err != nil || g.Data.Name != "G" {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestObjectAndRemoveStorage(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "2")
		if r.Method == http.MethodPost {
			if r.URL.Path != "/users/1/removestoragefiles" {
				t.Errorf("path %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, 200, `{"key":"ABCD2345","version":2,"data":{"key":"ABCD2345"}}`)
	})
	o, v, err := c.User(1).Object(context.Background(), zotero.TypeItem, "ABCD2345")
	if err != nil || o.Key != "ABCD2345" || v != 2 {
		t.Fatalf("%+v %d %v", o, v, err)
	}
	if err := c.User(1).RemoveStorageFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Group(1).RemoveStorageFiles(context.Background()); err == nil {
		t.Error("groups should be rejected")
	}
}
