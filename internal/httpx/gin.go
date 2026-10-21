package httpx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Gin adapts net/http middleware to a gin handler so the same middleware can
// run around the whole engine or only on selected route groups. Middleware
// that wraps the ResponseWriter must stay outside the engine (see
// server.New), because gin keeps writing to its own c.Writer.
func Gin(mw func(http.Handler) http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		called := false
		mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			c.Request = r
			c.Next()
		})).ServeHTTP(c.Writer, c.Request)
		if !called {
			c.Abort()
		}
	}
}
