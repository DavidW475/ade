package api

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist enthält den Build des Dashboards (web/ → internal/api/dist, siehe Makefile).
// Ohne Frontend-Build liegt hier nur .gitkeep; dann zeigt "/" einen Hinweis.
//
//go:embed all:dist
var dist embed.FS

const missingFrontend = `<!doctype html>
<html lang="de"><head><meta charset="utf-8"><title>ADE</title></head>
<body style="font-family:system-ui;padding:2rem">
<h1>ADE läuft</h1>
<p>Das Dashboard ist nicht eingebettet. Mit <code>make build</code> bauen.</p>
</body></html>
`

func staticHandler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return spaHandler(sub)
}

// spaHandler liefert Dateien aus fsys aus. Unbekannte Pfade ohne Dateiendung bekommen
// index.html, damit Client-Routen der Single-Page-App funktionieren.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Methode nicht erlaubt", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					// Vite versieht Assets mit Inhalts-Hash im Namen.
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
