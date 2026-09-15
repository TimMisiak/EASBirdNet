package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	tushandler "github.com/tus/tusd/v2/pkg/handler"
)

// testStore sends one file through a store the way the tus handler does --
// create, two chunks, finish -- and reads it back by name.
func testStore(t *testing.T, s Store) {
	t.Helper()
	ctx := t.Context()
	composer := tushandler.NewStoreComposer()
	s.UseIn(composer)
	if !composer.UsesLocker {
		t.Error("the store brings no locker")
	}

	id := "OWL-20260907-SR02/" + t.Name()
	data := []byte("RIFF....WAVEfmt this is not really audio")
	up, err := composer.Core.NewUpload(ctx, tushandler.FileInfo{ID: id, Size: int64(len(data))})
	if err != nil {
		t.Fatalf("new upload: %v", err)
	}
	for _, chunk := range [][]byte{data[:10], data[10:]} {
		info, err := up.GetInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := up.WriteChunk(ctx, info.Offset, bytes.NewReader(chunk)); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}

	// A retried request finds the upload again with the whole offset.
	again, err := composer.Core.GetUpload(ctx, id)
	if err != nil {
		t.Fatalf("get upload: %v", err)
	}
	if info, _ := again.GetInfo(ctx); info.Offset != int64(len(data)) {
		t.Errorf("offset = %d, want %d", info.Offset, len(data))
	}
	if err := again.FinishUpload(ctx); err != nil {
		t.Fatalf("finish: %v", err)
	}

	r, err := s.Open(ctx, Name(id))
	if err != nil {
		t.Fatalf("open %s: %v", Name(id), err)
	}
	defer r.Close()
	if got, _ := io.ReadAll(r); !bytes.Equal(got, data) {
		t.Errorf("stored %q, want %q", got, data)
	}

	if _, err := s.Open(ctx, Name("OWL-20260907-SR02/missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("open a missing file: err = %v, want ErrNotFound", err)
	}

	// A clip the server cut is written whole, and a second cut replaces it.
	clip := ClipName("OWL-20260907-SR02", "det_"+t.Name())
	for _, want := range []string{"RIFF a longer first clip", "RIFF second"} {
		if err := s.Put(ctx, clip, strings.NewReader(want)); err != nil {
			t.Fatalf("put %s: %v", clip, err)
		}
		r, err := s.Open(ctx, clip)
		if err != nil {
			t.Fatalf("open %s: %v", clip, err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != want {
			t.Errorf("clip = %q, want %q", got, want)
		}
	}

	// Deleting a card's audio takes its unfinished files and its clips too, and
	// nothing of a card whose reference only starts the same way.
	partial := "OWL-20260907-SR02/" + t.Name() + "-partial"
	up, err = composer.Core.NewUpload(ctx, tushandler.FileInfo{ID: partial, Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.WriteChunk(ctx, 0, bytes.NewReader(data[:10])); err != nil {
		t.Fatal(err)
	}
	neighbour := "OWL-20260907-SR020/" + t.Name()
	up, err = composer.Core.NewUpload(ctx, tushandler.FileInfo{ID: neighbour, Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.WriteChunk(ctx, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := up.FinishUpload(ctx); err != nil {
		t.Fatal(err)
	}
	neighbourClip := ClipName("OWL-20260907-SR020", "det_"+t.Name())
	if err := s.Put(ctx, neighbourClip, strings.NewReader("RIFF neighbour")); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAll(ctx, "OWL-20260907-SR02"); err != nil {
		t.Fatalf("delete the card's audio: %v", err)
	}
	for _, gone := range []string{Name(id), clip} {
		if _, err := s.Open(ctx, gone); !errors.Is(err, ErrNotFound) {
			t.Errorf("open deleted %s: err = %v, want ErrNotFound", gone, err)
		}
	}
	if _, err := composer.Core.GetUpload(ctx, partial); err == nil {
		t.Error("the unfinished upload is still there")
	}
	for _, kept := range []string{Name(neighbour), neighbourClip} {
		if r, err := s.Open(ctx, kept); err != nil {
			t.Errorf("the other card's %s went too: %v", kept, err)
		} else {
			r.Close()
		}
	}
	if err := s.DeleteAll(ctx, "OWL-20260907-SR02"); err != nil {
		t.Errorf("delete again: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "OWL-20260907-SR02/x"} {
		if err := s.DeleteAll(ctx, bad); err == nil {
			t.Errorf("DeleteAll(%q) was allowed", bad)
		}
	}
}

func TestClipName(t *testing.T) {
	for _, c := range []struct{ upload, det, want string }{
		{"OWL-20260907-SR02", "det_9c41", "clips/OWL-20260907-SR02/det_9c41.wav"},
		{"OWL-20260907-SR/../x", "det_1", "clips/OWL-20260907-SR_.._x/det_1.wav"},
		{"..", "det_1", "clips/__/det_1.wav"},
	} {
		if got := ClipName(c.upload, c.det); got != c.want {
			t.Errorf("ClipName(%q, %q) = %q, want %q", c.upload, c.det, got, c.want)
		}
	}
}

func TestLocal(t *testing.T) {
	s, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testStore(t, s)

	if _, err := s.Open(t.Context(), "../birdsense.json"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("open outside the directory: err = %v, want it refused", err)
	}
}

// TestAzure runs against Azurite, and is skipped unless
// BIRDSENSE_TEST_AZURITE names its blob endpoint, e.g.
//
//	npx azurite-blob --location /tmp/azurite &
//	BIRDSENSE_TEST_AZURITE=http://127.0.0.1:10000/devstoreaccount1 go test ./internal/storage
func TestAzure(t *testing.T) {
	endpoint := os.Getenv("BIRDSENSE_TEST_AZURITE")
	if endpoint == "" {
		t.Skip("set BIRDSENSE_TEST_AZURITE to Azurite's blob endpoint to run")
	}
	s, err := Open(Config{
		Backend:          BackendAzure,
		AzureEndpoint:    endpoint,
		AzureContainer:   "audio",
		AzureAccountName: "devstoreaccount1",
		// Azurite's published development key, not a secret.
		AzureAccountKey: "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==",
	})
	if err != nil {
		t.Fatal(err)
	}
	testStore(t, s)
}
