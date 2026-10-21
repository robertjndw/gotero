package conformance

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var groupsTests = []testCase{
	{"UserGroupsVersions", testGroupVersions},
	{"GroupShape", testGroupShape},
	{"MemberSeesGroupLibrary", testMemberSeesGroup},
	{"NonMemberDenied", testNonMemberDenied},
	{"MembershipChangeBumpsGroupVersion", testMembershipBump},
	{"AdminWritesMemberReads", testGroupWriteRead},
	{"GroupListHasLinkHeader", testGroupListLink},
}

type groupFixture struct {
	owner, admin, member, outsider User
	g                              Group
}

func (e *env) groupFixture(t *testing.T) groupFixture {
	f := groupFixture{owner: e.newUser(t), admin: e.newUser(t), member: e.newUser(t), outsider: e.newUser(t)}
	f.g = e.newGroup(t, f.owner, []User{f.member}, []User{f.admin})
	return f
}

func testGroupVersions(t *testing.T, e *env) {
	f := e.groupFixture(t)
	for _, u := range []User{f.owner, f.admin, f.member} {
		r := e.do(t, "GET", fmt.Sprintf("users/%d/groups?format=versions", u.ID), withKey(u.APIKey))
		mustStatus(t, r, 200)
		o := parseObj(t, r)
		if v := atInt(t, o, strconv.FormatInt(f.g.ID, 10)); v <= 0 {
			t.Errorf("user %d: group version = %d", u.ID, v)
		}
	}
	r := e.do(t, "GET", fmt.Sprintf("users/%d/groups?format=versions", f.outsider.ID), withKey(f.outsider.APIKey))
	mustStatus(t, r, 200)
	if has(parseObj(t, r), strconv.FormatInt(f.g.ID, 10)) {
		t.Error("outsider sees the group")
	}
}

func testGroupShape(t *testing.T, e *env) {
	f := e.groupFixture(t)
	r := e.do(t, "GET", fmt.Sprintf("groups/%d", f.g.ID), withKey(f.member.APIKey))
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if atInt(t, o, "id") != f.g.ID || atInt(t, o, "data", "id") != f.g.ID {
		t.Errorf("id mismatch in %v", o)
	}
	if atInt(t, o, "version") <= 0 || atInt(t, o, "data", "version") <= 0 {
		t.Error("version must be positive")
	}
	d := at[obj](t, o, "data")
	at[string](t, d, "name")
	at[string](t, d, "type")
	if atInt(t, d, "owner") != f.owner.ID {
		t.Errorf("owner = %v, want %d", d["owner"], f.owner.ID)
	}
	for _, k := range []string{"libraryEditing", "libraryReading", "fileEditing"} {
		at[string](t, d, k)
	}
	// Both must be arrays even when empty, or members sync read-only.
	admins := at[[]any](t, d, "admins")
	members := at[[]any](t, d, "members")
	if !slices.Contains(admins, any(float64(f.admin.ID))) {
		t.Errorf("admins %v lack %d", admins, f.admin.ID)
	}
	if !slices.Contains(members, any(float64(f.member.ID))) {
		t.Errorf("members %v lack %d", members, f.member.ID)
	}
}

func testMemberSeesGroup(t *testing.T, e *env) {
	f := e.groupFixture(t)
	r := e.groupLib(f.g, f.member).get(t, "items")
	mustStatus(t, r, 200)
	versionHeader(t, r)
}

func testNonMemberDenied(t *testing.T, e *env) {
	f := e.groupFixture(t)
	r := e.groupLib(f.g, f.outsider).get(t, "items")
	mustStatusAny(t, r, 403, 404)
	r = e.do(t, "GET", fmt.Sprintf("groups/%d", f.g.ID), withKey(f.outsider.APIKey))
	mustStatusAny(t, r, 403, 404)
}

func testMembershipBump(t *testing.T, e *env) {
	adder, ok := e.prov.(MemberAdder)
	if !ok {
		t.Skip("provisioner has no MemberAdder")
	}
	f := e.groupFixture(t)
	get := func() int64 {
		r := e.do(t, "GET", fmt.Sprintf("groups/%d", f.g.ID), withKey(f.owner.APIKey))
		mustStatus(t, r, 200)
		return atInt(t, parseObj(t, r), "version")
	}
	before := get()
	late := e.newUser(t)
	if err := adder.AddMember(ctxOf(t), f.g, late, false); err != nil {
		e.provisionErr(t, "add member", err)
	}
	if after := get(); after <= before {
		t.Errorf("group version %d -> %d, want bump", before, after)
	}
	mustStatus(t, e.groupLib(f.g, late).get(t, "items"), 200)
}

func testGroupWriteRead(t *testing.T, e *env) {
	f := e.groupFixture(t)
	key, _ := e.groupLib(f.g, f.admin).create(t, "items", bookItem("group book"))
	r := e.groupLib(f.g, f.member).get(t, "items/"+key)
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if at[string](t, o, "library", "type") != "group" || atInt(t, o, "library", "id") != f.g.ID {
		t.Errorf("library = %v", o["library"])
	}
}

// Zotero's getGroups parses the Link header unconditionally, so a missing
// header breaks group sync, even when the user has no groups.
func testGroupListLink(t *testing.T, e *env) {
	f := e.groupFixture(t)
	for _, u := range []User{f.member, f.outsider} {
		r := e.do(t, "GET", fmt.Sprintf("users/%d/groups", u.ID), withKey(u.APIKey))
		mustStatus(t, r, 200)
		if !strings.Contains(r.Header.Get("Link"), `rel="last"`) {
			t.Errorf("user %d: Link header %q lacks rel=\"last\"", u.ID, r.Header.Get("Link"))
		}
	}
}
