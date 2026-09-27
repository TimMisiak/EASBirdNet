package db

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) (Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "birdsense.json")
	s, err := OpenJSONFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// sameJSON compares documents the way they are stored.
func sameJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Errorf("%s:\n got  %s\n want %s", what, g, w)
	}
}

func seedCard(t *testing.T, s Store) (User, Recorder, Upload) {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUser(ctx, User{Email: " Jane@Example.com ", Name: "Jane Volunteer", Role: RoleVolunteer})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	r, err := s.CreateRecorder(ctx, Recorder{ID: "SW-02", Name: "Marymoor Park – Snag Row", Latitude: 47.66021, Longitude: -122.11384, Model: "SwiftOne"})
	if err != nil {
		t.Fatalf("create recorder: %v", err)
	}
	up, err := s.CreateUpload(ctx, Upload{
		ID: "OWL-20260907-SR02", RecorderID: r.ID,
		Recorder: RecorderSnapshot{Name: r.Name, Latitude: r.Latitude, Longitude: r.Longitude},
		UserID:   u.ID, UserName: u.Name, PulledOn: "2026-09-07",
		Nights:    []Night{{Date: "2026-08-24", Files: 24, Bytes: 9_192_000_000}, {Date: "2026-08-25", Files: 3, Bytes: 1_149_000_000, Flag: "partial"}},
		FileCount: 27, TotalBytes: 10_341_000_000, Status: StatusInProgress, StartedAt: now(),
	})
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}
	return u, r, up
}

func TestJSONFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	u, r, up := seedCard(t, s)

	if !strings.HasPrefix(u.ID, "usr_") || u.Email != "jane@example.com" {
		t.Errorf("user = %q / %q, want a usr_ id and a normalized email", u.ID, u.Email)
	}
	if u.CreatedAt.IsZero() || !u.UpdatedAt.Equal(u.CreatedAt) {
		t.Errorf("createdAt %v, updatedAt %v: want both set and equal on create", u.CreatedAt, u.UpdatedAt)
	}

	start := time.Date(2026, 8, 25, 4, 0, 0, 0, time.UTC)
	file := AudioFile{RecorderID: r.ID, Path: `DATA\20260824\20260825_040000.WAV`, SizeBytes: 383_000_000,
		Night: "2026-08-24", RecordedAt: &start, BlobName: "uploads/" + up.ID + "/3f9a0c2b7d1e4a65", Status: AudioUploaded}
	if err := s.UpsertAudioFiles(ctx, up.ID, []AudioFile{file}); err != nil {
		t.Fatalf("upsert audio: %v", err)
	}
	files, err := s.ListAudioFiles(ctx, up.ID)
	if err != nil || len(files) != 1 {
		t.Fatalf("list audio = %d files, %v; want 1", len(files), err)
	}
	if files[0].ID != AudioFileID(up.ID, file.Path) || files[0].Path != "DATA/20260824/20260825_040000.WAV" {
		t.Errorf("audio file = %q at %q, want the derived id and a forward-slash path", files[0].ID, files[0].Path)
	}

	det := Detection{AudioFileID: files[0].ID, RecorderID: r.ID, DetectedAt: start.Add(732 * time.Second), Night: "2026-08-24",
		StartSec: 732, EndSec: 735, ScientificName: "Strix varia", CommonName: "Barred Owl", Confidence: 0.91}
	if err := s.UpsertDetections(ctx, up.ID, []Detection{det}); err != nil {
		t.Fatalf("upsert detections: %v", err)
	}

	// Everything survives a reopen.
	s.Close()
	again, err := OpenJSONFile(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	gotU, err := again.GetUserByEmail(ctx, "JANE@example.com")
	if err != nil {
		t.Fatalf("user by email after reopen: %v", err)
	}
	sameJSON(t, "user", gotU, u)
	gotR, _ := again.GetRecorder(ctx, r.ID)
	sameJSON(t, "recorder", gotR, r)
	gotUp, _ := again.GetUpload(ctx, up.ID)
	sameJSON(t, "upload", gotUp, up)
	gotFiles, _ := again.ListAudioFiles(ctx, up.ID)
	sameJSON(t, "audio files", gotFiles, files)

	dets, err := again.ListDetections(ctx, DetectionFilter{UploadID: up.ID})
	if err != nil || len(dets) != 1 {
		t.Fatalf("detections after reopen = %d, %v; want 1", len(dets), err)
	}
	if dets[0].ReviewStatus != ReviewUnreviewed || dets[0].ID != DetectionID(files[0].ID, 732_000, "Strix varia") {
		t.Errorf("detection = %q / %q, want the derived id and unreviewed", dets[0].ID, dets[0].ReviewStatus)
	}
}

func TestJSONFileNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, _, up := seedCard(t, s)
	if err := s.UpsertAudioFiles(ctx, up.ID, []AudioFile{{Path: "a.wav"}}); err != nil {
		t.Fatal(err)
	}
	fileID := AudioFileID(up.ID, "a.wav")
	noop := func(*AudioFile) error { return nil }

	checks := map[string]error{}
	_, checks["get user"] = s.GetUser(ctx, "usr_missing")
	_, checks["user by email"] = s.GetUserByEmail(ctx, "nobody@example.com")
	_, checks["update recorder"] = s.UpdateRecorder(ctx, "SW-99", func(*Recorder) error { return nil })
	_, checks["get upload"] = s.GetUpload(ctx, "OWL-nope")
	// A document is addressed by its partition as well as its id.
	_, checks["audio file in the wrong upload"] = s.UpdateAudioFile(ctx, "OWL-other", fileID, noop)
	_, checks["get audio file in the wrong upload"] = s.GetAudioFile(ctx, "OWL-other", fileID)
	for what, err := range checks {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", what, err)
		}
	}
	if _, err := s.UpdateAudioFile(ctx, up.ID, fileID, noop); err != nil {
		t.Errorf("audio file in its own upload: %v", err)
	}
	if f, err := s.GetAudioFile(ctx, up.ID, fileID); err != nil || f.Path != "a.wav" {
		t.Errorf("get audio file in its own upload = %+v, %v", f, err)
	}
}

func TestJSONFileConflicts(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	jane, r, up := seedCard(t, s)

	if _, err := s.CreateUser(ctx, User{Email: "JANE@example.com", Name: "Jane Again"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate email: err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateRecorder(ctx, Recorder{ID: r.ID, Name: "Somewhere else"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate recorder: err = %v, want ErrConflict", err)
	}
	if _, err := s.CreateUpload(ctx, Upload{ID: up.ID}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate upload: err = %v, want ErrConflict", err)
	}

	tomas, err := s.CreateUser(ctx, User{Email: "tomas@example.com", Name: "Tomas Reyes"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateUser(ctx, tomas.ID, func(u *User) error { u.Email = jane.Email; return nil })
	if !errors.Is(err, ErrConflict) {
		t.Errorf("update onto a taken email: err = %v, want ErrConflict", err)
	}
	// Re-saving your own address is not a conflict.
	if _, err := s.UpdateUser(ctx, jane.ID, func(u *User) error { u.Name = "Jane V."; return nil }); err != nil {
		t.Errorf("update own record: %v", err)
	}
}

func TestJSONFileFailedMutateChangesNothing(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	_, _, up := seedCard(t, s)
	before, _ := os.ReadFile(path)

	boom := errors.New("not today")
	_, err := s.UpdateUpload(ctx, up.ID, func(u *Upload) error {
		u.FilesUploaded = 27
		u.Nights[0].Files = 0
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the mutate error back", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("the data file changed after a failed update")
	}
	got, _ := s.GetUpload(ctx, up.ID)
	sameJSON(t, "upload after failed update", got, up)
}

func TestJSONFileUpdateKeepsKeysAndStampsTime(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, _, up := seedCard(t, s)

	got, err := s.UpdateUpload(ctx, up.ID, func(u *Upload) error {
		u.ID = "OWL-hijacked"
		u.CreatedAt = time.Time{}
		u.UpdatedAt = time.Time{}
		u.FilesUploaded = 5
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != up.ID || !got.CreatedAt.Equal(up.CreatedAt) || got.UpdatedAt.IsZero() || got.FilesUploaded != 5 {
		t.Errorf("updated = id %q created %v updated %v files %d; want the id and createdAt kept, the change applied",
			got.ID, got.CreatedAt, got.UpdatedAt, got.FilesUploaded)
	}
	if _, err := s.GetUpload(ctx, "OWL-hijacked"); !errors.Is(err, ErrNotFound) {
		t.Error("an update was able to rename a document")
	}
}

func TestJSONFileReturnsCopies(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	_, _, up := seedCard(t, s)

	got, _ := s.GetUpload(ctx, up.ID)
	got.Nights[0].Files = 0
	list, _ := s.ListUploads(ctx, UploadFilter{})
	list[0].Nights[1].Flag = ""

	again, _ := s.GetUpload(ctx, up.ID)
	sameJSON(t, "stored upload", again, up)
}

func TestJSONFileListFilters(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	jane, r, first := seedCard(t, s)

	later := first
	later.ID, later.PulledOn, later.Status = "OWL-20260921-SR02", "2026-09-21", StatusProcessing
	if _, err := s.CreateUpload(ctx, later); err != nil {
		t.Fatal(err)
	}
	other := first
	other.ID, other.UserID, other.PulledOn = "OWL-20260914-SR02", "usr_someone", "2026-09-14"
	if _, err := s.CreateUpload(ctx, other); err != nil {
		t.Fatal(err)
	}

	mine, _ := s.ListUploads(ctx, UploadFilter{UserID: jane.ID})
	if len(mine) != 2 || mine[0].ID != later.ID || mine[1].ID != first.ID {
		t.Errorf("jane's uploads = %v, want newest card first, only hers", ids(mine))
	}
	processing, _ := s.ListUploads(ctx, UploadFilter{Status: StatusProcessing})
	if len(processing) != 1 || processing[0].ID != later.ID {
		t.Errorf("processing uploads = %v, want [%s]", ids(processing), later.ID)
	}

	base := time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC)
	mk := func(upload string, at time.Duration, species, review string) Detection {
		return Detection{AudioFileID: "af_" + upload, RecorderID: r.ID, DetectedAt: base.Add(at), StartSec: at.Seconds(),
			ScientificName: species, Confidence: 0.5 + at.Hours()/10, ReviewStatus: review}
	}
	if err := s.UpsertDetections(ctx, first.ID, []Detection{
		mk(first.ID, 0, "Strix varia", ReviewConfirmed),
		mk(first.ID, time.Hour, "Bubo virginianus", ReviewRejected),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDetections(ctx, later.ID, []Detection{
		mk(later.ID, 2*time.Hour, "Strix varia", ReviewConfirmed),
		mk(later.ID, 3*time.Hour, "Tyto alba", ""),
	}); err != nil {
		t.Fatal(err)
	}

	confirmedSince, _ := s.ListDetections(ctx, DetectionFilter{ReviewStatus: ReviewConfirmed, Since: base.Add(time.Minute)})
	if len(confirmedSince) != 1 || confirmedSince[0].UploadID != later.ID {
		t.Errorf("confirmed since = %d detections, want the one on %s", len(confirmedSince), later.ID)
	}
	onFirst, _ := s.ListDetections(ctx, DetectionFilter{UploadID: first.ID})
	if len(onFirst) != 2 || !onFirst[0].DetectedAt.Before(onFirst[1].DetectedAt) {
		t.Errorf("detections on %s = %d, want 2 in the order heard", first.ID, len(onFirst))
	}
	window, _ := s.ListDetections(ctx, DetectionFilter{Since: base.Add(time.Hour), Until: base.Add(3 * time.Hour)})
	if len(window) != 2 || window[0].ScientificName != "Bubo virginianus" || window[1].UploadID != later.ID {
		t.Errorf("heard from 1 h to before 3 h = %d detections, want the horned owl and the later barred owl", len(window))
	}
	confident, _ := s.ListDetections(ctx, DetectionFilter{MinConfidence: 0.7})
	if len(confident) != 2 || confident[0].Confidence != 0.7 {
		t.Errorf("at 70%% or more = %d detections, want the last two", len(confident))
	}
	unreviewed, _ := s.ListDetections(ctx, DetectionFilter{ReviewStatus: ReviewUnreviewed})
	if len(unreviewed) != 1 || unreviewed[0].ScientificName != "Tyto alba" {
		t.Errorf("unreviewed = %d, want the barn owl only", len(unreviewed))
	}
}

func TestJSONFileDeleteUpload(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	_, _, card := seedCard(t, s)
	other := "OWL-20260821-SR03"
	if _, err := s.CreateUpload(ctx, Upload{ID: other, Status: StatusInReview}); err != nil {
		t.Fatal(err)
	}
	stock := func(id string) {
		t.Helper()
		if err := s.UpsertAudioFiles(ctx, id, []AudioFile{{Path: "DATA/a.wav"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertDetections(ctx, id, []Detection{{AudioFileID: AudioFileID(id, "DATA/a.wav"), ScientificName: "Strix varia"}}); err != nil {
			t.Fatal(err)
		}
	}
	left := func(s Store, id string) int {
		t.Helper()
		files, err := s.ListAudioFiles(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		found, err := s.ListDetections(ctx, DetectionFilter{UploadID: id})
		if err != nil {
			t.Fatal(err)
		}
		return len(files) + len(found)
	}
	stock(card.ID)
	stock(other)

	if err := s.DeleteUpload(ctx, card.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetUpload(ctx, card.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted card: err = %v, want ErrNotFound", err)
	}
	if n := left(s, card.ID); n != 0 {
		t.Errorf("%d documents left under the deleted card", n)
	}
	if n := left(s, other); n != 2 {
		t.Errorf("the other card has %d documents, want its 2", n)
	}
	if err := s.DeleteUpload(ctx, card.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: err = %v, want ErrNotFound", err)
	}

	// Documents written under a card after it went are still swept up.
	stock(card.ID)
	if err := s.DeleteUpload(ctx, card.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete leftovers: err = %v, want ErrNotFound", err)
	}
	if n := left(s, card.ID); n != 0 {
		t.Errorf("%d leftover documents not swept", n)
	}

	reopened, err := OpenJSONFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetUpload(ctx, card.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted card is back after reopening: err = %v", err)
	}
	if _, err := reopened.GetUpload(ctx, other); err != nil || left(reopened, other) != 2 {
		t.Errorf("the other card after reopening: err = %v, %d documents", err, left(reopened, other))
	}
}

func TestDetectionSpeciesPrefersTheCorrection(t *testing.T) {
	d := Detection{ScientificName: "Bubo virginianus", CommonName: "Great Horned Owl"}
	if sci, _ := d.Species(); sci != "Bubo virginianus" {
		t.Errorf("unreviewed species = %q", sci)
	}
	d.Review = &Review{CorrectedScientificName: "Strix varia", CorrectedCommonName: "Barred Owl"}
	if sci, common := d.Species(); sci != "Strix varia" || common != "Barred Owl" {
		t.Errorf("corrected species = %q / %q, want the correction", sci, common)
	}
}

func TestIDsAreStable(t *testing.T) {
	if AudioFileID("OWL-1", `DATA\a.WAV`) != AudioFileID("OWL-1", "/DATA/a.WAV") {
		t.Error("the same file spelled two ways got two ids")
	}
	if AudioFileID("OWL-1", "a.WAV") == AudioFileID("OWL-2", "a.WAV") {
		t.Error("the same path on two cards shares an id")
	}
	if DetectionID("af_1", 732_000, "Strix varia") != DetectionID("af_1", 732_000, "Strix varia") {
		t.Error("detection id is not deterministic")
	}
	if NewID("usr") == NewID("usr") {
		t.Error("NewID repeated itself")
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, Config{Backend: "sqlite"}); err == nil {
		t.Error("unknown backend opened")
	}
	if _, err := Open(ctx, Config{Backend: BackendLocal}); err == nil {
		t.Error("local backend opened with no path")
	}
	if _, err := Open(ctx, Config{Backend: BackendCosmos, CosmosDatabase: "birdsense"}); err == nil {
		t.Error("cosmos backend opened with no endpoint")
	}
}

func TestOpenJSONFileRejectsAnotherVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "birdsense.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJSONFile(path); err == nil {
		t.Error("opened a file from a different version")
	}
}

func ids(us []Upload) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.ID
	}
	return out
}

// TestListDetectionsIsBounded holds the ceiling that keeps one request from
// taking the replica down. Neither backend can page or sort a cross-partition
// query, so every row a filter matches is decoded into the memory of the one
// process that is also running BirdNET: a filter matching more than
// MaxDetectionScan has to fail rather than be served. The Cosmos backend
// enforces the same ceiling in the same place (scanDocs), which nothing here
// can reach; this is the behaviour it has to match.
func TestListDetectionsIsBounded(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	_, _, card := seedCard(t, s)
	det := make([]Detection, 3)
	for i := range det {
		// StartSec differs so the ids do: they are derived from it (DetectionID).
		det[i] = Detection{
			AudioFileID: "af_1", DetectedAt: time.Date(2026, 9, 10, i, 0, 0, 0, time.UTC),
			StartSec: float64(i), ScientificName: "Strix varia", Confidence: 0.9,
		}
	}
	if err := s.UpsertDetections(ctx, card.ID, det); err != nil {
		t.Fatal(err)
	}

	// The ceiling is passed in here only so this doesn't have to store
	// MaxDetectionScan documents to reach it; ListDetections passes the real one.
	file := s.(*jsonFile)
	if _, err := file.listDetections(DetectionFilter{}, 2); !errors.Is(err, ErrTooMany) {
		t.Errorf("3 detections under a ceiling of 2 = %v, want ErrTooMany", err)
	}
	under, err := file.listDetections(DetectionFilter{}, 3)
	if err != nil || len(under) != 3 {
		t.Errorf("3 detections under a ceiling of 3 = %d, %v; want all 3", len(under), err)
	}
	// A filter that narrows below the ceiling is the way out, and the answer a
	// caller is told to reach for.
	narrowed, err := file.listDetections(DetectionFilter{Since: time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)}, 2)
	if err != nil || len(narrowed) != 2 {
		t.Errorf("narrowed to 2 under a ceiling of 2 = %d, %v; want both", len(narrowed), err)
	}
	if all, err := s.ListDetections(ctx, DetectionFilter{}); err != nil || len(all) != 3 {
		t.Errorf("ListDetections = %d, %v; want all 3 well under MaxDetectionScan", len(all), err)
	}

	// MaxDetectionScan is sized for the replica, not for the dataset: measured,
	// a decoded detection costs ~900 bytes of live heap and a little over twice
	// that in churn, against the 2 GiB the container has for BirdNET and
	// everything else. Raising it is a memory decision, so make it deliberately.
	if MaxDetectionScan > 400_000 {
		t.Errorf("MaxDetectionScan = %d: past ~400k a single list outgrows the replica's memory", MaxDetectionScan)
	}
}
