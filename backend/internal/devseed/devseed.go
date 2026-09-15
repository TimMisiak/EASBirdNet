// Package devseed fills an empty development database with a small, believable
// program: a roster, five recorders, cards in every state and a few weeks of
// reviewed detections, so every screen has something on it.
//
// It only runs in dev mode (BIRDSENSE_DB=local). These people are made up, and
// the server refuses to bootstrap any of them into a real database; see
// IsPlaceholderPerson.
package devseed

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

type person struct {
	name, email, provider, role string
	addedDaysAgo                int
}

var roster = []person{
	{"Dana Coordinator", "dana@eastsideaudubon.org", "microsoft", db.RoleAdmin, 196},
	{"Jane Volunteer", "jane@example.com", "google", db.RoleVolunteer, 153},
	{"Tomas Reyes", "tomas.reyes@example.com", "google", db.RoleVolunteer, 153},
	{"Priya Raman", "praman@example.org", "microsoft", db.RoleVolunteer, 107},
	{"Ellen Park", "ellen.park@example.com", "google", db.RoleAdmin, 98},
	{"Marcus Lee", "m.lee@example.com", "google", db.RoleVolunteer, 44},
}

type bird struct {
	common, scientific string
	// perNight is the most detections a recorder that hears it logs in a night.
	perNight int
}

var birds = []bird{
	{"Barred Owl", "Strix varia", 6},
	{"Western Screech-Owl", "Megascops kennicottii", 3},
	{"Great Horned Owl", "Bubo virginianus", 2},
	{"Northern Saw-whet Owl", "Aegolius acadicus", 2},
	{"Barn Owl", "Tyto alba", 1},
}

type station struct {
	id, name     string
	lat, lon     float64
	addedDaysAgo int
	hears        []string // scientific names
}

var stations = []station{
	{"SW-01", "Lake Sammamish – Sunset Beach", 47.55600, -122.06400, 184, []string{"Bubo virginianus", "Tyto alba"}},
	{"SW-02", "Marymoor Park – Snag Row", 47.66021, -122.11384, 184, []string{"Strix varia", "Megascops kennicottii", "Bubo virginianus"}},
	{"SW-03", "Bridle Trails – North", 47.65600, -122.17000, 165, []string{"Strix varia", "Megascops kennicottii", "Aegolius acadicus"}},
	{"SW-04", "Juanita Bay – Boardwalk", 47.70284, -122.21167, 165, []string{"Strix varia"}},
	{"SW-05", "Soaring Eagle – East Loop", 47.63914, -121.99640, 118, []string{"Strix varia", "Megascops kennicottii", "Bubo virginianus", "Aegolius acadicus"}},
}

// A card per state the screens need to show, most recent first. The recent
// ones reach into the landing page's seven-night window.
type card struct {
	recorder, volunteer   string // recorder id, volunteer email
	pulledDaysAgo, nights int
	status, detail, notes string
	// short puts one short night in the manifest, for the check screen.
	short bool
}

var cards = []card{
	{"SW-05", "tomas.reyes@example.com", 1, 14, db.StatusProcessing, "", "", false},
	// Mid-flight, so "Resume upload" has something to resume.
	{"SW-02", "jane@example.com", 2, 14, db.StatusInterrupted, "", "Batteries at 20% when swapped.", true},
	{"SW-04", "ellen.park@example.com", 3, 14, db.StatusNeedsAttention, "2 unreadable", "Two files failed checksum", false},
	{"SW-01", "praman@example.org", 4, 9, db.StatusNeedsAttention, "Short card", "Recorder knocked over partway through", false},
	{"SW-03", "m.lee@example.com", 5, 14, db.StatusResultsSent, "", "", false},
	{"SW-05", "tomas.reyes@example.com", 15, 14, db.StatusResultsSent, "", "", false},
	{"SW-02", "jane@example.com", 16, 14, db.StatusResultsSent, "", "", false},
	{"SW-04", "jane@example.com", 30, 13, db.StatusResultsSent, "", "", false},
	{"SW-02", "jane@example.com", 33, 14, db.StatusResultsSent, "", "", false},
}

const (
	filesPerNight = 24 // one-hour files, around the clock
	bytesPerFile  = int64(383_000_000)
	// inFlightFiles is how far the interrupted card got.
	inFlightFiles = 214
)

var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// IsPlaceholderPerson reports whether a name or address belongs to the seeded
// roster. Those people exist to exercise the frontend, and the server refuses
// to put one into a production database.
func IsPlaceholderPerson(name, email string) bool {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	for _, p := range roster {
		if strings.EqualFold(p.email, email) || (name != "" && strings.EqualFold(p.name, name)) {
			return true
		}
	}
	return false
}

// Seed writes the placeholder program into s if it holds no users, recorders or
// uploads, and reports whether it did. Dates are relative to now, so a freshly
// seeded database always has recent cards and detections; it ages from there,
// and deleting the file seeds it again.
func Seed(ctx context.Context, s db.Store, now time.Time) (bool, error) {
	if empty, err := isEmpty(ctx, s); err != nil || !empty {
		return false, err
	}
	now = now.UTC().Truncate(time.Second)
	local := now.In(pacific)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, pacific)
	// Times are clamped to now: a card pulled yesterday evening can't have
	// finished arriving yet when the seed runs just after midnight.
	notAfterNow := func(t time.Time) time.Time {
		if t.After(now) {
			return now
		}
		return t
	}
	at := func(daysAgo, hour int) time.Time {
		return notAfterNow(time.Date(today.Year(), today.Month(), today.Day()-daysAgo, hour, 0, 0, 0, pacific).UTC())
	}

	users := map[string]db.User{}
	var reviewers []db.User
	for _, p := range roster {
		signedIn := at(min(p.addedDaysAgo, 3), 19)
		u, err := s.CreateUser(ctx, db.User{
			Email: p.email, Name: p.name, Role: p.role,
			Identity:     &db.Identity{Provider: p.provider, Subject: "dev-" + strings.Split(p.email, "@")[0]},
			LastSignInAt: &signedIn,
			CreatedAt:    at(p.addedDaysAgo, 10),
		})
		if err != nil {
			return false, fmt.Errorf("devseed: adding %s: %w", p.email, err)
		}
		users[p.email] = u
		if p.role == db.RoleAdmin {
			reviewers = append(reviewers, u)
		}
	}

	recorders := map[string]station{}
	for _, st := range stations {
		if _, err := s.CreateRecorder(ctx, db.Recorder{
			ID: st.id, Name: st.name, Latitude: st.lat, Longitude: st.lon, Model: "SwiftOne",
			CreatedAt: at(st.addedDaysAgo, 10),
		}); err != nil {
			return false, fmt.Errorf("devseed: adding recorder %s: %w", st.id, err)
		}
		recorders[st.id] = st
	}

	// A fixed seed, so two fresh databases seeded on the same day match.
	rng := rand.New(rand.NewPCG(2026, 9))
	for _, c := range cards {
		st, vol := recorders[c.recorder], users[c.volunteer]
		pulled := today.AddDate(0, 0, -c.pulledDaysAgo)
		up := db.Upload{
			ID:         db.UploadID(pulled.Format("2006-01-02"), st.id),
			RecorderID: st.id,
			Recorder:   db.RecorderSnapshot{Name: st.name, Latitude: st.lat, Longitude: st.lon},
			UserID:     vol.ID, UserName: vol.Name,
			PulledOn: pulled.Format("2006-01-02"), Notes: c.notes,
			Nights: manifest(pulled, c.nights, c.short),
			Status: c.status, StatusDetail: c.detail,
			StartedAt: at(c.pulledDaysAgo, 20),
		}
		for _, n := range up.Nights {
			up.FileCount += n.Files
			up.TotalBytes += n.Bytes
		}

		if c.status == db.StatusInterrupted {
			up.FilesUploaded = inFlightFiles
			up.BytesUploaded = up.TotalBytes * inFlightFiles / int64(up.FileCount)
		} else {
			up.FilesUploaded, up.BytesUploaded = up.FileCount, up.TotalBytes
			received := notAfterNow(up.StartedAt.Add(5 * time.Hour))
			up.ReceivedAt = &received
		}
		processed := c.status == db.StatusNeedsAttention || c.status == db.StatusResultsSent
		var reviewedAt time.Time
		if processed {
			started := notAfterNow(up.ReceivedAt.Add(time.Hour))
			finished := notAfterNow(started.Add(8 * time.Hour))
			up.Analysis = &db.Analysis{
				Model: "BirdNET_GLOBAL_6K_V2.4", MinConfidence: 0.5, Sensitivity: 1, OverlapSec: 0,
				StartedAt: started, FinishedAt: &finished,
			}
			up.ProcessedAt = &finished
			reviewedAt = notAfterNow(finished.Add(24 * time.Hour))
		}
		if c.status == db.StatusResultsSent {
			sent := notAfterNow(reviewedAt.Add(24 * time.Hour))
			up.ResultsSentAt = &sent
		}

		var heard []db.Detection
		if processed {
			reviewer := reviewers[len(up.ID)%len(reviewers)]
			heard = detections(rng, up, st, reviewer, reviewedAt)
			up.FilesAnalyzed, up.DetectionCount = up.FileCount, len(heard)
		}
		if _, err := s.CreateUpload(ctx, up); err != nil {
			return false, fmt.Errorf("devseed: adding card %s: %w", up.ID, err)
		}
		if processed {
			if err := s.UpsertDetections(ctx, up.ID, heard); err != nil {
				return false, fmt.Errorf("devseed: adding detections for %s: %w", up.ID, err)
			}
		}
	}
	return true, nil
}

func isEmpty(ctx context.Context, s db.Store) (bool, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return false, err
	}
	recorders, err := s.ListRecorders(ctx)
	if err != nil {
		return false, err
	}
	uploads, err := s.ListUploads(ctx, db.UploadFilter{})
	if err != nil {
		return false, err
	}
	return len(users)+len(recorders)+len(uploads) == 0, nil
}

// manifest is the nights on a card pulled on the given day: the last is the
// partial night before it came out, and short adds one short night mid-card.
func manifest(pulled time.Time, n int, short bool) []db.Night {
	nights := make([]db.Night, n)
	for i := range nights {
		files, flag := filesPerNight, ""
		switch {
		case i == n-1:
			files, flag = 3, "partial"
		case short && i == n/2:
			files, flag = 11, "short"
		}
		nights[i] = db.Night{
			Date:  pulled.AddDate(0, 0, i-n).Format("2006-01-02"),
			Files: files, Bytes: int64(files) * bytesPerFile, Flag: flag,
		}
	}
	return nights
}

// detections is what BirdNET found on a card and a reviewer went through:
// mostly confirmed, with the occasional false positive turned down. Their audio
// file ids are the ones those recordings would get; the files themselves
// aren't seeded, since nothing reads them yet.
func detections(rng *rand.Rand, up db.Upload, st station, reviewer db.User, reviewedAt time.Time) []db.Detection {
	var out []db.Detection
	for _, n := range up.Nights {
		evening, _ := time.ParseInLocation("2006-01-02", n.Date, pacific)
		for _, b := range birds {
			if !hears(st, b) {
				continue
			}
			for range rng.IntN(b.perNight + 1) {
				// An hour-long file between 8 p.m. and 5 a.m., and a 3-second window in it.
				file := time.Date(evening.Year(), evening.Month(), evening.Day(), 20+rng.IntN(10), 0, 0, 0, pacific)
				startSec := float64(3 * rng.IntN(1200))
				path := fmt.Sprintf("DATA/%s/%s.WAV", file.Format("20060102"), file.Format("20060102_150405"))
				d := db.Detection{
					AudioFileID: db.AudioFileID(up.ID, path), RecorderID: st.id,
					DetectedAt: file.Add(time.Duration(startSec) * time.Second).UTC(),
					Night:      n.Date, StartSec: startSec, EndSec: startSec + 3,
					ScientificName: b.scientific, CommonName: b.common,
					Confidence:   math.Round((0.5+0.49*rng.Float64())*100) / 100,
					ReviewStatus: db.ReviewConfirmed,
					Review:       &db.Review{UserID: reviewer.ID, UserName: reviewer.Name, At: reviewedAt},
				}
				if rng.IntN(8) == 0 {
					d.ReviewStatus = db.ReviewRejected
				}
				out = append(out, d)
			}
		}
	}
	return out
}

func hears(st station, b bird) bool {
	for _, name := range st.hears {
		if name == b.scientific {
			return true
		}
	}
	return false
}
