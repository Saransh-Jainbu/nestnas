package server

import (
	"net/http"
	"strings"

	"github.com/Saransh-Jainbu/nestnas/internal/web"
)

// fonts serves the embedded typefaces at /fonts/<name>.woff2 (public, immutable, read-only).
func (s *Server) fonts() http.Handler {
	fs := http.StripPrefix("/fonts/", http.FileServer(http.FS(web.Fonts())))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".woff2") || r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fs.ServeHTTP(w, r)
	})
}
