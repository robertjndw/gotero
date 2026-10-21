package accounts_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
)

func runCLI(t *testing.T, svc *accounts.Service, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := svc.RunCLI(ctx(), args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), err
}

func TestCLIUserLifecycle(t *testing.T) {
	svc, store, _, _ := newService(t)

	out, _, err := runCLI(t, svc, "s3cret-password\n", "user", "add", "alice", "--password-stdin", "--email", "a@example.com", "--admin", "--id", "777")
	require.NoError(t, err)
	assert.Equal(t, "created user alice (id 777)\n", out)
	u, err := store.Users().GetByUsername(ctx(), "alice")
	require.NoError(t, err)
	assert.EqualValues(t, 777, u.ID)
	assert.True(t, u.IsAdmin)
	ok, _ := auth.VerifyPassword(u.PasswordHash, "s3cret-password")
	assert.True(t, ok, "stdin newline is trimmed")

	// Flags may come after the positional argument.
	_, _, err = runCLI(t, svc, "", "user", "add", "bob", "--password", "another-long-pw")
	require.NoError(t, err)

	_, _, err = runCLI(t, svc, "", "user", "add", "bob", "--password", "another-long-pw")
	assert.ErrorContains(t, err, "username")
	_, _, err = runCLI(t, svc, "short\n", "user", "add", "carl", "--password-stdin")
	assert.ErrorContains(t, err, "password")
	_, _, err = runCLI(t, svc, "", "user", "add", "carl")
	assert.ErrorContains(t, err, "password is required")
	_, _, err = runCLI(t, svc, "x", "user", "add", "carl", "--password-stdin", "--password", "x")
	assert.Error(t, err)
	_, _, err = runCLI(t, svc, "", "user", "add", "--password", "another-long-pw")
	assert.Error(t, err, "username missing")

	out, _, err = runCLI(t, svc, "", "user", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "bob")

	_, _, err = runCLI(t, svc, "brand-new-password", "user", "passwd", "bob", "--password-stdin")
	require.NoError(t, err, "no trailing newline is fine")
	_, err = svc.Authenticate(ctx(), "bob", "brand-new-password")
	require.NoError(t, err)
	_, _, err = runCLI(t, svc, "x\n", "user", "passwd", "ghost", "--password-stdin")
	assert.ErrorContains(t, err, "not found")
	_, _, err = runCLI(t, svc, "", "user", "passwd", "bob")
	assert.Error(t, err)

	_, _, err = runCLI(t, svc, "", "user", "disable", "bob")
	require.NoError(t, err)
	_, err = svc.Authenticate(ctx(), "bob", "brand-new-password")
	assert.ErrorIs(t, err, accounts.ErrDisabled)
	_, _, err = runCLI(t, svc, "", "user", "enable", "bob")
	require.NoError(t, err)
	_, err = svc.Authenticate(ctx(), "bob", "brand-new-password")
	assert.NoError(t, err)

	_, _, err = runCLI(t, svc, "", "user", "delete", "bob")
	assert.ErrorContains(t, err, "--yes")
	_, err = store.Users().GetByUsername(ctx(), "bob")
	require.NoError(t, err, "nothing deleted without --yes")
	_, _, err = runCLI(t, svc, "", "user", "delete", "bob", "--yes")
	require.NoError(t, err)
	_, err = store.Users().GetByUsername(ctx(), "bob")
	assert.ErrorIs(t, err, repo.ErrNotFound)

	_, _, err = runCLI(t, svc, "", "user", "delete", "alice", "--yes")
	assert.ErrorContains(t, err, "last administrator")
}

func TestCLIInviteCreate(t *testing.T) {
	svc, _, cfg, _ := newService(t)
	cfg.PublicURL = "https://zotero.example.org"
	out, _, err := runCLI(t, svc, "", "invite", "create", "--admin", "--hours", "1")
	require.NoError(t, err)
	link := strings.TrimSpace(out)
	require.True(t, strings.HasPrefix(link, "https://zotero.example.org/register?invite="), link)
	token := strings.TrimPrefix(link, "https://zotero.example.org/register?invite=")
	inv, valid, err := svc.LookupInvite(ctx(), token)
	require.NoError(t, err)
	assert.True(t, valid)
	assert.True(t, inv.IsAdmin)

	cfg.PublicURL = ""
	out, errOut, err := runCLI(t, svc, "", "invite", "create")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "/register?invite="))
	assert.Contains(t, errOut, "GOTERO_PUBLIC_URL")

	_, _, err = runCLI(t, svc, "", "invite", "create", "--hours", "0")
	assert.Error(t, err)
}

func TestCLIUnknownCommand(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, _, err := runCLI(t, svc, "", "user", "frobnicate")
	assert.ErrorContains(t, err, "unknown command")
	_, _, err = runCLI(t, svc, "", "user")
	assert.ErrorContains(t, err, "usage")
}
