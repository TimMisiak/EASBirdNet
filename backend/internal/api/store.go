package api

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Everything in this file is placeholder data. There is no database and no
// BirdNET ingestion yet, so the API keeps one in-memory copy of the program's
// state: enough for the frontend to be exercised end to end, reset on restart.
//
// When the real pipeline lands, the types stay and the store is replaced by
// queries. Nothing outside this file assumes the data is in memory.

// Station is one recorder in the field. The recorder ID doubles as the station
// ID -- a station has exactly one recorder in it at a time.
type Station struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	AddedOn   string  `json:"addedOn"` // YYYY-MM-DD
}

// Person is someone on the volunteer roster. There is no password: the roster
// is the allow-list, and sign-in is delegated to Google or Microsoft.
type Person struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Provider string `json:"provider"`
	Role     string `json:"role"`    // "volunteer" or "admin"
	AddedOn  string `json:"addedOn"` // YYYY-MM-DD
}

// Species is one confirmed-detection summary row on the public page. Only
// detections a trained volunteer has confirmed are counted here.
type Species struct {
	CommonName     string    `json:"commonName"`
	ScientificName string    `json:"scientificName"`
	Detections     int       `json:"detections"`
	Nights         int       `json:"nights"`
	Stations       []string  `json:"stations"`
	LastDetectedAt time.Time `json:"lastDetectedAt"`
}

// Night is one dusk-to-dawn block of recording found on a card.
type Night struct {
	Date  string `json:"date"` // YYYY-MM-DD, the evening the night began
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	// Flag is "" when the night looks normal, otherwise a short reason the
	// volunteer should eyeball it ("short", "partial").
	Flag string `json:"flag,omitempty"`
}

// Upload statuses. A card moves down this list; needs_attention is a side
// branch a coordinator resolves by hand.
const (
	StatusInProgress     = "in_progress"
	StatusInterrupted    = "interrupted"
	StatusProcessing     = "processing"
	StatusNeedsAttention = "needs_attention"
	StatusResultsSent    = "results_sent"
)

// Upload is one SD card on its way from a station into storage.
type Upload struct {
	Reference     string    `json:"reference"`
	StationID     string    `json:"stationId"`
	StationName   string    `json:"stationName"`
	VolunteerName string    `json:"volunteerName"`
	PulledOn      string    `json:"pulledOn"` // YYYY-MM-DD
	Notes         string    `json:"notes"`
	Nights        []Night   `json:"nights"`
	FileCount     int       `json:"fileCount"`
	FilesUploaded int       `json:"filesUploaded"`
	TotalBytes    int64     `json:"totalBytes"`
	BytesUploaded int64     `json:"bytesUploaded"`
	Status        string    `json:"status"`
	StatusDetail  string    `json:"statusDetail,omitempty"`
	StartedAt     time.Time `json:"startedAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// store holds the whole placeholder program. One mutex covers all of it --
// there is nothing here worth finer-grained locking.
type store struct {
	mu       sync.Mutex
	stations []Station
	people   []Person
	// lastPersonID only counts up, so a removed person's id is never handed to
	// someone new while it may still be in an admin's open tab.
	lastPersonID int
	uploads      []Upload
	species      []Species
	program      ProgramStats
}

// ProgramStats are the three headline numbers on the public page.
type ProgramStats struct {
	Recorders           int `json:"recorders"`
	NightsRecorded      int `json:"nightsRecorded"`
	ConfirmedDetections int `json:"confirmedDetections"`
}

func newStore() *store {
	s := &store{
		program: ProgramStats{Recorders: 5, NightsRecorded: 1284, ConfirmedDetections: 597},
	}
	s.seed()
	return s
}

// day is a calendar date, not an instant. The roster records the day someone
// was added, and sending it as a timestamp makes it land on the previous date
// for every reader west of UTC.
func day(y int, m time.Month, d int) string {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func at(y int, m time.Month, d, hh, mm int) time.Time {
	// Pacific is the only timezone this program cares about; fall back to a
	// fixed offset when the container has no tzdata.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PDT", -7*3600)
	}
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

// IsPlaceholderPerson reports whether a name or address belongs to the
// placeholder roster seeded below. Those people exist to exercise the
// frontend, and the server refuses to put one into a production database.
func IsPlaceholderPerson(name, email string) bool {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	for _, p := range newStore().People() {
		if strings.EqualFold(p.Email, email) || (name != "" && strings.EqualFold(p.Name, name)) {
			return true
		}
	}
	return false
}

func (s *store) seed() {
	s.stations = []Station{
		{"SW-01", "Lake Sammamish – Sunset Beach", 47.55600, -122.06400, day(2026, time.March, 14)},
		{"SW-02", "Marymoor Park – Snag Row", 47.66021, -122.11384, day(2026, time.March, 14)},
		{"SW-03", "Bridle Trails – North", 47.65600, -122.17000, day(2026, time.April, 2)},
		{"SW-04", "Juanita Bay – Boardwalk", 47.70284, -122.21167, day(2026, time.April, 2)},
		{"SW-05", "Soaring Eagle – East Loop", 47.63914, -121.99640, day(2026, time.May, 19)},
	}

	s.people = []Person{
		{"p1", "Dana Coordinator", "dana@eastsideaudubon.org", "Microsoft", RoleAdmin, day(2026, time.March, 2)},
		{"p2", "Jane Volunteer", "jane@example.com", "Google", RoleVolunteer, day(2026, time.April, 14)},
		{"p3", "Tomas Reyes", "tomas.reyes@example.com", "Google", RoleVolunteer, day(2026, time.April, 14)},
		{"p4", "Priya Raman", "praman@example.org", "Microsoft", RoleVolunteer, day(2026, time.May, 30)},
		{"p5", "Ellen Park", "ellen.park@example.com", "Google", RoleAdmin, day(2026, time.June, 8)},
		{"p6", "Marcus Lee", "m.lee@example.com", "Google", RoleVolunteer, day(2026, time.August, 1)},
	}
	s.lastPersonID = len(s.people)

	s.species = []Species{
		{"Barred Owl", "Strix varia", 412, 7,
			[]string{"Marymoor Park – Snag Row", "Juanita Bay – Boardwalk", "Bridle Trails – North", "Soaring Eagle – East Loop"},
			at(2026, time.September, 13, 3, 12)},
		{"Western Screech-Owl", "Megascops kennicottii", 96, 6,
			[]string{"Soaring Eagle – East Loop", "Bridle Trails – North", "Marymoor Park – Snag Row"},
			at(2026, time.September, 12, 23, 48)},
		{"Great Horned Owl", "Bubo virginianus", 58, 5,
			[]string{"Marymoor Park – Snag Row", "Lake Sammamish – Sunset Beach", "Soaring Eagle – East Loop"},
			at(2026, time.September, 13, 4, 40)},
		{"Northern Saw-whet Owl", "Aegolius acadicus", 24, 3,
			[]string{"Soaring Eagle – East Loop", "Bridle Trails – North"},
			at(2026, time.September, 11, 2, 5)},
		{"Barn Owl", "Tyto alba", 7, 2,
			[]string{"Lake Sammamish – Sunset Beach"},
			at(2026, time.September, 9, 1, 22)},
	}

	// One card mid-flight (so "Resume upload" has something to resume), one
	// card per other state the coordinator's table needs to show.
	inFlight := Upload{
		Reference: "OWL-20260907-SR02", StationID: "SW-02", StationName: "Marymoor Park – Snag Row",
		VolunteerName: "Jane Volunteer", PulledOn: "2026-09-07",
		Notes:  "Batteries at 20% when swapped.",
		Nights: sampleNights(time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC), 14),
		Status: StatusInterrupted, StartedAt: at(2026, time.September, 12, 20, 14),
	}
	inFlight.FileCount, inFlight.TotalBytes = totals(inFlight.Nights)
	inFlight.FilesUploaded = 214
	inFlight.BytesUploaded = int64(float64(inFlight.TotalBytes) * 214 / float64(inFlight.FileCount))
	inFlight.UpdatedAt = inFlight.StartedAt

	s.uploads = []Upload{
		inFlight,
		done("OWL-20260906-SR05", "SW-05", "Soaring Eagle – East Loop", "Tomas Reyes", "2026-09-06", 14, 336, StatusProcessing, "", ""),
		done("OWL-20260903-SR01", "SW-01", "Lake Sammamish – Sunset Beach", "Priya Raman", "2026-09-03", 9, 212, StatusNeedsAttention, "Short card", "Recorder knocked over Aug 30"),
		done("OWL-20260831-SR04", "SW-04", "Juanita Bay – Boardwalk", "Ellen Park", "2026-08-31", 14, 334, StatusNeedsAttention, "2 unreadable", "Two files failed checksum"),
		done("OWL-20260824-SR02", "SW-02", "Marymoor Park – Snag Row", "Jane Volunteer", "2026-08-24", 14, 336, StatusResultsSent, "", ""),
		done("OWL-20260821-SR03", "SW-03", "Bridle Trails – North", "Marcus Lee", "2026-08-21", 14, 336, StatusResultsSent, "", ""),
		done("OWL-20260810-SR04", "SW-04", "Juanita Bay – Boardwalk", "Jane Volunteer", "2026-08-10", 13, 312, StatusResultsSent, "", ""),
		done("OWL-20260727-SR02", "SW-02", "Marymoor Park – Snag Row", "Jane Volunteer", "2026-07-27", 14, 336, StatusResultsSent, "", ""),
	}
}

// done builds a finished card. Its per-night breakdown is not interesting once
// the card is in, so it carries counts only.
func done(ref, stationID, station, who, pulled string, nights, files int, status, detail, note string) Upload {
	const bytesPerFile = int64(410_000_000)
	t, _ := time.Parse("2006-01-02", pulled)
	return Upload{
		Reference: ref, StationID: stationID, StationName: station, VolunteerName: who,
		PulledOn: pulled, Notes: note,
		Nights:    make([]Night, nights),
		FileCount: files, FilesUploaded: files,
		TotalBytes: int64(files) * bytesPerFile, BytesUploaded: int64(files) * bytesPerFile,
		Status: status, StatusDetail: detail,
		StartedAt: t, UpdatedAt: t,
	}
}

// sampleNights is the manifest a full card usually has: 24 one-hour files a
// night, with the two odd nights the check screen is built to surface.
func sampleNights(first time.Time, n int) []Night {
	const bytesPerFile = int64(383_000_000)
	nights := make([]Night, n)
	for i := range nights {
		files := 24
		flag := ""
		switch i {
		case 6:
			files, flag = 11, "short"
		case n - 1:
			files, flag = 3, "partial"
		}
		nights[i] = Night{
			Date:  first.AddDate(0, 0, i).Format("2006-01-02"),
			Files: files, Bytes: int64(files) * bytesPerFile, Flag: flag,
		}
	}
	return nights
}

func today() string {
	return time.Now().Format("2006-01-02")
}

func totals(nights []Night) (files int, bytes int64) {
	for _, n := range nights {
		files += n.Files
		bytes += n.Bytes
	}
	return files, bytes
}

// --- reads ---

func (s *store) Stations() []Station {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Station(nil), s.stations...)
}

func (s *store) People() []Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Person(nil), s.people...)
}

func (s *store) SpeciesSummary() []Species {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Species(nil), s.species...)
}

func (s *store) Program() ProgramStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.program
}

// Uploads returns every card, newest first. A non-empty volunteer name filters
// to that person's cards.
func (s *store) Uploads(volunteer string) []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Upload, 0, len(s.uploads))
	for _, u := range s.uploads {
		if volunteer == "" || u.VolunteerName == volunteer {
			out = append(out, u)
		}
	}
	return out
}

func (s *store) Upload(ref string) (Upload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.uploads {
		if u.Reference == ref {
			return u, true
		}
	}
	return Upload{}, false
}

func (s *store) PersonByEmail(email string) (Person, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.people {
		if strings.EqualFold(p.Email, email) {
			return p, true
		}
	}
	return Person{}, false
}

// FirstWithRole is how the placeholder sign-in picks an account: there is no
// identity provider wired up, so "sign in as an admin" means "be the first
// admin on the roster".
func (s *store) FirstWithRole(role string) (Person, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.people {
		if p.Role == role {
			return p, true
		}
	}
	return Person{}, false
}

// --- writes ---

// AddStation registers a recorder. The ID is printed on the unit itself, so the
// coordinator types it; an empty one falls back to the next in sequence.
func (s *store) AddStation(id, name string, lat, lon float64) (Station, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		id = fmt.Sprintf("SW-%02d", len(s.stations)+1)
	}
	for _, existing := range s.stations {
		if strings.EqualFold(existing.ID, id) {
			return Station{}, fmt.Errorf("recorder %s is already in the field", existing.ID)
		}
	}
	st := Station{
		ID: id, Name: name,
		Latitude: lat, Longitude: lon, AddedOn: today(),
	}
	s.stations = append(s.stations, st)
	return st, nil
}

// ErrNoSuchStation means no recorder has the id asked for.
var ErrNoSuchStation = errors.New("no recorder has that id")

// UpdateStation renames or moves a recorder. The id is printed on the unit, so
// it never changes, and cards already sent keep the name and place they were
// recorded under.
func (s *store) UpdateStation(id, name string, lat, lon float64) (Station, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.stations, func(st Station) bool { return st.ID == id })
	if i < 0 {
		return Station{}, ErrNoSuchStation
	}
	st := s.stations[i]
	st.Name, st.Latitude, st.Longitude = name, lat, lon
	s.stations[i] = st
	return st, nil
}

// RemoveStation takes a recorder off the list. The stored version will set
// retiredAt instead; either way, past cards keep their own copy of the name.
func (s *store) RemoveStation(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.stations, func(st Station) bool { return st.ID == id })
	if i < 0 {
		return ErrNoSuchStation
	}
	s.stations = slices.Delete(s.stations, i, i+1)
	return nil
}

func (s *store) AddPerson(name, email, role string) Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		name = email
	}
	s.lastPersonID++
	p := Person{
		ID: fmt.Sprintf("p%d", s.lastPersonID), Name: name, Email: email,
		Provider: "—", Role: role, AddedOn: today(),
	}
	s.people = append(s.people, p)
	return p
}

// Roster rules. The store checks them under its lock, so two admins removing
// each other at the same moment can't leave the program with no admin at all.
var (
	ErrNoSuchPerson = errors.New("that person isn't on the roster")
	ErrEmailTaken   = errors.New("that address is already on the roster")
	ErrLastAdmin    = errors.New("the roster needs an admin; make someone else an admin first")
	ErrRemoveSelf   = errors.New("you can't remove yourself from the roster")
)

// UpdatePerson sets someone's name, address and role. An empty name falls back
// to the address, as it does in AddPerson.
func (s *store) UpdatePerson(id, name, email, role string) (Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.people, func(p Person) bool { return p.ID == id })
	if i < 0 {
		return Person{}, ErrNoSuchPerson
	}
	for j, other := range s.people {
		if j != i && strings.EqualFold(other.Email, email) {
			return Person{}, ErrEmailTaken
		}
	}
	p := s.people[i]
	if p.Role == RoleAdmin && role != RoleAdmin && s.admins() == 1 {
		return Person{}, ErrLastAdmin
	}
	if name == "" {
		name = email
	}
	// Placeholder cards point at their volunteer by name (stored uploads use
	// userId), so a rename has to take the person's cards with it.
	for k := range s.uploads {
		if s.uploads[k].VolunteerName == p.Name {
			s.uploads[k].VolunteerName = name
		}
	}
	p.Name, p.Email, p.Role = name, email, role
	s.people[i] = p
	return p, nil
}

// RemovePerson takes someone off the roster; by is the id of the admin doing
// it. The stored version will set removedAt instead, so cards still resolve.
func (s *store) RemovePerson(id, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.people, func(p Person) bool { return p.ID == id })
	switch {
	case i < 0:
		return ErrNoSuchPerson
	case id == by:
		return ErrRemoveSelf
	case s.people[i].Role == RoleAdmin && s.admins() == 1:
		return ErrLastAdmin
	}
	s.people = slices.Delete(s.people, i, i+1)
	return nil
}

// admins counts the admins on the roster. The caller holds s.mu.
func (s *store) admins() int {
	n := 0
	for _, p := range s.people {
		if p.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// CreateUpload registers a card the volunteer is about to send. The reference
// is what the volunteer quotes in email, so it is derived from the pull date
// and the recorder rather than being opaque.
func (s *store) CreateUpload(u Upload) Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	compact := strings.ReplaceAll(u.PulledOn, "-", "")
	u.Reference = fmt.Sprintf("OWL-%s-SR%s", compact, strings.TrimPrefix(u.StationID, "SW-"))
	u.FileCount, u.TotalBytes = totals(u.Nights)
	u.Status = StatusInProgress
	u.StartedAt = time.Now().UTC().Truncate(time.Second)
	u.UpdatedAt = u.StartedAt

	for i, existing := range s.uploads {
		if existing.Reference == u.Reference {
			// Same card again: this is a resume, so keep what already landed.
			u.FilesUploaded, u.BytesUploaded = existing.FilesUploaded, existing.BytesUploaded
			s.uploads[i] = u
			return u
		}
	}
	s.uploads = append([]Upload{u}, s.uploads...)
	return u
}

// RecordProgress marks files as received. It only ever moves forward, so a
// retried batch is harmless.
func (s *store) RecordProgress(ref string, files int, bytes int64, status string) (Upload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, u := range s.uploads {
		if u.Reference != ref {
			continue
		}
		if files > u.FilesUploaded {
			u.FilesUploaded = min(files, u.FileCount)
		}
		if bytes > u.BytesUploaded {
			u.BytesUploaded = min(bytes, u.TotalBytes)
		}
		if status != "" {
			u.Status = status
		}
		if u.FilesUploaded >= u.FileCount {
			u.Status = StatusProcessing
		}
		u.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		s.uploads[i] = u
		return u, true
	}
	return Upload{}, false
}
