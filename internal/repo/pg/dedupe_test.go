package pg

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/robertjndw/gotero/internal/repo"
)

// A repeated key keeps its first position with the last value, and the
// objects after it must still come through.
func TestDedupeObjects(t *testing.T) {
	in := []repo.Object{
		{Key: "AAAAAAA2", Version: 1},
		{Key: "BBBBBBB2", Version: 2},
		{Key: "AAAAAAA2", Version: 3},
		{Key: "CCCCCCC2", Version: 4},
		{Key: "BBBBBBB2", Version: 5},
		{Key: "DDDDDDD2", Version: 6},
	}
	got := dedupeObjects(in)
	want := []repo.Object{
		{Key: "AAAAAAA2", Version: 3},
		{Key: "BBBBBBB2", Version: 5},
		{Key: "CCCCCCC2", Version: 4},
		{Key: "DDDDDDD2", Version: 6},
	}
	assert.Equal(t, want, got)
}
