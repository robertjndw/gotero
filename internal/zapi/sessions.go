package zapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

const (
	loginSessionTTL = 30 * time.Minute
	// Creating sessions needs no credentials, so cap it per client address.
	sessionsPerHour = 30
	sessionWindow   = time.Hour
	// maxLimiterKeys bounds the limiter's memory under address spraying.
	maxLimiterKeys = 10000
)

// rateLimiter is a sliding window counter per key, kept in memory. Losing it
// on restart only resets the budget, which is acceptable for abuse control.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// allow records a hit. When the budget is spent it returns how long to wait.
func (r *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-r.window)
	recent := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= r.limit {
		r.hits[key] = recent
		return false, recent[0].Add(r.window).Sub(now)
	}
	if len(r.hits) >= maxLimiterKeys {
		for k, ts := range r.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(r.hits, k)
			}
		}
	}
	r.hits[key] = append(recent, now)
	return true, 0
}

func (a *API) registerSessions(g *gin.RouterGroup) {
	g.POST("/keys/sessions", a.createSession)
	g.GET("/keys/sessions/:token", a.getSession)
	g.DELETE("/keys/sessions/:token", a.cancelSession)
}

// clientName turns a User-Agent like "Zotero/7.0 (Mac)" into the name the
// approval page and the issued key show.
func clientName(ua string) string {
	name := ua
	if i := strings.IndexAny(name, "/ ("); i >= 0 {
		name = name[:i]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len(name) > 60 {
		name = name[:60]
	}
	if name == "" {
		return "Zotero"
	}
	return name
}

func (a *API) createSession(c *gin.Context) {
	ctx := c.Request.Context()
	if ok, wait := a.sessionLimit.allow(httpx.ClientIP(ctx), a.now()); !ok {
		c.Header("Retry-After", httpx.RetryAfter(wait))
		Fail(c, http.StatusTooManyRequests, "Too many login sessions requested")
		return
	}
	var req struct {
		UserID int64 `json:"userID"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10))
	if err != nil {
		a.FailErr(c, err)
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			Fail(c, http.StatusBadRequest, "Invalid JSON body")
			return
		}
	}
	s := repo.LoginSession{ClientName: clientName(c.GetHeader("User-Agent")), ExpiresAt: a.now().Add(loginSessionTTL)}
	if req.UserID > 0 {
		// Binding to a user that does not exist would make the session
		// impossible to approve, so ignore it instead.
		if _, err := a.store.Users().Get(ctx, req.UserID); err == nil {
			s.RequiredUserID = &req.UserID
		} else if !errors.Is(err, repo.ErrNotFound) {
			a.FailErr(c, err)
			return
		}
	}
	token := zotero.NewToken(64)
	s.TokenHash = auth.HashToken(token)
	if err := a.store.LoginSessions().Create(ctx, s); err != nil {
		a.FailErr(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"sessionToken": token,
		"loginURL":     httpx.BaseURL(ctx) + "/login?session=" + token,
	})
}

func (a *API) getSession(c *gin.Context) {
	ctx := c.Request.Context()
	hash := auth.HashToken(c.Param("token"))
	s, err := a.store.LoginSessions().Get(ctx, hash)
	if errors.Is(err, repo.ErrNotFound) {
		Fail(c, http.StatusNotFound, "Unknown login session")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if !s.ExpiresAt.After(a.now()) {
		Fail(c, http.StatusGone, "Login session has expired")
		return
	}
	switch s.Status {
	case "completed":
		if s.UserID == nil {
			a.FailErr(c, errors.New("completed login session without user"))
			return
		}
		u, err := a.store.Users().Get(ctx, *s.UserID)
		if err != nil {
			a.FailErr(c, err)
			return
		}
		out := gin.H{"status": "completed", "userID": u.ID, "username": u.Username, "displayName": u.DisplayName}
		if out["displayName"] == "" {
			out["displayName"] = u.Username
		}
		// Only the poll that wins the atomic clear gets the key; the client
		// stops polling once it has it.
		if s.APIKey != "" {
			key, err := a.store.LoginSessions().ConsumeAPIKey(ctx, hash)
			if err != nil {
				a.FailErr(c, err)
				return
			}
			if key != "" {
				out["apiKey"] = key
			}
		}
		c.JSON(http.StatusOK, out)
	case "cancelled":
		c.JSON(http.StatusOK, gin.H{"status": "cancelled"})
	default:
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
	}
}

func (a *API) cancelSession(c *gin.Context) {
	err := a.store.LoginSessions().Cancel(c.Request.Context(), auth.HashToken(c.Param("token")))
	if errors.Is(err, repo.ErrNotFound) {
		Fail(c, http.StatusNotFound, "Unknown login session")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
