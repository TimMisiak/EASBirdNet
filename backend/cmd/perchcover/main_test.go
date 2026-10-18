package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
)

func det(start, end float64, scientific, common string, confidence float64) birdnet.Detection {
	return birdnet.Detection{StartSec: start, EndSec: end, ScientificName: scientific, CommonName: common, Confidence: confidence}
}

// The kept audio is widened by the margin, rounded out to Perch's windows,
// clipped to the file and merged where stretches meet.
func TestKept(t *testing.T) {
	ds := []birdnet.Detection{
		det(12, 15, "Strix varia", "Barred Owl", 0.9),
		det(16, 19, "Strix varia", "Barred Owl", 0.9),
		det(40, 43, "Engine", "Engine", 0.8),
		det(598, 600, "Bubo virginianus", "Great Horned Owl", 0.4),
	}
	got := kept(ds, isBird, 2, 600)
	want := []span{{10, 25}, {595, 600}}
	if len(got) != len(want) {
		t.Fatalf("kept = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kept = %v, want %v", got, want)
		}
	}
	if l := length(got); l != 20 {
		t.Errorf("length = %v, want 20", l)
	}
	if all := kept(ds, func(birdnet.Detection) bool { return true }, 0, 600); len(all) != 3 {
		t.Errorf("with the engine: %v, want it kept too", all)
	}
}

// A saved pair of runs reports the saving and the cost: Perch hears 25 s of
// a 100 s file around BirdNET's owl, and misses the frog it heard elsewhere.
func TestReport(t *testing.T) {
	s := saved{
		Files: []file{{Path: "a.wav", DurationSec: 100}},
		BirdNET: birdnet.Result{Files: []birdnet.File{{Path: "a.wav", Detections: []birdnet.Detection{
			det(10, 13, "Strix varia", "Barred Owl", 0.9),
			det(60, 63, "Engine", "Engine", 0.9),
		}}}},
		Perch: birdnet.Result{Files: []birdnet.File{{Path: "a.wav", Detections: []birdnet.Detection{
			det(10, 15, "Strix varia", "Barred Owl", 0.8),
			det(80, 85, "Rana aurora", "Rana aurora", 0.6),
		}}}},
	}
	o := measure(s, selections[1], 2)
	if o.keptSec != 10 || o.perchIn != 1 || o.perchOut != 1 || len(o.lost) != 1 || o.lost[0] != "Rana aurora" {
		t.Errorf("birds, 2 s = %+v; want 10 s kept, the owl in and the frog lost", o)
	}
	var out bytes.Buffer
	report(&out, s, true)
	for _, want := range []string{
		"1 files, 2 min of audio",
		"birds only               5%    10%   15%",
		"50%, 1 species",
		"Perch only: Rana aurora",
		"birds only, 0 s: Rana aurora",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
}
