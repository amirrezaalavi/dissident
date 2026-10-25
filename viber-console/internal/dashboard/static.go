package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:static
var staticFS embed.FS

// StaticHandler serves the embedded SPA (static/ dir inside this package).
// /          → index.html (explicit, avoids FileServer directory redirect)
// /static/*  → static assets
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))

	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic(err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/static/")
		if path == "" || path == r.URL.Path {
			// "/" or anything non-/static: serve the SPA shell.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write(indexBytes)
			return
		}
		r.URL.Path = path
		fileServer.ServeHTTP(w, r)
	})
}
