package devseed

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

func openTemp(t *testing.T) db.Store {
	t.Helper()
	s, err := db.OpenJSONFile(filepath.Join(t.TempDir(), "birdsense.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSeedFillsAnEmptyDatabaseOnce(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	now := time.Date(2026, time.September, 14, 18, 0, 0, 0, time.UTC)

	if seeded, err := Seed(ctx, s, now); err != nil || !seeded {
		t.Fatalf("seed an empty database = %v, %v; want seeded", seeded, err)
	}
	users, _ := s.ListUsers(ctx)
	recorders, _ := s.ListRecorders(ctx)
	uploads, _ := s.ListUploads(ctx, db.UploadFilter{})
	detections, _ := s.ListDetections(ctx, db.DetectionFilter{})
	if len(users) != len(roster) || len(recorders) != len(stations) || len(uploads) != len(cards) || len(detections) == 0 {
		t.Fatalf("seeded %d users, %d recorders, %d uploads, %d detections", len(users), len(recorders), len(uploads), len(detections))
	}

	if seeded, err := Seed(ctx, s, now.Add(time.Hour)); err != nil || seeded {
		t.Errorf("seed again = %v, %v; want it left alone", seeded, err)
	}
	if again, _ := s.ListUploads(ctx, db.UploadFilter{}); len(again) != len(uploads) {
		t.Errorf("seeding twice left %d uploads, want %d", len(again), len(uploads))
	}
}

// The screens trust the data to hang together, so the seed has to.
func TestSeedIsCoherent(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	now := time.Date(2026, time.March, 9, 7, 30, 0, 0, time.UTC) // the morning after DST starts
	if _, err := Seed(ctx, s, now); err != nil {
		t.Fatal(err)
	}

	uploads, _ := s.ListUploads(ctx, db.UploadFilter{})
	nights := map[string][]string{}
	for _, u := range uploads {
		if _, err := s.GetRecorder(ctx, u.RecorderID); err != nil {
			t.Errorf("%s: recorder %s: %v", u.ID, u.RecorderID, err)
		}
		if user, err := s.GetUser(ctx, u.UserID); err != nil || user.Name != u.UserName {
			t.Errorf("%s: user %s = %+v, %v", u.ID, u.UserID, user, err)
		}
		if u.ID != db.UploadID(u.PulledOn, u.RecorderID) {
			t.Errorf("%s: id doesn't match pulledOn %s and recorder %s", u.ID, u.PulledOn, u.RecorderID)
		}
		files := 0
		for _, n := range u.Nights {
			files += n.Files
			nights[u.ID] = append(nights[u.ID], n.Date)
			if n.Date >= u.PulledOn {
				t.Errorf("%s: night %s isn't before the card was pulled", u.ID, n.Date)
			}
		}
		if files != u.FileCount || u.FilesUploaded > u.FileCount {
			t.Errorf("%s: %d of %d files, nights add up to %d", u.ID, u.FilesUploaded, u.FileCount, files)
		}
		for _, at := range []*time.Time{&u.StartedAt, u.ReceivedAt, u.ProcessedAt, u.ResultsSentAt} {
			if at != nil && at.After(now) {
				t.Errorf("%s: timestamp %v is in the future", u.ID, at)
			}
		}
	}

	detections, _ := s.ListDetections(ctx, db.DetectionFilter{})
	recent := 0
	for _, d := range detections {
		if !slices.Contains(nights[d.UploadID], d.Night) {
			t.Errorf("%s: night %s isn't on card %s", d.ID, d.Night, d.UploadID)
		}
		if d.DetectedAt.After(now) || (d.Review != nil && d.Review.At.After(now)) {
			t.Errorf("%s: heard or reviewed in the future", d.ID)
		}
		if d.ReviewStatus == db.ReviewConfirmed && d.DetectedAt.After(now.AddDate(0, 0, -7)) {
			recent++
		}
	}
	// The landing page's default window has something in it.
	if recent == 0 {
		t.Error("no confirmed detections in the last 7 days")
	}
}

func TestSeedLeavesAStartedDatabaseAlone(t *testing.T) {
	ctx := t.Context()
	s := openTemp(t)
	if _, err := s.CreateUser(ctx, db.User{Email: "ada@audubon.test", Role: db.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	if seeded, err := Seed(ctx, s, time.Now()); err != nil || seeded {
		t.Errorf("seed = %v, %v; want nothing written", seeded, err)
	}
	if recorders, _ := s.ListRecorders(ctx); len(recorders) != 0 {
		t.Errorf("seeded %d recorders into a started database", len(recorders))
	}
}

func TestIsPlaceholderPerson(t *testing.T) {
	for _, tc := range []struct {
		name, email string
		want        bool
	}{
		{"", " DANA@eastsideaudubon.org ", true},
		{"ellen park", "ellen@eastsideaudubon.org", true},
		{"Ada Admin", "ada@eastsideaudubon.org", false},
		{"", "", false},
	} {
		if got := IsPlaceholderPerson(tc.name, tc.email); got != tc.want {
			t.Errorf("IsPlaceholderPerson(%q, %q) = %v, want %v", tc.name, tc.email, got, tc.want)
		}
	}
}
