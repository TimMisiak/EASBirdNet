package review

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/EASBirdSense/BirdSense/backend/internal/db"
)

const (
	// bestClips is how many of a bout's detections a reviewer is played.
	bestClips = 3
	// maxBoutDetections keeps a bout's document well inside Cosmos's 2 MB:
	// a species heard all day with never a gap would otherwise be one bout,
	// holding every id. One that reaches it carries on as a second bout.
	maxBoutDetections = 2000
)

// Member is what bouting needs of a detection.
type Member struct {
	ID             string
	ScientificName string
	CommonName     string
	// Start and End are when it was heard: DetectedAt, and that plus the run
	// BirdNET's merged windows span.
	Start, End time.Time
	Confidence float64
}

// MemberOf is d as bouting sees it.
func MemberOf(d db.Detection) Member {
	return Member{
		ID: d.ID, ScientificName: d.ScientificName, CommonName: d.CommonName,
		Start: d.DetectedAt, End: d.DetectedAt.Add(time.Duration((d.EndSec - d.StartSec) * float64(time.Second))),
		Confidence: d.Confidence,
	}
}

// Bouts groups one card's detections into bouts of kind: each species
// apart, and a new bout wherever more than gap passes between the end of
// one detection and the start of the next. A bout crosses file boundaries,
// since it goes by when things were heard. They come back in the order they
// were heard.
func Bouts(card db.Upload, kind string, members []Member, gap time.Duration) []db.Bout {
	bySpecies := map[string][]Member{}
	for _, m := range members {
		bySpecies[m.ScientificName] = append(bySpecies[m.ScientificName], m)
	}
	var out []db.Bout
	for _, ms := range bySpecies {
		slices.SortFunc(ms, func(a, b Member) int {
			return cmp.Or(a.Start.Compare(b.Start), strings.Compare(a.ID, b.ID))
		})
		start, end := 0, ms[0].End
		for i := 1; i <= len(ms); i++ {
			if i < len(ms) && ms[i].Start.Sub(end) <= gap && i-start < maxBoutDetections {
				end = later(end, ms[i].End)
				continue
			}
			out = append(out, bout(card, kind, ms[start:i]))
			if i < len(ms) {
				start, end = i, ms[i].End
			}
		}
	}
	slices.SortFunc(out, func(a, b db.Bout) int {
		return cmp.Or(a.FirstHeardAt.Compare(b.FirstHeardAt), strings.Compare(a.ScientificName, b.ScientificName))
	})
	return out
}

// bout is one run of a species' detections, in the order heard, as a bout.
func bout(card db.Upload, kind string, ms []Member) db.Bout {
	b := db.Bout{
		ID: db.BoutID(card.ID, ms[0].ScientificName, ms[0].ID), UploadID: card.ID, RecorderID: card.RecorderID,
		Kind: kind, ScientificName: ms[0].ScientificName, CommonName: ms[0].CommonName,
		FirstHeardAt: ms[0].Start, LastHeardAt: ms[len(ms)-1].Start,
		DetectionIDs: make([]string, len(ms)), Status: db.BoutOpen, Reviews: []db.BoutVerdict{},
	}
	for i, m := range ms {
		b.DetectionIDs[i] = m.ID
	}
	best := slices.Clone(ms)
	slices.SortStableFunc(best, func(a, b Member) int { return cmp.Compare(b.Confidence, a.Confidence) })
	for _, m := range best[:min(bestClips, len(best))] {
		b.Best = append(b.Best, m.ID)
	}
	b.MaxConfidence = best[0].Confidence
	return b
}

// SpotChecks picks up to n of each species' bouts to check what the rules
// accepted, less the spot-checks a species already has that someone has
// heard (kept, by scientific name). Bouts are picked by id, which is a hash,
// so the pick is as good as random and preparing the card again picks the
// same ones.
func SpotChecks(bouts []db.Bout, n int, kept map[string]int) []db.Bout {
	bySpecies := map[string][]db.Bout{}
	for _, b := range bouts {
		bySpecies[b.ScientificName] = append(bySpecies[b.ScientificName], b)
	}
	var out []db.Bout
	for species, bs := range bySpecies {
		slices.SortFunc(bs, func(a, b db.Bout) int { return strings.Compare(a.ID, b.ID) })
		out = append(out, bs[:max(0, min(n-kept[species], len(bs)))]...)
	}
	slices.SortFunc(out, func(a, b db.Bout) int {
		return cmp.Or(a.FirstHeardAt.Compare(b.FirstHeardAt), strings.Compare(a.ScientificName, b.ScientificName))
	})
	return out
}

// sameBout says whether a stored bout is the one preparing would write, so
// it needn't be written again.
func sameBout(stored, made db.Bout) bool {
	return stored.Kind == made.Kind && stored.RecorderID == made.RecorderID &&
		stored.ScientificName == made.ScientificName && stored.CommonName == made.CommonName &&
		stored.FirstHeardAt.Equal(made.FirstHeardAt) && stored.LastHeardAt.Equal(made.LastHeardAt) &&
		slices.Equal(stored.DetectionIDs, made.DetectionIDs) && slices.Equal(stored.Best, made.Best) &&
		stored.MaxConfidence == made.MaxConfidence
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
