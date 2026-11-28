package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist contains the dashboard build (web/ → internal/api/dist, see Makefile).
// Without a frontend build only .gitkeep is here; then "/" shows a notice.
//
//go:embed all:dist
var dist embed.FS

const missingFrontend = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>ADE</title></head>
<body style="font-family:system-ui;padding:2rem">
<h1>ADE is running</h1>
<p>The dashboard is not embedded. Build it with <code>make build</code>.</p>
</body></html>
`

func staticHandler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return spaHandler(sub)
}

// spaHandler serves files from fsys. Unknown paths without a file extension get
// index.html, so client routes of the single-page app work.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite puts a content hash into asset names.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		serveIndex(w, fsys)
	})
}

func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		data = []byte(missingFrontend)
	}
	_, _ = w.Write(data)
}
