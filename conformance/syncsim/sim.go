// Package syncsim checks that many Zotero clients syncing one library
// through a server end up with the same library, and that nothing they were
// told was saved goes missing on the way.
//
// Each simulated client keeps its own local database and syncs it the way
// Zotero desktop does: upload first with If-Unmodified-Since-Version,
// download and retry on 412, three-way merge of local and remote changes,
// delete conflicts, a full resync after a per-object 412, ZFS file uploads
// and downloads, and a streaming connection that triggers syncs. Clients
// make random edits concurrently, optionally over a network that drops
// requests and responses. Afterwards every client must agree with the server
// and the server must agree with everything it acknowledged.
//
// Like the rest of the conformance kit it only speaks HTTP and WebSocket, so
// it runs against any server through a conformance.Provisioner.
package syncsim

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertjndw/gotero/conformance"
)

// Config describes one simulation.
type Config struct {
	// BaseURL is the API root, e.g. http://localhost:8080/api/
	BaseURL string
	// StreamURL enables streaming: clients sync when told to, and every
	// client must hear about the final version.
	StreamURL   string
	Provisioner conformance.Provisioner
	// HTTPClient supplies the transport; faults are layered on top.
	HTTPClient *http.Client

	// Seed makes the workload reproducible. In Concurrent mode the
	// interleaving still depends on timing; Stepped runs replay exactly.
	Seed uint64
	// Users is the number of accounts (default 1). With Group set they share
	// a group library owned by the first one.
	Users int
	// Devices is the number of clients per user (default 2). With more than
	// one, each user's own library is synced by several devices too.
	Devices int
	Group   bool
	// Steps is the number of user actions per client (default 40).
	Steps int
	// Concurrent runs every client in its own goroutine. Otherwise one
	// scheduler interleaves single steps deterministically.
	Concurrent bool
	// FaultRate is the chance that a request is dropped before reaching the
	// server or its response is lost after the server acted on it.
	FaultRate float64
	// Files adds attachments with file uploads, downloads and file conflicts.
	Files bool
	// Restart, if set, is called at random points of the workload to crash
	// and restart the server. In Concurrent mode requests are in flight when
	// it happens; clients see that as a network error.
	Restart func(ctx context.Context) error
	// RestartRate is the chance per step of calling Restart (default 0.02).
	RestartRate float64
	// BeforeCheck, if set, runs once every client has settled, before the
	// final checks, which then run against whatever server it leaves
	// behind. A backup restored into a fresh server, for example: every
	// client must sync with it cleanly and find it identical.
	BeforeCheck func(ctx context.Context) error
	// RetryAfterScale shrinks Retry-After waits on 429 and 503 (default 0.02).
	RetryAfterScale float64
	// Timeout bounds the whole run (default 5 minutes).
	Timeout time.Duration
	// Logf receives progress lines; nil discards them.
	Logf func(format string, args ...any)
}

func (c *Config) defaults() {
	if c.Users <= 0 {
		c.Users = 1
	}
	if c.Devices <= 0 {
		c.Devices = 2
	}
	if c.Steps <= 0 {
		c.Steps = 40
	}
	if c.Restart != nil && c.RestartRate <= 0 {
		c.RestartRate = 0.02
	}
	if c.RetryAfterScale <= 0 {
		c.RetryAfterScale = 0.02
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Minute
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
}

// Report is the outcome of a run. Failures is empty when the server passed.
type Report struct {
	Seed     uint64
	Failures []string
	// Stats counts what happened, to show the run exercised something.
	Stats map[string]int64
	// Traces holds each client's most recent requests when the run failed.
	Traces map[string][]string
}

func (r *Report) Summary() string {
	var keys []string
	for k := range r.Stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d:", r.Seed)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, r.Stats[k])
	}
	return b.String()
}

type stats struct {
	syncs, fullSyncs, preconditions, restarts, busy, conflicts, objectResets                               atomic.Int64
	fileConflicts, retries, networkErrors, loopLimit, cycleFixes, streamSyncs, aheadOfVersion, missingDeps atomic.Int64
	serverRestarts                                                                                         atomic.Int64

	mu  sync.Mutex
	ops map[string]int64
}

func (s *stats) countOp(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ops == nil {
		s.ops = map[string]int64{}
	}
	s.ops[name]++
}

func (s *stats) snapshot() map[string]int64 {
	out := map[string]int64{
		"syncs": s.syncs.Load(), "fullSyncs": s.fullSyncs.Load(), "preconditionRetries": s.preconditions.Load(),
		"downloadRestarts": s.restarts.Load(), "busy": s.busy.Load(), "conflicts": s.conflicts.Load(),
		"objectResets": s.objectResets.Load(), "fileConflicts": s.fileConflicts.Load(),
		"retryAfterWaits": s.retries.Load(), "networkErrors": s.networkErrors.Load(),
		"syncLoopLimit": s.loopLimit.Load(), "cycleFixes": s.cycleFixes.Load(), "streamSyncs": s.streamSyncs.Load(),
		"listingAheadOfVersion": s.aheadOfVersion.Load(), "missingDependencies": s.missingDeps.Load(),
		"serverRestarts": s.serverRestarts.Load(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for k, v := range s.ops {
		out["op."+k] = v
		total += v
	}
	out["ops"] = total
	return out
}

type sim struct {
	cfg     Config
	base    string
	stats   stats
	hist    *history
	clients []*client
	// verifiers read the server's state for the checks, without faults,
	// one per library with a key that can see it.
	verifiers map[string]*client

	mu       sync.Mutex
	failures []string

	// restartMu serializes restarts. Events for writes that committed
	// around a restart, before a client subscribed again, are lost; Zotero
	// covers that by syncing after it reconnects. So the stream check only
	// holds the server to writes made once everyone was back.
	restartMu sync.Mutex
	// restartFloor[lib] is the library version once every client had
	// subscribed again after the last restart.
	restartFloor map[string]int64
}

func (s *sim) fail(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	s.failures = append(s.failures, msg)
	s.cfg.Logf("FAIL: %s", msg)
}

type client struct {
	sim    *sim
	name   string
	user   conformance.User
	hc     *http.Client
	faults *faultTransport
	rng    *rand.Rand
	policy Policy
	skew   time.Duration
	libs   []*library
	trace  *trace
	stream *listener

	lmvMu  sync.Mutex
	maxLMV map[string]int64
}

func (s *sim) newClient(name string, u conformance.User, seed uint64, libs []string) *client {
	base := s.cfg.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	ft := &faultTransport{base: base, rng: newRand(seed, 1)}
	hc := *s.cfg.HTTPClient
	hc.Transport = ft
	// Download redirects are followed by hand, without the API key.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc.Timeout == 0 {
		hc.Timeout = time.Minute
	}
	rng := newRand(seed, 2)
	c := &client{
		sim: s, name: name, user: u, hc: &hc, faults: ft, rng: rng,
		policy: Policy(rng.IntN(2)),
		skew:   time.Duration(rng.IntN(120)-60) * time.Second,
		trace:  &trace{}, stream: newListener(), maxLMV: map[string]int64{},
	}
	for _, p := range libs {
		c.libs = append(c.libs, newLibrary(p))
	}
	return c
}

// Run provisions accounts, runs the clients and checks the result. The error
// is for setup problems; protocol and consistency failures are in the
// report.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	cfg.defaults()
	if cfg.BaseURL == "" || cfg.Provisioner == nil {
		return nil, errors.New("syncsim: BaseURL and Provisioner are required")
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	s := &sim{cfg: cfg, base: strings.TrimRight(cfg.BaseURL, "/") + "/", hist: newHistory()}
	if err := s.setup(ctx); err != nil {
		return nil, err
	}

	streamCtx, stopStreams := context.WithCancel(ctx)
	var streams sync.WaitGroup
	defer func() {
		stopStreams()
		streams.Wait()
	}()
	if cfg.StreamURL != "" {
		for _, c := range s.clients {
			streams.Go(func() { c.runStream(streamCtx, cfg.StreamURL) })
		}
		if err := s.waitSubscribed(ctx); err != nil {
			return nil, err
		}
	}

	for _, c := range s.clients {
		c.faults.setRate(cfg.FaultRate)
	}
	cfg.Logf("syncsim: seed %d, %d clients, %d steps each, concurrent=%v faults=%.2f files=%v group=%v",
		cfg.Seed, len(s.clients), cfg.Steps, cfg.Concurrent, cfg.FaultRate, cfg.Files, cfg.Group)
	if cfg.Concurrent {
		s.runConcurrent(ctx)
	} else {
		s.runStepped(ctx)
	}
	for _, c := range s.clients {
		c.faults.setRate(0)
	}
	if ctx.Err() == nil {
		s.quiesce(ctx)
	}
	if cfg.BeforeCheck != nil && ctx.Err() == nil {
		if err := cfg.BeforeCheck(ctx); err != nil {
			s.fail("before the checks: %v", err)
		} else {
			s.quiesce(ctx)
		}
	}
	if ctx.Err() == nil {
		s.check(ctx)
	}
	if err := ctx.Err(); err != nil {
		s.fail("run did not finish: %v", err)
	}

	rep := &Report{Seed: cfg.Seed, Failures: s.failures, Stats: s.stats.snapshot()}
	if len(rep.Failures) > 0 {
		rep.Traces = map[string][]string{}
		for _, c := range s.clients {
			rep.Traces[c.name] = c.trace.dump()
		}
	}
	return rep, nil
}

func (s *sim) setup(ctx context.Context) error {
	cfg := s.cfg
	users := make([]conformance.User, cfg.Users)
	for i := range users {
		u, err := cfg.Provisioner.NewUser(ctx)
		if err != nil {
			return fmt.Errorf("syncsim: new user: %w", err)
		}
		users[i] = u
	}
	group := ""
	if cfg.Group {
		g, err := cfg.Provisioner.NewGroup(ctx, users[0], users[1:], nil)
		if err != nil {
			return fmt.Errorf("syncsim: new group: %w", err)
		}
		group = fmt.Sprintf("groups/%d", g.ID)
	}
	seeds := newRand(cfg.Seed, 0)
	for i, u := range users {
		var libs []string
		if cfg.Devices > 1 || !cfg.Group {
			libs = append(libs, fmt.Sprintf("users/%d", u.ID))
		}
		if group != "" {
			libs = append(libs, group)
		}
		for d := range cfg.Devices {
			name := fmt.Sprintf("u%d.d%d", i, d)
			s.clients = append(s.clients, s.newClient(name, u, seeds.Uint64(), libs))
		}
	}
	s.verifiers = map[string]*client{}
	return nil
}

func (s *sim) waitSubscribed(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	for _, c := range s.clients {
		for !c.stream.isSubscribed() {
			if time.Now().After(deadline) {
				return fmt.Errorf("syncsim: %s did not subscribe to %s", c.name, s.cfg.StreamURL)
			}
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

// syncOnce runs a sync and sorts its error: network trouble and a busy
// library are expected and retried later, anything else fails the run.
func (s *sim) syncOnce(ctx context.Context, c *client) bool {
	err := c.sync(ctx)
	var pe *protocolError
	switch {
	case err == nil:
		return true
	case ctx.Err() != nil:
	case errors.As(err, &pe):
		s.fail("%s: %v", c.name, err)
	case errors.Is(err, errNetwork):
		s.stats.networkErrors.Add(1)
		if s.cfg.FaultRate == 0 && s.cfg.Restart == nil {
			s.fail("%s: %v", c.name, err)
		}
	case errors.Is(err, errBusy), errors.Is(err, errNoProgress):
	default:
		s.fail("%s: %v", c.name, err)
	}
	return false
}

// step is one thing a client does: edit, sync, or both.
func (s *sim) step(ctx context.Context, c *client) {
	if s.cfg.Restart != nil && c.rng.Float64() < s.cfg.RestartRate {
		s.restart(ctx)
	}
	switch r := c.rng.IntN(10); {
	case r < 6:
		c.randomOp()
	case r < 9:
		s.syncOnce(ctx, c)
	default:
		c.randomOp()
		s.syncOnce(ctx, c)
	}
}

func (s *sim) restart(ctx context.Context) {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	subs := map[string]int{}
	for _, c := range s.clients {
		subs[c.name] = c.stream.subscriptionCount()
	}
	if err := s.cfg.Restart(ctx); err != nil {
		if ctx.Err() == nil {
			s.fail("restarting the server: %v", err)
		}
		return
	}
	s.stats.serverRestarts.Add(1)
	if s.cfg.StreamURL != "" {
		deadline := time.Now().Add(15 * time.Second)
		for _, c := range s.clients {
			for c.stream.subscriptionCount() <= subs[c.name] && time.Now().Before(deadline) && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			if c.stream.subscriptionCount() <= subs[c.name] && ctx.Err() == nil {
				s.fail("%s: stream did not subscribe again after a server restart", c.name)
			}
		}
	}
	floor := map[string]int64{}
	for _, c := range s.clients {
		for _, l := range c.libs {
			if _, ok := floor[l.path]; ok {
				continue
			}
			v, err := s.serverVersion(ctx, l.path)
			if err != nil {
				if ctx.Err() == nil {
					s.fail("reading %s after a restart: %v", l.path, err)
				}
				return
			}
			floor[l.path] = v
		}
	}
	s.mu.Lock()
	s.restartFloor = floor
	s.mu.Unlock()
}

func (s *sim) runConcurrent(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range s.clients {
		wg.Go(func() {
			for i := 0; i < s.cfg.Steps && ctx.Err() == nil; i++ {
				select {
				case <-c.stream.wake:
					// Zotero's auto-sync after a stream notification.
					s.stats.streamSyncs.Add(1)
					s.syncOnce(ctx, c)
				default:
				}
				s.step(ctx, c)
			}
		})
	}
	wg.Wait()
}

func (s *sim) runStepped(ctx context.Context) {
	sched := newRand(s.cfg.Seed, 3)
	left := make([]int, len(s.clients))
	for i := range left {
		left[i] = s.cfg.Steps
	}
	for remaining := len(s.clients) * s.cfg.Steps; remaining > 0 && ctx.Err() == nil; remaining-- {
		i := sched.IntN(len(s.clients))
		for left[i] == 0 {
			i = (i + 1) % len(s.clients)
		}
		left[i]--
		s.step(ctx, s.clients[i])
	}
}

// quiesce lets every client sync until nobody has anything left to upload
// and all of them sit at the server's version.
func (s *sim) quiesce(ctx context.Context) {
	why := ""
	for round := 0; round < 12 && ctx.Err() == nil; round++ {
		for _, c := range s.clients {
			s.syncOnce(ctx, c)
		}
		if why = s.unsettled(ctx); why == "" {
			return
		}
	}
	s.fail("clients did not settle after 12 sync rounds without new edits: %s", why)
}

// unsettled names the first client that still has work, or "" if none has.
func (s *sim) unsettled(ctx context.Context) string {
	for _, c := range s.clients {
		for _, l := range c.libs {
			v, err := s.serverVersion(ctx, l.path)
			switch {
			case err != nil:
				return err.Error()
			case l.version != v:
				return fmt.Sprintf("%s %s at version %d, server at %d", c.name, l.path, l.version, v)
			case l.needFull:
				return fmt.Sprintf("%s %s needs a full sync", c.name, l.path)
			}
			if d, f := l.unsynced(); len(d)+len(f) > 0 {
				return fmt.Sprintf("%s %s has unsynced %s", c.name, l.path, strings.Join(append(d, f...), "; "))
			}
			for key, f := range l.files {
				if o := l.objs[kItem][key]; o != nil && f.md5 != str(o.data, "md5") {
					return fmt.Sprintf("%s %s: file of %s has md5 %q, item says %q", c.name, l.path, key, f.md5, str(o.data, "md5"))
				}
			}
		}
	}
	return ""
}
