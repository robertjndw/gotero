package accounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/sso"
)

// ExternalIdentity is what an OIDC or SAML provider asserted about a person.
// Subject is the provider's stable identifier; everything else is a hint.
type ExternalIdentity struct {
	Provider string
	sso.Identity
}

// ExternalPolicy is the per-provider configuration that decides what an
// unknown identity may do.
type ExternalPolicy struct {
	AutoProvision bool
	LinkByEmail   bool
}

type ExternalLogin struct {
	Identity ExternalIdentity
	Policy   ExternalPolicy
	// InviteHash is the hash of an invite token carried through the flow.
	InviteHash []byte
}

const (
	// maxUsernameBase leaves room for a "-xxxxxxxx" suffix within the 64
	// character username limit.
	maxUsernameBase = 54
	// numberedUsernames is the highest "-N" suffix tried before switching to
	// random ones.
	numberedUsernames = 20
	randomUsernames   = 10
)

// LoginExternal resolves an identity to a user: an existing link, a verified
// email match, an invite, auto-provisioning, in that order. It never sets or
// changes is_admin; only an invite can grant it, because gotero issued that.
func (s *Service) LoginExternal(ctx context.Context, in ExternalLogin) (repo.User, error) {
	id := in.Identity
	if id.Provider == "" || id.Subject == "" {
		return repo.User{}, errors.New("accounts: external identity needs a provider and subject")
	}
	if ValidateEmail(id.Email) != nil {
		id.Email = ""
	}
	// An unverified address must not end up on an account: email linking
	// would later treat it as proof of ownership.
	id.EmailVerified = id.Email != "" && id.EmailVerified

	if u, ok, err := s.loginLinked(ctx, id); ok || err != nil {
		return u, err
	}

	if in.Policy.LinkByEmail && id.EmailVerified {
		u, err := s.store.Users().GetByEmail(ctx, id.Email)
		switch {
		case err == nil:
			return s.linkByEmail(ctx, u, id)
		case !errors.Is(err, repo.ErrNotFound):
			return repo.User{}, err
		}
	}

	var inv repo.Invite
	switch {
	case in.InviteHash != nil:
		var err error
		if inv, err = s.validInviteHash(ctx, in.InviteHash); err != nil {
			return repo.User{}, err
		}
		if inv.Email != "" && (!id.EmailVerified || !strings.EqualFold(inv.Email, id.Email)) {
			return repo.User{}, ErrInviteEmailMismatch
		}
	case !in.Policy.AutoProvision:
		return repo.User{}, ErrNoAccount
	}
	return s.provisionExternal(ctx, id, inv)
}

// loginLinked handles the returning user. ok is false when the identity is
// not linked yet.
func (s *Service) loginLinked(ctx context.Context, id ExternalIdentity) (u repo.User, ok bool, err error) {
	ident, err := s.store.Identities().Get(ctx, id.Provider, id.Subject)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.User{}, false, nil
	}
	if err != nil {
		return repo.User{}, false, err
	}
	u, err = s.store.Users().Get(ctx, ident.UserID)
	if err != nil {
		return repo.User{}, false, err
	}
	if u.Disabled {
		return repo.User{}, false, ErrDisabled
	}
	if err := s.store.Identities().Touch(ctx, ident.ID, id.Email, s.now()); err != nil {
		return repo.User{}, false, err
	}
	return u, true, nil
}

func (s *Service) linkByEmail(ctx context.Context, u repo.User, id ExternalIdentity) (repo.User, error) {
	// Administrators link explicitly from their account page, so a compromised
	// or sloppy provider can never take over an admin by asserting its email.
	if u.IsAdmin {
		return repo.User{}, ErrLinkRequired
	}
	if u.Disabled {
		return repo.User{}, ErrDisabled
	}
	// Anyone can put any address on their own account. Linking to one
	// nobody checked would hand that account the real owner's first login.
	if !u.EmailVerified {
		return repo.User{}, ErrLinkRequired
	}
	now := s.now()
	_, err := s.store.Identities().Create(ctx, repo.Identity{
		UserID: u.ID, Provider: id.Provider, Subject: id.Subject, Email: id.Email, LastLoginAt: &now,
	})
	if errors.Is(err, repo.ErrConflict) {
		// Usually the subject changed for a provider the user is already
		// linked to. Silently re-linking would hide that, so a person has to
		// decide.
		return repo.User{}, ErrLinkRequired
	}
	if err != nil {
		return repo.User{}, err
	}
	return u, nil
}

// provisionExternal creates the account. The username is chosen by trying
// candidates against the insert itself, not by checking first, so two first
// logins racing for the same name cannot both pass a pre-check.
func (s *Service) provisionExternal(ctx context.Context, id ExternalIdentity, inv repo.Invite) (repo.User, error) {
	quota, err := s.DefaultQuota(ctx)
	if err != nil {
		return repo.User{}, err
	}
	u := repo.User{IsAdmin: inv.IsAdmin, QuotaBytes: quota}
	if id.EmailVerified {
		u.Email, u.EmailVerified = id.Email, true
	}
	ident := repo.Identity{Provider: id.Provider, Subject: id.Subject, Email: id.Email}
	name := deriveUsername(id)

	for candidate := range usernameCandidates(name) {
		u.Username = candidate
		u.DisplayName = id.DisplayName
		if u.DisplayName == "" || validateName(u.DisplayName) != nil {
			u.DisplayName = candidate
		}
		created, err := s.store.Users().CreateExternal(ctx, u, ident, inv.ID, s.now())
		switch {
		case err == nil:
			return created, nil
		case errors.Is(err, repo.ErrUsernameTaken):
			continue
		case errors.Is(err, repo.ErrIdentityLinked):
			// A concurrent first login for the same identity won.
			u, ok, err := s.loginLinked(ctx, id)
			if err != nil {
				return repo.User{}, err
			}
			if ok {
				return u, nil
			}
			return repo.User{}, ErrLinkRequired
		case errors.Is(err, repo.ErrInviteUnavailable):
			return repo.User{}, ErrInvalidInvite
		case errors.Is(err, repo.ErrConflict):
			// The email belongs to another account.
			return repo.User{}, ErrLinkRequired
		default:
			return repo.User{}, err
		}
	}
	return repo.User{}, fmt.Errorf("accounts: no free username for %q", name)
}

// deriveUsername picks the first source that sanitises to a valid username.
func deriveUsername(id ExternalIdentity) string {
	local, _, _ := strings.Cut(id.Email, "@")
	for _, src := range []string{id.Username, local, id.DisplayName} {
		if n := sanitizeUsername(src); n != "" {
			return n
		}
	}
	return "user"
}

// sanitizeUsername maps a free-form string onto what ValidateUsername
// accepts, or returns "" when nothing usable is left.
func sanitizeUsername(src string) string {
	var b strings.Builder
	dash := false
	for _, r := range src {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			// Collapse runs of anything else, including '-', into one dash.
			if !dash {
				b.WriteByte('-')
			}
			dash = true
		}
	}
	n := strings.Trim(b.String(), "-._")
	if len(n) > maxUsernameBase {
		n = strings.TrimRight(n[:maxUsernameBase], "-._")
	}
	if ValidateUsername(n) != nil {
		return ""
	}
	return n
}

// usernameCandidates yields the name, then name-2 to name-20, then random
// suffixes.
func usernameCandidates(base string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if !yield(base) {
			return
		}
		for i := 2; i <= numberedUsernames; i++ {
			if !yield(fmt.Sprintf("%s-%d", base, i)) {
				return
			}
		}
		for range randomUsernames {
			var b [4]byte
			_, _ = rand.Read(b[:])
			if !yield(base + "-" + hex.EncodeToString(b[:])) {
				return
			}
		}
	}
}

// LinkExternal attaches an identity to a signed-in user. It is the only path
// that may link an administrator.
func (s *Service) LinkExternal(ctx context.Context, userID int64, id ExternalIdentity) error {
	u, err := s.store.Users().Get(ctx, userID)
	if err != nil {
		return err
	}
	if u.Disabled {
		return ErrDisabled
	}
	if ValidateEmail(id.Email) != nil || !id.EmailVerified {
		id.Email = ""
	}
	existing, err := s.store.Identities().Get(ctx, id.Provider, id.Subject)
	switch {
	case err == nil:
		if existing.UserID != userID {
			return ErrIdentityInUse
		}
		return nil
	case !errors.Is(err, repo.ErrNotFound):
		return err
	}
	now := s.now()
	_, err = s.store.Identities().Create(ctx, repo.Identity{
		UserID: userID, Provider: id.Provider, Subject: id.Subject, Email: id.Email, LastLoginAt: &now,
	})
	if errors.Is(err, repo.ErrConflict) {
		// Either someone linked the identity meanwhile or the user already
		// has another account at this provider.
		return ErrIdentityInUse
	}
	return err
}

// UnlinkIdentity removes a link unless that would leave the user with no way
// to sign in.
func (s *Service) UnlinkIdentity(ctx context.Context, userID int64, provider string) error {
	u, err := s.store.Users().Get(ctx, userID)
	if err != nil {
		return err
	}
	idents, err := s.store.Identities().ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	found, others := false, 0
	for _, i := range idents {
		switch {
		case i.Provider == provider:
			found = true
		case s.providerConfigured(i.Provider):
			// A link to a provider that was removed from the config is still
			// listed, but nobody can sign in through it anymore.
			others++
		}
	}
	if !found {
		return repo.ErrNotFound
	}
	if others == 0 && (u.PasswordHash == "" || !s.LocalLoginAllowed(u)) {
		return ErrLastLoginMethod
	}
	return s.store.Identities().Delete(ctx, userID, provider)
}

func (s *Service) providerConfigured(name string) bool {
	for _, p := range s.cfg.OIDC {
		if p.Name == name {
			return true
		}
	}
	for _, p := range s.cfg.SAML {
		if p.Name == name {
			return true
		}
	}
	return false
}
