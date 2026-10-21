package gotero

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/robertjndw/gotero/conformance"
)

// fakeAdmin mimics the CSRF and cookie rules of docs/dev/admin-api.md.
type fakeAdmin struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	nextID int64
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	f.calls = append(f.calls, r.Method+" "+path)
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	if path == "/login" {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["password"] == "wrong" {
			reply(401, map[string]any{"error": map[string]string{"code": "bad_credentials", "message": "nope"}})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "gotero_session", Value: "s-" + in["username"], Path: "/"})
		w.Header().Set("X-CSRF-Token", "csrf-"+in["username"])
		reply(200, map[string]any{"csrfToken": "csrf-" + in["username"]})
		return
	}
	c, err := r.Cookie("gotero_session")
	if err != nil {
		reply(401, nil)
		return
	}
	if r.Method != http.MethodGet {
		if r.Header.Get("X-CSRF-Token") != "csrf-"+strings.TrimPrefix(c.Value, "s-") || r.Header.Get("Origin") == "" {
			reply(403, map[string]any{"error": map[string]string{"code": "csrf", "message": "csrf"}})
			return
		}
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch {
	case path == "/admin/users" && r.Method == http.MethodPost:
		f.nextID++
		reply(201, map[string]any{"id": 100 + f.nextID})
	case path == "/me/keys" && r.Method == http.MethodPost:
		reply(201, map[string]any{"key": "KEY" + c.Value})
	case path == "/me/keys" && r.Method == http.MethodGet:
		reply(200, map[string]any{"items": []map[string]any{{"id": 7, "prefix": "KEYs"}}})
	case path == "/me/keys/7" && r.Method == http.MethodDelete:
		reply(204, nil)
	case path == "/groups" && r.Method == http.MethodPost:
		reply(201, map[string]any{"id": 55})
	case strings.HasPrefix(path, "/groups/55/members/"):
		reply(200, map[string]any{"role": in["role"]})
	case strings.HasPrefix(path, "/admin/users/") && r.Method == http.MethodPatch:
		if in["quotaBytes"] != float64(2048) {
			f.t.Errorf("quota body = %v", in)
		}
		reply(200, map[string]any{})
	case strings.HasSuffix(path, "/approve"):
		reply(200, map[string]any{"status": "completed"})
	default:
		reply(404, nil)
	}
}

func newFake(t *testing.T) (*fakeAdmin, *Provisioner) {
	f := &fakeAdmin{t: t}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, err := New(Options{AdminURL: srv.URL + "/admin/api", AdminUser: "root", AdminPass: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func TestNewUserFlow(t *testing.T) {
	f, p := newFake(t)
	u, err := p.NewUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != 101 || u.APIKey != "KEYs-"+u.Username || u.Username == "" || u.Password == "" {
		t.Errorf("user = %+v", u)
	}
	want := []string{"POST /login", "POST /admin/users", "POST /login", "POST /me/keys"}
	if strings.Join(f.calls, ";") != strings.Join(want, ";") {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestGroupMembershipQuotaApprovalRevoke(t *testing.T) {
	_, p := newFake(t)
	ctx := context.Background()
	owner, _ := p.NewUser(ctx)
	m, _ := p.NewUser(ctx)
	g, err := p.NewGroup(ctx, owner, []conformance.User{m}, []conformance.User{m})
	if err != nil || g.ID != 55 {
		t.Fatalf("group = %+v err=%v", g, err)
	}
	if err := p.AddMember(ctx, g, m, false); err != nil {
		t.Error(err)
	}
	if err := p.SetQuota(ctx, m, 2048); err != nil {
		t.Error(err)
	}
	if err := p.ApproveSession(ctx, m, "tok"); err != nil {
		t.Error(err)
	}
	m.APIKey = "KEYs-xyz"
	if err := p.RevokeKey(ctx, m); err != nil {
		t.Error(err)
	}
}

func TestCSRFMissingIsSurfaced(t *testing.T) {
	_, p := newFake(t)
	s := p.newSession()
	if err := s.login(context.Background(), "u", "wrong"); err == nil {
		t.Fatal("bad login accepted")
	}
	s = p.newSession()
	err := s.do(context.Background(), http.MethodPost, "/groups", map[string]string{}, nil)
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Errorf("err = %v, want 401 apiError", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	if _, err := New(Options{AdminURL: "nope", AdminUser: "a", AdminPass: "b"}); err == nil {
		t.Error("relative url accepted")
	}
	if _, err := New(Options{AdminURL: "http://h/admin/api"}); err == nil {
		t.Error("missing credentials accepted")
	}
	got, err := DefaultAdminURL("http://localhost:8080/api/")
	if err != nil || got != "http://localhost:8080/admin/api" {
		t.Errorf("DefaultAdminURL = %q %v", got, err)
	}
}
