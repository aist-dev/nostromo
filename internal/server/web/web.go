// Package web embeds the browser UI.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed index.html app.js app.css vendor
var files embed.FS

// Handler serves the UI. Files are embedded, so the page makes no external requests.
func Handler() http.Handler {
	sub, _ := fs.Sub(files, ".")
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			h.Set("Cache-Control", "public, max-age=86400")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
