package analysis

import (
	"cmp"
	"slices"

	"github.com/ngaitonde/EASBirdNet/backend/internal/birdnet"
)

// BirdNET scores a file in 3-second windows, so a bird calling for a minute is
// twenty results. The queue stores it as one detection: consecutive windows in
// which the same species was heard are merged, and the merged detection keeps
// the highest confidence of its windows.

const (
	// adjacentSec is how far apart a window's end and the next window's start
	// may be and still count as consecutive. analyze.py rounds times to the
	// millisecond; a real gap is a whole window.
	adjacentSec = 0.05
	// clipPadSec is how much of the recording a clip keeps either side of what
	// was heard, so the call doesn't start on the first sample.
	clipPadSec = 1.0
	// maxClipSec is the longest clip. A dawn chorus can run for many minutes;
	// its clip is the stretch around the most confident window.
	maxClipSec = 30.0
)

// heard is one species, heard in a run of consecutive windows.
type heard struct {
	// StartSec is the first window's start, EndSec the last window's end, and
	// Confidence the highest of the run.
	birdnet.Detection
	// PeakStartSec and PeakEndSec are the window with that confidence (the
	// earliest, on a tie).
	PeakStartSec, PeakEndSec float64
}

// merge joins each species' consecutive windows into one detection. What it
// returns is sorted like BirdNET's own output: by start, then by confidence,
// highest first.
func merge(found []birdnet.Detection) []heard {
	sorted := slices.Clone(found)
	slices.SortStableFunc(sorted, func(a, b birdnet.Detection) int {
		return cmp.Or(cmp.Compare(a.ScientificName, b.ScientificName), cmp.Compare(a.StartSec, b.StartSec))
	})

	var out []heard
	for _, d := range sorted {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.ScientificName == d.ScientificName && d.StartSec <= last.EndSec+adjacentSec {
				last.EndSec = max(last.EndSec, d.EndSec)
				if d.Confidence > last.Confidence {
					last.Confidence, last.PeakStartSec, last.PeakEndSec = d.Confidence, d.StartSec, d.EndSec
				}
				continue
			}
		}
		out = append(out, heard{Detection: d, PeakStartSec: d.StartSec, PeakEndSec: d.EndSec})
	}

	slices.SortStableFunc(out, func(a, b heard) int {
		return cmp.Or(cmp.Compare(a.StartSec, b.StartSec), cmp.Compare(b.Confidence, a.Confidence))
	})
	return out
}

// clipSpan is the stretch of the recording cut out for a detection: the run
// and clipPadSec either side, or, for a run too long for that, the maxClipSec
// centred on its most confident window. The end may be past the end of the
// recording; the cut is clamped to it.
func clipSpan(h heard) (startSec, endSec float64) {
	startSec, endSec = h.StartSec-clipPadSec, h.EndSec+clipPadSec
	if endSec-startSec > maxClipSec {
		mid := (h.PeakStartSec + h.PeakEndSec) / 2
		startSec = min(max(mid-maxClipSec/2, startSec), endSec-maxClipSec)
		endSec = startSec + maxClipSec
	}
	return max(startSec, 0), endSec
}
