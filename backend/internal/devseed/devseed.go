// Package devseed fills an empty development database with a small, believable
// program: a roster to sign in as and five recorders to send cards from. It
// adds no cards: those come from uploading real audio, so every card in dev
// has files behind it.
//
// It only runs in dev mode (BIRDSENSE_DB=local). These people are made up, and
// the server refuses to bootstrap any of them into a real database; see
// IsPlaceholderPerson.
package devseed

import (
	"context"
	"fmt"
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

type station struct {
	id, name     string
	lat, lon     float64
	addedDaysAgo int
}

var stations = []station{
	{"SW-01", "Lake Sammamish – Sunset Beach", 47.55600, -122.06400, 184},
	{"SW-02", "Marymoor Park – Snag Row", 47.66021, -122.11384, 184},
	{"SW-03", "Bridle Trails – North", 47.65600, -122.17000, 165},
	{"SW-04", "Juanita Bay – Boardwalk", 47.70284, -122.21167, 165},
	{"SW-05", "Soaring Eagle – East Loop", 47.63914, -121.99640, 118},
}

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
// uploads, and reports whether it did. Dates are relative to now, and deleting
// the file seeds it again.
func Seed(ctx context.Context, s db.Store, now time.Time) (bool, error) {
	if empty, err := isEmpty(ctx, s); err != nil || !empty {
		return false, err
	}
	local := now.In(pacific)
	at := func(daysAgo, hour int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day()-daysAgo, hour, 0, 0, 0, pacific).UTC()
	}

	for _, p := range roster {
		signedIn := at(min(p.addedDaysAgo, 3), 19)
		if _, err := s.CreateUser(ctx, db.User{
			Email: p.email, Name: p.name, Role: p.role,
			Identity:     &db.Identity{Provider: p.provider, Subject: "dev-" + strings.Split(p.email, "@")[0]},
			LastSignInAt: &signedIn,
			CreatedAt:    at(p.addedDaysAgo, 10),
		}); err != nil {
			return false, fmt.Errorf("devseed: adding %s: %w", p.email, err)
		}
	}

	for _, st := range stations {
		if _, err := s.CreateRecorder(ctx, db.Recorder{
			ID: st.id, Name: st.name, Latitude: st.lat, Longitude: st.lon, Model: "SwiftOne",
			CreatedAt: at(st.addedDaysAgo, 10),
		}); err != nil {
			return false, fmt.Errorf("devseed: adding recorder %s: %w", st.id, err)
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
