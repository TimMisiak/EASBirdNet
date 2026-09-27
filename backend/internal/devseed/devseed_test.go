package devseed

import (
	"path/filepath"
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
	if len(users) != len(roster) || len(recorders) != len(stations) {
		t.Fatalf("seeded %d users, %d recorders", len(users), len(recorders))
	}
	// Cards come from real uploads, so every one has audio behind it.
	uploads, _ := s.ListUploads(ctx, db.UploadFilter{})
	detections, _ := s.ListDetections(ctx, db.DetectionFilter{})
	if len(uploads) != 0 || len(detections) != 0 {
		t.Errorf("seeded %d uploads, %d detections; want none", len(uploads), len(detections))
	}
	for _, u := range users {
		if u.CreatedAt.After(now) || (u.LastSignInAt != nil && u.LastSignInAt.After(now)) {
			t.Errorf("%s: added or signed in in the future", u.Email)
		}
	}

	if seeded, err := Seed(ctx, s, now.Add(time.Hour)); err != nil || seeded {
		t.Errorf("seed again = %v, %v; want it left alone", seeded, err)
	}
	if again, _ := s.ListUsers(ctx); len(again) != len(users) {
		t.Errorf("seeding twice left %d users, want %d", len(again), len(users))
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
