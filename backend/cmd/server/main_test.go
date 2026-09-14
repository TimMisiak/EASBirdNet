package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The API and the frontend register overlapping patterns on one mux, and a bad
// combination panics inside http.ServeMux at startup rather than failing a
// package test. Wire them together here so that's caught by `go test ./...`.
func TestRoutesCoexist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>birdsense</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := newMux(config{StaticDir: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/health", http.StatusOK},
		{"/api/v1/public/overview", http.StatusOK},
		{"/api/v1/uploads", http.StatusUnauthorized},
		{"/api/v1/nope", http.StatusNotFound},
		{"/", http.StatusOK},
		{"/stations/mercer-slough", http.StatusOK},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}
}
