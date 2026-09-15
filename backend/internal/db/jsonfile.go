package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

// jsonFile is the local development backend. The whole dataset lives in memory
// behind one mutex and is written out in full after every change, atomically
// (temp file, fsync, rename), so a crash never leaves a half-written file.
//
// That is plenty for a developer's machine and hopeless for a season of real
// detections -- it is not meant for production, and nothing about its layout
// is a migration path to Cosmos DB.
type jsonFile struct {
	mu   sync.Mutex
	path string
	data fileData
}

// fileData is the file on disk. Each map is keyed by document id.
type fileData struct {
	Version    int                  `json:"version"`
	Users      map[string]User      `json:"users"`
	Recorders  map[string]Recorder  `json:"recorders"`
	Uploads    map[string]Upload    `json:"uploads"`
	AudioFiles map[string]AudioFile `json:"audioFiles"`
	Detections map[string]Detection `json:"detections"`
}

const fileVersion = 1

var _ Store = (*jsonFile)(nil)

// OpenJSONFile opens the data file at path, creating it (and its directory)
// empty if it doesn't exist.
func OpenJSONFile(path string) (Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("db: creating the directory for %s: %w", path, err)
	}
	s := &jsonFile{path: path}

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.data.Version = fileVersion
		s.data.fill()
		if err := s.save(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("db: reading %s: %w", path, err)
	default:
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("db: %s is not a Birdsense data file: %w", path, err)
		}
		if s.data.Version != fileVersion {
			return nil, fmt.Errorf("db: %s is version %d, this build reads version %d", path, s.data.Version, fileVersion)
		}
		s.data.fill()
	}
	return s, nil
}

// fill makes sure every map exists, so a hand-trimmed file still loads.
func (d *fileData) fill() {
	if d.Users == nil {
		d.Users = map[string]User{}
	}
	if d.Recorders == nil {
		d.Recorders = map[string]Recorder{}
	}
	if d.Uploads == nil {
		d.Uploads = map[string]Upload{}
	}
	if d.AudioFiles == nil {
		d.AudioFiles = map[string]AudioFile{}
	}
	if d.Detections == nil {
		d.Detections = map[string]Detection{}
	}
}

func (s *jsonFile) Close() error { return nil }

// --- users ---

func (s *jsonFile) GetUser(_ context.Context, id string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.data.Users, id)
}

func (s *jsonFile) GetUserByEmail(_ context.Context, email string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = normalizeEmail(email)
	for _, u := range s.data.Users {
		if u.Email == email {
			return clone(u), nil
		}
	}
	return User{}, ErrNotFound
}

func (s *jsonFile) ListUsers(_ context.Context) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list(s.data.Users, func(User) bool { return true })
	sortUsers(out)
	return out, nil
}

func (s *jsonFile) CreateUser(_ context.Context, u User) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := prepareUser(&u, now()); err != nil {
		return User{}, err
	}
	if _, taken := s.data.Users[u.ID]; taken || s.emailTaken(u.Email, u.ID) {
		return User{}, ErrConflict
	}
	return u, put(s, s.data.Users, u.ID, clone(u))
}

func (s *jsonFile) UpdateUser(_ context.Context, id string, mutate func(*User) error) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return update(s, s.data.Users, id, func(u *User, old User) error {
		if err := mutate(u); err != nil {
			return err
		}
		u.keep(old, now())
		if s.emailTaken(u.Email, u.ID) {
			return ErrConflict
		}
		return nil
	})
}

// emailTaken reports whether someone other than exceptID has the address.
func (s *jsonFile) emailTaken(email, exceptID string) bool {
	for _, u := range s.data.Users {
		if u.Email == email && u.ID != exceptID {
			return true
		}
	}
	return false
}

// --- recorders ---

func (s *jsonFile) GetRecorder(_ context.Context, id string) (Recorder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.data.Recorders, id)
}

func (s *jsonFile) ListRecorders(_ context.Context) ([]Recorder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list(s.data.Recorders, func(Recorder) bool { return true })
	sortRecorders(out)
	return out, nil
}

func (s *jsonFile) CreateRecorder(_ context.Context, r Recorder) (Recorder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := prepareRecorder(&r, now()); err != nil {
		return Recorder{}, err
	}
	if _, taken := s.data.Recorders[r.ID]; taken {
		return Recorder{}, ErrConflict
	}
	return r, put(s, s.data.Recorders, r.ID, clone(r))
}

func (s *jsonFile) UpdateRecorder(_ context.Context, id string, mutate func(*Recorder) error) (Recorder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return update(s, s.data.Recorders, id, func(r *Recorder, old Recorder) error {
		if err := mutate(r); err != nil {
			return err
		}
		r.keep(old, now())
		return nil
	})
}

// --- uploads ---

func (s *jsonFile) GetUpload(_ context.Context, id string) (Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return get(s.data.Uploads, id)
}

func (s *jsonFile) ListUploads(_ context.Context, f UploadFilter) ([]Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list(s.data.Uploads, f.match)
	sortUploads(out)
	return out, nil
}

func (s *jsonFile) CreateUpload(_ context.Context, u Upload) (Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := prepareUpload(&u, now()); err != nil {
		return Upload{}, err
	}
	if _, taken := s.data.Uploads[u.ID]; taken {
		return Upload{}, ErrConflict
	}
	return u, put(s, s.data.Uploads, u.ID, clone(u))
}

func (s *jsonFile) UpdateUpload(_ context.Context, id string, mutate func(*Upload) error) (Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return update(s, s.data.Uploads, id, func(u *Upload, old Upload) error {
		if err := mutate(u); err != nil {
			return err
		}
		u.keep(old, now())
		return nil
	})
}

func (s *jsonFile) DeleteUpload(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload, had := s.data.Uploads[id]
	files := take(s.data.AudioFiles, func(f AudioFile) bool { return f.UploadID == id })
	detections := take(s.data.Detections, func(d Detection) bool { return d.UploadID == id })
	delete(s.data.Uploads, id)
	if had || len(files) > 0 || len(detections) > 0 {
		// One write for the lot, so the file never holds half a card.
		if err := s.save(); err != nil {
			maps.Copy(s.data.AudioFiles, files)
			maps.Copy(s.data.Detections, detections)
			if had {
				s.data.Uploads[id] = upload
			}
			return err
		}
	}
	if !had {
		return ErrNotFound
	}
	return nil
}

// --- audio files ---

func (s *jsonFile) GetAudioFile(_ context.Context, uploadID, id string) (AudioFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Cosmos addresses a document by (partition key, id); so does this.
	f, err := get(s.data.AudioFiles, id)
	if err == nil && f.UploadID != uploadID {
		return AudioFile{}, ErrNotFound
	}
	return f, err
}

func (s *jsonFile) ListAudioFiles(_ context.Context, uploadID string) ([]AudioFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list(s.data.AudioFiles, func(f AudioFile) bool { return f.UploadID == uploadID })
	sortAudioFiles(out)
	return out, nil
}

func (s *jsonFile) UpsertAudioFiles(_ context.Context, uploadID string, files []AudioFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	batch := make(map[string]AudioFile, len(files))
	for _, f := range files {
		if err := prepareAudioFile(uploadID, &f, t); err != nil {
			return err
		}
		if old, ok := s.data.AudioFiles[f.ID]; ok && old.UploadID != uploadID {
			return fmt.Errorf("%w: audio file %s already belongs to upload %s", ErrConflict, f.ID, old.UploadID)
		}
		batch[f.ID] = clone(f)
	}
	return putAll(s, s.data.AudioFiles, batch)
}

func (s *jsonFile) UpdateAudioFile(_ context.Context, uploadID, id string, mutate func(*AudioFile) error) (AudioFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Cosmos addresses a document by (partition key, id); so does this.
	if f, ok := s.data.AudioFiles[id]; !ok || f.UploadID != uploadID {
		return AudioFile{}, ErrNotFound
	}
	return update(s, s.data.AudioFiles, id, func(f *AudioFile, old AudioFile) error {
		if err := mutate(f); err != nil {
			return err
		}
		f.keep(old, now())
		return nil
	})
}

// --- detections ---

func (s *jsonFile) ListDetections(_ context.Context, f DetectionFilter) ([]Detection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := list(s.data.Detections, f.match)
	sortDetections(out)
	return out, nil
}

func (s *jsonFile) UpsertDetections(_ context.Context, uploadID string, detections []Detection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now()
	batch := make(map[string]Detection, len(detections))
	for _, d := range detections {
		if err := prepareDetection(uploadID, &d, t); err != nil {
			return err
		}
		if old, ok := s.data.Detections[d.ID]; ok && old.UploadID != uploadID {
			return fmt.Errorf("%w: detection %s already belongs to upload %s", ErrConflict, d.ID, old.UploadID)
		}
		batch[d.ID] = clone(d)
	}
	return putAll(s, s.data.Detections, batch)
}

func (s *jsonFile) UpdateDetection(_ context.Context, uploadID, id string, mutate func(*Detection) error) (Detection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.data.Detections[id]; !ok || d.UploadID != uploadID {
		return Detection{}, ErrNotFound
	}
	return update(s, s.data.Detections, id, func(d *Detection, old Detection) error {
		if err := mutate(d); err != nil {
			return err
		}
		d.keep(old, now())
		return nil
	})
}

// --- plumbing (callers hold s.mu) ---

func get[T any](m map[string]T, id string) (T, error) {
	v, ok := m[id]
	if !ok {
		var zero T
		return zero, ErrNotFound
	}
	return clone(v), nil
}

func list[T any](m map[string]T, keep func(T) bool) []T {
	out := []T{}
	for _, v := range m {
		if keep(v) {
			out = append(out, clone(v))
		}
	}
	return out
}

// take removes the entries matching drop from m and returns them.
func take[T any](m map[string]T, drop func(T) bool) map[string]T {
	out := map[string]T{}
	for id, v := range m {
		if drop(v) {
			out[id] = v
			delete(m, id)
		}
	}
	return out
}

// update applies a change to a copy of the stored document and only stores it
// if apply succeeds and the file is written.
func update[T any](s *jsonFile, m map[string]T, id string, apply func(v *T, old T) error) (T, error) {
	var zero T
	old, ok := m[id]
	if !ok {
		return zero, ErrNotFound
	}
	v := clone(old)
	if err := apply(&v, old); err != nil {
		return zero, err
	}
	if err := put(s, m, id, clone(v)); err != nil {
		return zero, err
	}
	return v, nil
}

func put[T any](s *jsonFile, m map[string]T, id string, v T) error {
	return putAll(s, m, map[string]T{id: v})
}

// putAll stores a batch and writes the file, putting the previous entries back
// if the write fails so memory never runs ahead of disk.
func putAll[T any](s *jsonFile, m map[string]T, batch map[string]T) error {
	type prior struct {
		v   T
		had bool
	}
	undo := make(map[string]prior, len(batch))
	for id, v := range batch {
		old, had := m[id]
		undo[id] = prior{old, had}
		m[id] = v
	}
	if err := s.save(); err != nil {
		for id, p := range undo {
			if p.had {
				m[id] = p.v
			} else {
				delete(m, id)
			}
		}
		return err
	}
	return nil
}

func (s *jsonFile) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("db: encoding %s: %w", s.path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".*")
	if err != nil {
		return fmt.Errorf("db: writing %s: %w", s.path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened

	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("db: writing %s: %w", s.path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("db: writing %s: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("db: writing %s: %w", s.path, err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("db: writing %s: %w", s.path, err)
	}
	return nil
}

// clone deep-copies a document, so nothing a caller does to a returned value
// (appending to Nights, say) reaches the stored copy. A JSON round trip is the
// same copy Cosmos DB makes, which keeps the two backends honest.
func clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("db: cloning %T: %v", v, err))
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(fmt.Sprintf("db: cloning %T: %v", v, err))
	}
	return out
}
