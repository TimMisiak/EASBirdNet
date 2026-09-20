package api

import (
	"strings"
	"time"
	_ "time/tzdata" // the runtime image has no zoneinfo; see pacific

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/retention"
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

// QueueStatus is where BirdNET analysis stands on this server: not a stored
// document but the running process's own state, sent alongside the
// coordinator's card lists so a card sitting in processing says why. States
// are internal/analysis's, plus "off" for a server running no queue.
type QueueStatus struct {
	State string `json:"state"`
	// Detail is what is wrong, when anything is. It names server-side paths,
	// so it goes only to the admin routes.
	Detail string `json:"detail,omitempty"`
	// Since is when the queue entered this state, absent when it has none.
	Since *time.Time `json:"since,omitempty"`
}

// Upload is one SD card on its way from a station into storage.
type Upload struct {
	Reference     string  `json:"reference"`
	StationID     string  `json:"stationId"`
	StationName   string  `json:"stationName"`
	VolunteerName string  `json:"volunteerName"`
	PulledOn      string  `json:"pulledOn"` // YYYY-MM-DD
	Notes         string  `json:"notes"`
	Nights        []Night `json:"nights"`
	FileCount     int     `json:"fileCount"`
	FilesUploaded int     `json:"filesUploaded"`
	TotalBytes    int64   `json:"totalBytes"`
	BytesUploaded int64   `json:"bytesUploaded"`
	// FilesAnalyzed, FilesFailed and DetectionCount are BirdNET's progress
	// through the card once it has been received.
	FilesAnalyzed  int        `json:"filesAnalyzed"`
	FilesFailed    int        `json:"filesFailed"`
	DetectionCount int        `json:"detectionCount"`
	Status         string     `json:"status"`
	StatusDetail   string     `json:"statusDetail,omitempty"`
	Analysis       *Analysis  `json:"analysis,omitempty"`
	StartedAt      time.Time  `json:"startedAt"`
	ReceivedAt     *time.Time `json:"receivedAt,omitempty"`
	ProcessedAt    *time.Time `json:"processedAt,omitempty"`
	// AudioExpiresAt is when the card's original recordings are due to be
	// removed, and AudioDeletedAt when they were. Only one of them is ever
	// set, and neither is when retention is off. The card's detections and
	// their clips are kept either way.
	AudioExpiresAt *time.Time `json:"audioExpiresAt,omitempty"`
	AudioDeletedAt *time.Time `json:"audioDeletedAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// Analysis is how BirdNET was run over a card.
type Analysis struct {
	Model         string     `json:"model,omitempty"` // known once the first file is done
	MinConfidence float64    `json:"minConfidence"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

// AudioFile is one file on a card as a coordinator sees it: where it is in
// being sent and analyzed.
type AudioFile struct {
	ID           string     `json:"id"`
	Path         string     `json:"path"`
	Night        string     `json:"night"` // YYYY-MM-DD
	Bytes        int64      `json:"bytes"`
	Status       string     `json:"status"` // db.AudioPending, db.AudioUploaded, db.AudioAnalyzing, ...
	StatusDetail string     `json:"statusDetail,omitempty"`
	RecordedAt   *time.Time `json:"recordedAt,omitempty"`
	UploadedAt   *time.Time `json:"uploadedAt,omitempty"`
	AnalyzedAt   *time.Time `json:"analyzedAt,omitempty"`
	// AudioDeletedAt is when the recording itself was removed under the
	// retention policy. Status still says what BirdNET made of it, and its
	// detections and their clips are still there.
	AudioDeletedAt *time.Time `json:"audioDeletedAt,omitempty"`
	DetectionCount int        `json:"detectionCount"`
}

// Detection is one thing BirdNET heard in a file: a species over a run of
// consecutive windows, at the confidence of the best of them.
type Detection struct {
	ID             string    `json:"id"`
	AudioFileID    string    `json:"audioFileId"`
	StartSec       float64   `json:"startSec"`
	EndSec         float64   `json:"endSec"`
	DetectedAt     time.Time `json:"detectedAt"`
	ScientificName string    `json:"scientificName"`
	CommonName     string    `json:"commonName"`
	Confidence     float64   `json:"confidence"`
	// Clip is the stretch of the file that can be played, when one was cut.
	Clip         *Clip   `json:"clip,omitempty"`
	ReviewStatus string  `json:"reviewStatus"` // db.ReviewUnreviewed, db.ReviewConfirmed or db.ReviewRejected
	Review       *Review `json:"review,omitempty"`
}

// ListedDetection is a detection in the list of every card's detections, with
// the card it is on and where that card was recorded.
type ListedDetection struct {
	Detection
	Reference   string `json:"reference"`
	StationName string `json:"stationName"`
	Night       string `json:"night"` // YYYY-MM-DD
}

// DetectionWindow is the date range the server bounded a list of every card's
// detections to because the request named none of its own. Absent when the
// request asked for its own dates.
type DetectionWindow struct {
	Since time.Time `json:"since"`
	Days  int       `json:"days"`
}

// SpeciesCount is one species BirdNET labelled detections with, and how many.
type SpeciesCount struct {
	ScientificName string `json:"scientificName"`
	CommonName     string `json:"commonName"`
	Detections     int    `json:"detections"`
}

// Clip is where a detection's clip sits in its file, in seconds.
type Clip struct {
	StartSec float64 `json:"startSec"`
	EndSec   float64 `json:"endSec"`
}

// Review is who gave a detection its review status, and when.
type Review struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
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
// name are the copies taken then, not the recorder's or person's today. The
// retention policy is the server's, not the card's, so it is passed in.
func uploadOf(u db.Upload, keep retention.Policy) Upload {
	nights := make([]Night, len(u.Nights))
	for i, n := range u.Nights {
		nights[i] = Night(n)
	}
	return Upload{
		Reference: u.ID, StationID: u.RecorderID, StationName: u.Recorder.Name,
		VolunteerName: u.UserName, PulledOn: u.PulledOn, Notes: u.Notes, Nights: nights,
		FileCount: u.FileCount, FilesUploaded: u.FilesUploaded,
		TotalBytes: u.TotalBytes, BytesUploaded: u.BytesUploaded,
		FilesAnalyzed: u.FilesAnalyzed, FilesFailed: u.FilesFailed, DetectionCount: u.DetectionCount,
		Status: u.Status, StatusDetail: u.StatusDetail, Analysis: analysisOf(u.Analysis),
		StartedAt: u.StartedAt, ReceivedAt: u.ReceivedAt, ProcessedAt: u.ProcessedAt,
		AudioExpiresAt: keep.ExpiresAt(u), AudioDeletedAt: u.AudioDeletedAt, UpdatedAt: u.UpdatedAt,
	}
}

func analysisOf(a *db.Analysis) *Analysis {
	if a == nil {
		return nil
	}
	return &Analysis{Model: a.Model, MinConfidence: a.MinConfidence, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
}

func audioFileOf(f db.AudioFile) AudioFile {
	return AudioFile{
		ID: f.ID, Path: f.Path, Night: f.Night, Bytes: f.SizeBytes,
		Status: f.Status, StatusDetail: f.StatusDetail,
		RecordedAt: f.RecordedAt, UploadedAt: f.UploadedAt, AnalyzedAt: f.AnalyzedAt,
		AudioDeletedAt: f.AudioDeletedAt, DetectionCount: f.DetectionCount,
	}
}

func detectionOf(d db.Detection) Detection {
	out := Detection{
		ID: d.ID, AudioFileID: d.AudioFileID, StartSec: d.StartSec, EndSec: d.EndSec, DetectedAt: d.DetectedAt,
		ScientificName: d.ScientificName, CommonName: d.CommonName,
		Confidence: d.Confidence, ReviewStatus: d.ReviewStatus,
	}
	if d.Clip != nil {
		out.Clip = &Clip{StartSec: d.Clip.StartSec, EndSec: d.Clip.EndSec}
	}
	if d.Review != nil {
		out.Review = &Review{By: d.Review.UserName, At: d.Review.At}
	}
	return out
}

func cardFileOf(f db.AudioFile) CardFile {
	return CardFile{Path: f.Path, Bytes: f.SizeBytes, Night: f.Night, Status: f.Status}
}

// cardFilesOf is a card's list as it was last read off the card: every file
// but those taken off it when the card was registered again.
func cardFilesOf(stored []db.AudioFile) []CardFile {
	out := []CardFile{}
	for _, f := range stored {
		if f.StatusDetail != db.AudioDetailNotOnCard {
			out = append(out, cardFileOf(f))
		}
	}
	return out
}

func mapAll[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
