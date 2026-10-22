package zapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func failErrWith(t *testing.T, ctx context.Context, err error) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	a := &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	a.FailErr(c, err)
	return w
}

func TestFailErrBusyIsRetryable(t *testing.T) {
	w := failErrWith(t, context.Background(), fmt.Errorf("lock library: %w", repo.ErrBusy))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	require.NoError(t, err, "Retry-After must be an integer")
	assert.GreaterOrEqual(t, secs, 3)
	assert.LessOrEqual(t, secs, 8)
}

// An aborted query can come back as a busy-looking database error; with the
// client gone there is nobody to send the 503 to.
func TestFailErrClientCancelBeatsBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := failErrWith(t, ctx, fmt.Errorf("list: %w", repo.ErrBusy))
	assert.Empty(t, w.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusOK, w.Code, "nothing is written")
	assert.Empty(t, w.Body.String())
}
