package accounts_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/sso"
)

func extID(sub, email string) accounts.ExternalIdentity {
	return accounts.ExternalIdentity{Provider: "corp", Identity: sso.Identity{Subject: sub, Email: email, EmailVerified: true}}
}

func login(id accounts.ExternalIdentity, p accounts.ExternalPolicy) accounts.ExternalLogin {
	return accounts.ExternalLogin{Identity: id, Policy: p}
}

func mkUser(t *testing.T, svc *accounts.Service, name, email string, admin bool) repo.User {
	t.Helper()
	u, err := svc.CreateUser(ctx(), accounts.CreateUserInput{
		Username: name, Password: "long-enough-pw", Email: email, IsAdmin: admin,
	})
	require.NoError(t, err)
	return u
}

func mkInvite(t *testing.T, svc *accounts.Service, email string, admin bool) []byte {
	t.Helper()
	_, tok, err := svc.CreateInvite(ctx(), accounts.CreateInviteInput{Email: email, IsAdmin: admin, TTL: time.Hour})
	require.NoError(t, err)
	return auth.HashToken(tok)
}

func TestLoginExternalNoAccount(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, err := svc.LoginExternal(ctx(), login(extID("s1", "a@example.com"), accounts.ExternalPolicy{}))
	assert.ErrorIs(t, err, accounts.ErrNoAccount)
	_, err = svc.LoginExternal(ctx(), login(accounts.ExternalIdentity{Provider: "corp"}, accounts.ExternalPolicy{AutoProvision: true}))
	assert.Error(t, err, "subject is required")
}

func TestLoginExternalAutoProvision(t *testing.T) {
	svc, store, _, _ := newService(t)
	id := extID("s1", "Jane.Doe@example.com")
	id.DisplayName = "Jane Doe"
	u, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{AutoProvision: true}))
	require.NoError(t, err)
	assert.Equal(t, "Jane.Doe", u.Username, "email local part when no username claim")
	assert.Equal(t, "Jane Doe", u.DisplayName)
	assert.Equal(t, "Jane.Doe@example.com", u.Email)
	assert.False(t, u.IsAdmin)
	assert.Empty(t, u.PasswordHash)

	again, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{}))
	require.NoError(t, err)
	assert.Equal(t, u.ID, again.ID, "second login follows the link")
	i, err := store.Identities().Get(ctx(), "corp", "s1")
	require.NoError(t, err)
	assert.NotNil(t, i.LastLoginAt)

	// An empty hash is never a valid password.
	_, err = svc.Authenticate(ctx(), "Jane.Doe", "")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials)
	_, err = svc.Authenticate(ctx(), "Jane.Doe", "anything-at-all")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials)
}

func TestLoginExternalNeverAdminByAutoProvision(t *testing.T) {
	svc, _, _, _ := newService(t)
	id := extID("s1", "root@example.com")
	id.Username = "admin"
	u, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{AutoProvision: true, LinkByEmail: true}))
	require.NoError(t, err)
	assert.False(t, u.IsAdmin)
}

func TestLoginExternalUnverifiedEmailNotStored(t *testing.T) {
	svc, _, _, _ := newService(t)
	id := extID("s1", "victim@example.com")
	id.EmailVerified = false
	u, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{AutoProvision: true}))
	require.NoError(t, err)
	assert.Empty(t, u.Email)
}

func TestLoginExternalInvalidEmailDropped(t *testing.T) {
	svc, _, _, _ := newService(t)
	id := extID("s1", "not an email")
	id.Username = "pat"
	u, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{AutoProvision: true}))
	require.NoError(t, err)
	assert.Equal(t, "pat", u.Username)
	assert.Empty(t, u.Email)
}

func TestLoginExternalUsernames(t *testing.T) {
	svc, _, _, _ := newService(t)
	pol := accounts.ExternalPolicy{AutoProvision: true}
	names := map[string]string{} // subject -> username
	for i, sub := range []string{"a", "b", "c"} {
		id := extID(sub, "")
		id.Username = "sam"
		u, err := svc.LoginExternal(ctx(), login(id, pol))
		require.NoError(t, err, i)
		names[sub] = u.Username
	}
	assert.Equal(t, map[string]string{"a": "sam", "b": "sam-2", "c": "sam-3"}, names)

	cases := []struct {
		id   accounts.ExternalIdentity
		want string
	}{
		{accounts.ExternalIdentity{Identity: sso.Identity{Username: "Jörg Müller"}}, "J-rg-M-ller"},
		{accounts.ExternalIdentity{Identity: sso.Identity{Username: "x", Email: "who@example.com"}}, "who"},
		{accounts.ExternalIdentity{Identity: sso.Identity{DisplayName: "Zed Zed"}}, "Zed-Zed"},
		{accounts.ExternalIdentity{Identity: sso.Identity{Username: "!!"}}, "user"},
		{accounts.ExternalIdentity{Identity: sso.Identity{Username: strings.Repeat("q", 100)}}, strings.Repeat("q", 54)},
	}
	for i, tc := range cases {
		tc.id.Provider, tc.id.Subject = "corp", "case-"+string(rune('a'+i))
		u, err := svc.LoginExternal(ctx(), login(tc.id, pol))
		require.NoError(t, err)
		assert.Equal(t, tc.want, u.Username)
		assert.NoError(t, accounts.ValidateUsername(u.Username))
	}
}

func TestLoginExternalUsernameRandomSuffix(t *testing.T) {
	svc, _, _, _ := newService(t)
	pol := accounts.ExternalPolicy{AutoProvision: true}
	for i := range 21 { // sam, sam-2 .. sam-20
		id := extID("s"+string(rune('a'+i)), "")
		id.Username = "sam"
		_, err := svc.LoginExternal(ctx(), login(id, pol))
		require.NoError(t, err)
	}
	id := extID("last", "")
	id.Username = "sam"
	u, err := svc.LoginExternal(ctx(), login(id, pol))
	require.NoError(t, err)
	assert.Regexp(t, `^sam-[0-9a-f]{8}$`, u.Username)
}

func TestLoginExternalDisabled(t *testing.T) {
	svc, _, _, _ := newService(t)
	pol := accounts.ExternalPolicy{AutoProvision: true, LinkByEmail: true}
	u, err := svc.LoginExternal(ctx(), login(extID("s1", "a@example.com"), pol))
	require.NoError(t, err)
	_, err = svc.UpdateUser(ctx(), u.ID, accounts.UserPatch{Disabled: ptr(true)}, 0)
	require.NoError(t, err)
	_, err = svc.LoginExternal(ctx(), login(extID("s1", "a@example.com"), pol))
	assert.ErrorIs(t, err, accounts.ErrDisabled)

	// Email linking onto a disabled account is refused as well.
	_, err = svc.LoginExternal(ctx(), login(extID("s2", "a@example.com"), pol))
	assert.ErrorIs(t, err, accounts.ErrDisabled)
}

func ptr[T any](v T) *T { return &v }

func TestLoginExternalEmailLink(t *testing.T) {
	svc, store, _, _ := newService(t)
	local := mkUser(t, svc, "local", "Local@Example.com", false)

	t.Run("policy off", func(t *testing.T) {
		_, err := svc.LoginExternal(ctx(), login(extID("s1", "local@example.com"), accounts.ExternalPolicy{}))
		assert.ErrorIs(t, err, accounts.ErrNoAccount)
	})
	t.Run("policy off with auto-provision hits the email clash", func(t *testing.T) {
		_, err := svc.LoginExternal(ctx(), login(extID("s1", "local@example.com"), accounts.ExternalPolicy{AutoProvision: true}))
		assert.ErrorIs(t, err, accounts.ErrLinkRequired)
		_, err = store.Identities().Get(ctx(), "corp", "s1")
		assert.ErrorIs(t, err, repo.ErrNotFound, "nothing was created")
	})
	t.Run("unverified", func(t *testing.T) {
		id := extID("s1", "local@example.com")
		id.EmailVerified = false
		_, err := svc.LoginExternal(ctx(), login(id, accounts.ExternalPolicy{LinkByEmail: true}))
		assert.ErrorIs(t, err, accounts.ErrNoAccount)
	})
	t.Run("verified", func(t *testing.T) {
		u, err := svc.LoginExternal(ctx(), login(extID("s1", "LOCAL@example.com"), accounts.ExternalPolicy{LinkByEmail: true}))
		require.NoError(t, err)
		assert.Equal(t, local.ID, u.ID)
		i, err := store.Identities().Get(ctx(), "corp", "s1")
		require.NoError(t, err)
		assert.Equal(t, local.ID, i.UserID)
		assert.NotNil(t, i.LastLoginAt)
		// The password keeps working.
		_, err = svc.Authenticate(ctx(), "local", "long-enough-pw")
		assert.NoError(t, err)
	})
	t.Run("subject changed for linked provider", func(t *testing.T) {
		_, err := svc.LoginExternal(ctx(), login(extID("s-new", "local@example.com"), accounts.ExternalPolicy{LinkByEmail: true}))
		assert.ErrorIs(t, err, accounts.ErrLinkRequired)
		_, err = store.Identities().Get(ctx(), "corp", "s-new")
		assert.ErrorIs(t, err, repo.ErrNotFound)
	})
}

func TestLoginExternalNeverLinksAdminByEmail(t *testing.T) {
	svc, store, _, _ := newService(t)
	admin := mkUser(t, svc, "root", "root@example.com", true)
	for _, pol := range []accounts.ExternalPolicy{
		{LinkByEmail: true}, {LinkByEmail: true, AutoProvision: true},
	} {
		_, err := svc.LoginExternal(ctx(), login(extID("s1", "root@example.com"), pol))
		assert.ErrorIs(t, err, accounts.ErrLinkRequired)
	}
	ids, err := store.Identities().ListByUser(ctx(), admin.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)

	// An explicit link while signed in is the supported way.
	require.NoError(t, svc.LinkExternal(ctx(), admin.ID, extID("s1", "root@example.com")))
	u, err := svc.LoginExternal(ctx(), login(extID("s1", "root@example.com"), accounts.ExternalPolicy{}))
	require.NoError(t, err)
	assert.Equal(t, admin.ID, u.ID)
	assert.True(t, u.IsAdmin, "login does not change admin status")
}

func TestLoginExternalInvite(t *testing.T) {
	svc, store, _, _ := newService(t)

	t.Run("claims the invite and honours is_admin", func(t *testing.T) {
		h := mkInvite(t, svc, "", true)
		u, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s1", "inv@example.com"), InviteHash: h})
		require.NoError(t, err)
		assert.True(t, u.IsAdmin)
		inv, err := store.Invites().GetByHash(ctx(), h)
		require.NoError(t, err)
		require.NotNil(t, inv.UsedBy)
		assert.Equal(t, u.ID, *inv.UsedBy)

		_, err = svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s2", "x@example.com"), InviteHash: h})
		assert.ErrorIs(t, err, accounts.ErrInvalidInvite, "single use")
	})
	t.Run("non-admin invite", func(t *testing.T) {
		h := mkInvite(t, svc, "", false)
		u, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s3", ""), InviteHash: h})
		require.NoError(t, err)
		assert.False(t, u.IsAdmin)
	})
	t.Run("email must match and be verified", func(t *testing.T) {
		h := mkInvite(t, svc, "want@example.com", false)
		_, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s4", "other@example.com"), InviteHash: h})
		assert.ErrorIs(t, err, accounts.ErrInviteEmailMismatch)
		unverified := extID("s4", "want@example.com")
		unverified.EmailVerified = false
		_, err = svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: unverified, InviteHash: h})
		assert.ErrorIs(t, err, accounts.ErrInviteEmailMismatch)
		_, err = svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s4", ""), InviteHash: h})
		assert.ErrorIs(t, err, accounts.ErrInviteEmailMismatch)

		u, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s4", "WANT@example.com"), InviteHash: h})
		require.NoError(t, err)
		assert.Equal(t, "WANT@example.com", u.Email)
	})
	t.Run("unknown invite", func(t *testing.T) {
		_, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{
			Identity: extID("s5", ""), Policy: accounts.ExternalPolicy{AutoProvision: true}, InviteHash: auth.HashToken("nope"),
		})
		assert.ErrorIs(t, err, accounts.ErrInvalidInvite, "no fallback to auto-provision")
	})
	t.Run("email clash leaves the invite unused", func(t *testing.T) {
		mkUser(t, svc, "taken", "taken@example.com", false)
		h := mkInvite(t, svc, "", false)
		_, err := svc.LoginExternal(ctx(), accounts.ExternalLogin{Identity: extID("s6", "taken@example.com"), InviteHash: h})
		assert.ErrorIs(t, err, accounts.ErrLinkRequired)
		inv, err := store.Invites().GetByHash(ctx(), h)
		require.NoError(t, err)
		assert.Nil(t, inv.UsedBy)
	})
}

func TestLinkExternal(t *testing.T) {
	svc, store, _, _ := newService(t)
	a := mkUser(t, svc, "alice", "", false)
	b := mkUser(t, svc, "bob", "", false)

	require.NoError(t, svc.LinkExternal(ctx(), a.ID, extID("s1", "a@example.com")))
	require.NoError(t, svc.LinkExternal(ctx(), a.ID, extID("s1", "a@example.com")), "same user again is a no-op")
	assert.ErrorIs(t, svc.LinkExternal(ctx(), b.ID, extID("s1", "")), accounts.ErrIdentityInUse)
	assert.ErrorIs(t, svc.LinkExternal(ctx(), a.ID, extID("s2", "")), accounts.ErrIdentityInUse, "second account at the same provider")
	assert.ErrorIs(t, svc.LinkExternal(ctx(), 9999, extID("s3", "")), repo.ErrNotFound)

	_, err := svc.UpdateUser(ctx(), b.ID, accounts.UserPatch{Disabled: ptr(true)}, 0)
	require.NoError(t, err)
	assert.ErrorIs(t, svc.LinkExternal(ctx(), b.ID, extID("s4", "")), accounts.ErrDisabled)

	list, err := store.Identities().ListByUser(ctx(), a.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "s1", list[0].Subject)
	_, err = store.Identities().Get(ctx(), "corp", "s4")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestUnlinkIdentityGuard(t *testing.T) {
	svc, _, cfg, _ := newService(t)

	t.Run("password and local login", func(t *testing.T) {
		u := mkUser(t, svc, "alice", "", false)
		require.NoError(t, svc.LinkExternal(ctx(), u.ID, extID("s1", "")))
		require.NoError(t, svc.UnlinkIdentity(ctx(), u.ID, "corp"))
		assert.ErrorIs(t, svc.UnlinkIdentity(ctx(), u.ID, "corp"), repo.ErrNotFound)
	})
	t.Run("no password", func(t *testing.T) {
		u, err := svc.LoginExternal(ctx(), login(extID("s2", ""), accounts.ExternalPolicy{AutoProvision: true}))
		require.NoError(t, err)
		assert.ErrorIs(t, svc.UnlinkIdentity(ctx(), u.ID, "corp"), accounts.ErrLastLoginMethod)
	})
	t.Run("other identity remains", func(t *testing.T) {
		u, err := svc.LoginExternal(ctx(), login(extID("s3", ""), accounts.ExternalPolicy{AutoProvision: true}))
		require.NoError(t, err)
		other := accounts.ExternalIdentity{Provider: "okta", Identity: sso.Identity{Subject: "o1"}}
		require.NoError(t, svc.LinkExternal(ctx(), u.ID, other))
		require.NoError(t, svc.UnlinkIdentity(ctx(), u.ID, "okta"))
		assert.ErrorIs(t, svc.UnlinkIdentity(ctx(), u.ID, "corp"), accounts.ErrLastLoginMethod)
	})
	t.Run("link to a removed provider does not count", func(t *testing.T) {
		u, err := svc.LoginExternal(ctx(), login(extID("s5", ""), accounts.ExternalPolicy{AutoProvision: true}))
		require.NoError(t, err)
		gone := accounts.ExternalIdentity{Provider: "retired", Identity: sso.Identity{Subject: "r1"}}
		require.NoError(t, svc.LinkExternal(ctx(), u.ID, gone))
		assert.ErrorIs(t, svc.UnlinkIdentity(ctx(), u.ID, "corp"), accounts.ErrLastLoginMethod)
		assert.NoError(t, svc.UnlinkIdentity(ctx(), u.ID, "retired"), "a dead link can always go")
	})
	t.Run("local login off", func(t *testing.T) {
		cfg.LocalLogin = false
		t.Cleanup(func() { cfg.LocalLogin = true })
		member := mkUser(t, svc, "member", "", false)
		admin := mkUser(t, svc, "boss", "", true)
		require.NoError(t, svc.LinkExternal(ctx(), member.ID, extID("m1", "")))
		require.NoError(t, svc.LinkExternal(ctx(), admin.ID, extID("b1", "")))
		assert.ErrorIs(t, svc.UnlinkIdentity(ctx(), member.ID, "corp"), accounts.ErrLastLoginMethod,
			"a password does not count for non-admins")
		assert.NoError(t, svc.UnlinkIdentity(ctx(), admin.ID, "corp"), "admins keep password login")
	})
}

func TestLocalLoginOff(t *testing.T) {
	svc, _, cfg, _ := newService(t)
	member := mkUser(t, svc, "member", "", false)
	admin := mkUser(t, svc, "boss", "", true)
	cfg.LocalLogin = false

	assert.True(t, svc.LocalLoginAllowed(admin))
	assert.False(t, svc.LocalLoginAllowed(member))

	_, err := svc.Login(ctx(), "member", "long-enough-pw", "1.1.1.1")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials, "correct password looks wrong")
	_, err = svc.Login(ctx(), "member", "wrong-password", "1.1.1.1")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials)

	_, err = svc.UpdateUser(ctx(), member.ID, accounts.UserPatch{Disabled: ptr(true)}, 0)
	require.NoError(t, err)
	_, err = svc.Login(ctx(), "member", "long-enough-pw", "1.1.1.1")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials, "disabled does not leak a confirmed password")

	u, err := svc.Login(ctx(), "boss", "long-enough-pw", "1.1.1.1")
	require.NoError(t, err)
	assert.Equal(t, admin.ID, u.ID)

	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "newbie", Password: "long-enough-pw"}, "")
	assert.ErrorIs(t, err, accounts.ErrLocalLoginDisabled)
	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "newbie", Password: "long-enough-pw"}, "token")
	assert.ErrorIs(t, err, accounts.ErrLocalLoginDisabled)
}
