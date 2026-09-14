// Package web serves the frontend directory. The frontend has no build step,
// so these are the exact files that live in frontend/ -- what you edit is what
// the browser gets.
package web

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Register mounts the static file handler at the root of mux. It is the
// catch-all: every path the API didn't claim lands here.
//
// The pattern is "/" with no method, not "GET /": a method-restricted root
// pattern conflicts with the API's "/api/" catch-all and panics at startup.
// Non-GET requests are rejected here instead.
func Register(mux *http.ServeMux, dir string, log *slog.Logger) {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		// Not fatal -- the API still works -- but it always means a wrong
		// BIRDSENSE_STATIC_DIR, so say so loudly once at startup.
		log.Warn("no index.html in static dir", "static_dir", dir, "err", err)
	}
	mux.Handle("/", onlyGET(cacheHeaders(fallbackToIndex(dir, http.FileServer(http.Dir(dir))))))
}

// onlyGET rejects writes to static paths; the root pattern can't restrict the
// method itself without colliding with the API's catch-all.
func onlyGET(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// fallbackToIndex serves index.html for extension-less paths that don't exist
// on disk, so client-side routes survive a page reload. Requests for a missing
// file (/js/typo.js) still 404, which keeps broken imports visible.
func fallbackToIndex(dir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := filepath.Clean("/" + r.URL.Path)
		if filepath.Ext(clean) == "" {
			if _, err := os.Stat(filepath.Join(dir, clean)); os.IsNotExist(err) {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		next.ServeHTTP(w, r)
	})
}

// cacheHeaders keeps HTML uncached and lets everything else sit in the browser
// cache briefly. Assets aren't content-hashed yet, so the max-age stays short;
// raise it once a build step fingerprints filenames.
func cacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ext := filepath.Ext(r.URL.Path); ext == "" || strings.EqualFold(ext, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		next.ServeHTTP(w, r)
	})
}
