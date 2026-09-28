package analysis

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/perf"
)

// With a recorder set, each pass over a file is one task record, saying what
// the file was, how it went, and how long each phase took.
func TestEachPassIsRecorded(t *testing.T) {
	f := newFixture(t)
	f.bird.answer = owls
	f.card(db.StatusProcessing, owlFile, brokenFile)
	var out bytes.Buffer
	f.queue.Perf = perf.New(perf.NewWriterSink(&out), perf.Config{Instance: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}

	tasks := map[string]perf.Task{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec perf.Task
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if rec.Type == perf.TypeTask {
			tasks[rec.Path] = rec
		}
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d task records, want one per file: %s", len(tasks), out.String())
	}
	owl := tasks[owlFile]
	if owl.Model != "birdnet" || owl.Card != ref || owl.Result != "ok" || owl.Detections != 2 ||
		owl.AudioSec != 3600 || owl.Bytes != int64(len(owlFile)) || owl.Workers != 1 {
		t.Errorf("owl task = %+v", owl)
	}
	phases := make([]string, 0, len(owl.Phases))
	for p := range owl.Phases {
		phases = append(phases, p)
	}
	slices.Sort(phases)
	if !slices.Equal(phases, []string{"analyze", "clip", "download", "store"}) {
		t.Errorf("owl phases = %v", owl.Phases)
	}
	if broken := tasks[brokenFile]; broken.Result != "unreadable" || broken.Error != "unreadable audio" {
		t.Errorf("broken task = %+v; want unreadable", broken)
	}
}
