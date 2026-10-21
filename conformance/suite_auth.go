package conformance

import (
	"fmt"
	"testing"
)

var authTests = []testCase{
	{"KeysCurrentShape", testKeysCurrentShape},
	{"InvalidKeyRejected", testInvalidKey},
	{"MissingKeyOnLibraryEndpoint", testMissingKey},
	{"BearerAndQueryKeyAccepted", testAltKeyTransports},
	{"APIVersion2Rejected", testAPIVersion2},
	{"DeleteCurrentKeyRevokes", testDeleteCurrentKey},
	{"ProvisionerRevokedKeyRejected", testProvisionerRevoke},
	{"LegacyLogin", testLegacyLogin},
	{"LegacyLoginBadPassword", testLegacyLoginBad},
	{"OtherUsersLibraryForbidden", testCrossUserForbidden},
}

func testKeysCurrentShape(t *testing.T, e *env) {
	u := e.newUser(t)
	r := e.do(t, "GET", "keys/current", withKey(u.APIKey))
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if got := atInt(t, o, "userID"); got != u.ID {
		t.Errorf("userID = %d, want %d", got, u.ID)
	}
	if got := at[string](t, o, "username"); got != u.Username && u.Username != "" {
		t.Errorf("username = %q, want %q", got, u.Username)
	}
	if got := at[string](t, o, "key"); got != u.APIKey {
		t.Errorf("key = %q, want the key used", got)
	}
	for _, k := range []string{"library", "files", "notes", "write"} {
		at[bool](t, o, "access", "user", k)
	}
	// Missing groups.all makes the client treat member groups as unreadable.
	at[obj](t, o, "access", "groups", "all")
	at[bool](t, o, "access", "groups", "all", "library")
	at[bool](t, o, "access", "groups", "all", "write")
}

func testInvalidKey(t *testing.T, e *env) {
	r := e.do(t, "GET", "keys/current", withKey("ABCDEFGHIJKLMNOPQRSTUVWX"))
	mustStatus(t, r, 403)
}

func testMissingKey(t *testing.T, e *env) {
	u := e.newUser(t)
	r := e.do(t, "GET", fmt.Sprintf("users/%d/items", u.ID))
	mustStatus(t, r, 403)
}

func testAltKeyTransports(t *testing.T, e *env) {
	u := e.newUser(t)
	r := e.do(t, "GET", "keys/current", withHeader("Authorization", "Bearer "+u.APIKey))
	mustStatus(t, r, 200)
	r = e.do(t, "GET", "keys/current?key="+u.APIKey)
	mustStatus(t, r, 200)
	r = e.do(t, "GET", fmt.Sprintf("users/%d/items?key=%s", u.ID, u.APIKey))
	mustStatus(t, r, 200)
	versionHeader(t, r)
}

// Only API version 3 is served (protocol-notes, response rules). Servers that
// tolerate other versions can list this test in Config.Skip.
func testAPIVersion2(t *testing.T, e *env) {
	u := e.newUser(t)
	r := e.do(t, "GET", "keys/current", withKey(u.APIKey), withHeader("Zotero-API-Version", "2"))
	mustStatus(t, r, 400)
}

func testDeleteCurrentKey(t *testing.T, e *env) {
	u := e.newUser(t)
	r := e.do(t, "DELETE", "keys/current", withKey(u.APIKey))
	mustStatus(t, r, 204)
	r = e.do(t, "GET", "keys/current", withKey(u.APIKey))
	mustStatus(t, r, 403)
}

func testProvisionerRevoke(t *testing.T, e *env) {
	rev, ok := e.prov.(KeyRevoker)
	if !ok {
		t.Skip("provisioner has no KeyRevoker")
	}
	u := e.newUser(t)
	if err := rev.RevokeKey(ctxOf(t), u); err != nil {
		e.provisionErr(t, "revoke key", err)
	}
	r := e.do(t, "GET", "keys/current", withKey(u.APIKey))
	mustStatus(t, r, 403)
}

func loginBody(u User, password string) obj {
	return obj{
		"username": u.Username, "password": password, "name": "conformance",
		"access": obj{"user": obj{"library": true, "files": true, "notes": true, "write": true}},
	}
}

func testLegacyLogin(t *testing.T, e *env) {
	u := e.newUser(t)
	if u.Username == "" || u.Password == "" {
		t.Skip("user has no credentials")
	}
	r := e.do(t, "POST", "keys", withJSON(loginBody(u, u.Password)))
	mustStatus(t, r, 201)
	o := parseObj(t, r)
	key := at[string](t, o, "key")
	if got := atInt(t, o, "userID"); got != u.ID {
		t.Errorf("userID = %d, want %d", got, u.ID)
	}
	at[string](t, o, "username")
	at[obj](t, o, "access")
	mustStatus(t, e.do(t, "GET", "keys/current", withKey(key)), 200)
}

func testLegacyLoginBad(t *testing.T, e *env) {
	u := e.newUser(t)
	if u.Username == "" || u.Password == "" {
		t.Skip("user has no credentials")
	}
	r := e.do(t, "POST", "keys", withJSON(loginBody(u, u.Password+"-wrong")))
	mustStatus(t, r, 403)
}

func testCrossUserForbidden(t *testing.T, e *env) {
	a, b := e.newUser(t), e.newUser(t)
	r := e.do(t, "GET", fmt.Sprintf("users/%d/items", a.ID), withKey(b.APIKey))
	mustStatus(t, r, 403)
	r = e.do(t, "POST", fmt.Sprintf("users/%d/items", a.ID), withKey(b.APIKey), withJSON([]obj{bookItem("x")}))
	mustStatus(t, r, 403)
}
