package main

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// withWeb serves the built UI from dir next to the API. The API keeps its
// paths; any other GET is a file of the UI, or index.html for the UI's own
// routes (it routes on the client). Hashed assets are cached for good;
// index.html is revalidated, so a new build shows at once.
func withWeb(api http.Handler, dir string) http.Handler {
	root := os.DirFS(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") || p == "/healthz" || p == "/readyz" || p == "/metrics" ||
			(r.Method != http.MethodGet && r.Method != http.MethodHead) {
			api.ServeHTTP(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+p), "/")
		if name != "" && serveFile(w, r, root, name, strings.HasPrefix(name, "assets/")) {
			return
		}
		if !serveFile(w, r, root, "index.html", false) {
			http.NotFound(w, r)
		}
	})
}

// serveFile serves a regular file of root; it reports false if there is none.
func serveFile(w http.ResponseWriter, r *http.Request, root fs.FS, name string, immutable bool) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	rs, seekable := f.(io.ReadSeeker)
	if err != nil || st.IsDir() || !seekable {
		return false
	}
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
	return true
}
