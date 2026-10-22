// Package accounts holds the account rules shared by the admin API and the
// CLI: user creation and validation, invites, registration policy, quotas
// and API key issuing. Keeping them here means "gotero user add" and the web
// UI can never disagree about what a valid user is.
package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo"
)

const (
	settingOpenRegistration = "open_registration"
	settingDefaultQuota     = "default_quota_bytes"

	// maxPasswordLength keeps argon2 from being fed megabytes of input.
	maxPasswordLength = 1024
	maxNameLength     = 200
)

var (
	ErrRegistrationClosed = errors.New("accounts: registration is closed")
	ErrInvalidInvite      = errors.New("accounts: invite is invalid, used or expired")
	ErrLastAdmin          = errors.New("accounts: cannot remove the last administrator")
	ErrSelfModification   = errors.New("accounts: administrators cannot demote, disable or delete themselves")
	ErrOwnsGroups         = errors.New("accounts: user still owns groups")
	ErrBadCredentials     = errors.New("accounts: bad credentials")
	ErrDisabled           = errors.New("accounts: account is disabled")

	ErrLocalLoginDisabled = errors.New("accounts: password sign-in is disabled")
	ErrNoAccount          = errors.New("accounts: no account for this identity")
	// ErrLinkRequired deliberately covers both "the email belongs to an
	// administrator" and "the email is taken by an account we cannot link", so
	// it cannot be used to find out which addresses are admins.
	ErrLinkRequired        = errors.New("accounts: an account with this email exists; sign in to it and connect the provider")
	ErrIdentityInUse       = errors.New("accounts: identity is already linked")
	ErrLastLoginMethod     = errors.New("accounts: this is the last way to sign in")
	ErrInviteEmailMismatch = errors.New("accounts: invite was issued for a different email address")
)

// ValidationError carries per-field messages for 422 responses.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for k, v := range e.Fields {
		parts = append(parts, k+": "+v)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// ConflictError is a uniqueness clash (username or email) with the field it
// concerns, so the UI can mark the right input.
type ConflictError struct{ Fields map[string]string }

func (e *ConflictError) Error() string { return "conflict" }
func (e *ConflictError) Unwrap() error { return repo.ErrConflict }

func fieldErr(field, msg string) *ValidationError {
	return &ValidationError{Fields: map[string]string{field: msg}}
}

// Service bundles the dependencies of the account operations.
type Service struct {
	store repo.Store
	auth  *auth.Authenticator // may be nil in tools that do not serve API keys
	guard *auth.LoginGuard
	// adminMu makes "is there another admin?" and the change that depends on
	// it atomic. Two admins demoting each other at the same moment would
	// otherwise both pass the check and leave the instance without an admin.
	// A process-local lock is enough because gotero runs as one instance.
	adminMu sync.Mutex
	cfg     *config.Config
	log     *slog.Logger
	now     func() time.Time
}

// RateLimitedError is returned by Login when too many attempts were made.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "accounts: too many login attempts" }

func New(store repo.Store, a *auth.Authenticator, cfg *config.Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, auth: a, guard: auth.NewLoginGuard(4), cfg: cfg, log: log, now: time.Now}
}

// SetClock replaces the time source, for tests.
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
	s.guard.SetClock(now)
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}$`)

func ValidateUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return fieldErr("username", "Use 2 to 64 letters, digits, dots, dashes or underscores.")
	}
	return nil
}

func ValidatePassword(p string) error {
	if len(p) < auth.MinPasswordLength {
		return fieldErr("password", fmt.Sprintf("Use at least %d characters.", auth.MinPasswordLength))
	}
	if len(p) > maxPasswordLength {
		return fieldErr("password", "Password is too long.")
	}
	return nil
}

func ValidateEmail(e string) error {
	if e == "" {
		return nil
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return fieldErr("email", "Enter a valid email address.")
	}
	return nil
}

func mergeValidation(errs ...error) error {
	out := map[string]string{}
	for _, err := range errs {
		var ve *ValidationError
		if errors.As(err, &ve) {
			for k, v := range ve.Fields {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &ValidationError{Fields: out}
}

// Optional distinguishes "field absent" from an explicit null, which matters
// for quotas: absent means "use the default", null means unlimited.
type Optional[T any] struct {
	Set   bool
	Value *T
}

func Some[T any](v T) Optional[T] { return Optional[T]{Set: true, Value: &v} }
func Null[T any]() Optional[T]    { return Optional[T]{Set: true} }

type CreateUserInput struct {
	// ID is only for migrating accounts from zotero.org; zero allocates one.
	ID          int64
	Username    string
	Password    string
	Email       string
	DisplayName string
	IsAdmin     bool
	Quota       Optional[int64]
}

func (in CreateUserInput) validate() error {
	return mergeValidation(ValidateUsername(in.Username), ValidatePassword(in.Password), ValidateEmail(in.Email),
		validateName(in.DisplayName), validateQuota(in.Quota))
}

func validateName(n string) error {
	if len(n) > maxNameLength {
		return fieldErr("displayName", "Name is too long.")
	}
	return nil
}

func validateQuota(q Optional[int64]) error {
	if q.Set && q.Value != nil && *q.Value < 0 {
		return fieldErr("quotaBytes", "Quota cannot be negative.")
	}
	return nil
}

func (s *Service) buildUser(ctx context.Context, in CreateUserInput) (repo.User, error) {
	if err := in.validate(); err != nil {
		return repo.User{}, err
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return repo.User{}, err
	}
	quota := in.Quota.Value
	if !in.Quota.Set {
		if quota, err = s.DefaultQuota(ctx); err != nil {
			return repo.User{}, err
		}
	}
	name := in.DisplayName
	if name == "" {
		name = in.Username
	}
	return repo.User{
		ID: in.ID, Username: in.Username, DisplayName: name, Email: in.Email,
		PasswordHash: hash, IsAdmin: in.IsAdmin, QuotaBytes: quota,
	}, nil
}

// CreateUser validates and creates an account.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (repo.User, error) {
	u, err := s.buildUser(ctx, in)
	if err != nil {
		return repo.User{}, err
	}
	created, err := s.store.Users().Create(ctx, u)
	return created, s.mapCreateErr(ctx, u, err)
}

// mapCreateErr names the clashing field. The repo only reports a unique
// violation, so we look the username up to tell the two apart.
func (s *Service) mapCreateErr(ctx context.Context, u repo.User, err error) error {
	if !errors.Is(err, repo.ErrConflict) {
		return err
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		return err
	}
	if _, gerr := s.store.Users().GetByUsername(ctx, u.Username); gerr == nil {
		return &ConflictError{Fields: map[string]string{"username": "That username is already taken."}}
	}
	if u.ID != 0 {
		if _, gerr := s.store.Users().Get(ctx, u.ID); gerr == nil {
			return &ConflictError{Fields: map[string]string{"id": "That user ID is already in use."}}
		}
	}
	return &ConflictError{Fields: map[string]string{"email": "That email address is already in use."}}
}

// EnsureBootstrapAdmin creates the configured admin when none exists. It is
// safe to call on every start.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context, c config.AdminBootstrap) error {
	if c.Username == "" || c.Password == "" {
		return nil
	}
	n, err := s.store.Users().CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	existing, err := s.store.Users().GetByUsername(ctx, c.Username)
	switch {
	case err == nil:
		// Promoting an existing account behind the operator's back would be a
		// surprising privilege change, so only warn.
		s.log.Warn("bootstrap admin skipped: user exists but is not an administrator", "username", existing.Username)
		return nil
	case !errors.Is(err, repo.ErrNotFound):
		return err
	}
	u, err := s.CreateUser(ctx, CreateUserInput{
		Username: c.Username, Password: c.Password, Email: c.Email, IsAdmin: true, Quota: Null[int64](),
	})
	if err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	s.log.Info("created bootstrap admin", "username", u.Username)
	return nil
}

// Authenticate verifies credentials with uniform timing for unknown users.
func (s *Service) Authenticate(ctx context.Context, username, password string) (repo.User, error) {
	u, err := s.authenticate(ctx, username, password)
	if err != nil {
		return repo.User{}, err
	}
	return u, nil
}

// authenticate also returns the user alongside ErrDisabled, so Login can
// apply the local-login policy before telling the caller anything.
func (s *Service) authenticate(ctx context.Context, username, password string) (repo.User, error) {
	u, err := s.store.Users().GetByUsername(ctx, username)
	if errors.Is(err, repo.ErrNotFound) {
		_, _ = s.guard.Verify(ctx, "", password)
		return repo.User{}, ErrBadCredentials
	}
	if err != nil {
		return repo.User{}, err
	}
	ok, err := s.guard.Verify(ctx, u.PasswordHash, password)
	if err != nil || !ok {
		return repo.User{}, ErrBadCredentials
	}
	if u.Disabled {
		return u, ErrDisabled
	}
	return u, nil
}

// LocalLoginAllowed reports whether the user may sign in with a password.
// Administrators always may, so the instance cannot lock itself out.
func (s *Service) LocalLoginAllowed(u repo.User) bool { return s.cfg.LocalLogin || u.IsAdmin }

// Login is Authenticate for requests from the network: it is rate limited
// per (username, client IP) and per client IP.
func (s *Service) Login(ctx context.Context, username, password, ip string) (repo.User, error) {
	name := strings.ToLower(username)
	if wait, ok := s.guard.Attempt(name, ip); !ok {
		return repo.User{}, &RateLimitedError{RetryAfter: wait}
	}
	u, err := s.authenticate(ctx, username, password)
	if err == nil || errors.Is(err, ErrDisabled) {
		// The password is only verified after the check above, so a correct
		// password for an account that may not use one looks like a wrong one.
		if !s.LocalLoginAllowed(u) {
			return repo.User{}, ErrBadCredentials
		}
		s.guard.Succeeded(name, ip)
	}
	if err != nil {
		return repo.User{}, err
	}
	return u, nil
}

type UserPatch struct {
	DisplayName *string
	Email       *string
	IsAdmin     *bool
	Disabled    *bool
	Quota       Optional[int64]
	Password    *string
}

// UpdateUser applies a partial update. actorID is the administrator making
// the change, or 0 for the CLI, which may act on any account.
func (s *Service) UpdateUser(ctx context.Context, id int64, p UserPatch, actorID int64) (repo.User, error) {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, err := s.store.Users().Get(ctx, id)
	if err != nil {
		return repo.User{}, err
	}
	var errs []error
	if p.Email != nil {
		errs = append(errs, ValidateEmail(*p.Email))
	}
	if p.DisplayName != nil {
		errs = append(errs, validateName(*p.DisplayName))
	}
	if p.Password != nil {
		errs = append(errs, ValidatePassword(*p.Password))
	}
	errs = append(errs, validateQuota(p.Quota))
	if err := mergeValidation(errs...); err != nil {
		return repo.User{}, err
	}

	demoting := p.IsAdmin != nil && !*p.IsAdmin && u.IsAdmin
	disabling := p.Disabled != nil && *p.Disabled && !u.Disabled
	if actorID != 0 && actorID == id && (demoting || disabling) {
		return repo.User{}, ErrSelfModification
	}
	if (demoting || disabling) && u.IsAdmin && !u.Disabled {
		if err := s.requireOtherAdmin(ctx); err != nil {
			return repo.User{}, err
		}
	}

	if p.DisplayName != nil {
		u.DisplayName = *p.DisplayName
	}
	if p.Email != nil {
		u.Email = *p.Email
	}
	if p.IsAdmin != nil {
		u.IsAdmin = *p.IsAdmin
	}
	if p.Disabled != nil {
		u.Disabled = *p.Disabled
	}
	if p.Quota.Set {
		u.QuotaBytes = p.Quota.Value
	}
	if p.Password != nil {
		if u.PasswordHash, err = auth.HashPassword(*p.Password); err != nil {
			return repo.User{}, err
		}
	}
	out, err := s.store.Users().Update(ctx, u)
	if err != nil {
		if errors.Is(err, repo.ErrConflict) {
			// The username never changes here, so only the email can clash.
			return repo.User{}, &ConflictError{Fields: map[string]string{"email": "That email address is already in use."}}
		}
		return repo.User{}, err
	}
	// Cached keys outlive a password change or disable by up to the cache TTL,
	// and web sessions would stay valid indefinitely, so cut both now.
	if p.Password != nil || disabling {
		s.dropCredentials(ctx, id)
	}
	return out, nil
}

func (s *Service) dropCredentials(ctx context.Context, id int64) {
	if s.auth != nil {
		s.auth.InvalidateUser(id)
	}
	if err := s.store.WebSessions().DeleteForUser(ctx, id); err != nil {
		s.log.Warn("could not delete web sessions", "user", id, "err", err)
	}
}

// SetPassword changes a password after validating it. Every web session and
// cached API key of the user is dropped, so callers that want to keep the
// user logged in must issue a fresh session afterwards.
func (s *Service) SetPassword(ctx context.Context, id int64, password string) error {
	_, err := s.UpdateUser(ctx, id, UserPatch{Password: &password}, 0)
	return err
}

// requireOtherAdmin fails when the acting target is the only active admin.
func (s *Service) requireOtherAdmin(ctx context.Context) error {
	n, err := s.store.Users().CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// DeleteUser removes the account together with its library.
func (s *Service) DeleteUser(ctx context.Context, id, actorID int64) error {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, err := s.store.Users().Get(ctx, id)
	if err != nil {
		return err
	}
	if actorID != 0 && actorID == id {
		return ErrSelfModification
	}
	if u.IsAdmin && !u.Disabled {
		if err := s.requireOtherAdmin(ctx); err != nil {
			return err
		}
	}
	if err := s.store.Users().Delete(ctx, id); err != nil {
		if errors.Is(err, repo.ErrConflict) {
			return ErrOwnsGroups
		}
		return err
	}
	if s.auth != nil {
		s.auth.InvalidateUser(id)
	}
	return nil
}

// Register creates an account from the public signup form. With an invite
// token the invite is consumed atomically with the user.
func (s *Service) Register(ctx context.Context, in CreateUserInput, inviteToken string) (repo.User, error) {
	if !s.cfg.LocalLogin {
		return repo.User{}, ErrLocalLoginDisabled
	}
	if inviteToken == "" {
		open, err := s.OpenRegistration(ctx)
		if err != nil {
			return repo.User{}, err
		}
		if !open {
			return repo.User{}, ErrRegistrationClosed
		}
		in.IsAdmin = false
		in.ID = 0
		in.Quota = Optional[int64]{}
		return s.CreateUser(ctx, in)
	}

	inv, err := s.validInvite(ctx, inviteToken)
	if err != nil {
		return repo.User{}, err
	}
	if inv.Email != "" {
		if in.Email != "" && !strings.EqualFold(in.Email, inv.Email) {
			return repo.User{}, fieldErr("email", "This invitation was issued for a different email address.")
		}
		in.Email = inv.Email
	}
	in.IsAdmin = inv.IsAdmin
	in.ID = 0
	in.Quota = Optional[int64]{}
	u, err := s.buildUser(ctx, in)
	if err != nil {
		return repo.User{}, err
	}
	created, err := s.store.Users().CreateWithInvite(ctx, u, inv.ID, s.now())
	if err != nil {
		if errors.Is(err, repo.ErrConflict) {
			// Either the username/email clashed or somebody used the invite
			// between our check and the insert.
			if _, gerr := s.validInvite(ctx, inviteToken); gerr != nil {
				return repo.User{}, ErrInvalidInvite
			}
		}
		return repo.User{}, s.mapCreateErr(ctx, u, err)
	}
	return created, nil
}

func (s *Service) validInvite(ctx context.Context, token string) (repo.Invite, error) {
	return s.validInviteHash(ctx, auth.HashToken(token))
}

// validInviteHash is validInvite for flows that only carry the token hash,
// like SSO state stored across the redirect to the provider.
func (s *Service) validInviteHash(ctx context.Context, hash []byte) (repo.Invite, error) {
	inv, err := s.store.Invites().GetByHash(ctx, hash)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Invite{}, ErrInvalidInvite
	}
	if err != nil {
		return repo.Invite{}, err
	}
	if inv.UsedAt != nil || inv.UsedBy != nil || !inv.ExpiresAt.After(s.now()) {
		return repo.Invite{}, ErrInvalidInvite
	}
	return inv, nil
}

// LookupInvite reports whether a token can still be redeemed.
func (s *Service) LookupInvite(ctx context.Context, token string) (repo.Invite, bool, error) {
	inv, err := s.validInvite(ctx, token)
	if errors.Is(err, ErrInvalidInvite) {
		return repo.Invite{}, false, nil
	}
	return inv, err == nil, err
}

type CreateInviteInput struct {
	CreatedBy *int64
	Email     string
	IsAdmin   bool
	TTL       time.Duration
}

// MaxInviteTTL stops absurd lifetimes from ever being stored.
const MaxInviteTTL = 365 * 24 * time.Hour

// InviteToken derives the invite's secret from the stored row with an HMAC
// over the instance secret. Only the token's hash is persisted, yet the
// admin list has to show a copyable link for every open invite, so the token
// must be recomputable from the row. The HMAC keeps it unguessable for anyone
// without GOTERO_SECRET. Rotating that secret invalidates the displayed links
// of existing invites but not their stored hashes.
func (s *Service) InviteToken(inv repo.Invite) string {
	var by int64
	if inv.CreatedBy != nil {
		by = *inv.CreatedBy
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.Secret))
	_, _ = fmt.Fprintf(mac, "invite|%d|%d|%s|%t", inv.ExpiresAt.UTC().UnixMicro(), by, inv.Email, inv.IsAdmin)
	return encodeToken(mac.Sum(nil))
}

// CreateInvite returns the stored invite and its raw token.
func (s *Service) CreateInvite(ctx context.Context, in CreateInviteInput) (repo.Invite, string, error) {
	if err := ValidateEmail(in.Email); err != nil {
		return repo.Invite{}, "", err
	}
	if in.TTL <= 0 || in.TTL > MaxInviteTTL {
		return repo.Invite{}, "", fieldErr("expiresInHours", "Choose between 1 hour and 365 days.")
	}
	// Postgres keeps microseconds, and the token is derived from the expiry,
	// so truncate up front. Two invites with identical inputs in the same
	// microsecond collide on the unique hash; nudging the expiry resolves it.
	exp := s.now().Add(in.TTL).UTC().Truncate(time.Microsecond)
	for range 5 {
		probe := repo.Invite{Email: in.Email, IsAdmin: in.IsAdmin, CreatedBy: in.CreatedBy, ExpiresAt: exp}
		token := s.InviteToken(probe)
		probe.TokenHash = auth.HashToken(token)
		inv, err := s.store.Invites().Create(ctx, probe)
		if errors.Is(err, repo.ErrConflict) {
			exp = exp.Add(time.Microsecond)
			continue
		}
		return inv, token, err
	}
	return repo.Invite{}, "", repo.ErrConflict
}

// Instance settings.

func (s *Service) OpenRegistration(ctx context.Context) (bool, error) {
	var v bool
	ok, err := s.store.Settings().Get(ctx, settingOpenRegistration, &v)
	if err != nil {
		return false, err
	}
	if !ok {
		return s.cfg.OpenRegistration, nil
	}
	return v, nil
}

func (s *Service) SetOpenRegistration(ctx context.Context, v bool) error {
	return s.store.Settings().Set(ctx, settingOpenRegistration, v)
}

// DefaultQuota is nil for unlimited.
func (s *Service) DefaultQuota(ctx context.Context) (*int64, error) {
	var v *int64
	ok, err := s.store.Settings().Get(ctx, settingDefaultQuota, &v)
	if err != nil {
		return nil, err
	}
	if ok {
		return v, nil
	}
	if s.cfg.DefaultQuotaMB > 0 {
		b := s.cfg.DefaultQuotaMB << 20
		return &b, nil
	}
	return nil, nil
}

func (s *Service) SetDefaultQuota(ctx context.Context, q *int64) error {
	if q != nil && *q < 0 {
		return fieldErr("defaultQuotaBytes", "Quota cannot be negative.")
	}
	return s.store.Settings().Set(ctx, settingDefaultQuota, q)
}

// API keys.

var keyAlphabet = []byte("abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789")

// NewKeyString makes a 24 character key like zotero.org's.
func NewKeyString() (string, error) {
	out := make([]byte, 24)
	max := big.NewInt(int64(len(keyAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = keyAlphabet[n.Int64()]
	}
	return string(out), nil
}

func FullAccess() repo.KeyAccess {
	return repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"}
}

// CreateKey stores a new key and returns it with the raw value.
func (s *Service) CreateKey(ctx context.Context, userID int64, name string, access repo.KeyAccess) (repo.APIKey, string, error) {
	name = strings.TrimSpace(name)
	var errs []error
	if name == "" || len(name) > 100 {
		errs = append(errs, fieldErr("name", "Enter a name of up to 100 characters."))
	}
	switch access.Groups {
	case "all", "read", "none":
	default:
		errs = append(errs, fieldErr("access", "Group access must be all, read or none."))
	}
	if err := mergeValidation(errs...); err != nil {
		return repo.APIKey{}, "", err
	}
	raw, err := NewKeyString()
	if err != nil {
		return repo.APIKey{}, "", err
	}
	k, err := s.store.Keys().Create(ctx, repo.APIKey{
		UserID: userID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: name, Access: access,
	})
	return k, raw, err
}

// RevokeKey deletes one of the user's keys and evicts it from the cache.
func (s *Service) RevokeKey(ctx context.Context, userID, keyID int64) error {
	keys, err := s.store.Keys().ListByUser(ctx, userID)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if k.ID != keyID {
			continue
		}
		if err := s.store.Keys().Delete(ctx, userID, keyID); err != nil {
			return err
		}
		if s.auth != nil {
			s.auth.Invalidate(k.Hash)
		}
		return nil
	}
	return repo.ErrNotFound
}
