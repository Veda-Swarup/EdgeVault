package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedConsole(t *testing.T) {
	for _, tc := range []struct{ path, contentType, contains string }{
		{"/", "text/html", "EdgeVault"},
		{"/app.js", "javascript", "withLease"},
		{"/style.css", "text/css", ":root"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res := httptest.NewRecorder()
			Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if res.Code != 200 || !strings.Contains(res.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(res.Body.String(), tc.contains) {
				t.Fatalf("unexpected asset response: %d %s", res.Code, res.Header().Get("Content-Type"))
			}
			if !strings.Contains(res.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
				t.Fatal("missing frame protection")
			}
		})
	}
}

func TestConsoleDoesNotExposeSourceOrMaskUnknownAPIs(t *testing.T) {
	for _, path := range []string{"/web.go", "/package.json", "/vite.config.js", "/v1/unknown", "/missing.js"} {
		res := httptest.NewRecorder()
		Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != 404 {
			t.Errorf("%s returned %d", path, res.Code)
		}
	}
}
