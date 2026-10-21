package conformance

import (
	"strings"
	"testing"
	"time"
)

var sessionsTests = []testCase{
	{"CreateSession", testSessionCreate},
	{"PendingStatus", testSessionPending},
	{"ApprovedSessionCompletes", testSessionApprove},
	{"MismatchedUserCannotApprove", testSessionMismatch},
	{"DeleteCancels", testSessionCancel},
	{"UnknownSession404", testSessionUnknown},
}

func (e *env) newSession(t *testing.T, body obj) (token string) {
	r := e.do(t, "POST", "keys/sessions", withJSON(body))
	mustStatus(t, r, 201)
	o := parseObj(t, r)
	token = at[string](t, o, "sessionToken")
	// Android appends "&app=1", so the URL must already have a query.
	if u := at[string](t, o, "loginURL"); !strings.Contains(u, "?") || !strings.HasPrefix(u, "http") {
		t.Errorf("loginURL = %q, want absolute URL containing '?'", u)
	}
	return token
}

func testSessionCreate(t *testing.T, e *env) {
	if e.newSession(t, obj{}) == "" {
		t.Error("empty token")
	}
}

func testSessionPending(t *testing.T, e *env) {
	tok := e.newSession(t, obj{})
	r := e.do(t, "GET", "keys/sessions/"+tok)
	mustStatus(t, r, 200)
	if s := at[string](t, parseObj(t, r), "status"); s != "pending" {
		t.Errorf("status = %q, want pending", s)
	}
}

func needApprover(t *testing.T, e *env) SessionApprover {
	a, ok := e.prov.(SessionApprover)
	if !ok {
		t.Skip("provisioner has no SessionApprover")
	}
	return a
}

func testSessionApprove(t *testing.T, e *env) {
	a := needApprover(t, e)
	u := e.newUser(t)
	tok := e.newSession(t, obj{})
	if err := a.ApproveSession(ctxOf(t), u, tok); err != nil {
		e.provisionErr(t, "approve session", err)
	}
	r := e.do(t, "GET", "keys/sessions/"+tok)
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if s := at[string](t, o, "status"); s != "completed" {
		t.Fatalf("status = %q, want completed", s)
	}
	if atInt(t, o, "userID") != u.ID {
		t.Errorf("userID = %v, want %d", o["userID"], u.ID)
	}
	at[string](t, o, "username")
	key := at[string](t, o, "apiKey")
	r = e.do(t, "GET", "keys/current", withKey(key))
	mustStatus(t, r, 200)
	if atInt(t, parseObj(t, r), "userID") != u.ID {
		t.Error("issued key belongs to another user")
	}
}

func testSessionMismatch(t *testing.T, e *env) {
	a := needApprover(t, e)
	owner, other := e.newUser(t), e.newUser(t)
	tok := e.newSession(t, obj{"userID": owner.ID})
	// The approval is expected to fail; only the resulting state matters.
	_ = a.ApproveSession(ctxOf(t), other, tok)
	r := e.do(t, "GET", "keys/sessions/"+tok)
	mustStatus(t, r, 200)
	if s := at[string](t, parseObj(t, r), "status"); s == "completed" {
		t.Error("session completed for a different user than the bound userID")
	}
}

func testSessionCancel(t *testing.T, e *env) {
	tok := e.newSession(t, obj{})
	r := e.do(t, "DELETE", "keys/sessions/"+tok)
	mustStatusAny(t, r, 200, 204)
	deadline := time.Now().Add(3 * time.Second)
	for {
		r = e.do(t, "GET", "keys/sessions/"+tok)
		mustStatus(t, r, 200)
		if s := at[string](t, parseObj(t, r), "status"); s == "cancelled" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status never became cancelled: %s", r)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func testSessionUnknown(t *testing.T, e *env) {
	mustStatus(t, e.do(t, "GET", "keys/sessions/"+randHex(16)), 404)
}
