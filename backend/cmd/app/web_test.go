package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The API keeps its paths; every other path is the built UI: a file of it,
// or index.html for the UI's own routes.
func TestWithWeb_RoutesAPIAndUI(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<html>ui</html>", "assets/app-1a2b.js": "js"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "api "+r.URL.Path) })
	h := withWeb(api, dir)

	for _, c := range []struct{ path, want, cache string }{
		{"/api/chats", "api /api/chats", ""},
		{"/readyz", "api /readyz", ""},
		{"/healthz", "api /healthz", ""},
		{"/metrics", "api /metrics", ""},
		{"/", "<html>ui</html>", "no-cache"},
		{"/dashboard", "<html>ui</html>", "no-cache"},
		{"/chat/3f6c/kampanya", "<html>ui</html>", "no-cache"},
		{"/assets/app-1a2b.js", "js", "public, max-age=31536000, immutable"},
		{"/assets/../../etc/passwd", "<html>ui</html>", "no-cache"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if got := rec.Body.String(); got != c.want || rec.Header().Get("Cache-Control") != c.cache {
			t.Errorf("GET %s = %d %q (Cache-Control %q); want %q (%q)", c.path, rec.Code, got,
				rec.Header().Get("Cache-Control"), c.want, c.cache)
		}
	}
}
