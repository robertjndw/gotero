package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
)

// ErrNoCredentials means the request carried no API key at all, as opposed
// to an invalid one. zotero.org answers both with 403, but logging them
// differently helps when debugging client setups.
var (
	ErrNoCredentials = errors.New("auth: no API key")
	ErrInvalidKey    = errors.New("auth: invalid API key")
)

// Principal is an authenticated API key and its owner.
type Principal struct {
	Key  repo.APIKey
	User repo.User
	// Raw is the key as sent by the client; keys/current echoes it back.
	Raw string
}

// KeyFromRequest extracts an API key the way zotero.org accepts it: the
// Zotero-API-Key header, a Bearer token, or the key query parameter.
func KeyFromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("Zotero-API-Key")); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.URL.Query().Get("key")
}

// Authenticator resolves API keys to principals. Sync clients send dozens
// of requests per sync, so lookups are cached briefly. Every code path that
// revokes a key or disables a user must call Invalidate/InvalidateUser,
// which keeps revocation immediate on this (single) instance.
type Authenticator struct {
	keys  repo.KeyRepo
	users repo.UserRepo
	ttl   time.Duration
	now   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedPrincipal // by string(hash)
}

type cachedPrincipal struct {
	p       Principal
	expires time.Time
}

// maxCacheEntries bounds memory if someone sprays random keys at us; only
// valid keys are cached, so this is generous.
const maxCacheEntries = 10_000

func NewAuthenticator(keys repo.KeyRepo, users repo.UserRepo) *Authenticator {
	return &Authenticator{keys: keys, users: users, ttl: 30 * time.Second, now: time.Now, cache: map[string]cachedPrincipal{}}
}

// Authenticate looks up raw. It returns ErrInvalidKey for unknown keys and
// keys of disabled users.
func (a *Authenticator) Authenticate(ctx context.Context, raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, ErrNoCredentials
	}
	// Reject obviously malformed input before touching the database.
	if len(raw) > 128 {
		return Principal{}, ErrInvalidKey
	}
	hash := HashToken(raw)
	now := a.now()
	a.mu.Lock()
	if c, ok := a.cache[string(hash)]; ok && now.Before(c.expires) {
		a.mu.Unlock()
		c.p.Raw = raw
		return c.p, nil
	}
	a.mu.Unlock()

	key, err := a.keys.GetByHash(ctx, hash)
	if errors.Is(err, repo.ErrNotFound) {
		return Principal{}, ErrInvalidKey
	}
	if err != nil {
		return Principal{}, err
	}
	user, err := a.users.Get(ctx, key.UserID)
	if errors.Is(err, repo.ErrNotFound) {
		return Principal{}, ErrInvalidKey
	}
	if err != nil {
		return Principal{}, err
	}
	if user.Disabled {
		return Principal{}, ErrInvalidKey
	}
	_ = a.keys.Touch(ctx, key.ID, now) // best effort, coalesced by the repo

	p := Principal{Key: key, User: user, Raw: raw}
	a.mu.Lock()
	if len(a.cache) >= maxCacheEntries {
		clear(a.cache)
	}
	a.cache[string(hash)] = cachedPrincipal{p: p, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return p, nil
}

// Invalidate drops a cached key, by its hash.
func (a *Authenticator) Invalidate(hash []byte) {
	a.mu.Lock()
	delete(a.cache, string(hash))
	a.mu.Unlock()
}

// InvalidateUser drops every cached key of a user, e.g. after disabling the
// account, changing the password or deleting it.
func (a *Authenticator) InvalidateUser(userID int64) {
	a.mu.Lock()
	for k, c := range a.cache {
		if c.p.User.ID == userID {
			delete(a.cache, k)
		}
	}
	a.mu.Unlock()
}
