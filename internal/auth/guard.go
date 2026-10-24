package auth

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Login guard limits. Attempts are counted per (username, source) pair and
// per source, where a source is an IPv4 address or an IPv6 /64: a single
// host usually holds a whole /64 and can rotate through it freely. IPv6
// /48s get a wider budget on top, since that is what one site or customer
// is typically given. There is deliberately no per-username limit across
// all sources: that would let anyone lock the real user (typically "admin")
// out by guessing from elsewhere.
const (
	maxPairAttempts = 10
	maxIPAttempts   = 50
	maxNetAttempts  = 200
	attemptWindow   = 15 * time.Minute
	maxGuardKeys    = 100_000
)

type guardKey struct {
	key   string
	limit int
}

// sources returns the per-host source of ip (an IPv4 address or IPv6 /64)
// and, for IPv6, the wider /48 site prefix.
func sources(ip string) (src, wide string) {
	src = ip
	if a, err := netip.ParseAddr(ip); err == nil {
		a = a.Unmap().WithZone("")
		src = a.String()
		if a.Is6() {
			src = netip.PrefixFrom(a, 64).Masked().String()
			wide = netip.PrefixFrom(a, 48).Masked().String()
		}
	}
	return src, wide
}

// SourceKey is the rate limit bucket for a client address: the address
// itself for IPv4, its /64 for IPv6, since one host can rotate through a
// whole /64 at will.
func SourceKey(ip string) string {
	src, _ := sources(ip)
	return src
}

// guardKeys names the counters one attempt draws from.
func guardKeys(username, ip string) []guardKey {
	src, wide := sources(ip)
	keys := []guardKey{{"u:" + username + "\x00" + src, maxPairAttempts}, {"ip:" + src, maxIPAttempts}}
	if wide != "" {
		keys = append(keys, guardKey{"net:" + wide, maxNetAttempts})
	}
	return keys
}

// LoginGuard throttles password logins and caps how many argon2 checks run
// at once. Both matter: argon2 costs ~19 MiB per verification, so unbounded
// parallel logins are a memory exhaustion vector even when rate limits hold.
type LoginGuard struct {
	now func() time.Time
	sem chan struct{}

	mu       sync.Mutex
	attempts map[string][]time.Time
	swept    time.Time
}

// NewLoginGuard allows maxConcurrent password verifications in parallel.
func NewLoginGuard(maxConcurrent int) *LoginGuard {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &LoginGuard{now: time.Now, sem: make(chan struct{}, maxConcurrent), attempts: map[string][]time.Time{}}
}

// SetClock replaces the time source, for tests.
func (g *LoginGuard) SetClock(now func() time.Time) { g.now = now }

// Attempt reserves one login attempt before the password is checked, so a
// burst of parallel requests cannot all slip in before the first failure is
// recorded. It returns how long to wait when the attempt is refused.
func (g *LoginGuard) Attempt(username, ip string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.sweep(now)
	keys := guardKeys(username, ip)
	for _, k := range keys {
		ts := g.live(k.key, now)
		if len(ts) >= k.limit {
			return ts[len(ts)-k.limit].Add(attemptWindow).Sub(now), false
		}
	}
	for _, k := range keys {
		if _, ok := g.attempts[k.key]; !ok && len(g.attempts) >= maxGuardKeys {
			g.evict(now, keys)
		}
		g.attempts[k.key] = append(g.attempts[k.key], now)
	}
	return 0, true
}

// evict makes room for a new key. Refusing new keys instead would let a
// spray from many sources lock out everyone else. A full table consists
// mostly of the sprayer's own keys, so dropping a random one rarely helps
// anyone but them, and each source's budget still bounds what they gain.
// keep are the counters of the attempt being recorded; dropping one of them
// would forget part of this very attempt.
func (g *LoginGuard) evict(now time.Time, keep []guardKey) {
	// Sweeping early frees expired keys before live ones get dropped, but a
	// spray refills the table at once, so a forced full scan on every
	// eviction would serialize all logins behind it. Cap it at once a second.
	if now.Sub(g.swept) >= time.Second {
		g.swept = time.Time{}
		g.sweep(now)
	}
	for len(g.attempts) >= maxGuardKeys {
		for k := range g.attempts {
			if slices.ContainsFunc(keep, func(gk guardKey) bool { return gk.key == k }) {
				continue
			}
			delete(g.attempts, k)
			break
		}
	}
}

// Succeeded forgives the pair's attempts after a correct password and
// refunds this attempt's reservation on the IP budget, so only failures
// count there. Otherwise an office behind one NAT address would be locked
// out after 50 ordinary logins. Earlier failures from the IP stay counted,
// so one valid account cannot launder guesses for others.
func (g *LoginGuard) Succeeded(username, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := guardKeys(username, ip)
	delete(g.attempts, keys[0].key)
	for _, k := range keys[1:] {
		if ts := g.attempts[k.key]; len(ts) > 0 {
			if len(ts) == 1 {
				delete(g.attempts, k.key)
			} else {
				g.attempts[k.key] = ts[:len(ts)-1]
			}
		}
	}
}

// Verify checks a password under the concurrency cap. Unknown users (empty
// hash) still cost the same, so timing does not reveal account existence.
func (g *LoginGuard) Verify(ctx context.Context, hash, password string) (bool, error) {
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-g.sem }()
	if hash == "" {
		VerifyNoUser(password)
		return false, nil
	}
	return VerifyPassword(hash, password)
}

func (g *LoginGuard) live(key string, now time.Time) []time.Time {
	ts := g.attempts[key]
	cut := now.Add(-attemptWindow)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	if i == len(ts) {
		delete(g.attempts, key)
		return nil
	}
	ts = ts[i:]
	g.attempts[key] = ts
	return ts
}

func (g *LoginGuard) sweep(now time.Time) {
	if now.Sub(g.swept) < time.Minute {
		return
	}
	g.swept = now
	for k := range g.attempts {
		g.live(k, now)
	}
}
