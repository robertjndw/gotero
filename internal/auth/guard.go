package auth

import (
	"context"
	"sync"
	"time"
)

// Login guard limits. Attempts are counted per (username, IP) pair and per
// IP. There is deliberately no per-username limit across all IPs: that would
// let anyone lock the real user (typically "admin") out by guessing from
// elsewhere.
const (
	maxPairAttempts = 10
	maxIPAttempts   = 50
	attemptWindow   = 15 * time.Minute
	maxGuardKeys    = 100_000
)

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
	pair, ipKey := "u:"+username+"\x00"+ip, "ip:"+ip
	for key, limit := range map[string]int{pair: maxPairAttempts, ipKey: maxIPAttempts} {
		ts := g.live(key, now)
		if len(ts) >= limit {
			return ts[len(ts)-limit].Add(attemptWindow).Sub(now), false
		}
	}
	for _, key := range []string{pair, ipKey} {
		if _, ok := g.attempts[key]; !ok && len(g.attempts) >= maxGuardKeys {
			// Under a spray of fresh keys, fail closed for new keys rather
			// than grow without bound.
			return time.Minute, false
		}
		g.attempts[key] = append(g.attempts[key], now)
	}
	return 0, true
}

// Succeeded forgives the pair's attempts after a correct password and
// refunds this attempt's reservation on the IP budget, so only failures
// count there. Otherwise an office behind one NAT address would be locked
// out after 50 ordinary logins. Earlier failures from the IP stay counted,
// so one valid account cannot launder guesses for others.
func (g *LoginGuard) Succeeded(username, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.attempts, "u:"+username+"\x00"+ip)
	key := "ip:" + ip
	if ts := g.attempts[key]; len(ts) > 0 {
		if len(ts) == 1 {
			delete(g.attempts, key)
		} else {
			g.attempts[key] = ts[:len(ts)-1]
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
