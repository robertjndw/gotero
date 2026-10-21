package zotero

import (
	"encoding/json"
	"testing"
)

func marshalString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteResponseAlwaysHasAllMembers(t *testing.T) {
	want := `{"successful":{},"success":{},"unchanged":{},"failed":{}}`
	if got := marshalString(t, WriteResponse{}); got != want {
		t.Errorf("zero value: %s", got)
	}
	if got := marshalString(t, NewWriteResponse()); got != want {
		t.Errorf("new: %s", got)
	}
	if got := marshalString(t, &WriteResponse{}); got != want {
		t.Errorf("pointer: %s", got)
	}
}

func TestWriteResponseFull(t *testing.T) {
	r := NewWriteResponse()
	r.Successful["0"] = json.RawMessage(`{"key":"ABCD2345","version":12}`)
	r.Success["0"] = "ABCD2345"
	r.Unchanged["1"] = "EFGH6789"
	r.Failed["2"] = WriteFailure{Key: "JKLM2345", Code: 412, Message: "Item has been modified since specified version"}
	r.Failed["3"] = WriteFailure{Code: 400, Message: "bad", Data: json.RawMessage(`{"field":"x"}`)}
	want := `{"successful":{"0":{"key":"ABCD2345","version":12}},"success":{"0":"ABCD2345"},"unchanged":{"1":"EFGH6789"},` +
		`"failed":{"2":{"key":"JKLM2345","code":412,"message":"Item has been modified since specified version"},"3":{"code":400,"message":"bad","data":{"field":"x"}}}}`
	if got := marshalString(t, r); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	var back WriteResponse
	if err := json.Unmarshal([]byte(want), &back); err != nil || back.Failed["2"].Code != 412 {
		t.Errorf("unmarshal: %v %+v", err, back)
	}
}

func TestDeletedResponse(t *testing.T) {
	want := `{"collections":[],"searches":[],"items":[],"tags":[],"settings":[]}`
	if got := marshalString(t, DeletedResponse{}); got != want {
		t.Errorf("got %s", got)
	}
	got := marshalString(t, DeletedResponse{Items: []string{"ABCD2345"}, Tags: []string{"a"}})
	want = `{"collections":[],"searches":[],"items":["ABCD2345"],"tags":["a"],"settings":[]}`
	if got != want {
		t.Errorf("got %s", got)
	}
}

func TestKeyInfoJSON(t *testing.T) {
	k := KeyInfo{
		Key: "K", UserID: 1, Username: "u", DisplayName: "U",
		Access: Access{
			User:   UserAccess{Library: true, Files: true, Notes: true, Write: true},
			Groups: map[string]GroupAccess{AllGroups: {Library: true, Write: true}},
		},
	}
	want := `{"key":"K","userID":1,"username":"u","displayName":"U","access":{"user":{"library":true,"files":true,"notes":true,"write":true},"groups":{"all":{"library":true,"write":true}}}}`
	if got := marshalString(t, k); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Read-only key with no group access still has "groups".
	k = KeyInfo{Key: "K", Access: Access{User: UserAccess{Library: true}}}
	want = `{"key":"K","userID":0,"username":"","displayName":"","access":{"user":{"library":true},"groups":{}}}`
	if got := marshalString(t, k); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGroupJSON(t *testing.T) {
	g := Group{ID: 2, Version: 5, Data: GroupData{ID: 2, Version: 5, Name: "G", Owner: 1, Type: "Private", LibraryEditing: "members", LibraryReading: "all", FileEditing: "admins"}}
	b := marshalString(t, g)
	var back struct {
		Links map[string]any `json:"links"`
		Data  struct {
			Admins  []int64 `json:"admins"`
			Members []int64 `json:"members"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(b), &back); err != nil {
		t.Fatal(err)
	}
	if back.Data.Admins == nil || back.Data.Members == nil || back.Links == nil {
		t.Errorf("null arrays or links in %s", b)
	}
}

func TestObjectEnvelope(t *testing.T) {
	zero := 0
	o := Object{
		Key: "ABCD2345", Version: 3,
		Library: Library{Type: "user", ID: 1, Name: "me"},
		Meta:    Meta{CreatorSummary: "Doe", NumChildren: &zero},
		Data:    json.RawMessage(`{"key":"ABCD2345"}`),
	}
	want := `{"key":"ABCD2345","version":3,"library":{"type":"user","id":1,"name":"me","links":{}},"links":{},"meta":{"creatorSummary":"Doe","numChildren":0},"data":{"key":"ABCD2345"}}`
	if got := marshalString(t, o); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestFulltextContent(t *testing.T) {
	tests := []struct {
		in   FulltextContent
		want string
	}{
		{FulltextContent{Content: "x", IndexedChars: 1, TotalChars: 5}, `{"content":"x","indexedChars":1,"totalChars":5}`},
		{FulltextContent{Content: "", IndexedChars: 0, TotalChars: 0}, `{"content":"","indexedChars":0,"totalChars":0}`},
		{FulltextContent{Content: "p", IndexedPages: 2, TotalPages: 9}, `{"content":"p","indexedPages":2,"totalPages":9}`},
		{FulltextContent{Key: "ABCD2345", Content: "p", IndexedPages: 0, TotalPages: 3}, `{"key":"ABCD2345","content":"p","indexedPages":0,"totalPages":3}`},
	}
	for _, tc := range tests {
		if got := marshalString(t, tc.in); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
	var f FulltextContent
	if err := json.Unmarshal([]byte(`{"key":"K","content":"c","indexedChars":1,"totalChars":2,"indexedPages":0,"totalPages":0}`), &f); err != nil {
		t.Fatal(err)
	}
	if f.UsesPages() || f.IndexedChars != 1 || f.TotalChars != 2 {
		t.Errorf("%+v", f)
	}
}
