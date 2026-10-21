package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.NoRoute(Handler())
	cases := []struct {
		method, path string
		code         int
		contains     string
	}{
		{"GET", "/", 200, "<"},
		{"GET", "/admin/users", 200, "<"},
		{"GET", "/login?session=abc", 200, "<"},
		{"GET", "/api/users/1/nope", 404, "not_found"},
		{"GET", "/admin/api/nope", 404, "not_found"},
		{"POST", "/somewhere", 405, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s %s: got %d %.60q", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestReservedDoesNotMatchLookalikes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.NoRoute(Handler())
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/streams-page", nil))
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
}
