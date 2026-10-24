package gotero_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/robertjndw/gotero/conformance/gotero"
	"github.com/robertjndw/gotero/conformance/syncsim"
)

// TestSyncSim runs simulated Zotero clients against a live gotero server and
// checks that they converge. It skips unless GOTERO_CONFORMANCE_URL is set.
// Over a real network and the real image it catches what the in-process run
// in internal/server cannot, such as proxy or timeout behaviour.
func TestSyncSim(t *testing.T) {
	cfg, ok, err := gotero.ConfigFromEnv()
	if !ok {
		t.Skipf("%s not set", gotero.EnvURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	scenarios := []struct {
		name string
		sim  syncsim.Config
	}{
		{"devices", syncsim.Config{Devices: 3, Steps: 60, Concurrent: true, Files: true}},
		{"group", syncsim.Config{Users: 3, Devices: 2, Group: true, Steps: 40, Concurrent: true, Files: true}},
		{"faults", syncsim.Config{Users: 2, Devices: 2, Group: true, Steps: 40, Concurrent: true, Files: true, FaultRate: 0.15}},
	}
	for i, sc := range scenarios {
		for _, seed := range syncsim.Seeds(uint64(100 * (i + 1))) {
			t.Run(fmt.Sprintf("%s/seed=%d", sc.name, seed), func(t *testing.T) {
				sim := sc.sim
				sim.Seed = seed
				sim.BaseURL = cfg.BaseURL
				sim.StreamURL = cfg.StreamURL
				sim.Provisioner = cfg.Provisioner
				syncsim.Test(t, sim)
			})
		}
	}
}

// Environment for TestSyncSimRestart.
const (
	// EnvRestartCmd is a shell command that kills the server and brings it
	// back, possibly as a different version. It must not return before the
	// replacement has been started; the test then waits for /healthz.
	EnvRestartCmd = "GOTERO_SYNCSIM_RESTART_CMD"
	// EnvRestartRate is the chance per client step of running it.
	EnvRestartRate = "GOTERO_SYNCSIM_RESTART_RATE"
)

// TestSyncSimRestart runs the simulation while an outside command crashes
// and restarts the server, with requests in flight. Pointing the command at
// a newer image turns it into an upgrade test: clients that synced with the
// old version must carry on with the new one without losing anything.
func TestSyncSimRestart(t *testing.T) {
	cmd := os.Getenv(EnvRestartCmd)
	if cmd == "" {
		t.Skipf("%s not set", EnvRestartCmd)
	}
	cfg, ok, err := gotero.ConfigFromEnv()
	if !ok {
		t.Skipf("%s not set", gotero.EnvURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	rate := 0.03
	if s := os.Getenv(EnvRestartRate); s != "" {
		if rate, err = strconv.ParseFloat(s, 64); err != nil {
			t.Fatalf("%s: %v", EnvRestartRate, err)
		}
	}
	health, err := healthURL(cfg.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	restart := func(ctx context.Context) error { return runAndWait(ctx, EnvRestartCmd, cmd, health) }
	for _, seed := range syncsim.Seeds(900) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			syncsim.Test(t, syncsim.Config{
				Users: 2, Devices: 2, Group: true, Steps: 60, Concurrent: true, Files: true,
				Seed: seed, BaseURL: cfg.BaseURL, StreamURL: cfg.StreamURL, Provisioner: cfg.Provisioner,
				Restart: restart, RestartRate: rate, Timeout: 15 * time.Minute,
			})
		})
	}
}

// runAndWait runs the shell command configured in env and waits for the
// replacement server it starts to report healthy.
func runAndWait(ctx context.Context, env, cmd, health string) error {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", env, err, out)
	}
	return waitHealthy(ctx, health, 2*time.Minute)
}

func healthURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path, u.RawQuery = "/healthz", ""
	return u.String(), nil
}

func waitHealthy(ctx context.Context, health string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server not healthy after %v: %w", limit, err)
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// EnvBackupRestoreCmd is a shell command that backs the server up, destroys
// it with its database and files, and restores the backup into a new one.
// It must not return before the replacement has been started.
const EnvBackupRestoreCmd = "GOTERO_SYNCSIM_BACKUP_RESTORE_CMD"

// TestSyncSimBackupRestore lets clients build up libraries, then replaces
// the server with one restored from a backup. Every client must sync with
// the restored server without errors and find exactly what it had: same
// objects, same versions, same file bytes.
func TestSyncSimBackupRestore(t *testing.T) {
	cmd := os.Getenv(EnvBackupRestoreCmd)
	if cmd == "" {
		t.Skipf("%s not set", EnvBackupRestoreCmd)
	}
	cfg, ok, err := gotero.ConfigFromEnv()
	if !ok {
		t.Skipf("%s not set", gotero.EnvURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	health, err := healthURL(cfg.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	syncsim.Test(t, syncsim.Config{
		Users: 2, Devices: 2, Group: true, Steps: 60, Concurrent: true, Files: true,
		Seed: 950, BaseURL: cfg.BaseURL, StreamURL: cfg.StreamURL, Provisioner: cfg.Provisioner,
		Timeout: 15 * time.Minute,
		BeforeCheck: func(ctx context.Context) error {
			return runAndWait(ctx, EnvBackupRestoreCmd, cmd, health)
		},
	})
}
