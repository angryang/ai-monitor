// Package server: SPA static handler. Serves the embedded Vite build
// (internal/webui/assets/) and falls back to index.html for unknown
// routes so the SPA's client-side router takes over.
//
// In dev builds (//go:build !prod), the handler instead points at the
// filesystem under web/dist so live edits show up without a rebuild. The
// build tag is set by Makefile's web-build target.
package server

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/angryang/ai-monitor/internal/webui"
)

// handleStatic returns the SPA handler. The signature matches what we
// register on the root mux; it ignores path parameters entirely so the
// SPA can take them over via vue-router.
func (s *Server) handleStatic() http.Handler {
	// Try the embedded FS first; fall back to the disk path used by the
	// web-build target when running outside a build artifact.
	embedded := webui.FS()

	if !isEmptyFS(embedded) {
		return s.spaHandler(embedded)
	}
	// Dev fallback: serve from web/dist if present, otherwise return a
	// 404 with a clear message.
	distPath := s.devDistPath()
	if distPath == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			WriteAPIError(w, http.StatusNotFound, "spa_not_built", "run `make web-build` to embed the SPA")
		})
	}
	return s.spaHandler(os.DirFS(distPath))
}

// devDistPath returns the path to the dev-time SPA bundle, or empty if
// it can't be located. We look in ./web/dist first, then ./internal/webui/assets
// (where the embed points).
func (s *Server) devDistPath() string {
	candidates := []string{
		"web/dist",
		"./web/dist",
		"internal/webui/assets",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			// confirm an index.html exists
			if _, err := os.Stat(filepath.Join(abs, "index.html")); err == nil {
				return abs
			}
		}
	}
	return ""
}

// isEmptyFS returns true when the embedded FS reports ErrNoAssets for
// every Open call. We avoid importing the sentinel into the test suite by
// checking the string error message.
func isEmptyFS(fsys fs.FS) bool {
	_, err := fsys.Open("index.html")
	if err == nil {
		return false
	}
	if errors.Is(err, webui.ErrNoAssets) {
		return true
	}
	// Fallback: try opening the root to see if the FS is populated.
	_, err = fsys.Open(".")
	if err != nil {
		return true
	}
	return false
}

// spaHandler wires the FS into a no-fuss handler with index.html fallback.
//
// We deliberately do NOT use http.FileServer here. Go's net/http.serveFile
// hard-redirects any request whose URL.Path ends with "index.html" to "./"
// (it wants "/foo/index.html" → "/foo/"). Because this handler always
// rewrites SPA fallbacks to "index.html", every browser load landed in an
// infinite 301 loop → "ERR_TOO_MANY_REDIRECTS" / white screen.
//
// Instead we open the asset from the fsys, set Content-Type by extension,
// and stream the body. embed.FS and os.DirFS both satisfy fs.File with a
// ReadSeeker, so http.ServeContent is a perfect fit (it honours range
// requests and 304s using the file's ModTime as ETag).
func (s *Server) spaHandler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API routes are handled by their dedicated sub-muxes; if we
		// reach here on /metrics, /auth/* or /admin/* it's because the
		// caller registered them after New(). To be safe, refuse to
		// serve those paths as SPA assets.
		if isAPIPath(r.URL.Path) {
			WriteAPIError(w, http.StatusNotFound, "not_found", r.URL.Path)
			return
		}
		// Try the requested asset; if missing, fall back to index.html
		// so vue-router can resolve the route client-side.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fsys.Open(path); err != nil {
			path = "index.html"
		}
		f, err := fsys.Open(path)
		if err != nil {
			WriteAPIError(w, http.StatusNotFound, "not_found", path)
			return
		}
		defer f.Close()

		stat, err := f.Stat()
		if err != nil {
			WriteAPIError(w, http.StatusInternalServerError, "stat_failed", err.Error())
			return
		}

		ctype := mimeByExt(path)
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}

		// Prefer http.ServeContent for range support + 304 handling.
		// It requires an io.ReadSeeker; embed.FS and os.DirFS both yield one.
		if rs, ok := f.(io.ReadSeeker); ok {
			http.ServeContent(w, r, path, stat.ModTime(), rs)
			return
		}
		_, _ = io.Copy(w, f)
	})
}

// mimeByExt returns a Content-Type for known static asset extensions. We
// avoid mime.TypeByExtension because the default Go installation only ships
// a tiny built-in table; the explicit switch keeps behaviour consistent
// across CI/dev/prod and predictable for the SPA's hashed assets.
func mimeByExt(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".map":
		return "application/json; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// isAPIPath tells the SPA handler which paths are owned by the API and
// must not be served as static assets.
func isAPIPath(p string) bool {
	switch {
	case p == "/metrics", p == "/healthz":
		return true
	case strings.HasPrefix(p, "/auth/"):
		return true
	case strings.HasPrefix(p, "/admin/"):
		return true
	case strings.HasPrefix(p, "/api/"):
		return true
	}
	return false
}
