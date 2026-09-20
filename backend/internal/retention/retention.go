// Package retention removes a card's original recordings once they have been
// kept long enough, and leaves everything a reviewer needs behind.
//
// A card is ~128 GB of audio; its detections and their clips are a few
// megabytes. Keeping the originals for good is most of the storage bill, and
// nothing in Birdsense reads one after BirdNET has: the only reader is
// internal/analysis, and the only audio the frontend ever plays is a clip.
// So the originals expire and the clips do not.
//
// Like the analysis queue, the work is in the database rather than in memory:
// a card past its window with files that still have a BlobName is work, and a
// sweep interrupted half way is finished by the next one. Sweeper.Run wakes
// every few hours, so a card's audio goes within a few hours of its date, not
// on the stroke of it.
//
// Two things are deliberately never swept:
//
//   - a card analysis hasn't finished with. Only in_review, needs_attention
//     and results_sent cards are looked at, and on them only files BirdNET has
//     finished with (analyzed or failed). A card stuck in processing keeps its
//     audio and shows up on the coordinator's card page as a thing to fix,
//     rather than quietly losing the audio it is still waiting to read.
//   - clips. They are under their own storage prefix (storage.ClipName) and
//     nothing here names it. Only a coordinator deleting the card removes
//     those.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/db"
	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// every is how often a sweep runs. The window is measured in days, so the
// exact hour doesn't matter; this is often enough that a restart-heavy day
// still sweeps, and rare enough to be invisible in the Cosmos bill.
const every = 6 * time.Hour

// settled are the card statuses whose files BirdNET is done with. A card in
// any other status keeps its audio however old it is.
var settled = []string{db.StatusInReview, db.StatusNeedsAttention, db.StatusResultsSent}

// Policy is how long a card's originals are kept. The zero value keeps them
// for good, which is what BIRDSENSE_AUDIO_RETENTION_DAYS=0 asks for.
type Policy struct {
	// Window is how long after a card is received its originals are removed.
	Window time.Duration
}

// On reports whether originals expire at all.
func (p Policy) On() bool { return p.Window > 0 }

// ExpiresAt is when a card's originals are due to be removed, for the
// coordinator's card page. It is nil when nothing is due: retention is off,
// the card hasn't been received or analyzed yet, or its audio has already
// gone.
func (p Policy) ExpiresAt(u db.Upload) *time.Time {
	if !p.On() || u.ReceivedAt == nil || u.AudioDeletedAt != nil || !isSettled(u.Status) {
		return nil
	}
	at := u.ReceivedAt.Add(p.Window)
	return &at
}

// due reports whether a card's originals may be swept now.
func (p Policy) due(u db.Upload, now time.Time) bool {
	at := p.ExpiresAt(u)
	return at != nil && !now.Before(*at)
}

func isSettled(status string) bool {
	for _, s := range settled {
		if s == status {
			return true
		}
	}
	return false
}

// Sweeper removes expired originals. One runs in the server process.
type Sweeper struct {
	store  db.Store
	files  storage.Store
	policy Policy
	log    *slog.Logger
	now    func() time.Time
}

// New makes a sweeper for the policy. Run does nothing if the policy is off.
func New(store db.Store, files storage.Store, policy Policy, log *slog.Logger) *Sweeper {
	return &Sweeper{store: store, files: files, policy: policy, log: log, now: time.Now}
}

// Run sweeps now and then every few hours, until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	if !s.policy.On() {
		return
	}
	for {
		s.Sweep(ctx)
		if !sleep(ctx, every) {
			return
		}
	}
}

// Sweep removes the originals of every card past its window, once.
func (s *Sweeper) Sweep(ctx context.Context) {
	now := s.now()
	for _, status := range settled {
		uploads, err := s.store.ListUploads(ctx, db.UploadFilter{Status: status})
		if err != nil {
			s.log.Error("audio retention: listing cards", "status", status, "err", err)
			continue
		}
		for _, u := range uploads {
			if ctx.Err() != nil {
				return
			}
			if s.policy.due(u, now) {
				s.sweepCard(ctx, u, now)
			}
		}
	}
}

// sweepCard removes the originals still stored for one card. The blob goes
// first and the document is marked after: a sweep that stops in between leaves
// a document pointing at a blob that isn't there, which the next sweep marks
// and internal/analysis already reads as "the audio isn't in storage". Marking
// first and failing to delete would leak the blob instead, with nothing left
// naming it.
func (s *Sweeper) sweepCard(ctx context.Context, u db.Upload, now time.Time) {
	files, err := s.store.ListAudioFiles(ctx, u.ID)
	if err != nil {
		s.log.Error("audio retention: listing a card's files", "upload", u.ID, "err", err)
		return
	}
	removed, bytes, left := 0, int64(0), 0
	for _, f := range files {
		if f.BlobName == "" {
			continue
		}
		if f.Status != db.AudioAnalyzed && f.Status != db.AudioFailed {
			// BirdNET may still want this one. Keep it and the card with it.
			left++
			continue
		}
		if err := s.files.Delete(ctx, f.BlobName); err != nil {
			s.log.Error("audio retention: deleting a recording", "upload", u.ID, "file", f.ID, "err", err)
			left++
			continue
		}
		_, err := s.store.UpdateAudioFile(ctx, u.ID, f.ID, func(f *db.AudioFile) error {
			f.BlobName = ""
			f.AudioDeletedAt = &now
			return nil
		})
		if err != nil {
			s.log.Error("audio retention: marking a recording removed", "upload", u.ID, "file", f.ID, "err", err)
			continue
		}
		removed++
		bytes += f.SizeBytes
	}
	if removed == 0 {
		return
	}
	s.log.Info("audio retention: removed a card's recordings",
		"upload", u.ID, "files", removed, "bytes", bytes, "kept", left)
	if left > 0 {
		// Some of the card is still stored, so it isn't done yet.
		return
	}
	if _, err := s.store.UpdateUpload(ctx, u.ID, func(u *db.Upload) error {
		u.AudioDeletedAt = &now
		return nil
	}); err != nil {
		s.log.Error("audio retention: marking a card's audio removed", "upload", u.ID, "err", err)
	}
}

// sleep waits d, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
