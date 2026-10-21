// Package gotero provisions conformance users and groups through the gotero
// admin API (docs/dev/admin-api.md).
package gotero

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/gotero/conformance"
)

// Options configures a Provisioner.
type Options struct {
	// AdminURL is the admin API root, e.g. http://localhost:8080/admin/api
	AdminURL  string
	AdminUser string
	AdminPass string
	// HTTPClient is optional; its cookie jar is never used, each session gets its own.
	HTTPClient *http.Client
}

// Provisioner implements conformance.Provisioner plus the optional
// MemberAdder, KeyRevoker, QuotaSetter and SessionApprover interfaces.
type Provisioner struct {
	opt    Options
	origin string

	mu    sync.Mutex
	admin *session
}

var (
	_ conformance.Provisioner     = (*Provisioner)(nil)
	_ conformance.MemberAdder     = (*Provisioner)(nil)
	_ conformance.KeyRevoker      = (*Provisioner)(nil)
	_ conformance.QuotaSetter     = (*Provisioner)(nil)
	_ conformance.SessionApprover = (*Provisioner)(nil)
)

// New validates the options and returns a Provisioner. No request is made yet.
func New(opt Options) (*Provisioner, error) {
	u, err := url.Parse(opt.AdminURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("gotero: AdminURL must be an absolute URL")
	}
	if opt.AdminUser == "" || opt.AdminPass == "" {
		return nil, errors.New("gotero: admin username and password are required")
	}
	opt.AdminURL = strings.TrimRight(opt.AdminURL, "/")
	return &Provisioner{opt: opt, origin: u.Scheme + "://" + u.Host}, nil
}

// session is one logged-in cookie jar with its CSRF token.
type session struct {
	p    *Provisioner
	hc   *http.Client
	mu   sync.Mutex
	csrf string
}

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("admin api: HTTP %d %s: %s", e.Status, e.Code, e.Message)
}

func (p *Provisioner) newSession() *session {
	jar, _ := cookiejar.New(nil)
	hc := http.Client{Timeout: 30 * time.Second}
	if p.opt.HTTPClient != nil {
		hc = *p.opt.HTTPClient
	}
	hc.Jar = jar
	return &session{p: p, hc: &hc}
}

func (s *session) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.p.opt.AdminURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The server rejects state-changing requests without a matching Origin
	// and the CSRF token echoed from the login response.
	req.Header.Set("Origin", s.p.origin)
	s.mu.Lock()
	if s.csrf != "" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	s.mu.Unlock()

	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if tok := resp.Header.Get("X-CSRF-Token"); tok != "" {
		s.mu.Lock()
		s.csrf = tok
		s.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		ae := &apiError{Status: resp.StatusCode}
		var env struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Code, ae.Message = env.Error.Code, env.Error.Message
		} else {
			ae.Message = strings.TrimSpace(string(raw))
		}
		return ae
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (s *session) login(ctx context.Context, user, pass string) error {
	var me struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := s.do(ctx, http.MethodPost, "/login", map[string]string{"username": user, "password": pass}, &me); err != nil {
		return fmt.Errorf("login as %q: %w", user, err)
	}
	if me.CSRFToken != "" {
		s.mu.Lock()
		s.csrf = me.CSRFToken
		s.mu.Unlock()
	}
	return nil
}

func (p *Provisioner) adminSession(ctx context.Context) (*session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.admin != nil {
		return p.admin, nil
	}
	s := p.newSession()
	if err := s.login(ctx, p.opt.AdminUser, p.opt.AdminPass); err != nil {
		return nil, err
	}
	p.admin = s
	return s, nil
}

func (p *Provisioner) userSession(ctx context.Context, u conformance.User) (*session, error) {
	if u.Username == "" || u.Password == "" {
		return nil, fmt.Errorf("user %d has no credentials: %w", u.ID, conformance.ErrUnsupported)
	}
	s := p.newSession()
	return s, s.login(ctx, u.Username, u.Password)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUser creates a regular user and a full-access API key for it.
func (p *Provisioner) NewUser(ctx context.Context) (conformance.User, error) {
	adm, err := p.adminSession(ctx)
	if err != nil {
		return conformance.User{}, err
	}
	name := "conf" + randHex(6)
	u := conformance.User{Username: name, Password: randHex(12)}
	var created struct {
		ID int64 `json:"id"`
	}
	err = adm.do(ctx, http.MethodPost, "/admin/users", map[string]any{
		"username": name, "password": u.Password, "email": name + "@conformance.invalid",
		"displayName": name, "isAdmin": false, "quotaBytes": nil,
	}, &created)
	if err != nil {
		return u, fmt.Errorf("create user: %w", err)
	}
	u.ID = created.ID
	us, err := p.userSession(ctx, u)
	if err != nil {
		return u, err
	}
	var key struct {
		Key string `json:"key"`
	}
	err = us.do(ctx, http.MethodPost, "/me/keys", map[string]any{
		"name": "conformance",
		"access": map[string]any{
			"library": true, "notes": true, "write": true, "files": true, "groups": "all",
		},
	}, &key)
	if err != nil {
		return u, fmt.Errorf("create api key: %w", err)
	}
	u.APIKey = key.Key
	return u, nil
}

func (p *Provisioner) putMember(ctx context.Context, s *session, gid int64, u conformance.User, role string) error {
	path := fmt.Sprintf("/groups/%d/members/%d", gid, u.ID)
	if err := s.do(ctx, http.MethodPut, path, map[string]string{"role": role}, nil); err != nil {
		return fmt.Errorf("add member %d: %w", u.ID, err)
	}
	return nil
}

// NewGroup creates the group as owner and then adds members and admins.
func (p *Provisioner) NewGroup(ctx context.Context, owner conformance.User, members, admins []conformance.User) (conformance.Group, error) {
	ownerSess, err := p.userSession(ctx, owner)
	if err != nil {
		return conformance.Group{}, err
	}
	var g struct {
		ID int64 `json:"id"`
	}
	err = ownerSess.do(ctx, http.MethodPost, "/groups", map[string]string{
		"name": "conf-" + randHex(4), "description": "",
		"libraryReading": "members", "libraryEditing": "members", "fileEditing": "members",
	}, &g)
	if err != nil {
		return conformance.Group{}, fmt.Errorf("create group: %w", err)
	}
	for _, m := range members {
		if err := p.putMember(ctx, ownerSess, g.ID, m, "member"); err != nil {
			return conformance.Group{}, err
		}
	}
	for _, a := range admins {
		if err := p.putMember(ctx, ownerSess, g.ID, a, "admin"); err != nil {
			return conformance.Group{}, err
		}
	}
	return conformance.Group{ID: g.ID}, nil
}

// AddMember adds a member as the instance admin, who may manage every group.
func (p *Provisioner) AddMember(ctx context.Context, g conformance.Group, member conformance.User, admin bool) error {
	adm, err := p.adminSession(ctx)
	if err != nil {
		return err
	}
	role := "member"
	if admin {
		role = "admin"
	}
	return p.putMember(ctx, adm, g.ID, member, role)
}

// RevokeKey deletes the user's API key through the account endpoints.
func (p *Provisioner) RevokeKey(ctx context.Context, u conformance.User) error {
	us, err := p.userSession(ctx, u)
	if err != nil {
		return err
	}
	var keys struct {
		Items []struct {
			ID     int64  `json:"id"`
			Prefix string `json:"prefix"`
		} `json:"items"`
	}
	if err := us.do(ctx, http.MethodGet, "/me/keys", nil, &keys); err != nil {
		return err
	}
	for _, k := range keys.Items {
		if k.Prefix != "" && strings.HasPrefix(u.APIKey, k.Prefix) {
			return us.do(ctx, http.MethodDelete, fmt.Sprintf("/me/keys/%d", k.ID), nil, nil)
		}
	}
	return fmt.Errorf("no key with prefix matching user %d", u.ID)
}

// SetQuota sets the user's storage quota in bytes.
func (p *Provisioner) SetQuota(ctx context.Context, u conformance.User, bytes int64) error {
	adm, err := p.adminSession(ctx)
	if err != nil {
		return err
	}
	return adm.do(ctx, http.MethodPatch, fmt.Sprintf("/admin/users/%d", u.ID), map[string]any{"quotaBytes": bytes}, nil)
}

// ApproveSession approves a browser login session as the given user.
func (p *Provisioner) ApproveSession(ctx context.Context, u conformance.User, token string) error {
	us, err := p.userSession(ctx, u)
	if err != nil {
		return err
	}
	return us.do(ctx, http.MethodPost, "/login-sessions/"+url.PathEscape(token)+"/approve", nil, nil)
}
