package web

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The third-party origins the frontend reaches. Each one is a decision
// recorded in CLAUDE.md (*No build step*): Leaflet and tus-js-client from
// jsDelivr, the webfonts from Google, map tiles from OpenStreetMap. Adding an
// origin here is adding a dependency -- put it in that list too.
const (
	jsDelivr  = "https://cdn.jsdelivr.net"
	fontsCSS  = "https://fonts.googleapis.com"
	fontFiles = "https://fonts.gstatic.com"
	osmTiles  = "https://tile.openstreetmap.org"
)

// SecurityOptions configures SecurityHeaders.
type SecurityOptions struct {
	// StaticDir is the frontend directory, read once to hash index.html's
	// inline import map. Empty means there is nothing to hash.
	StaticDir string
	// HTTPS says the app is reached over TLS, which turns on HSTS. Off in dev,
	// which is http://localhost -- the same rule the session cookie's Secure
	// flag follows.
	HTTPS bool
	Log   *slog.Logger
}

// SecurityHeaders puts the response headers that don't depend on the request
// on every reply: the content security policy, and the smaller headers that
// stop a browser sniffing a type, framing the app, or leaking a path in a
// Referer. It wraps the whole mux, not just the frontend, so an API response
// carries them too.
//
// The policy is built once, at startup, because it embeds a hash of
// index.html's import map (see importMapHash).
func SecurityHeaders(next http.Handler, opts SecurityOptions) http.Handler {
	csp := contentSecurityPolicy(importMapHash(opts.StaticDir, opts.Log))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		// frame-ancestors above already says this; X-Frame-Options is for
		// whatever doesn't implement it.
		h.Set("X-Frame-Options", "DENY")
		// Cross-origin requests carry the origin and no path: a path here
		// names a card or a detection. OpenStreetMap's tile policy wants to
		// see who is asking, and the origin is enough for that.
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if opts.HTTPS {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy is the whole policy, one directive per line so that a
// new dependency is a one-line diff that says which capability it bought.
//
// Two allowances are worth knowing about:
//
//   - style-src keeps 'unsafe-inline'. Every component writes a <style> block
//     into its own shadow root and several set a style attribute, and with no
//     build step there is nothing to hash them. Adopted stylesheets
//     (shared-styles.js) aren't subject to this; component <style> blocks are.
//   - script-src names one hash, index.html's import map, and nothing else
//     inline. Scripts are the directive worth keeping strict.
func contentSecurityPolicy(importMap string) string {
	script := "script-src 'self' " + jsDelivr
	if importMap != "" {
		script += " '" + importMap + "'"
	}
	return strings.Join([]string{
		"default-src 'none'",
		script,
		// Google Fonts serves the @font-face rules; Leaflet's stylesheet is
		// linked inside <bs-station-map>'s shadow root.
		"style-src 'self' 'unsafe-inline' " + fontsCSS + " " + jsDelivr,
		"font-src " + fontFiles,
		// data: is Leaflet's 1x1 placeholder gif; jsDelivr serves its marker
		// and layer images, named by relative URLs in leaflet.css.
		"img-src 'self' data: " + jsDelivr + " " + osmTiles,
		// A clip is fetched, then played from a blob URL (bs-spectrogram).
		"media-src 'self' blob:",
		"connect-src 'self'",
		"object-src 'none'",
		"worker-src 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// importMapScript finds index.html's inline import map. Nothing else inline is
// allowed to run, so this is deliberately the one shape it matches.
var importMapScript = regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`)

// importMapHash returns the CSP source expression for index.html's import map,
// or "" if there isn't one.
//
// It is hashed here, at startup, rather than written out as a constant,
// because there is no build step to keep a constant in step with the file --
// and an import map the policy doesn't name is silently ignored by the
// browser, which loses the recorders map without an error anyone would see.
// An external import map would avoid this, but no browser supports one.
func importMapHash(dir string, log *slog.Logger) string {
	if dir == "" {
		return ""
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		// Register already warned about a missing index.html.
		return ""
	}
	m := importMapScript.FindSubmatch(index)
	if m == nil {
		return ""
	}
	sum := sha256.Sum256(m[1])
	hash := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	if log != nil {
		log.Info("import map allowed by the content security policy", "hash", hash)
	}
	return hash
}
