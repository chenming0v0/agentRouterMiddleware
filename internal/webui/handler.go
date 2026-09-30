// Package webui serves the embedded single-page application.
package webui

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// NewHandler serves fsys as the SPA. Unknown extension-less paths fall back to
// index.html so client-side routes work; assets under assets/ get long-lived
// cache headers.
func NewHandler(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys, files: http.FileServerFS(fsys)}
}

type handler struct {
	fsys  fs.FS
	files http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if strings.HasPrefix(name, "api/") || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	if f, err := h.fsys.Open(name); err == nil {
		_ = f.Close()
		setCacheHeaders(w, name)
		h.files.ServeHTTP(w, r)
		return
	}
	// SPA fallback: no file extension means a client-side route.
	if path.Ext(name) == "" {
		h.serveIndex(w, r)
		return
	}
	http.NotFound(w, r)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.fsys, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
}

func setCacheHeaders(w http.ResponseWriter, name string) {
	switch {
	case strings.HasPrefix(name, "assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case name == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	}
}
