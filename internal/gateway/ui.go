package gateway

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webui holds the built web UI (web/, built by make web into webui/dist).
//
//go:embed all:webui
var webui embed.FS

// uiHandler serves the UI under /ui/. Paths that aren't files (directories
// included) get index.html, so the app's own routes survive a reload. Hashed assets are
// cached for good; index.html never is.
func uiHandler(root fs.FS) http.Handler {
	dist, err := fs.Sub(root, "webui/dist")
	if err == nil {
		_, err = fs.Stat(dist, "index.html")
	}
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			page(w, http.StatusNotFound, "No UI", "This gateway was built without its web UI. Build it with make web, then rebuild the gateway.")
		})
	}
	files := http.FileServerFS(dist)
	return http.StripPrefix("/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if fi, err := fs.Stat(dist, name); name == "" || err != nil || fi.IsDir() {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, dist, "index.html")
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	}))
}
