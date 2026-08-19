package server

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// Extra content types the Go builtin table may lack on a minimal image.
func init() {
	for ext, typ := range map[string]string{
		".woff":        "font/woff",
		".woff2":       "font/woff2",
		".ttf":         "font/ttf",
		".otf":         "font/otf",
		".ico":         "image/x-icon",
		".map":         "application/json",
		".txt":         "text/plain; charset=utf-8",
		".webmanifest": "application/manifest+json",
		".mjs":         "text/javascript; charset=utf-8",
		".js":          "text/javascript; charset=utf-8",
		".css":         "text/css; charset=utf-8",
		".svg":         "image/svg+xml",
		".wasm":        "application/wasm",
	} {
		_ = mime.AddExtensionType(ext, typ)
	}
}

// handleSPA serves the embedded frontend: real files as-is; extension-less
// paths that match nothing are client routes and get index.html; a missing
// path that names a file — it has an extension, or lives under assets/ — is
// a genuine 404 (a stale hashed bundle, a favicon we do not ship) rather
// than an HTML page the requester cannot use. Without an index.html the
// build is missing and a plain notice is returned for the routes.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if s.serveUIFile(w, r, name) {
		return
	}
	if name != "index.html" && !isClientRoute(name) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if name != "index.html" && s.serveUIFile(w, r, "index.html") {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(notBuiltNotice))
}

// isClientRoute reports whether a cleaned, slash-less UI path that matched
// no embedded file should fall back to index.html: only paths whose last
// segment has no extension and that are not under the Vite assets/ dir.
func isClientRoute(name string) bool {
	if name == "assets" || strings.HasPrefix(name, "assets/") {
		return false
	}
	return path.Ext(name) == ""
}

// serveUIFile writes the named file from the UI FS if it exists as a
// regular file. Vite's hashed assets/ get immutable caching; index.html is
// never cached.
func (s *Server) serveUIFile(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.ui == nil {
		return false
	}
	fi, err := fs.Stat(s.ui, name)
	if err != nil || fi.IsDir() {
		return false
	}
	data, err := fs.ReadFile(s.ui, name)
	if err != nil {
		return false
	}
	h := w.Header()
	switch {
	case name == "index.html":
		h.Set("Cache-Control", "no-cache")
	case strings.HasPrefix(name, "assets/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		h.Set("Cache-Control", "public, max-age=3600")
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", http.DetectContentType(data))
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}
