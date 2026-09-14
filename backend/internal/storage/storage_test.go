package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
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
