package api

import (
	"strings"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo; see pacific

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
)

// These are the API's JSON shapes -- what the frontend is built against. They
// are not the stored documents in internal/db: the *Of functions below map one
// onto the other, following "API mapping" in SCHEMA.md.

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
	Role     string `json:"role"`    // db.RoleVolunteer or db.RoleAdmin
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

// ProgramStats are the three headline numbers on the public page.
type ProgramStats struct {
	Recorders           int `json:"recorders"`
	NightsRecorded      int `json:"nightsRecorded"`
	ConfirmedDetections int `json:"confirmedDetections"`
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

// CardFile is one audio file on a card. The browser lists them when it
// registers a card, and gets them back with where each one stands.
type CardFile struct {
	Path   string `json:"path"` // relative to the card's root, forward slashes
	Bytes  int64  `json:"bytes"`
	Night  string `json:"night"`            // YYYY-MM-DD, the evening the night began
	Status string `json:"status,omitempty"` // db.AudioPending, db.AudioUploaded, ...; ignored on the way in
}

// pacific is the program's timezone: the day someone was added is the day it
// was in East King County. time/tzdata is embedded because the runtime image
// has no zoneinfo, and a fixed offset would be an hour out half the year.
var pacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

// dateOf is the calendar date of an instant, in Pacific time.
func dateOf(t time.Time) string {
	return t.In(pacific).Format("2006-01-02")
}

func stationOf(r db.Recorder) Station {
	return Station{
		ID: r.ID, Name: r.Name,
		Latitude: r.Latitude, Longitude: r.Longitude,
		AddedOn: dateOf(r.CreatedAt),
	}
}

func personOf(u db.User) Person {
	// The provider is only known once someone has signed in with it.
	provider := "—"
	if u.Identity != nil && u.Identity.Provider != "" {
		provider = strings.ToUpper(u.Identity.Provider[:1]) + u.Identity.Provider[1:]
	}
	return Person{
		ID: u.ID, Name: u.Name, Email: u.Email,
		Provider: provider, Role: u.Role, AddedOn: dateOf(u.CreatedAt),
	}
}

// uploadOf shows a card as it was registered: the station name and volunteer
// name are the copies taken then, not the recorder's or person's today.
func uploadOf(u db.Upload) Upload {
	nights := make([]Night, len(u.Nights))
	for i, n := range u.Nights {
		nights[i] = Night(n)
	}
	return Upload{
		Reference: u.ID, StationID: u.RecorderID, StationName: u.Recorder.Name,
		VolunteerName: u.UserName, PulledOn: u.PulledOn, Notes: u.Notes, Nights: nights,
		FileCount: u.FileCount, FilesUploaded: u.FilesUploaded,
		TotalBytes: u.TotalBytes, BytesUploaded: u.BytesUploaded,
		Status: u.Status, StatusDetail: u.StatusDetail,
		StartedAt: u.StartedAt, UpdatedAt: u.UpdatedAt,
	}
}

func cardFileOf(f db.AudioFile) CardFile {
	return CardFile{Path: f.Path, Bytes: f.SizeBytes, Night: f.Night, Status: f.Status}
}

func mapAll[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
