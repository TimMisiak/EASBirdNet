package analysis

import (
	"slices"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// A replica packs the two models: BirdNET's files first, every one of them
// started before any Perch file is, and Perch beside the BirdNET still
// running where its memory fits, on the cores left free -- and on all of them
// once it has the replica to itself.
func TestTheQueuePacksBothModels(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.queue.Capacity = Capacity{Cores: 2, MaxTasks: 2, Memory: DefaultCosts[birdnet.ModelBirdNET].Memory + DefaultCosts[birdnet.ModelPerch].Memory}
	f.bird.answer = owls
	const (
		bn1, bn2, bn3 = "DATA/a/Marymoor_20260912_010000.wav", "DATA/a/Marymoor_20260912_020000.wav", "DATA/a/Marymoor_20260912_030000.wav"
	)
	f.bird.took = func(model, audio string) time.Duration {
		if model == birdnet.ModelPerch && audio == owlFile {
			return 150 * time.Millisecond
		}
		return 40 * time.Millisecond
	}
	// An older card waiting for Perch alone, and a newer one BirdNET hasn't
	// started on.
	f.card(db.StatusProcessing, owlFile, quietFile)
	f.birdnetOnly(owlFile, quietFile)
	f.bird.events = nil
	newer := "OWL-20260914-SR05"
	received := testNow
	if _, err := f.store.CreateUpload(t.Context(), db.Upload{ID: newer, Status: db.StatusProcessing, ReceivedAt: &received}); err != nil {
		t.Fatal(err)
	}
	var docs []db.AudioFile
	for _, p := range []string{bn1, bn2, bn3} {
		blob := storage.Name(newer + "/" + p[len(p)-10:len(p)-4])
		f.put(blob, p)
		docs = append(docs, db.AudioFile{Path: p, Night: "2026-09-12", Status: db.AudioUploaded, BlobName: blob})
	}
	if err := f.store.UpsertAudioFiles(t.Context(), newer, docs); err != nil {
		t.Fatal(err)
	}

	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}

	cost := func(model string) int64 { return DefaultCosts[model].Memory }
	running := map[string]bool{}
	var memory int64
	birdnetStarted, packed := 0, false
	var perchThreads []int
	for _, e := range f.bird.events {
		k := e.model + " " + e.audio
		if !e.start {
			delete(running, k)
			memory -= cost(e.model)
			continue
		}
		running[k] = true
		memory += cost(e.model)
		if len(running) > f.queue.Capacity.MaxTasks {
			t.Errorf("%d running at once, over MaxTasks %d", len(running), f.queue.Capacity.MaxTasks)
		}
		if memory > f.queue.Capacity.Memory {
			t.Errorf("%d MB of tasks at once, over the budget of %d MB", memory>>20, f.queue.Capacity.Memory>>20)
		}
		switch e.model {
		case birdnet.ModelBirdNET:
			birdnetStarted++
		case birdnet.ModelPerch:
			if birdnetStarted < 3 {
				t.Errorf("Perch started on %s with BirdNET files still waiting", e.audio)
			}
			perchThreads = append(perchThreads, e.threads)
			for r := range running {
				if r[:len(birdnet.ModelBirdNET)] == birdnet.ModelBirdNET {
					packed = true
				}
			}
		}
	}
	if !packed {
		t.Error("Perch never ran beside BirdNET")
	}
	// Every file gets Perch once BirdNET is done with it: the older card's
	// two and the newer card's three. The first shared the two cores with
	// BirdNET; the rest, which don't fit beside each other, had both.
	if !slices.Equal(perchThreads, []int{1, 2, 2, 2, 2}) {
		t.Errorf("Perch threads = %v, want [1 2 2 2 2]", perchThreads)
	}
	for _, id := range []string{ref, newer} {
		if u, _ := f.store.GetUpload(t.Context(), id); u.Status != db.StatusInReview {
			t.Errorf("%s = %s, want in_review", id, u.Status)
		}
	}
}

// A task bigger than the whole budget still runs, alone, rather than never.
func TestATaskOverTheBudgetRunsAlone(t *testing.T) {
	f := newFixture(t)
	f.queue.Perch = true
	f.queue.Capacity = Capacity{Cores: 4, MaxTasks: 4, Memory: 1 << 30}
	f.bird.answer = owls
	f.bird.took = func(string, string) time.Duration { return 10 * time.Millisecond }
	f.card(db.StatusProcessing, owlFile, quietFile)
	if err := f.queue.drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	perch, most := 0, 0
	for _, e := range f.bird.events {
		if e.model == birdnet.ModelPerch && e.start {
			perch++
			if e.threads != 4 {
				t.Errorf("Perch alone ran on %d threads, want all 4 cores", e.threads)
			}
		}
	}
	running := 0
	for _, e := range f.bird.events {
		if e.start {
			running++
		} else {
			running--
		}
		if e.model == birdnet.ModelPerch {
			most = max(most, running)
		}
	}
	if perch != 2 || most != 1 {
		t.Errorf("Perch ran %d times, with up to %d running at once; want twice, alone", perch, most)
	}
	if u := f.upload(); u.Status != db.StatusInReview {
		t.Errorf("card = %s, want in_review", u.Status)
	}
}
