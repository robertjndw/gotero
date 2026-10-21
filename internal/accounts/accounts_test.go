package accounts_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func ctx() context.Context { return context.Background() }

func newService(t *testing.T) (*accounts.Service, *pg.Store, *config.Config, *bytes.Buffer) {
	t.Helper()
	store := pg.New(pgtest.New(t))
	cfg := &config.Config{Secret: strings.Repeat("s", 32)}
	logs := &bytes.Buffer{}
	svc := accounts.New(store, auth.NewAuthenticator(store.Keys(), store.Users()), cfg,
		slog.New(slog.NewTextHandler(logs, nil)))
	return svc, store, cfg, logs
}

func fields(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *accounts.ValidationError
	require.ErrorAs(t, err, &ve)
	return ve.Fields
}

func TestCreateUserValidation(t *testing.T) {
	svc, _, _, _ := newService(t)
	cases := []struct {
		name string
		in   accounts.CreateUserInput
		bad  string
	}{
		{"short name", accounts.CreateUserInput{Username: "a", Password: "long-enough-pw"}, "username"},
		{"long name", accounts.CreateUserInput{Username: strings.Repeat("a", 65), Password: "long-enough-pw"}, "username"},
		{"bad chars", accounts.CreateUserInput{Username: "has space", Password: "long-enough-pw"}, "username"},
		{"unicode", accounts.CreateUserInput{Username: "jörg", Password: "long-enough-pw"}, "username"},
		{"short password", accounts.CreateUserInput{Username: "okname", Password: "123456789"}, "password"},
		{"bad email", accounts.CreateUserInput{Username: "okname", Password: "long-enough-pw", Email: "nope"}, "email"},
		{"email with name", accounts.CreateUserInput{Username: "okname", Password: "long-enough-pw", Email: "A <a@b.co>"}, "email"},
		{"negative quota", accounts.CreateUserInput{Username: "okname", Password: "long-enough-pw", Quota: accounts.Some(int64(-1))}, "quotaBytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateUser(ctx(), tc.in)
			assert.Contains(t, fields(t, err), tc.bad)
		})
	}

	for _, name := range []string{"ab", "A.b_c-d", strings.Repeat("x", 64)} {
		_, err := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: name, Password: "long-enough-pw"})
		assert.NoError(t, err, name)
	}
}

func TestCreateUserUniquenessAndExplicitID(t *testing.T) {
	svc, store, _, _ := newService(t)
	u, err := svc.CreateUser(ctx(), accounts.CreateUserInput{ID: 4242, Username: "Alice", Password: "long-enough-pw", Email: "a@example.com"})
	require.NoError(t, err)
	assert.EqualValues(t, 4242, u.ID)
	assert.Equal(t, "Alice", u.DisplayName, "display name defaults to the username")
	assert.NotEqual(t, "long-enough-pw", u.PasswordHash)
	ok, err := auth.VerifyPassword(u.PasswordHash, "long-enough-pw")
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "ALICE", Password: "long-enough-pw"})
	var ce *accounts.ConflictError
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Fields, "username")
	assert.ErrorIs(t, err, repo.ErrConflict)

	_, err = svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "other", Password: "long-enough-pw", Email: "A@example.com"})
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Fields, "email")

	_, err = svc.CreateUser(ctx(), accounts.CreateUserInput{ID: 4242, Username: "other", Password: "long-enough-pw"})
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Fields, "id")

	_, err = store.Users().GetByUsername(ctx(), "other")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	svc, store, _, logs := newService(t)
	boot := config.AdminBootstrap{Username: "root", Password: "bootstrap-pass", Email: "root@example.com"}

	require.NoError(t, svc.EnsureBootstrapAdmin(ctx(), config.AdminBootstrap{}), "nothing configured")
	n, _ := store.Users().CountAdmins(ctx())
	assert.Zero(t, n)

	require.NoError(t, svc.EnsureBootstrapAdmin(ctx(), boot))
	u, err := store.Users().GetByUsername(ctx(), "root")
	require.NoError(t, err)
	assert.True(t, u.IsAdmin)
	assert.Equal(t, "root@example.com", u.Email)
	assert.Nil(t, u.QuotaBytes, "admins are unlimited")

	// Idempotent, and it does not reset a changed password.
	hash := u.PasswordHash
	require.NoError(t, svc.EnsureBootstrapAdmin(ctx(), boot))
	again, _ := store.Users().GetByUsername(ctx(), "root")
	assert.Equal(t, hash, again.PasswordHash)

	// With an admin around, a different configured name is ignored.
	require.NoError(t, svc.EnsureBootstrapAdmin(ctx(), config.AdminBootstrap{Username: "second", Password: "bootstrap-pass"}))
	_, err = store.Users().GetByUsername(ctx(), "second")
	assert.ErrorIs(t, err, repo.ErrNotFound)
	assert.NotContains(t, logs.String(), "level=WARN")
}

func TestEnsureBootstrapAdminExistingNonAdmin(t *testing.T) {
	svc, store, _, logs := newService(t)
	_, err := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "root", Password: "regular-user-pass"})
	require.NoError(t, err)

	require.NoError(t, svc.EnsureBootstrapAdmin(ctx(), config.AdminBootstrap{Username: "ROOT", Password: "bootstrap-pass"}))
	u, _ := store.Users().GetByUsername(ctx(), "root")
	assert.False(t, u.IsAdmin, "never promoted silently")
	assert.Contains(t, logs.String(), "level=WARN")
	n, _ := store.Users().CountAdmins(ctx())
	assert.Zero(t, n)
}

func TestEnsureBootstrapAdminRejectsWeakPassword(t *testing.T) {
	svc, _, _, _ := newService(t)
	err := svc.EnsureBootstrapAdmin(ctx(), config.AdminBootstrap{Username: "root", Password: "weak"})
	assert.Error(t, err)
}

func TestInvitesAreAtomicAndSingleUse(t *testing.T) {
	svc, store, _, _ := newService(t)
	admin, err := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "root", Password: "bootstrap-pass", IsAdmin: true})
	require.NoError(t, err)

	inv, token, err := svc.CreateInvite(ctx(), accounts.CreateInviteInput{CreatedBy: &admin.ID, IsAdmin: true, TTL: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, token, svc.InviteToken(inv), "the token is reproducible from the row")
	_, valid, err := svc.LookupInvite(ctx(), token)
	require.NoError(t, err)
	assert.True(t, valid)

	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "bad name", Password: "long-enough-pw"}, token)
	assert.Contains(t, fields(t, err), "username")
	_, valid, _ = svc.LookupInvite(ctx(), token)
	assert.True(t, valid, "validation failures do not consume the invite")

	u, err := svc.Register(ctx(), accounts.CreateUserInput{Username: "newbie", Password: "long-enough-pw", IsAdmin: false}, token)
	require.NoError(t, err)
	assert.True(t, u.IsAdmin, "the invite decides the admin flag")
	stored, _ := store.Invites().GetByHash(ctx(), auth.HashToken(token))
	require.NotNil(t, stored.UsedBy)
	assert.Equal(t, u.ID, *stored.UsedBy)

	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "again", Password: "long-enough-pw"}, token)
	assert.ErrorIs(t, err, accounts.ErrInvalidInvite)
	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "again", Password: "long-enough-pw"}, "garbage")
	assert.ErrorIs(t, err, accounts.ErrInvalidInvite)
	_, valid, _ = svc.LookupInvite(ctx(), token)
	assert.False(t, valid)
}

func TestConcurrentRegistrationsShareOneInvite(t *testing.T) {
	svc, store, _, _ := newService(t)
	_, token, err := svc.CreateInvite(ctx(), accounts.CreateInviteInput{TTL: time.Hour})
	require.NoError(t, err)

	errs := make(chan error, 6)
	for i := range 6 {
		go func() {
			_, err := svc.Register(ctx(), accounts.CreateUserInput{Username: "racer" + string(rune('a'+i)), Password: "long-enough-pw"}, token)
			errs <- err
		}()
	}
	wins := 0
	for range 6 {
		if err := <-errs; err == nil {
			wins++
		}
	}
	assert.Equal(t, 1, wins)
	users, _ := store.Users().List(ctx(), "racer", 0, 0)
	assert.Len(t, users, 1, "losers leave no accounts behind")
}

func TestInviteValidationAndExpiry(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, _, err := svc.CreateInvite(ctx(), accounts.CreateInviteInput{TTL: 0})
	assert.Contains(t, fields(t, err), "expiresInHours")
	_, _, err = svc.CreateInvite(ctx(), accounts.CreateInviteInput{TTL: time.Hour, Email: "x"})
	assert.Contains(t, fields(t, err), "email")

	now := time.Now()
	svc.SetClock(func() time.Time { return now })
	_, token, err := svc.CreateInvite(ctx(), accounts.CreateInviteInput{TTL: time.Hour})
	require.NoError(t, err)
	now = now.Add(2 * time.Hour)
	_, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "late", Password: "long-enough-pw"}, token)
	assert.ErrorIs(t, err, accounts.ErrInvalidInvite)
}

func TestRegistrationAndQuotaSettings(t *testing.T) {
	svc, _, cfg, _ := newService(t)
	_, err := svc.Register(ctx(), accounts.CreateUserInput{Username: "walkin", Password: "long-enough-pw"}, "")
	assert.ErrorIs(t, err, accounts.ErrRegistrationClosed)

	open, err := svc.OpenRegistration(ctx())
	require.NoError(t, err)
	assert.False(t, open)
	cfg.OpenRegistration = true
	open, _ = svc.OpenRegistration(ctx())
	assert.True(t, open, "config is the default")
	require.NoError(t, svc.SetOpenRegistration(ctx(), false))
	open, _ = svc.OpenRegistration(ctx())
	assert.False(t, open, "the stored setting overrides it")

	q, err := svc.DefaultQuota(ctx())
	require.NoError(t, err)
	assert.Nil(t, q)
	cfg.DefaultQuotaMB = 3
	q, _ = svc.DefaultQuota(ctx())
	require.NotNil(t, q)
	assert.EqualValues(t, 3<<20, *q)
	u, err := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "quota1", Password: "long-enough-pw"})
	require.NoError(t, err)
	assert.EqualValues(t, 3<<20, *u.QuotaBytes)

	require.NoError(t, svc.SetDefaultQuota(ctx(), nil))
	q, _ = svc.DefaultQuota(ctx())
	assert.Nil(t, q, "an explicit unlimited beats the config default")
	require.NoError(t, svc.SetOpenRegistration(ctx(), true))
	u, err = svc.Register(ctx(), accounts.CreateUserInput{Username: "walkin", Password: "long-enough-pw", IsAdmin: true}, "")
	require.NoError(t, err)
	assert.False(t, u.IsAdmin)
	assert.Nil(t, u.QuotaBytes)

	n := int64(77)
	require.NoError(t, svc.SetDefaultQuota(ctx(), &n))
	u, _ = svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "quota2", Password: "long-enough-pw"})
	assert.EqualValues(t, 77, *u.QuotaBytes)
	u, _ = svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "quota3", Password: "long-enough-pw", Quota: accounts.Null[int64]()})
	assert.Nil(t, u.QuotaBytes)
	neg := int64(-1)
	assert.Error(t, svc.SetDefaultQuota(ctx(), &neg))
}

func TestAdminGuards(t *testing.T) {
	svc, store, _, _ := newService(t)
	a1, _ := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "admin1", Password: "long-enough-pw", IsAdmin: true})
	a2, _ := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "admin2", Password: "long-enough-pw", IsAdmin: true})
	no, yes := false, true

	_, err := svc.UpdateUser(ctx(), a1.ID, accounts.UserPatch{IsAdmin: &no}, a1.ID)
	assert.ErrorIs(t, err, accounts.ErrSelfModification)
	_, err = svc.UpdateUser(ctx(), a1.ID, accounts.UserPatch{Disabled: &yes}, a1.ID)
	assert.ErrorIs(t, err, accounts.ErrSelfModification)
	assert.ErrorIs(t, svc.DeleteUser(ctx(), a1.ID, a1.ID), accounts.ErrSelfModification)

	_, err = svc.UpdateUser(ctx(), a2.ID, accounts.UserPatch{Disabled: &yes}, a1.ID)
	require.NoError(t, err, "a second admin exists")
	_, err = svc.UpdateUser(ctx(), a1.ID, accounts.UserPatch{IsAdmin: &no}, 0)
	assert.ErrorIs(t, err, accounts.ErrLastAdmin, "disabled admins do not count")
	assert.ErrorIs(t, svc.DeleteUser(ctx(), a1.ID, 0), accounts.ErrLastAdmin)

	// Disabling or deleting an already disabled admin never strands the instance.
	require.NoError(t, svc.DeleteUser(ctx(), a2.ID, 0))
	still, _ := store.Users().Get(ctx(), a1.ID)
	assert.True(t, still.IsAdmin)

	_, err = svc.UpdateUser(ctx(), 999, accounts.UserPatch{Disabled: &yes}, 0)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestDisablingRevokesCachedKeysAndSessions(t *testing.T) {
	store := pg.New(pgtest.New(t))
	cfg := &config.Config{Secret: strings.Repeat("s", 32)}
	a := auth.NewAuthenticator(store.Keys(), store.Users())
	svc := accounts.New(store, a, cfg, nil)
	u, _ := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "dave", Password: "long-enough-pw"})
	_, raw, err := svc.CreateKey(ctx(), u.ID, "k", accounts.FullAccess())
	require.NoError(t, err)
	require.NoError(t, store.WebSessions().Create(ctx(), repo.WebSession{IDHash: []byte("s"), UserID: u.ID, CSRFToken: "c", ExpiresAt: time.Now().Add(time.Hour)}))
	_, err = a.Authenticate(ctx(), raw)
	require.NoError(t, err)

	yes := true
	_, err = svc.UpdateUser(ctx(), u.ID, accounts.UserPatch{Disabled: &yes}, 0)
	require.NoError(t, err)
	_, err = a.Authenticate(ctx(), raw)
	assert.ErrorIs(t, err, auth.ErrInvalidKey, "cache was invalidated")
	_, err = store.WebSessions().Get(ctx(), []byte("s"))
	assert.ErrorIs(t, err, repo.ErrNotFound)

	_, err = svc.Authenticate(ctx(), "dave", "long-enough-pw")
	assert.ErrorIs(t, err, accounts.ErrDisabled)
	_, err = svc.Authenticate(ctx(), "dave", "wrong")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials)
	_, err = svc.Authenticate(ctx(), "nobody", "wrong")
	assert.ErrorIs(t, err, accounts.ErrBadCredentials)
}

func TestSetPasswordAndKeys(t *testing.T) {
	svc, _, _, _ := newService(t)
	u, _ := svc.CreateUser(ctx(), accounts.CreateUserInput{Username: "erin", Password: "long-enough-pw"})
	assert.Contains(t, fields(t, svc.SetPassword(ctx(), u.ID, "short")), "password")
	require.NoError(t, svc.SetPassword(ctx(), u.ID, "another-long-pw"))
	_, err := svc.Authenticate(ctx(), "erin", "another-long-pw")
	require.NoError(t, err)

	_, _, err = svc.CreateKey(ctx(), u.ID, " ", accounts.FullAccess())
	assert.Contains(t, fields(t, err), "name")
	k, raw, err := svc.CreateKey(ctx(), u.ID, "ok", accounts.FullAccess())
	require.NoError(t, err)
	assert.Len(t, raw, 24)
	assert.Equal(t, raw[:4], k.Prefix)
	assert.ErrorIs(t, svc.RevokeKey(ctx(), u.ID+1, k.ID), repo.ErrNotFound)
	require.NoError(t, svc.RevokeKey(ctx(), u.ID, k.ID))
	assert.ErrorIs(t, svc.RevokeKey(ctx(), u.ID, k.ID), repo.ErrNotFound)
}
