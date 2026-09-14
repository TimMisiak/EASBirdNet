package db

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// These structs are the stored documents. Their JSON tags are the document
// shape in Cosmos DB and in the local JSON file alike, and SCHEMA.md documents
// them field by field -- change one, change the other.
//
// They are not the API's JSON shapes. Handlers map from these to whatever the
// frontend is built against (see "API mapping" in SCHEMA.md).
//
// Timestamps are instants, stored in UTC. Calendar dates ("the evening a night
// began", "the day a card was pulled") are YYYY-MM-DD strings: sent as a
// timestamp, a date lands on the previous day for every reader west of UTC.

// Roles. A volunteer can upload cards; an admin can also manage the roster and
// the recorders.
const (
	RoleVolunteer = "volunteer"
	RoleAdmin     = "admin"
)

// User is someone on the roster. There is no password: the roster is the
// allow-list, and sign-in is delegated to Google or Microsoft.
type User struct {
	ID string `json:"id"`
	// Email is stored trimmed and lower-cased, and is unique across users.
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	// Identity is bound the first time the person signs in, so a later sign-in
	// is matched on the provider's stable subject rather than only the address.
	Identity     *Identity  `json:"identity,omitempty"`
	LastSignInAt *time.Time `json:"lastSignInAt,omitempty"`
	// RemovedAt takes someone off the roster without breaking the uploads and
	// reviews that point at them.
	RemovedAt *time.Time `json:"removedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// Identity is the external account a user signs in with.
type Identity struct {
	Provider string `json:"provider"` // "google" or "microsoft"
	Subject  string `json:"subject"`  // the OIDC "sub" claim
}

// Recorder is one listening station: the device and the place it is mounted,
// as a single entity. Its ID is printed on the unit, so a coordinator types it.
type Recorder struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Latitude  float64    `json:"latitude"`
	Longitude float64    `json:"longitude"`
	Model     string     `json:"model,omitempty"`
	Notes     string     `json:"notes,omitempty"`
	RetiredAt *time.Time `json:"retiredAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
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

// Upload is one SD card on its way from a recorder into storage and through
// BirdNET. Its ID is the reference a volunteer quotes in email.
type Upload struct {
	ID         string `json:"id"`
	RecorderID string `json:"recorderId"`
	// Recorder is copied from the recorder when the card is registered, so a
	// unit that is renamed or moved later doesn't change where this card was
	// heard.
	Recorder RecorderSnapshot `json:"recorder"`
	UserID   string           `json:"userId"`
	UserName string           `json:"userName"`

	PulledOn string  `json:"pulledOn"` // YYYY-MM-DD
	Notes    string  `json:"notes,omitempty"`
	Nights   []Night `json:"nights"`

	FileCount     int   `json:"fileCount"`
	TotalBytes    int64 `json:"totalBytes"`
	FilesUploaded int   `json:"filesUploaded"`
	BytesUploaded int64 `json:"bytesUploaded"`

	Status       string `json:"status"`
	StatusDetail string `json:"statusDetail,omitempty"`

	Analysis *Analysis `json:"analysis,omitempty"`

	StartedAt     time.Time  `json:"startedAt"`
	ReceivedAt    *time.Time `json:"receivedAt,omitempty"`
	ProcessedAt   *time.Time `json:"processedAt,omitempty"`
	ResultsSentAt *time.Time `json:"resultsSentAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// RecorderSnapshot is the part of a Recorder an upload keeps for itself.
type RecorderSnapshot struct {
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
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

// Analysis records how BirdNET was run over a card, so a detection can be
// traced back to the model and settings that produced it.
type Analysis struct {
	Model         string     `json:"model"` // e.g. "BirdNET_GLOBAL_6K_V2.4"
	MinConfidence float64    `json:"minConfidence"`
	Sensitivity   float64    `json:"sensitivity"`
	OverlapSec    float64    `json:"overlapSec"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

// Audio file statuses.
const (
	AudioPending  = "pending"  // registered from the card, not yet in storage
	AudioUploaded = "uploaded" // in blob storage, not yet analyzed
	AudioAnalyzed = "analyzed" // BirdNET has run over it
	AudioFailed   = "failed"   // unreadable, bad checksum, or analysis failed
)

// AudioFile is one recording from a card.
type AudioFile struct {
	ID         string `json:"id"`
	UploadID   string `json:"uploadId"`
	RecorderID string `json:"recorderId"`
	// Path is the file's path relative to the card root, with forward slashes.
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	Night     string `json:"night"` // YYYY-MM-DD, the evening the night began
	// RecordedAt is when the recording started, when it is known.
	RecordedAt  *time.Time `json:"recordedAt,omitempty"`
	DurationSec float64    `json:"durationSec,omitempty"`
	SampleRate  int        `json:"sampleRate,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	// BlobName is where the audio lives in the storage account's audio
	// container; see AudioBlobName.
	BlobName       string     `json:"blobName"`
	Status         string     `json:"status"`
	StatusDetail   string     `json:"statusDetail,omitempty"`
	UploadedAt     *time.Time `json:"uploadedAt,omitempty"`
	AnalyzedAt     *time.Time `json:"analyzedAt,omitempty"`
	DetectionCount int        `json:"detectionCount"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// Review statuses. Only confirmed detections are ever shown publicly.
const (
	ReviewUnreviewed = "unreviewed"
	ReviewConfirmed  = "confirmed"
	ReviewRejected   = "rejected"
)

// Detection is one BirdNET result above the analysis threshold: a species heard
// in one window of one audio file.
type Detection struct {
	ID          string `json:"id"`
	UploadID    string `json:"uploadId"`
	AudioFileID string `json:"audioFileId"`
	RecorderID  string `json:"recorderId"`
	// DetectedAt is the file's start time plus StartSec.
	DetectedAt     time.Time `json:"detectedAt"`
	Night          string    `json:"night"` // YYYY-MM-DD
	StartSec       float64   `json:"startSec"`
	EndSec         float64   `json:"endSec"`
	ScientificName string    `json:"scientificName"`
	CommonName     string    `json:"commonName"`
	Confidence     float64   `json:"confidence"`
	ReviewStatus   string    `json:"reviewStatus"`
	Review         *Review   `json:"review,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Review is a trained volunteer's verdict on a detection. A confirmed
// detection may correct the species BirdNET suggested.
type Review struct {
	UserID                  string    `json:"userId"`
	UserName                string    `json:"userName"`
	At                      time.Time `json:"at"`
	CorrectedScientificName string    `json:"correctedScientificName,omitempty"`
	CorrectedCommonName     string    `json:"correctedCommonName,omitempty"`
	Note                    string    `json:"note,omitempty"`
}

// Species is the species a detection counts as: the reviewer's correction when
// there is one, otherwise what BirdNET said.
func (d Detection) Species() (scientificName, commonName string) {
	if d.Review != nil && d.Review.CorrectedScientificName != "" {
		return d.Review.CorrectedScientificName, d.Review.CorrectedCommonName
	}
	return d.ScientificName, d.CommonName
}

// --- ids ---

// NewID returns a random id such as "usr_3f9a0c2b7d1e4a65".
func NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("db: reading random bytes: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// UploadID is a card's reference, the one a volunteer quotes in email: the day
// it was pulled and the recorder it came from. Recorder "SW-02" pulled on
// "2026-09-07" is "OWL-20260907-SR02". Registering the same card again yields
// the same id, which is how a resume finds what already landed.
func UploadID(pulledOn, recorderID string) string {
	return "OWL-" + strings.ReplaceAll(pulledOn, "-", "") + "-SR" + strings.TrimPrefix(recorderID, "SW-")
}

// CardPath normalizes a path on a card: forward slashes, no leading slash.
func CardPath(p string) string {
	p = path.Clean("/" + strings.ReplaceAll(p, `\`, "/"))
	return strings.TrimPrefix(p, "/")
}

// AudioFileID is derived from the card and the file's path, so registering the
// same card again (a resume) yields the same ids instead of duplicates.
func AudioFileID(uploadID, cardPath string) string {
	return "af_" + digest(uploadID, CardPath(cardPath))
}

// DetectionID is derived from what was heard where, so re-ingesting the same
// BirdNET output overwrites rather than duplicates.
func DetectionID(audioFileID string, startMs int64, scientificName string) string {
	return "det_" + digest(audioFileID, fmt.Sprint(startMs), scientificName)
}

// AudioBlobName is where a card's file is stored in the audio blob container.
func AudioBlobName(uploadID, cardPath string) string {
	return "uploads/" + uploadID + "/" + CardPath(cardPath)
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
