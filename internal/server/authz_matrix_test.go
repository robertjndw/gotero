package server

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Zotero file sync identifies files by md5
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/internal/sso/ssotest"
	"github.com/robertjndw/gotero/zotero"
)

// TestAuthzMatrix sends every registered route a request from every
// principal that matters for it and checks the answer against a
// classification table. A route missing from the table fails the test, so a
// new endpoint cannot ship without someone deciding who may call it.
//
// Denied requests must answer 401, 403 or 404 (never a success or a
// redirect to the data), must not echo any seeded sentinel string, and must
// leave the database exactly as it was. Allowed principals only have to get
// past authorization: the point is to keep the deny cases honest, i.e. to
// prove the request was well formed enough that a missing check would have
// let it through.
func TestAuthzMatrix(t *testing.T) {
	fx := authzSetup(t)
	table := authzTable(fx)

	registered := map[string]bool{}
	for _, r := range fx.srv.engine.Routes() {
		id := r.Method + " " + r.Path
		registered[id] = true
		if _, ok := table[id]; !ok {
			t.Errorf("route %s is not classified: add it to authzTable in internal/server/authz_matrix_test.go "+
				"with the policy that says who may call it", id)
		}
	}
	for id := range table {
		if !registered[id] {
			t.Errorf("authzTable classifies %s, but no such route is registered: remove the stale entry", id)
		}
	}

	ids := make([]string, 0, len(table))
	for id := range table {
		if registered[id] {
			ids = append(ids, id)
		}
	}
	// Deny checks run first and against untouched fixtures. Allowed calls
	// mutate state, so they run afterwards with reads before writes and
	// deletes last, which keeps the seeded objects around for as long as
	// possible. Deletes go in reverse order so DELETE items/:key runs before
	// the bulk DELETE items and still finds its object.
	sort.Slice(ids, func(i, j int) bool {
		mi, mj := authzMethodRank(ids[i]), authzMethodRank(ids[j])
		if mi != mj {
			return mi < mj
		}
		if strings.HasPrefix(ids[i], http.MethodDelete) {
			return ids[i] > ids[j]
		}
		return ids[i] < ids[j]
	})

	for _, id := range ids {
		r := table[id]
		for _, name := range authzSortedPrincipals(r.policy, true) {
			p := fx.principal(t, name)
			t.Run(id+"/"+name, func(t *testing.T) { fx.checkDenied(t, id, r, p) })
		}
	}
	for _, id := range ids {
		r := table[id]
		for _, name := range authzSortedPrincipals(r.policy, false) {
			p := fx.principal(t, name)
			t.Run(id+"/"+name, func(t *testing.T) { fx.checkAllowed(t, id, r, p, r.policy[name].forbid) })
		}
	}
}

// The websocket's topic subscriptions are authorized per API key inside the
// stream protocol rather than at the HTTP layer, so the matrix can only call
// /stream public. This checks the part that matters: which library topics a
// key may listen to.
func TestAuthzStreamTopics(t *testing.T) {
	fx := authzSetup(t)
	userTopic := fmt.Sprintf("/users/%d", fx.userA.ID)
	groupTopic := fmt.Sprintf("/groups/%d", fx.group.ID)
	for _, tc := range []struct {
		principal string
		want      []string
	}{
		{"anonymous", nil},
		{"garbage-key", nil},
		{"b-full", nil},
		{"a-no-library", nil},
		{"a-full", []string{userTopic, groupTopic}},
		{"a-readonly", []string{userTopic, groupTopic}},
		{"owner-full", []string{groupTopic}},
	} {
		t.Run(tc.principal, func(t *testing.T) {
			p := fx.principal(t, tc.principal)
			got := fx.subscribe(t, p.key, []string{userTopic, groupTopic})
			if !slices.Equal(got, tc.want) {
				t.Errorf("subscribed topics %v, want %v", got, tc.want)
			}
		})
	}
}

const (
	authzSentinel = "authzsecret"
	// The API lets a client that claims a newer version than the server
	// through, so this satisfies every If-Unmodified-Since-Version check.
	// Without it a missing permission check would still answer 412/428 and
	// the matrix could not tell the difference.
	authzFarVersion = "999999999"
	authzGarbage    = "AuthzGarbage0000000000000"
)

func authzMethodRank(id string) int {
	method, _, _ := strings.Cut(id, " ")
	return slices.Index([]string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, method)
}

// authzExpect is what one principal should get from one route.
type authzExpect struct {
	deny bool
	// forbid is a string that must not appear in an allowed response, for
	// principals that may call a route but must see filtered data.
	forbid string
}

type authzPolicy map[string]authzExpect

func authzPol(allow, deny []string) authzPolicy {
	p := authzPolicy{}
	for _, n := range allow {
		p[n] = authzExpect{}
	}
	for _, n := range deny {
		p[n] = authzExpect{deny: true}
	}
	return p
}

// with returns a copy of p with extra entries.
func (p authzPolicy) with(name string, e authzExpect) authzPolicy {
	out := authzPolicy{}
	for k, v := range p {
		out[k] = v
	}
	out[name] = e
	return out
}

func authzSortedPrincipals(p authzPolicy, deny bool) []string {
	var out []string
	for n, e := range p {
		if e.deny == deny {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// authzRoute is one classified route plus what it takes to build a request
// that reaches its authorization check.
type authzRoute struct {
	policy authzPolicy
	// params fills the path parameters.
	params map[string]string
	// noteParams replaces params for the a-no-notes principal on reads, so it
	// asks for a note directly.
	noteParams map[string]string
	// tokenParam is the path parameter that is itself the credential; the
	// garbage-token principal replaces it.
	tokenParam string
	query      string
	header     map[string]string
	body       func(p *authzPrincipal) (contentType string, body []byte)
	// denyRedirect accepts a 303 to a page of this site as a denial. The SSO
	// start routes are browser navigations and answer refusals that way.
	denyRedirect bool
}

func authzJSON(v any) func(*authzPrincipal) (string, []byte) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return func(*authzPrincipal) (string, []byte) { return "application/json", b }
}

func authzForm(v url.Values) func(*authzPrincipal) (string, []byte) {
	return func(*authzPrincipal) (string, []byte) {
		return "application/x-www-form-urlencoded", []byte(v.Encode())
	}
}

// Principal names. API key principals carry Zotero-API-Key; web principals
// carry the session cookie, the CSRF header and an Origin.
const (
	pAnon           = "anonymous"
	pGarbageKey     = "garbage-key"
	pGarbageToken   = "garbage-token"
	pAFull          = "a-full"
	pARead          = "a-readonly"
	pANoNotes       = "a-no-notes"
	pANoFiles       = "a-no-files"
	pANoLib         = "a-no-library"
	pADisposable    = "a-disposable-key"
	pBFull          = "b-full"
	pOwnerFull      = "owner-full"
	pAnonOrigin     = "anonymous-same-origin"
	pAnonBadOrigin  = "anonymous-foreign-origin"
	pGarbageCookie  = "garbage-cookie"
	pWebA           = "web-a"
	pWebANoCSRF     = "web-a-no-csrf"
	pWebABadOrigin  = "web-a-foreign-origin"
	pWebANoOrigin   = "web-a-no-origin"
	pWebAKeyOnly    = "web-a-apikey-no-cookie"
	pWebADisposable = "web-a-disposable-session"
	pWebB           = "web-b"
	pWebC           = "web-c"
	pWebOwner       = "web-owner"
	pWebOwnerNoCSRF = "web-owner-no-csrf"
	pWebOwnerBadOr  = "web-owner-foreign-origin"
	pWebGroupAdmin  = "web-group-admin"
	pWebAdmin       = "web-admin"
	pWebAdminNoCSRF = "web-admin-no-csrf"
	pWebAdminBadOr  = "web-admin-foreign-origin"
	pWebAdminKey    = "web-admin-apikey-no-cookie"
	pWebDisabled    = "web-disabled-user"
)

type authzPrincipal struct {
	name     string
	key      string
	bearer   bool
	cookie   string
	csrf     string
	origin   string
	password string
	// prepare runs before each request, for principals whose state a denied
	// request may legitimately consume.
	prepare func(t *testing.T)
}

type authzLib struct {
	base     string // e.g. /api/users/12
	sentinel string
	item     string
	note     string
	coll     string
	search   string
	attach   string // has a stored file and full text
	upload   string // attachment without a file, for authorize calls
	tag      string
}

type authzFixture struct {
	url   string
	srv   *Server
	pool  *pgxpool.Pool
	store *pg.Store
	acc   *accounts.Service

	userA, userB, owner, member, groupAdmin, userC, disabled, victim, admin repo.User
	passwords                                                               map[int64]string

	group, group2 repo.Group
	userLib       authzLib
	groupLib      authzLib

	principals map[string]*authzPrincipal

	revocableKeyID int64
	importID       int64
	inviteID       int64
	inviteToken    string
	boundLogin     string // login session token bound to user A
	openLogin      string // unbound login session token
	downloadToken  string
	uploadKey      string
	uploadPayload  []byte
}

func (fx *authzFixture) principal(t *testing.T, name string) *authzPrincipal {
	t.Helper()
	p, ok := fx.principals[name]
	if !ok {
		t.Fatalf("unknown principal %q", name)
	}
	return p
}

func authzSetup(t *testing.T) *authzFixture {
	t.Helper()
	ctx := context.Background()

	// SSO is configured so the linking routes reach their session and CSRF
	// checks instead of answering 404 for an unknown provider. The SAML SP
	// needs the public URL up front, hence the unstarted server.
	oidc := ssotest.NewOIDCServer(t)
	idp := ssotest.NewSAMLIdP(t)
	certFile, keyFile := ssotest.WriteSPKeyPair(t, false)
	srv := httptest.NewUnstartedServer(nil)
	publicURL := "http://" + srv.Listener.Addr().String()
	cfg := &config.Config{
		Listen: ":0", LocalLogin: true, Secret: strings.Repeat("s", 32), PublicURL: publicURL,
		MaxRequestBytes: 1 << 20, MaxUploadBytes: 1 << 20,
		Storage: config.Storage{Backend: "local", LocalPath: t.TempDir()},
		OIDC: []config.OIDCProvider{{
			Name: "corp", Issuer: oidc.Issuer(), ClientID: ssotest.OIDCClientID, ClientSecret: "s3cret",
			DisplayName: "Corp", Scopes: []string{"openid", "email"}, UsernameClaim: "preferred_username",
		}},
		SAML: []config.SAMLProvider{{
			Name: "okta", IDPMetadataFile: idp.WriteMetadataFile(t), DisplayName: "Okta",
			EmailAttribute: "email", UsernameAttribute: "uid", NameAttribute: "displayName",
		}},
		SAMLSP: config.SAMLServiceProvider{CertFile: certFile, KeyFile: keyFile},
	}
	pool := pgtest.New(t)
	s, err := New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pool)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)

	store := pg.New(pool)
	fx := &authzFixture{
		url: srv.URL, srv: s, pool: pool, store: store,
		acc:        accounts.New(store, auth.NewAuthenticator(store.Keys(), store.Users()), cfg, nil),
		passwords:  map[int64]string{},
		principals: map[string]*authzPrincipal{},
	}
	user := func(name, display, email string, admin bool) repo.User {
		pw := name + " long password"
		u, err := fx.acc.CreateUser(ctx, accounts.CreateUserInput{
			Username: name, Password: pw, DisplayName: display, Email: email, IsAdmin: admin, EmailVerified: true,
		})
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		fx.passwords[u.ID] = pw
		return u
	}
	fx.admin = user("authzadmin", "", "", true)
	// A's email is a sentinel: user lookups must never reveal it.
	fx.userA = user("alice", "Alice Authz", "alice.authzsecretmail@example.org", false)
	fx.userB = user("bob", "", "", false)
	fx.owner = user("olga", "", "", false)
	fx.member = user("mia", "", "", false)
	fx.groupAdmin = user("gary", "", "", false)
	fx.userC = user("carl", "", "", false)
	fx.disabled = user("dora", "", "", false)
	fx.victim = user("victor", "", "", false)
	disable := true
	if _, err := fx.acc.UpdateUser(ctx, fx.disabled.ID, accounts.UserPatch{Disabled: &disable}, fx.admin.ID); err != nil {
		t.Fatal(err)
	}

	// Both groups are members-only to read, and only admins may edit the
	// library or its files, so A as a plain member can read but not write.
	mkGroup := func(name string) repo.Group {
		g, err := store.Groups().Create(ctx, repo.Group{
			OwnerID: fx.owner.ID, Name: name,
			LibraryReading: "members", LibraryEditing: "admins", FileEditing: "admins",
		})
		if err != nil {
			t.Fatal(err)
		}
		for uid, role := range map[int64]string{fx.userA.ID: "member", fx.member.ID: "member", fx.groupAdmin.ID: "admin"} {
			if err := store.Groups().SetMember(ctx, g.ID, uid, role); err != nil {
				t.Fatal(err)
			}
		}
		return g
	}
	fx.group = mkGroup("authzsecretg group")
	fx.group2 = mkGroup("authzsecretg second group")

	full := accounts.FullAccess()
	keyed := func(name string, u repo.User, access repo.KeyAccess) *authzPrincipal {
		_, raw, err := fx.acc.CreateKey(ctx, u.ID, name, access)
		if err != nil {
			t.Fatal(err)
		}
		p := &authzPrincipal{name: name, key: raw, password: fx.passwords[u.ID]}
		fx.principals[name] = p
		return p
	}
	keyed(pAFull, fx.userA, full)
	keyed(pARead, fx.userA, repo.KeyAccess{Library: true, Notes: true, Files: true, Groups: "read"})
	keyed(pANoNotes, fx.userA, repo.KeyAccess{Library: true, Write: true, Files: true, Groups: "all"})
	keyed(pANoFiles, fx.userA, repo.KeyAccess{Library: true, Notes: true, Write: true, Groups: "all"})
	keyed(pANoLib, fx.userA, repo.KeyAccess{Groups: "none"})
	keyed(pADisposable, fx.userA, full)
	keyed(pBFull, fx.userB, full)
	keyed(pOwnerFull, fx.owner, full)
	revocable, _, err := fx.acc.CreateKey(ctx, fx.userA.ID, "revocable", full)
	if err != nil {
		t.Fatal(err)
	}
	fx.revocableKeyID = revocable.ID
	_, adminKey, err := fx.acc.CreateKey(ctx, fx.admin.ID, "admin key", full)
	if err != nil {
		t.Fatal(err)
	}

	fx.principals[pAnon] = &authzPrincipal{name: pAnon}
	fx.principals[pGarbageKey] = &authzPrincipal{name: pGarbageKey, key: authzGarbage}
	fx.principals[pGarbageToken] = &authzPrincipal{name: pGarbageToken}
	fx.principals[pAnonOrigin] = &authzPrincipal{name: pAnonOrigin, origin: fx.url}
	fx.principals[pAnonBadOrigin] = &authzPrincipal{name: pAnonBadOrigin, origin: "https://evil.example"}
	fx.principals[pGarbageCookie] = &authzPrincipal{name: pGarbageCookie, cookie: authzGarbage, csrf: authzGarbage, origin: fx.url}

	web := func(name string, u repo.User) *authzPrincipal {
		cookie, csrf := fx.webSession(t, u.ID)
		p := &authzPrincipal{name: name, cookie: cookie, csrf: csrf, origin: fx.url, password: fx.passwords[u.ID]}
		fx.principals[name] = p
		return p
	}
	webA := web(pWebA, fx.userA)
	fx.principals[pWebANoCSRF] = &authzPrincipal{name: pWebANoCSRF, cookie: webA.cookie, origin: fx.url, password: webA.password}
	fx.principals[pWebABadOrigin] = &authzPrincipal{name: pWebABadOrigin, cookie: webA.cookie, csrf: webA.csrf, origin: "https://evil.example", password: webA.password}
	fx.principals[pWebANoOrigin] = &authzPrincipal{name: pWebANoOrigin, cookie: webA.cookie, csrf: webA.csrf, password: webA.password}
	fx.principals[pWebAKeyOnly] = &authzPrincipal{name: pWebAKeyOnly, key: fx.principals[pAFull].key, bearer: true, origin: fx.url, password: webA.password}
	web(pWebADisposable, fx.userA)
	web(pWebB, fx.userB)
	web(pWebC, fx.userC)
	webOwner := web(pWebOwner, fx.owner)
	fx.principals[pWebOwnerNoCSRF] = &authzPrincipal{name: pWebOwnerNoCSRF, cookie: webOwner.cookie, origin: fx.url}
	fx.principals[pWebOwnerBadOr] = &authzPrincipal{name: pWebOwnerBadOr, cookie: webOwner.cookie, csrf: webOwner.csrf, origin: "https://evil.example"}
	web(pWebGroupAdmin, fx.groupAdmin)
	webAdmin := web(pWebAdmin, fx.admin)
	fx.principals[pWebAdminNoCSRF] = &authzPrincipal{name: pWebAdminNoCSRF, cookie: webAdmin.cookie, origin: fx.url}
	fx.principals[pWebAdminBadOr] = &authzPrincipal{name: pWebAdminBadOr, cookie: webAdmin.cookie, csrf: webAdmin.csrf, origin: "https://evil.example"}
	fx.principals[pWebAdminKey] = &authzPrincipal{name: pWebAdminKey, key: adminKey, bearer: true, origin: fx.url}
	disabledWeb := web(pWebDisabled, fx.disabled)
	// The server deletes a disabled user's session when it sees it, which is
	// correct but would turn every later request into a plain "no session".
	disabledCSRF, disabledCookie := disabledWeb.csrf, disabledWeb.cookie
	disabledWeb.prepare = func(t *testing.T) {
		t.Helper()
		ctx := context.Background()
		hash := auth.HashToken(disabledCookie)
		_ = store.WebSessions().Delete(ctx, hash)
		err := store.WebSessions().Create(ctx, repo.WebSession{
			IDHash: hash, UserID: fx.disabled.ID, CSRFToken: disabledCSRF, ExpiresAt: time.Now().Add(14 * 24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("recreate disabled session: %v", err)
		}
	}

	fx.userLib = fx.seedLibrary(t, fmt.Sprintf("/api/users/%d", fx.userA.ID), fx.principals[pAFull].key, "authzsecreta")
	fx.groupLib = fx.seedLibrary(t, fmt.Sprintf("/api/groups/%d", fx.group.ID), fx.principals[pOwnerFull].key, "authzsecretg")

	// Self-authenticating file URLs, for the /files routes.
	r := fx.raw(t, http.MethodGet, fx.userLib.base+"/items/"+fx.userLib.attach+"/file", fx.principals[pAFull].key, nil, "", nil)
	if r.status != http.StatusFound {
		t.Fatalf("download redirect: %d %s", r.status, r.body)
	}
	fx.downloadToken = r.header.Get("Location")[strings.LastIndex(r.header.Get("Location"), "/")+1:]
	fx.uploadPayload = []byte("authzsecreta second upload")
	up := fx.authorizeUpload(t, fx.userLib, fx.principals[pAFull].key, fx.userLib.upload, fx.uploadPayload)
	fx.uploadKey = up.UploadKey

	job, err := store.Imports().Create(ctx, repo.ImportJob{
		UserID: fx.userA.ID, Status: "queued", SourceAPIKey: "authzsecretimportkey0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.importID = job.ID
	inv, invToken, err := fx.acc.CreateInvite(ctx, accounts.CreateInviteInput{CreatedBy: &fx.admin.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fx.inviteID, fx.inviteToken = inv.ID, invToken
	fx.boundLogin = fx.loginSession(t, &fx.userA.ID)
	fx.openLogin = fx.loginSession(t, nil)
	// Lets the register route succeed for its allowed caller, so a missing
	// Origin check would be visible as a new account.
	if err := fx.acc.SetOpenRegistration(ctx, true); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx *authzFixture) webSession(t *testing.T, userID int64) (cookie, csrf string) {
	t.Helper()
	cookie, csrf = zotero.NewToken(64), zotero.NewToken(32)
	err := fx.store.WebSessions().Create(context.Background(), repo.WebSession{
		IDHash: auth.HashToken(cookie), UserID: userID, CSRFToken: csrf, ExpiresAt: time.Now().Add(14 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cookie, csrf
}

func (fx *authzFixture) loginSession(t *testing.T, required *int64) string {
	t.Helper()
	token := zotero.NewToken(64)
	err := fx.store.LoginSessions().Create(context.Background(), repo.LoginSession{
		TokenHash: auth.HashToken(token), RequiredUserID: required, ClientName: "Zotero",
		Status: "pending", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type authzResp struct {
	status int
	header http.Header
	body   []byte
}

func (fx *authzFixture) raw(t *testing.T, method, path, key string, body []byte, ctype string, hdr map[string]string) authzResp {
	t.Helper()
	target := path
	if !strings.HasPrefix(target, "http") {
		target = fx.url + path
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Zotero-API-Version", "3")
	if key != "" {
		req.Header.Set("Zotero-API-Key", key)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return authzSend(t, req)
}

func authzSend(t *testing.T, req *http.Request) authzResp {
	t.Helper()
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return authzResp{status: res.StatusCode, header: res.Header, body: b}
}

// create writes one object through the API and returns its key.
func (fx *authzFixture) create(t *testing.T, base, key, typ string, obj map[string]any) string {
	t.Helper()
	b, _ := json.Marshal([]any{obj})
	r := fx.raw(t, http.MethodPost, base+"/"+typ, key, b, "application/json", nil)
	var out struct {
		Success map[string]string `json:"success"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil || out.Success["0"] == "" {
		t.Fatalf("create %s in %s: %d %s", typ, base, r.status, r.body)
	}
	return out.Success["0"]
}

type authzUpload struct {
	URL       string `json:"url"`
	UploadKey string `json:"uploadKey"`
}

func authzFileForm(data []byte) url.Values {
	sum := md5.Sum(data) //nolint:gosec // see import
	return url.Values{
		"md5": {hex.EncodeToString(sum[:])}, "filename": {"file.pdf"},
		"filesize": {strconv.Itoa(len(data))}, "mtime": {"1700000000000"},
	}
}

func (fx *authzFixture) authorizeUpload(t *testing.T, l authzLib, key, item string, data []byte) authzUpload {
	t.Helper()
	r := fx.raw(t, http.MethodPost, l.base+"/items/"+item+"/file", key, []byte(authzFileForm(data).Encode()),
		"application/x-www-form-urlencoded", map[string]string{"If-None-Match": "*"})
	var up authzUpload
	if r.status != http.StatusOK || json.Unmarshal(r.body, &up) != nil || up.UploadKey == "" {
		t.Fatalf("authorize upload: %d %s", r.status, r.body)
	}
	return up
}

// seedLibrary fills a library with one of everything, each carrying the
// sentinel so a leak shows up in any response body.
func (fx *authzFixture) seedLibrary(t *testing.T, base, key, s string) authzLib {
	t.Helper()
	l := authzLib{base: base, sentinel: s, tag: s + "tag"}
	l.coll = fx.create(t, base, key, "collections", map[string]any{"name": s + " collection"})
	l.item = fx.create(t, base, key, "items", map[string]any{
		"itemType": "book", "title": s + " item", "collections": []string{l.coll},
		"tags": []map[string]any{{"tag": l.tag}},
	})
	l.note = fx.create(t, base, key, "items", map[string]any{
		"itemType": "note", "note": "<p>" + s + "note body</p>",
		"tags": []map[string]any{{"tag": s + "notetag"}},
	})
	attachment := func(title string) string {
		return fx.create(t, base, key, "items", map[string]any{
			"itemType": "attachment", "linkMode": "imported_file", "title": title,
			"filename": "file.pdf", "contentType": "application/pdf",
		})
	}
	l.attach = attachment(s + " attachment")
	l.upload = attachment(s + " empty attachment")
	l.search = fx.create(t, base, key, "searches", map[string]any{
		"name":       s + " search",
		"conditions": []map[string]any{{"condition": "title", "operator": "contains", "value": "x"}},
	})
	setting, _ := json.Marshal(map[string]any{"value": []map[string]any{{"name": l.tag, "color": "#ff0000"}}})
	if r := fx.raw(t, http.MethodPut, base+"/settings/tagColors", key, setting, "application/json", nil); r.status != http.StatusNoContent {
		t.Fatalf("seed setting: %d %s", r.status, r.body)
	}
	ft, _ := json.Marshal(map[string]any{"content": s + " fulltext", "indexedChars": 10, "totalChars": 10})
	if r := fx.raw(t, http.MethodPut, base+"/items/"+l.attach+"/fulltext", key, ft, "application/json", nil); r.status != http.StatusNoContent {
		t.Fatalf("seed fulltext: %d %s", r.status, r.body)
	}
	data := []byte(s + " file bytes")
	up := fx.authorizeUpload(t, l, key, l.attach, data)
	if r := fx.raw(t, http.MethodPost, up.URL, "", data, "application/octet-stream", nil); r.status != http.StatusCreated {
		t.Fatalf("seed upload: %d %s", r.status, r.body)
	}
	r := fx.raw(t, http.MethodPost, base+"/items/"+l.attach+"/file", key, []byte(url.Values{"upload": {up.UploadKey}}.Encode()),
		"application/x-www-form-urlencoded", map[string]string{"If-None-Match": "*"})
	if r.status != http.StatusNoContent {
		t.Fatalf("seed register: %d %s", r.status, r.body)
	}
	return l
}

// fingerprint hashes every table a request could change. Columns that record
// harmless bookkeeping (last use of a key, last sync, the sliding session
// expiry) are left out because a denied request may still bump them, and so
// are sessions of disabled users, which the server rightly drops on sight.
func (fx *authzFixture) fingerprint(t *testing.T) string {
	t.Helper()
	tables := map[string]string{
		"libraries": "t::text", "groups": "t::text", "group_members": "t::text",
		"users":    "(to_jsonb(t) - 'last_sync_at')::text",
		"api_keys": "(to_jsonb(t) - 'last_used_at')::text",
		"(SELECT * FROM web_sessions WHERE user_id NOT IN (SELECT id FROM users WHERE disabled))": "(to_jsonb(t) - 'expires_at')::text",
		"login_sessions": "t::text", "invites": "t::text", "instance_settings": "t::text",
		"collections": "t::text", "searches": "t::text", "items": "t::text", "item_tags": "t::text",
		"settings": "t::text", "deleted_log": "t::text", "fulltext": "t::text", "blobs": "t::text",
		"attachment_files": "t::text", "upload_tokens": "t::text", "write_tokens": "t::text",
		"import_jobs": "t::text", "import_objects": "t::text", "user_identities": "t::text", "sso_states": "t::text",
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf(
			"(SELECT coalesce(string_agg(x, ',' ORDER BY x), '') FROM (SELECT %s AS x FROM %s t) s)", tables[n], n))
	}
	var sum string
	q := "SELECT md5(concat_ws('#', " + strings.Join(parts, ", ") + "))"
	if err := fx.pool.QueryRow(context.Background(), q).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

var authzParamRe = regexp.MustCompile(`:[A-Za-z]+`)

func (fx *authzFixture) request(t *testing.T, id string, r authzRoute, p *authzPrincipal) authzResp {
	t.Helper()
	method, pattern, _ := strings.Cut(id, " ")
	params := r.params
	if p.name == pANoNotes && method == http.MethodGet && r.noteParams != nil {
		params = r.noteParams
	}
	path := authzParamRe.ReplaceAllStringFunc(pattern, func(m string) string {
		name := m[1:]
		if p.name == pGarbageToken && name == r.tokenParam {
			return authzGarbage
		}
		v, ok := params[name]
		if !ok {
			t.Fatalf("%s: no value for path parameter %s", id, m)
		}
		return url.PathEscape(v)
	})
	target := fx.url + path
	if r.query != "" {
		target += "?" + r.query
	}
	var body []byte
	var ctype string
	if r.body != nil {
		ctype, body = r.body(p)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(path, "/api/") {
		req.Header.Set("Zotero-API-Version", "3")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	if p.key != "" {
		req.Header.Set("Zotero-API-Key", p.key)
		if p.bearer {
			req.Header.Set("Authorization", "Bearer "+p.key)
		}
	}
	if p.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "gotero_session", Value: p.cookie})
	}
	if p.csrf != "" {
		req.Header.Set("X-CSRF-Token", p.csrf)
	}
	if p.origin != "" {
		req.Header.Set("Origin", p.origin)
	}
	return authzSend(t, req)
}

func (fx *authzFixture) checkDenied(t *testing.T, id string, r authzRoute, p *authzPrincipal) {
	if p.prepare != nil {
		p.prepare(t)
	}
	before := fx.fingerprint(t)
	res := fx.request(t, id, r, p)
	after := fx.fingerprint(t)

	loc := res.header.Get("Location")
	localRedirect := res.status == http.StatusSeeOther && strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//")
	switch {
	case res.status == http.StatusUnauthorized, res.status == http.StatusForbidden, res.status == http.StatusNotFound:
	case r.denyRedirect && localRedirect:
	default:
		t.Errorf("want 401, 403 or 404, got %d (Location %q): %s", res.status, loc, authzTrim(res.body))
	}
	if strings.Contains(strings.ToLower(string(res.body)), authzSentinel) {
		t.Errorf("denied response leaks seeded data: %s", authzTrim(res.body))
	}
	if before != after {
		t.Errorf("denied request changed the database (status %d): %s", res.status, authzTrim(res.body))
	}
}

func (fx *authzFixture) checkAllowed(t *testing.T, id string, r authzRoute, p *authzPrincipal, forbid string) {
	if p.prepare != nil {
		p.prepare(t)
	}
	res := fx.request(t, id, r, p)
	t.Logf("status %d", res.status)
	if res.status == http.StatusUnauthorized || res.status == http.StatusForbidden || res.status >= 500 {
		t.Errorf("allowed principal got %d: %s", res.status, authzTrim(res.body))
	}
	if forbid != "" && strings.Contains(strings.ToLower(string(res.body)), forbid) {
		t.Errorf("response contains %q, which this principal must not see: %s", forbid, authzTrim(res.body))
	}
}

func authzTrim(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

// subscribe opens the stream, asks for topics with key and returns the
// topics the server granted.
func (fx *authzFixture) subscribe(t *testing.T, key string, topics []string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(fx.url, "http")+"/stream", nil) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	msg, _ := json.Marshal(map[string]any{
		"action":        "createSubscriptions",
		"subscriptions": []map[string]any{{"apiKey": key, "topics": topics}},
	})
	if key == "" {
		msg, _ = json.Marshal(map[string]any{
			"action": "createSubscriptions", "subscriptions": []map[string]any{{"topics": topics}},
		})
	}
	if err := ws.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for subscriptionsCreated: %v", err)
		}
		var ev struct {
			Event         string `json:"event"`
			Subscriptions []struct {
				Topics []string `json:"topics"`
			} `json:"subscriptions"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Event != "subscriptionsCreated" {
			continue
		}
		var got []string
		for _, s := range ev.Subscriptions {
			got = append(got, s.Topics...)
		}
		return got
	}
}

// authzTable is the classification of every route. Each entry names who may
// call the route and who must be refused.
func authzTable(fx *authzFixture) map[string]authzRoute {
	var (
		// Cookies never authenticate the Zotero API.
		zapiAnon = []string{pAnon, pGarbageKey, pWebA}

		keyAny     = authzPol([]string{pAFull, pANoLib}, zapiAnon)
		keyOwnUser = authzPol([]string{pAFull, pANoLib}, append(slices.Clone(zapiAnon), pBFull))
		public     = authzPol([]string{pAnon}, nil)
		token      = authzPol([]string{pAnon}, []string{pGarbageToken})
		origin     = authzPol([]string{pAnonOrigin}, []string{pAnon, pAnonBadOrigin})

		sessDeny    = []string{pAnon, pGarbageCookie, pWebAKeyOnly, pWebDisabled}
		csrfDeny    = append(slices.Clone(sessDeny), pWebANoCSRF, pWebABadOrigin, pWebANoOrigin)
		session     = authzPol([]string{pWebA}, sessDeny)
		sessionCSRF = authzPol([]string{pWebA}, csrfDeny)
		// Resources that belong to user A: B must not reach them either.
		sessionOwn     = authzPol([]string{pWebA}, append(slices.Clone(sessDeny), pWebB))
		sessionOwnCSRF = authzPol([]string{pWebA}, append(slices.Clone(csrfDeny), pWebB))

		adminDeny = []string{pAnon, pGarbageCookie, pWebA, pWebAdminKey, pWebDisabled}
		admin     = authzPol([]string{pWebAdmin}, adminDeny)
		adminCSRF = authzPol([]string{pWebAdmin}, append(slices.Clone(adminDeny), pWebAdminNoCSRF, pWebAdminBadOr))

		groupMember  = authzPol([]string{pWebA, pWebOwner, pWebGroupAdmin, pWebAdmin}, []string{pAnon, pGarbageCookie, pWebB, pWebAKeyOnly, pWebDisabled})
		groupManDeny = []string{pAnon, pGarbageCookie, pWebB, pWebA, pWebOwnerNoCSRF, pWebOwnerBadOr, pWebDisabled}
		groupManage  = authzPol([]string{pWebOwner, pWebGroupAdmin}, groupManDeny)
		groupOwner   = authzPol([]string{pWebOwner}, append(slices.Clone(groupManDeny), pWebGroupAdmin))
		ssoLink      = authzPol([]string{pWebA}, []string{pAnon, pGarbageCookie, pWebANoCSRF, pWebABadOrigin, pWebANoOrigin, pWebAKeyOnly, pWebDisabled})
		farVersion   = map[string]string{"If-Unmodified-Since-Version": authzFarVersion}
		groupID      = strconv.FormatInt(fx.group.ID, 10)
		userAID      = strconv.FormatInt(fx.userA.ID, 10)
		groupsHidden = authzExpect{forbid: "authzsecretg"}
		ssoLinkBody  = func(p *authzPrincipal) (string, []byte) { return authzForm(url.Values{"csrf": {p.csrf}})(p) }
		passwordBody = func(p *authzPrincipal) (string, []byte) {
			return authzJSON(map[string]string{"currentPassword": p.password, "newPassword": "a brand new passphrase"})(p)
		}
	)

	table := map[string]authzRoute{
		"GET /healthz": {policy: public},
		"GET /stream":  {policy: public},

		// Zotero API outside libraries.
		"GET /api/keys/current":    {policy: keyAny},
		"DELETE /api/keys/current": {policy: authzPol([]string{pADisposable}, zapiAnon)},
		// Knowing a key is enough to describe it, but presenting a different
		// key must not let anyone read someone else's.
		"GET /api/keys/:key": {
			policy:     authzPol([]string{pAnon, pAFull}, []string{pGarbageToken, pBFull}),
			params:     map[string]string{"key": fx.principals[pAFull].key},
			tokenParam: "key",
		},
		"POST /api/keys": {
			policy: public,
			body: authzJSON(map[string]string{
				"username": fx.userA.Username, "password": fx.passwords[fx.userA.ID], "name": "authz legacy",
			}),
		},
		"POST /api/keys/sessions":          {policy: public},
		"GET /api/keys/sessions/:token":    {policy: token, params: map[string]string{"token": fx.openLogin}, tokenParam: "token"},
		"DELETE /api/keys/sessions/:token": {policy: token, params: map[string]string{"token": fx.openLogin}, tokenParam: "token"},
		"GET /api/users/:id/groups":        {policy: keyOwnUser, params: map[string]string{"id": userAID}},
		"GET /api/groups/:id": {
			policy: authzPol([]string{pOwnerFull, pAFull, pARead}, append(slices.Clone(zapiAnon), pBFull, pANoLib)),
			params: map[string]string{"id": groupID},
		},
		"GET /files/download/:token": {policy: token, params: map[string]string{"token": fx.downloadToken}, tokenParam: "token"},
		"POST /files/upload/:uploadKey": {
			policy: token, params: map[string]string{"uploadKey": fx.uploadKey}, tokenParam: "uploadKey",
			body: func(*authzPrincipal) (string, []byte) { return "application/octet-stream", fx.uploadPayload },
		},

		// Web UI API: public routes, guarded by Origin where they change state.
		"GET /admin/api/setup":          {policy: public},
		"GET /admin/api/invites/:token": {policy: public, params: map[string]string{"token": fx.inviteToken}},
		"POST /admin/api/login": {
			policy: origin,
			body:   authzJSON(map[string]string{"username": fx.userA.Username, "password": fx.passwords[fx.userA.ID]}),
		},
		"POST /admin/api/register": {
			policy: origin,
			body:   authzJSON(map[string]string{"username": "newcomer", "password": "newcomer long password", "email": "newcomer@example.org"}),
		},
		// A logout with a stolen or missing CSRF token must not end the
		// victim's session.
		"POST /admin/api/logout": {
			policy: authzPol([]string{pWebADisposable}, []string{pAnon, pAnonBadOrigin, pWebANoCSRF, pWebABadOrigin, pWebANoOrigin}),
		},

		// Signed-in user.
		"GET /admin/api/me":      {policy: session},
		"PATCH /admin/api/me":    {policy: sessionCSRF, body: authzJSON(map[string]string{"displayName": "Changed By Request"})},
		"GET /admin/api/me/keys": {policy: session},
		"POST /admin/api/me/keys": {
			policy: sessionCSRF, body: authzJSON(map[string]any{"name": "authz new key", "access": accounts.FullAccess()}),
		},
		"DELETE /admin/api/me/keys/:id": {
			policy: sessionOwnCSRF, params: map[string]string{"id": strconv.FormatInt(fx.revocableKeyID, 10)},
		},
		// Changing the password logs out every session, so the allowed call
		// runs as a separate user to keep web-a usable for later checks.
		"POST /admin/api/me/password": {
			policy: authzPol([]string{pWebC}, csrfDeny), body: passwordBody,
		},
		"DELETE /admin/api/me/identities/:provider": {policy: sessionCSRF, params: map[string]string{"provider": "corp"}},
		"GET /admin/api/me/groups":                  {policy: session.with(pWebB, groupsHidden)},
		"GET /admin/api/users/lookup": {
			policy: session.with(pWebB, authzExpect{forbid: "authzsecretmail"}),
			query:  "q=authzsecretmail",
		},
		"GET /admin/api/login-sessions/:token": {policy: session, params: map[string]string{"token": fx.boundLogin}},
		// The bound session may only be approved, or cancelled, by user A.
		"POST /admin/api/login-sessions/:token/approve": {policy: sessionOwnCSRF, params: map[string]string{"token": fx.boundLogin}},
		"POST /admin/api/login-sessions/:token/cancel":  {policy: sessionOwnCSRF, params: map[string]string{"token": fx.boundLogin}},
		"GET /admin/api/imports":                        {policy: session},
		"POST /admin/api/imports": {
			// Malformed on purpose: a valid key would make the server call
			// zotero.org.
			policy: sessionCSRF, body: authzJSON(map[string]any{"apiKey": "x"}),
		},
		"GET /admin/api/imports/:id":         {policy: sessionOwn, params: map[string]string{"id": strconv.FormatInt(fx.importID, 10)}},
		"POST /admin/api/imports/:id/cancel": {policy: sessionOwnCSRF, params: map[string]string{"id": strconv.FormatInt(fx.importID, 10)}},

		// Groups in the web UI. Outsiders get 404 so group IDs cannot be probed.
		"GET /admin/api/groups":     {policy: session.with(pWebB, groupsHidden)},
		"POST /admin/api/groups":    {policy: sessionCSRF, body: authzJSON(map[string]string{"name": "authz new group"})},
		"GET /admin/api/groups/:id": {policy: groupMember, params: map[string]string{"id": groupID}},
		"PATCH /admin/api/groups/:id": {
			policy: groupManage, params: map[string]string{"id": groupID},
			body: authzJSON(map[string]string{"description": "patched by request"}),
		},
		// Deleting runs on the second group so the first survives the
		// allowed call.
		"DELETE /admin/api/groups/:id":      {policy: groupOwner, params: map[string]string{"id": strconv.FormatInt(fx.group2.ID, 10)}},
		"GET /admin/api/groups/:id/members": {policy: groupMember, params: map[string]string{"id": groupID}},
		"PUT /admin/api/groups/:id/members/:userId": {
			policy: groupManage, params: map[string]string{"id": groupID, "userId": strconv.FormatInt(fx.member.ID, 10)},
			body: authzJSON(map[string]string{"role": "member"}),
		},
		"DELETE /admin/api/groups/:id/members/:userId": {
			policy: groupManage, params: map[string]string{"id": groupID, "userId": strconv.FormatInt(fx.member.ID, 10)},
		},

		// Instance administration.
		"GET /admin/api/admin/stats": {policy: admin},
		"GET /admin/api/admin/users": {policy: admin},
		"POST /admin/api/admin/users": {
			policy: adminCSRF,
			body:   authzJSON(map[string]any{"username": "minted", "password": "minted long password", "isAdmin": true}),
		},
		"PATCH /admin/api/admin/users/:id": {
			policy: adminCSRF, params: map[string]string{"id": strconv.FormatInt(fx.victim.ID, 10)},
			body: authzJSON(map[string]any{"isAdmin": true}),
		},
		"DELETE /admin/api/admin/users/:id": {policy: adminCSRF, params: map[string]string{"id": strconv.FormatInt(fx.victim.ID, 10)}},
		"DELETE /admin/api/admin/users/:id/identities/:provider": {
			policy: adminCSRF, params: map[string]string{"id": strconv.FormatInt(fx.victim.ID, 10), "provider": "corp"},
		},
		"GET /admin/api/admin/invites":        {policy: admin},
		"POST /admin/api/admin/invites":       {policy: adminCSRF, body: authzJSON(map[string]any{"isAdmin": true})},
		"DELETE /admin/api/admin/invites/:id": {policy: adminCSRF, params: map[string]string{"id": strconv.FormatInt(fx.inviteID, 10)}},
		"GET /admin/api/admin/settings":       {policy: admin},
		"PATCH /admin/api/admin/settings":     {policy: adminCSRF, body: authzJSON(map[string]any{"defaultQuotaBytes": 12345})},
		"POST /admin/api/admin/gc":            {policy: adminCSRF, body: authzJSON(map[string]any{"dryRun": true})},

		// SSO. Starting a login and the IdP-facing endpoints are public by
		// nature; a POST to start links the provider to the signed-in
		// account and needs the session, its CSRF token and our Origin.
		"GET /auth/oidc/:provider/start":    {policy: public, params: map[string]string{"provider": "corp"}},
		"GET /auth/oidc/:provider/callback": {policy: public, params: map[string]string{"provider": "corp"}},
		"GET /auth/saml/:provider/start":    {policy: public, params: map[string]string{"provider": "okta"}},
		"POST /auth/saml/:provider/acs":     {policy: public, params: map[string]string{"provider": "okta"}},
		"GET /auth/saml/:provider/finish":   {policy: public, params: map[string]string{"provider": "okta"}},
		"GET /auth/saml/:provider/metadata": {policy: public, params: map[string]string{"provider": "okta"}},
		"POST /auth/oidc/:provider/start":   {policy: ssoLink, params: map[string]string{"provider": "corp"}, body: ssoLinkBody, denyRedirect: true},
		"POST /auth/saml/:provider/start":   {policy: ssoLink, params: map[string]string{"provider": "okta"}, body: ssoLinkBody, denyRedirect: true},
	}

	for _, kind := range []string{"users", "groups"} {
		for id, r := range authzLibraryRoutes(fx, kind, farVersion) {
			table[id] = r
		}
	}
	return table
}

// Library access classes, see authzLibraryRoutes.
const (
	libRead = iota
	libWrite
	libFileRead
	libFileWrite
)

// authzLibraryRoutes classifies the routes registered under both
// /api/users/:id and /api/groups/:id. The user library belongs to A; the
// group library to the owner, with A as a plain member who may read but,
// as only admins edit, not write.
func authzLibraryRoutes(fx *authzFixture, kind string, farVersion map[string]string) map[string]authzRoute {
	l, id := fx.userLib, strconv.FormatInt(fx.userA.ID, 10)
	zapiAnon := []string{pAnon, pGarbageKey, pWebA}
	policies := map[int]authzPolicy{
		libRead: authzPol([]string{pAFull, pARead, pANoFiles}, append(slices.Clone(zapiAnon), pBFull, pANoLib)).
			with(pANoNotes, authzExpect{forbid: "authzsecretanote"}),
		libWrite:     authzPol([]string{pAFull, pANoNotes, pANoFiles}, append(slices.Clone(zapiAnon), pBFull, pANoLib, pARead)),
		libFileRead:  authzPol([]string{pAFull, pARead, pANoNotes}, append(slices.Clone(zapiAnon), pBFull, pANoLib, pANoFiles)),
		libFileWrite: authzPol([]string{pAFull}, append(slices.Clone(zapiAnon), pBFull, pANoLib, pANoFiles, pARead)),
	}
	if kind == "groups" {
		l, id = fx.groupLib, strconv.FormatInt(fx.group.ID, 10)
		// Group keys have no separate notes or files switch: "all" or
		// "read" grants both.
		read := authzPol([]string{pOwnerFull, pAFull, pARead, pANoFiles}, append(slices.Clone(zapiAnon), pBFull, pANoLib))
		write := authzPol([]string{pOwnerFull}, append(slices.Clone(zapiAnon), pBFull, pANoLib, pAFull, pARead))
		policies = map[int]authzPolicy{libRead: read, libWrite: write, libFileRead: read, libFileWrite: write}
	}

	p := func(kv ...string) map[string]string {
		m := map[string]string{"id": id}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	itemP, noteP := p("key", l.item), p("key", l.note)
	injected := l.sentinel + " injected"
	book := authzJSON(map[string]any{"itemType": "book", "title": injected})
	coll := authzJSON(map[string]any{"name": injected})
	search := authzJSON(map[string]any{
		"name": injected, "conditions": []map[string]any{{"condition": "title", "operator": "contains", "value": "y"}},
	})
	tagColors := []map[string]any{{"name": injected, "color": "#000000"}}

	type spec struct {
		method, path string
		class        int
		r            authzRoute
	}
	specs := []spec{
		{http.MethodGet, "/items", libRead, authzRoute{}},
		{http.MethodGet, "/items/top", libRead, authzRoute{}},
		{http.MethodGet, "/items/trash", libRead, authzRoute{}},
		{http.MethodGet, "/items/tags", libRead, authzRoute{}},
		{http.MethodGet, "/items/top/tags", libRead, authzRoute{}},
		{http.MethodGet, "/items/:key", libRead, authzRoute{params: itemP, noteParams: noteP}},
		{http.MethodGet, "/items/:key/children", libRead, authzRoute{params: itemP, noteParams: noteP}},
		{http.MethodGet, "/items/:key/tags", libRead, authzRoute{params: itemP, noteParams: noteP}},
		{http.MethodGet, "/collections", libRead, authzRoute{}},
		{http.MethodGet, "/collections/top", libRead, authzRoute{}},
		{http.MethodGet, "/collections/:key", libRead, authzRoute{params: p("key", l.coll)}},
		{http.MethodGet, "/collections/:key/collections", libRead, authzRoute{params: p("key", l.coll)}},
		{http.MethodGet, "/collections/:key/items", libRead, authzRoute{params: p("key", l.coll)}},
		{http.MethodGet, "/collections/:key/items/top", libRead, authzRoute{params: p("key", l.coll)}},
		{http.MethodGet, "/collections/:key/tags", libRead, authzRoute{params: p("key", l.coll)}},
		{http.MethodGet, "/searches", libRead, authzRoute{}},
		{http.MethodGet, "/searches/:key", libRead, authzRoute{params: p("key", l.search)}},
		{http.MethodGet, "/tags", libRead, authzRoute{}},
		{http.MethodGet, "/tags/:name", libRead, authzRoute{params: p("name", l.tag)}},
		{http.MethodGet, "/settings", libRead, authzRoute{}},
		{http.MethodGet, "/settings/:key", libRead, authzRoute{params: p("key", "tagColors")}},
		{http.MethodGet, "/deleted", libRead, authzRoute{query: "since=0"}},
		{http.MethodGet, "/fulltext", libRead, authzRoute{query: "since=0"}},
		{http.MethodGet, "/items/:key/fulltext", libRead, authzRoute{params: p("key", l.attach)}},
		{http.MethodGet, "/items/:key/file", libFileRead, authzRoute{params: p("key", l.attach)}},

		{http.MethodPost, "/items", libWrite, authzRoute{body: func(q *authzPrincipal) (string, []byte) {
			ct, b := book(q)
			return ct, append(append([]byte("["), b...), ']')
		}}},
		{http.MethodPut, "/items/:key", libWrite, authzRoute{params: itemP, body: book}},
		{http.MethodPatch, "/items/:key", libWrite, authzRoute{params: itemP, body: authzJSON(map[string]any{"title": injected})}},
		{http.MethodDelete, "/items", libWrite, authzRoute{query: "itemKey=" + l.item}},
		{http.MethodDelete, "/items/:key", libWrite, authzRoute{params: itemP}},
		{http.MethodPost, "/collections", libWrite, authzRoute{body: authzJSON([]any{map[string]any{"name": injected}})}},
		{http.MethodPut, "/collections/:key", libWrite, authzRoute{params: p("key", l.coll), body: coll}},
		{http.MethodPatch, "/collections/:key", libWrite, authzRoute{params: p("key", l.coll), body: coll}},
		{http.MethodDelete, "/collections", libWrite, authzRoute{query: "collectionKey=" + l.coll}},
		{http.MethodDelete, "/collections/:key", libWrite, authzRoute{params: p("key", l.coll)}},
		{http.MethodPost, "/searches", libWrite, authzRoute{body: func(q *authzPrincipal) (string, []byte) {
			ct, b := search(q)
			return ct, append(append([]byte("["), b...), ']')
		}}},
		{http.MethodPut, "/searches/:key", libWrite, authzRoute{params: p("key", l.search), body: search}},
		{http.MethodPatch, "/searches/:key", libWrite, authzRoute{params: p("key", l.search), body: search}},
		{http.MethodDelete, "/searches", libWrite, authzRoute{query: "searchKey=" + l.search}},
		{http.MethodDelete, "/searches/:key", libWrite, authzRoute{params: p("key", l.search)}},
		{http.MethodPost, "/settings", libWrite, authzRoute{body: authzJSON(map[string]any{"tagColors": map[string]any{"value": tagColors}})}},
		{http.MethodPut, "/settings/:key", libWrite, authzRoute{params: p("key", "tagColors"), body: authzJSON(map[string]any{"value": tagColors})}},
		{http.MethodDelete, "/settings", libWrite, authzRoute{query: "settingKey=tagColors"}},
		{http.MethodDelete, "/settings/:key", libWrite, authzRoute{params: p("key", "tagColors")}},
		{http.MethodDelete, "/tags", libWrite, authzRoute{query: "tag=" + url.QueryEscape(l.tag)}},
		{http.MethodPost, "/fulltext", libWrite, authzRoute{body: authzJSON([]any{map[string]any{
			"key": l.attach, "content": injected, "indexedChars": 1, "totalChars": 1,
		}})}},
		{http.MethodPut, "/items/:key/fulltext", libWrite, authzRoute{params: p("key", l.attach), body: authzJSON(map[string]any{
			"content": injected, "indexedChars": 1, "totalChars": 1,
		})}},
		{http.MethodPost, "/items/:key/file", libFileWrite, authzRoute{
			params: p("key", l.upload), body: authzForm(authzFileForm([]byte(injected))),
			header: map[string]string{"If-None-Match": "*"},
		}},
	}
	if kind == "users" {
		specs = append(specs, spec{http.MethodPost, "/removestoragefiles", libFileWrite, authzRoute{}})
	}

	out := map[string]authzRoute{}
	for _, s := range specs {
		r := s.r
		r.policy = policies[s.class]
		if r.params == nil {
			r.params = p()
		}
		if s.method != http.MethodGet {
			h := map[string]string{}
			for k, v := range farVersion {
				h[k] = v
			}
			for k, v := range r.header {
				h[k] = v
			}
			r.header = h
		}
		out[s.method+" /api/"+kind+"/:id"+s.path] = r
	}
	return out
}
