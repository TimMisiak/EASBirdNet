// Package db is Birdsense's persistence layer. One Store interface has two
// backends: Azure Cosmos DB (NoSQL API) in production and a single JSON file
// for local development. Config.Backend picks between them.
//
// The interface is entity-specific rather than a generic query builder. Both
// backends have to produce the same answers, and a short list of named methods
// is far easier to keep identical than a query language. Filtering and sorting
// rules live in this file and are shared by both.
package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	// ErrNotFound means no document has that id (or matches that lookup).
	ErrNotFound = errors.New("db: not found")
	// ErrConflict means a create collided with an existing id or unique field,
	// or an update lost a race it couldn't retry its way out of.
	ErrConflict = errors.New("db: conflict")
)

// Store is everything the API can ask of the database.
//
// Creates fill in CreatedAt and UpdatedAt; updates and upserts refresh
// UpdatedAt. An update is a read-modify-write: mutate receives the current
// document, and returning an error from it abandons the change. mutate may run
// more than once if the document changes underneath it, so it must not have
// side effects. It cannot change a document's id or partition key.
type Store interface {
	GetUser(ctx context.Context, id string) (User, error)
	// GetUserByEmail matches case-insensitively.
	GetUserByEmail(ctx context.Context, email string) (User, error)
	// ListUsers returns everyone, removed users included, sorted by name.
	ListUsers(ctx context.Context) ([]User, error)
	// CreateUser assigns an id when u.ID is empty. ErrConflict if the email is
	// already on the roster.
	CreateUser(ctx context.Context, u User) (User, error)
	UpdateUser(ctx context.Context, id string, mutate func(*User) error) (User, error)

	GetRecorder(ctx context.Context, id string) (Recorder, error)
	// ListRecorders returns every recorder, retired ones included, sorted by id.
	ListRecorders(ctx context.Context) ([]Recorder, error)
	// CreateRecorder requires an id. ErrConflict if it is taken.
	CreateRecorder(ctx context.Context, r Recorder) (Recorder, error)
	UpdateRecorder(ctx context.Context, id string, mutate func(*Recorder) error) (Recorder, error)

	GetUpload(ctx context.Context, id string) (Upload, error)
	// ListUploads returns matching uploads, newest card first.
	ListUploads(ctx context.Context, f UploadFilter) ([]Upload, error)
	// CreateUpload requires an id. ErrConflict if it is taken.
	CreateUpload(ctx context.Context, u Upload) (Upload, error)
	UpdateUpload(ctx context.Context, id string, mutate func(*Upload) error) (Upload, error)
	// DeleteUpload removes a card for good, with its audio files and
	// detections. The card goes last, so a delete that fails part way leaves
	// it listed to be deleted again. ErrNotFound if there is no such card;
	// anything still stored under its id is removed all the same.
	DeleteUpload(ctx context.Context, id string) error

	GetAudioFile(ctx context.Context, uploadID, id string) (AudioFile, error)
	// ListAudioFiles returns a card's files sorted by path.
	ListAudioFiles(ctx context.Context, uploadID string) ([]AudioFile, error)
	// UpsertAudioFiles writes whole documents, creating or replacing each. A
	// file with no id gets AudioFileID(uploadID, path).
	UpsertAudioFiles(ctx context.Context, uploadID string, files []AudioFile) error
	UpdateAudioFile(ctx context.Context, uploadID, id string, mutate func(*AudioFile) error) (AudioFile, error)

	GetDetection(ctx context.Context, uploadID, id string) (Detection, error)
	// ListDetections returns matching detections in the order they were heard.
	ListDetections(ctx context.Context, f DetectionFilter) ([]Detection, error)
	// UpsertDetections writes whole documents, creating or replacing each. A
	// detection with no id gets DetectionID from its file, start and species.
	UpsertDetections(ctx context.Context, uploadID string, detections []Detection) error
	UpdateDetection(ctx context.Context, uploadID, id string, mutate func(*Detection) error) (Detection, error)

	Close() error
}

// UploadFilter narrows ListUploads. Zero fields match everything.
type UploadFilter struct {
	UserID string
	Status string
}

// DetectionFilter narrows ListDetections. Zero fields match everything; leaving
// UploadID empty searches every card.
type DetectionFilter struct {
	UploadID string
	// AudioFileID keeps one file's detections. Set UploadID with it, so the
	// query stays in the card's partition.
	AudioFileID  string
	ReviewStatus string
	// Since keeps detections heard at or after this instant, and Until those
	// heard before it.
	Since time.Time
	Until time.Time
	// MinConfidence keeps detections at or above it.
	MinConfidence float64
}

// Backends.
const (
	BackendCosmos = "cosmos"
	BackendLocal  = "local"
)

// Config selects and configures a backend.
type Config struct {
	// Backend is BackendCosmos (the default when empty) or BackendLocal.
	Backend string
	// LocalPath is the JSON file the local backend reads and writes.
	LocalPath string
	// CosmosEndpoint is the account URI, e.g. https://acct.documents.azure.com:443/.
	CosmosEndpoint string
	// CosmosDatabase is the database holding the five containers.
	CosmosDatabase string
	// CosmosKey switches from Entra ID auth to an account key. It exists for
	// the Cosmos DB emulator; production accounts disable key auth.
	CosmosKey string
}

// Open connects to the configured backend.
func Open(ctx context.Context, cfg Config) (Store, error) {
	switch cfg.Backend {
	case BackendCosmos, "":
		return OpenCosmos(ctx, cfg)
	case BackendLocal:
		if cfg.LocalPath == "" {
			return nil, errors.New("db: the local backend needs a file path")
		}
		return OpenJSONFile(cfg.LocalPath)
	default:
		return nil, fmt.Errorf("db: unknown backend %q (want %q or %q)", cfg.Backend, BackendCosmos, BackendLocal)
	}
}

// --- rules shared by both backends ---

func now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

// normalizeEmail is the stored and compared form of an address.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (f UploadFilter) match(u Upload) bool {
	return (f.UserID == "" || u.UserID == f.UserID) &&
		(f.Status == "" || u.Status == f.Status)
}

func (f DetectionFilter) match(d Detection) bool {
	return (f.UploadID == "" || d.UploadID == f.UploadID) &&
		(f.AudioFileID == "" || d.AudioFileID == f.AudioFileID) &&
		(f.ReviewStatus == "" || d.ReviewStatus == f.ReviewStatus) &&
		(f.Since.IsZero() || !d.DetectedAt.Before(f.Since)) &&
		(f.Until.IsZero() || d.DetectedAt.Before(f.Until)) &&
		d.Confidence >= f.MinConfidence
}

func sortUsers(us []User) {
	slices.SortFunc(us, func(a, b User) int {
		return cmpThen(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
}

func sortRecorders(rs []Recorder) {
	slices.SortFunc(rs, func(a, b Recorder) int { return strings.Compare(a.ID, b.ID) })
}

// sortUploads puts the most recently pulled card first.
func sortUploads(us []Upload) {
	slices.SortFunc(us, func(a, b Upload) int {
		return cmpThen(strings.Compare(b.PulledOn, a.PulledOn), b.StartedAt.Compare(a.StartedAt), strings.Compare(a.ID, b.ID))
	})
}

func sortAudioFiles(fs []AudioFile) {
	slices.SortFunc(fs, func(a, b AudioFile) int { return strings.Compare(a.Path, b.Path) })
}

func sortDetections(ds []Detection) {
	slices.SortFunc(ds, func(a, b Detection) int {
		return cmpThen(a.DetectedAt.Compare(b.DetectedAt), strings.Compare(a.ID, b.ID))
	})
}

// cmpThen returns the first non-zero comparison.
func cmpThen(cs ...int) int {
	for _, c := range cs {
		if c != 0 {
			return c
		}
	}
	return 0
}

// prepare* fill in what a create or upsert owes every document, and reject
// what neither backend should store.

func prepareUser(u *User, t time.Time) error {
	if u.ID == "" {
		u.ID = NewID("usr")
	}
	u.Email = normalizeEmail(u.Email)
	if u.Email == "" {
		return errors.New("db: a user needs an email")
	}
	stamp(&u.CreatedAt, &u.UpdatedAt, t)
	return nil
}

func prepareRecorder(r *Recorder, t time.Time) error {
	if r.ID == "" {
		return errors.New("db: a recorder needs an id")
	}
	stamp(&r.CreatedAt, &r.UpdatedAt, t)
	return nil
}

func prepareUpload(u *Upload, t time.Time) error {
	if u.ID == "" {
		return errors.New("db: an upload needs an id")
	}
	stamp(&u.CreatedAt, &u.UpdatedAt, t)
	return nil
}

func prepareAudioFile(uploadID string, f *AudioFile, t time.Time) error {
	if f.UploadID != "" && f.UploadID != uploadID {
		return fmt.Errorf("db: audio file %s belongs to upload %s, not %s", f.ID, f.UploadID, uploadID)
	}
	f.UploadID = uploadID
	f.Path = CardPath(f.Path)
	if f.ID == "" {
		f.ID = AudioFileID(uploadID, f.Path)
	}
	stamp(&f.CreatedAt, &f.UpdatedAt, t)
	return nil
}

func prepareDetection(uploadID string, d *Detection, t time.Time) error {
	if d.UploadID != "" && d.UploadID != uploadID {
		return fmt.Errorf("db: detection %s belongs to upload %s, not %s", d.ID, d.UploadID, uploadID)
	}
	d.UploadID = uploadID
	if d.ID == "" {
		d.ID = DetectionID(d.AudioFileID, int64(d.StartSec*1000), d.ScientificName)
	}
	if d.ReviewStatus == "" {
		d.ReviewStatus = ReviewUnreviewed
	}
	stamp(&d.CreatedAt, &d.UpdatedAt, t)
	return nil
}

func stamp(created, updated *time.Time, t time.Time) {
	if created.IsZero() {
		*created = t
	}
	*updated = t
}

// keep runs after a mutate func: it puts back what an update may not change
// (id, partition key, creation time) and stamps UpdatedAt.

func (u *User) keep(old User, t time.Time) {
	u.ID, u.CreatedAt, u.UpdatedAt = old.ID, old.CreatedAt, t
	u.Email = normalizeEmail(u.Email)
}

func (r *Recorder) keep(old Recorder, t time.Time) {
	r.ID, r.CreatedAt, r.UpdatedAt = old.ID, old.CreatedAt, t
}

func (u *Upload) keep(old Upload, t time.Time) {
	u.ID, u.CreatedAt, u.UpdatedAt = old.ID, old.CreatedAt, t
}

func (f *AudioFile) keep(old AudioFile, t time.Time) {
	f.ID, f.UploadID, f.CreatedAt, f.UpdatedAt = old.ID, old.UploadID, old.CreatedAt, t
}

func (d *Detection) keep(old Detection, t time.Time) {
	d.ID, d.UploadID, d.CreatedAt, d.UpdatedAt = old.ID, old.UploadID, old.CreatedAt, t
}
