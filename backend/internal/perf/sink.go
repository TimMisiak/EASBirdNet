package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/ngaitonde/EASBirdNet/backend/internal/storage"
)

// A Sink is where records go, one JSON object per line.
type Sink interface {
	// Header is the Run record, which a sink may repeat wherever a reader
	// might start.
	Header(rec any) error
	Write(rec any) error
	// Flush makes what has been written durable.
	Flush(ctx context.Context) error
}

// WriterSink writes records straight to an io.Writer: a local file, for the
// benchmark.
type WriterSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriterSink returns a sink that writes to w.
func NewWriterSink(w io.Writer) *WriterSink { return &WriterSink{w: w} }

func (s *WriterSink) Header(rec any) error { return s.Write(rec) }

func (s *WriterSink) Write(rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(line, '\n'))
	return err
}

func (s *WriterSink) Flush(context.Context) error { return nil }

// Segments are rolled at whichever of these comes first. Every flush rewrites
// the whole segment, so a segment is kept small: ten minutes of samples is
// about 40 KB.
const (
	segmentAge   = 10 * time.Minute
	segmentBytes = 1 << 20
	// A segment that can't be stored for this long is dropped rather than
	// held in memory for ever; the next one starts with the header again.
	segmentMost = 16 << 20
)

// StoreSink writes records into file storage (storage.PerfName), which is
// Blob Storage in Azure and a directory in development. Blobs can't be
// appended to through storage.Store, so records are held in a segment that
// each Flush rewrites whole, and a new segment starts every few minutes. A
// replica killed without warning loses what it wrote since the last flush.
type StoreSink struct {
	store    storage.Store
	instance string
	now      func() time.Time

	mu     sync.Mutex
	header []byte
	buf    bytes.Buffer
	start  time.Time
	// dirty is set when there is something the store doesn't have yet.
	dirty bool
}

// NewStoreSink returns a sink that writes under instance's directory.
func NewStoreSink(store storage.Store, instance string) *StoreSink {
	return &StoreSink{store: store, instance: instance, now: time.Now}
}

func (s *StoreSink) Header(rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header = append(line, '\n')
	return nil
}

func (s *StoreSink) Write(rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() == 0 {
		s.start = s.now().UTC()
		s.buf.Write(s.header)
	}
	s.buf.Write(line)
	s.buf.WriteByte('\n')
	s.dirty = true
	return nil
}

// Flush stores the current segment, and starts a new one if it is old or
// large enough. The store is called without the lock held, so samples and
// tasks aren't held up behind a slow upload.
func (s *StoreSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	start := s.start
	body := bytes.Clone(s.buf.Bytes())
	s.dirty = false
	s.mu.Unlock()

	name := storage.PerfName(start.Format(time.DateOnly), s.instance, start.Format("20060102T150405Z"))
	err := s.store.Put(ctx, name, bytes.NewReader(body))

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.start.Equal(start) {
		return err
	}
	switch {
	case err != nil:
		s.dirty = true
		if s.buf.Len() > segmentMost {
			s.buf.Reset()
			s.dirty = false
		}
	case s.now().Sub(start) >= segmentAge || s.buf.Len() >= segmentBytes:
		// Whatever was written while the upload ran is in the next flush of
		// this segment, not a new one; only roll once it has all been stored.
		if s.buf.Len() == len(body) {
			s.buf.Reset()
		}
	}
	return err
}
