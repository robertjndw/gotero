package gotero_test

import (
	"testing"

	"github.com/robertjndw/gotero/conformance"
	"github.com/robertjndw/gotero/conformance/gotero"
)

// TestConformance runs every suite against a live gotero server. It skips
// unless GOTERO_CONFORMANCE_URL is set.
func TestConformance(t *testing.T) {
	cfg, ok, err := gotero.ConfigFromEnv()
	if !ok {
		t.Skipf("%s not set", gotero.EnvURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	conformance.Run(t, cfg)
}
