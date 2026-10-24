package auth

import (
	"context"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuardBurstIsBounded(t *testing.T) {
	g := NewLoginGuard(4)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 500 {
		wg.Go(func() {
			if _, ok := g.Attempt("admin", "203.0.113.1"); ok {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if n := allowed.Load(); n != maxPairAttempts {
		t.Fatalf("parallel burst got %d attempts, want %d", n, maxPairAttempts)
	}
}

func TestGuardNoCrossIPLockout(t *testing.T) {
	g := NewLoginGuard(1)
	for range 100 {
		g.Attempt("admin", "198.51.100.66")
	}
	if _, ok := g.Attempt("admin", "203.0.113.1"); !ok {
		t.Fatal("an attacker elsewhere locked the real admin out")
	}
}

func TestGuardIPBudgetAndWindow(t *testing.T) {
	g := NewLoginGuard(1)
	now := time.Unix(0, 0)
	g.SetClock(func() time.Time { return now })
	for i := range maxIPAttempts {
		if _, ok := g.Attempt("user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "192.0.2.1"); !ok {
			t.Fatalf("attempt %d refused early", i)
		}
	}
	wait, ok := g.Attempt("fresh", "192.0.2.1")
	if ok || wait <= 0 || wait > attemptWindow {
		t.Fatalf("IP budget not enforced: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(attemptWindow + time.Second)
	if _, ok := g.Attempt("fresh", "192.0.2.1"); !ok {
		t.Fatal("window did not expire")
	}
}

func TestGuardSucceededForgivesPair(t *testing.T) {
	g := NewLoginGuard(1)
	for range maxPairAttempts {
		g.Attempt("bob", "192.0.2.9")
	}
	g.Succeeded("bob", "192.0.2.9")
	if _, ok := g.Attempt("bob", "192.0.2.9"); !ok {
		t.Fatal("success did not reset the pair")
	}
}

func TestGuardVerifyWaitsForSlot(t *testing.T) {
	g := NewLoginGuard(2)
	hash, _ := HashPassword("pw-for-the-test")
	// Occupy both slots: a third verification must wait, and give up when
	// its context ends instead of piling up argon2 work.
	g.sem <- struct{}{}
	g.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.Verify(ctx, hash, "pw-for-the-test"); err == nil {
		t.Fatal("Verify ran without a free slot")
	}
	<-g.sem
	if ok, err := g.Verify(context.Background(), hash, "pw-for-the-test"); !ok || err != nil {
		t.Fatalf("Verify with a free slot: %v %v", ok, err)
	}
}

func TestGuardSuccessfulLoginsDoNotExhaustIP(t *testing.T) {
	g := NewLoginGuard(1)
	for i := range 3 * maxIPAttempts {
		name := "user" + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
		if _, ok := g.Attempt(name, "192.0.2.50"); !ok {
			t.Fatalf("login %d from a busy NAT address was refused", i)
		}
		g.Succeeded(name, "192.0.2.50")
	}
}

// Regression: rotating through the addresses of one IPv6 /64 gave every
// guess a fresh budget.
func TestGuardIPv6RotationSharesBudget(t *testing.T) {
	g := NewLoginGuard(1)
	allowed := 0
	for i := range 1000 {
		ip := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 15: byte(i), 14: byte(i >> 8)}).String()
		if _, ok := g.Attempt("admin", ip); ok {
			allowed++
		}
	}
	if allowed != maxPairAttempts {
		t.Fatalf("got %d guesses from one /64, want %d", allowed, maxPairAttempts)
	}
}

func TestGuardIPv6SiteBudget(t *testing.T) {
	g := NewLoginGuard(1)
	allowed := 0
	for i := range 1000 {
		// A different /64 of the same /48 each time.
		ip := netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 1, 6: byte(i >> 8), 7: byte(i), 15: 1}).String()
		if _, ok := g.Attempt("user"+strconv.Itoa(i), ip); ok {
			allowed++
		}
	}
	if allowed != maxNetAttempts {
		t.Fatalf("got %d guesses from one /48, want %d", allowed, maxNetAttempts)
	}
	if _, ok := g.Attempt("admin", "2001:db8:2::1"); !ok {
		t.Fatal("another /48 was refused")
	}
}

func TestGuardMappedIPv4IsTheSameSource(t *testing.T) {
	g := NewLoginGuard(1)
	for range maxPairAttempts {
		g.Attempt("bob", "192.0.2.7")
	}
	if _, ok := g.Attempt("bob", "::ffff:192.0.2.7"); ok {
		t.Fatal("the IPv4-mapped form got its own budget")
	}
}

// Regression: once the table was full, every new source was refused, so a
// spray from many addresses locked out everyone else.
func TestGuardFullTableStillAdmitsNewSources(t *testing.T) {
	g := NewLoginGuard(1)
	for i := range maxGuardKeys {
		g.attempts["ip:spray"+strconv.Itoa(i)] = []time.Time{g.now()}
	}
	if _, ok := g.Attempt("alice", "203.0.113.50"); !ok {
		t.Fatal("a new source was refused because the table was full")
	}
	if n := len(g.attempts); n > maxGuardKeys {
		t.Fatalf("table grew to %d keys", n)
	}
}
