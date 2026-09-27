package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func newTestMux(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>birdsense</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return mux, dir
}

func TestServesIndex(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

func TestUnknownRouteFallsBackToIndex(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stations/mercer-slough", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "<h1>birdsense</h1>" {
		t.Errorf("body = %q, want index.html", body)
	}
}

func TestMissingAssetIs404(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/js/typo.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestNonGETIsRejected(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, dir := newTestMux(t)
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), SecurityOptions{StaticDir: dir, HTTPS: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"script-src 'self' " + jsDelivr,
		"style-src 'self' 'unsafe-inline' " + fontsCSS + " " + jsDelivr,
		"font-src " + fontFiles,
		osmTiles,
		"media-src 'self' blob:",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy is missing %q:\n%s", want, csp)
		}
	}
}

// HSTS over plain http would be ignored by the browser, but claiming it in dev
// is still a lie about the transport; the flag follows the session cookie's.
func TestHSTSIsOffWithoutHTTPS(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), SecurityOptions{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want none", got)
	}
}

// The import map is hashed out of the file that ships, so it can't drift from
// the policy -- but it can be missed, by a reformat the regexp doesn't match
// or by an inline script nobody hashed. Either silently loses the recorders
// map (a blocked import map is not an error the browser reports anywhere a
// volunteer or a test would see), so check the real index.html.
func TestShippedIndexIsAllowedByThePolicy(t *testing.T) {
	const frontend = "../../../frontend"
	index, err := os.ReadFile(filepath.Join(frontend, "index.html"))
	if err != nil {
		t.Fatal(err)
	}

	hash := importMapHash(frontend, nil)
	if hash == "" {
		t.Fatal("no import map found in frontend/index.html; if it was removed on purpose, drop this check")
	}
	if csp := contentSecurityPolicy(hash); !strings.Contains(csp, "'"+hash+"'") {
		t.Errorf("policy does not name the import map hash %q:\n%s", hash, csp)
	}

	inline := regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)
	for _, m := range inline.FindAllSubmatch(index, -1) {
		attrs, body := string(m[1]), strings.TrimSpace(string(m[2]))
		if body == "" || strings.Contains(attrs, `type="importmap"`) {
			continue
		}
		t.Errorf("index.html has an inline script the policy blocks: <script%s>", attrs)
	}
}
