package zoteroclient

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// maxPauseSeconds clamps server supplied waits so converting to a
	// time.Duration can't overflow.
	maxPauseSeconds = 1_000_000
	// maxRetryWait is the longest Retry-After we are willing to sit out. A
	// server asking for more is told no by returning its response unchanged.
	maxRetryWait = 5 * time.Minute

	fallbackBase = time.Second
	fallbackCap  = 30 * time.Second
)

// parseRetryAfter reads a Retry-After style value: whole seconds or an HTTP
// date. It never returns a negative duration.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if n, err := strconv.ParseUint(v, 10, 64); err == nil {
		return time.Duration(min(n, maxPauseSeconds)) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	d := t.Sub(now)
	if d < 0 {
		return 0, true
	}
	return min(d, maxPauseSeconds*time.Second), true
}

// fallbackDelay is an exponential wait with jitter, used when the server gave
// no usable Retry-After. Jitter keeps parallel workers from retrying in step.
func fallbackDelay(attempt int) time.Duration {
	d := fallbackCap
	if attempt < 5 {
		d = min(fallbackBase<<attempt, fallbackCap)
	}
	half := d / 2
	return half + rand.N(half+1) //nolint:gosec // jitter, not security
}
