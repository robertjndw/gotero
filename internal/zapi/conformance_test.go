package zapi_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/robertjndw/gotero/conformance"
	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// provisioner creates users, keys and groups straight in the repository, so
// the conformance suites exercise the HTTP layer and nothing else.
type provisioner struct {
	env *testEnv
	n   atomic.Int64
}

var (
	_ conformance.Provisioner = (*provisioner)(nil)
	_ conformance.MemberAdder = (*provisioner)(nil)
	_ conformance.KeyRevoker  = (*provisioner)(nil)
	_ conformance.QuotaSetter = (*provisioner)(nil)

	_ conformance.SessionApprover = (*provisioner)(nil)
)

// passwordHash is computed once: argon2 is deliberately slow and every test
// user can share the same password.
const testPassword = "conformance-password"

var testPasswordHash = func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
}()

func (p *provisioner) NewUser(ctx context.Context) (conformance.User, error) {
	name := fmt.Sprintf("user%d", p.n.Add(1))
	u, err := p.env.Store.Users().Create(ctx, repo.User{
		Username: name, DisplayName: name, PasswordHash: testPasswordHash,
	})
	if err != nil {
		return conformance.User{}, err
	}
	key := zotero.NewAPIKey()
	_, err = p.env.Store.Keys().Create(ctx, repo.APIKey{
		UserID: u.ID, Hash: auth.HashToken(key), Prefix: key[:4], Name: "conformance",
		Access: repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"},
	})
	if err != nil {
		return conformance.User{}, err
	}
	return conformance.User{ID: u.ID, Username: name, Password: testPassword, APIKey: key}, nil
}

func (p *provisioner) NewGroup(ctx context.Context, owner conformance.User, members, admins []conformance.User) (conformance.Group, error) {
	g, err := p.env.Store.Groups().Create(ctx, repo.Group{
		OwnerID: owner.ID, Name: fmt.Sprintf("group%d", p.n.Add(1)),
	})
	if err != nil {
		return conformance.Group{}, err
	}
	for _, m := range members {
		if err := p.env.Store.Groups().SetMember(ctx, g.ID, m.ID, "member"); err != nil {
			return conformance.Group{}, err
		}
	}
	for _, m := range admins {
		if err := p.env.Store.Groups().SetMember(ctx, g.ID, m.ID, "admin"); err != nil {
			return conformance.Group{}, err
		}
	}
	return conformance.Group{ID: g.ID}, nil
}

func (p *provisioner) AddMember(ctx context.Context, g conformance.Group, m conformance.User, admin bool) error {
	role := "member"
	if admin {
		role = "admin"
	}
	return p.env.Store.Groups().SetMember(ctx, g.ID, m.ID, role)
}

func (p *provisioner) RevokeKey(ctx context.Context, u conformance.User) error {
	h := auth.HashToken(u.APIKey)
	if err := p.env.Store.Keys().DeleteByHash(ctx, h); err != nil {
		return err
	}
	// The admin API does the same after revoking a key.
	p.env.Auth.Invalidate(h)
	return nil
}

func (p *provisioner) SetQuota(ctx context.Context, u conformance.User, bytes int64) error {
	user, err := p.env.Store.Users().Get(ctx, u.ID)
	if err != nil {
		return err
	}
	user.QuotaBytes = &bytes
	_, err = p.env.Store.Users().Update(ctx, user)
	return err
}

// ApproveSession does what the admin API's approval endpoint does: refuse a
// session bound to someone else, issue a full-access key and complete the
// session with it.
func (p *provisioner) ApproveSession(ctx context.Context, u conformance.User, token string) error {
	hash := auth.HashToken(token)
	s, err := p.env.Store.LoginSessions().Get(ctx, hash)
	if err != nil {
		return err
	}
	if s.RequiredUserID != nil && *s.RequiredUserID != u.ID {
		return fmt.Errorf("session belongs to user %d", *s.RequiredUserID)
	}
	raw := zotero.NewAPIKey()
	if _, err := p.env.Store.Keys().Create(ctx, repo.APIKey{
		UserID: u.ID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: "Zotero", Access: accounts.FullAccess(),
	}); err != nil {
		return err
	}
	return p.env.Store.LoginSessions().Complete(ctx, hash, u.ID, raw)
}

func TestConformance(t *testing.T) {
	env := newEnv(t)
	conformance.Run(t, conformance.Config{
		BaseURL:     env.URL + "/api/",
		Provisioner: &provisioner{env: env},
		Suites:      []string{"auth", "groups", "write", "read", "deleted", "settings", "tags", "fulltext", "files", "sessions", "quirks"},
	})
}
