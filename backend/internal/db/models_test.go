package db

import (
	"context"
	"strings"
	"testing"
)

// A recorder id becomes a Cosmos item id and partition key, a path segment in
// every card reference built from it, and a blob-name prefix. An id none of
// those three accept produces cards that can't be fetched or deleted, so the
// whitelist is worth keeping.
func TestRecorderIDProblem(t *testing.T) {
	for _, id := range []string{"SW-02", "sw-02", "02", "SW02", "A1", strings.Repeat("S", MaxRecorderIDLen)} {
		if problem := RecorderIDProblem(id); problem != "" {
			t.Errorf("RecorderIDProblem(%q) = %q, want it allowed", id, problem)
		}
	}
	for _, id := range []string{
		"", " ", "SW 02", "SW/02", `SW\02`, "SW?02", "SW#02", "SW.02", "SW_02", "SW-02'",
		"-02", "SW-", "Süd-02", strings.Repeat("S", MaxRecorderIDLen+1),
	} {
		if RecorderIDProblem(id) == "" {
			t.Errorf("RecorderIDProblem(%q) = %q, want it refused", id, "")
		}
	}
}

// The reference is what a volunteer quotes, so the recorder's part of it has
// to belong to exactly one recorder.
func TestUploadIDIsOneRecorderPerReference(t *testing.T) {
	if got := UploadID("2026-09-07", "SW-02"); got != "OWL-20260907-SR02" {
		t.Errorf("UploadID = %q, want OWL-20260907-SR02", got)
	}
	// "02" is why addStation refuses an id sharing a RecorderRef with one
	// already in the field: on the same pull date it names the same card.
	if UploadID("2026-09-07", "02") != UploadID("2026-09-07", "SW-02") {
		t.Fatal("UploadID no longer collides; addStation's ref check can go")
	}
}

func TestCreateRecorderRefusesAnUnusableID(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.CreateRecorder(context.Background(), Recorder{ID: "SW/02", Name: "Marymoor"}); err == nil {
		t.Error("CreateRecorder took an id Cosmos would reject")
	}
}
