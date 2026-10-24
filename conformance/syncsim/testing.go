package syncsim

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Environment variables read by Test.
const (
	// EnvSeed replays one seed instead of the configured ones.
	EnvSeed = "SYNCSIM_SEED"
	// EnvRuns runs each scenario with this many consecutive seeds (soak).
	EnvRuns = "SYNCSIM_RUNS"
	// EnvBase shifts the seeds, so scheduled soak runs explore new ones
	// each time instead of repeating the same set.
	EnvBase = "SYNCSIM_BASE"
	// EnvSteps overrides Config.Steps.
	EnvSteps = "SYNCSIM_STEPS"
	// EnvArtifacts is a directory that receives the client traces of
	// failed runs.
	EnvArtifacts = "SYNCSIM_ARTIFACTS"
)

// Seeds returns the seeds a scenario should run with: SYNCSIM_SEED if set,
// otherwise SYNCSIM_RUNS (default 1) consecutive seeds starting at base,
// shifted by SYNCSIM_BASE million.
func Seeds(base uint64) []uint64 {
	if s := os.Getenv(EnvSeed); s != "" {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			return []uint64{v}
		}
	}
	n := 1
	if s := os.Getenv(EnvRuns); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			n = v
		}
	}
	if s := os.Getenv(EnvBase); s != "" {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil {
			base += v * 1_000_000
		}
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = base + uint64(i)
	}
	return out
}

// Test runs one simulation as a test and fails it with the seed, every
// failure and the clients' recent requests.
func Test(t testing.TB, cfg Config) {
	t.Helper()
	if s := os.Getenv(EnvSteps); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			cfg.Steps = v
		}
	}
	if cfg.Logf == nil {
		cfg.Logf = t.Logf
	}
	rep, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())
	if rep.Stats["ops"] == 0 {
		t.Error("the workload made no edits")
	}
	if len(rep.Failures) == 0 {
		return
	}
	const maxShown = 40
	shown := rep.Failures[:min(len(rep.Failures), maxShown)]
	t.Errorf("%d consistency failures (replay with %s=%d):\n%s",
		len(rep.Failures), EnvSeed, rep.Seed, strings.Join(shown, "\n"))
	dir := os.Getenv(EnvArtifacts)
	for _, name := range slices.Sorted(maps.Keys(rep.Traces)) {
		lines := rep.Traces[name]
		if dir != "" {
			// The directory is the operator's SYNCSIM_ARTIFACTS; the rest is
			// the sanitized test name and a client name.
			p := filepath.Join(dir, sanitize(t.Name()), name+".log")
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err == nil { //nolint:gosec // see above
				_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600) //nolint:gosec // see above
				continue
			}
		}
		t.Logf("--- %s, last %d requests:\n%s", name, min(len(lines), 60), strings.Join(lines[max(0, len(lines)-60):], "\n"))
	}
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r == ':' {
			return '_'
		}
		return r
	}, s)
}
