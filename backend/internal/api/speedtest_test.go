package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestSpeedTest(t *testing.T) {
	mux, _ := newTestMux(t)
	jane := signInAs(t, mux, "jane@example.com")

	if rec := do(t, mux, http.MethodPost, "/api/v1/speedtest", "x", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out = %d, want 401", rec.Code)
	}

	body := strings.Repeat("x", 1<<20)
	rec := do(t, mux, http.MethodPost, "/api/v1/speedtest", body, jane)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /speedtest = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := decodeInto[struct{ Bytes int64 }](t, rec); got.Bytes != 1<<20 {
		t.Errorf("bytes = %d, want %d", got.Bytes, 1<<20)
	}

	big := strings.Repeat("x", speedTestMaxBytes+1)
	if rec := do(t, mux, http.MethodPost, "/api/v1/speedtest", big, jane); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d, want 413", rec.Code)
	}
}
