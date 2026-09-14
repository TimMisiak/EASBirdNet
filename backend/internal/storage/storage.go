// Package storage keeps card audio. Browsers send files with the tus resumable
// upload protocol, so a backend is a tusd data store plus a way to read a
// finished file back: a directory for local development, and Azure Blob
// Storage in production. Config.Backend picks between them.
//
// Both lay files out the same way. An upload's bytes are at Name(id), and tusd
// keeps its own record of the upload beside them, at Name(id) + ".info".
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tus/tusd/v2/pkg/azurestore"
	"github.com/tus/tusd/v2/pkg/filestore"
	tushandler "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
)

// ErrNotFound means nothing is stored under that name.
var ErrNotFound = errors.New("storage: not found")

// Store is where card audio lives.
type Store interface {
	// UseIn makes this store the data store of a tusd handler, with a lock
	// per upload so a retried request can't write over one still running.
	UseIn(composer *tushandler.StoreComposer)
	// Open reads a stored file by name, as Name gave it.
	Open(ctx context.Context, name string) (io.ReadCloser, error)
}

// prefix is where every upload goes, so the card's audio and nothing else is
// under it (and a lifecycle rule can target it).
const prefix = "uploads"

// Name is where a tus upload's bytes are stored: the blob name in Azure, the
// path under the directory locally. Upload ids start with the card reference,
// so a card's files share a prefix.
func Name(uploadID string) string {
	return prefix + "/" + uploadID
}

// Backends.
const (
	BackendLocal = "local"
	BackendAzure = "azure"
)

// Config selects and configures a backend.
type Config struct {
	// Backend is BackendLocal or BackendAzure.
	Backend string
	// LocalDir is the directory the local backend writes into.
	LocalDir string
	// AzureEndpoint is the storage account's blob endpoint, e.g.
	// https://stbirdsenseprod.blob.core.windows.net.
	AzureEndpoint string
	// AzureContainer is the blob container audio goes into.
	AzureContainer string
	// AzureAccountName and AzureAccountKey switch from Entra ID to shared key
	// auth. They exist for the Azurite emulator in tests; the production
	// account has shared keys turned off.
	AzureAccountName string
	AzureAccountKey  string
}

// Open connects to the configured backend.
func Open(cfg Config) (Store, error) {
	switch cfg.Backend {
	case BackendLocal:
		return OpenLocal(cfg.LocalDir)
	case BackendAzure:
		return OpenAzure(cfg)
	default:
		return nil, fmt.Errorf("storage: unknown backend %q (want %q or %q)", cfg.Backend, BackendLocal, BackendAzure)
	}
}

// --- local directory ---

type local struct {
	dir    string
	tus    filestore.FileStore
	locker *memorylocker.MemoryLocker
}

// OpenLocal stores files under dir, at the same names they would have in Azure.
func OpenLocal(dir string) (Store, error) {
	if dir == "" {
		return nil, errors.New("storage: the local backend needs a directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, prefix), 0o775); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &local{dir: dir, tus: filestore.New(filepath.Join(dir, prefix)), locker: memorylocker.New()}, nil
}

func (s *local) UseIn(composer *tushandler.StoreComposer) {
	s.tus.UseIn(composer)
	s.locker.UseIn(composer)
}

func (s *local) Open(_ context.Context, name string) (io.ReadCloser, error) {
	// A name is a slash-separated path inside the directory, never out of it.
	if !fs.ValidPath(name) {
		return nil, fmt.Errorf("storage: invalid name %q", name)
	}
	f, err := os.Open(filepath.Join(s.dir, filepath.FromSlash(name)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return f, err
}

// --- Azure Blob Storage ---

type azure struct {
	service azurestore.AzService
	tus     *azurestore.AzureStore
	locker  *memorylocker.MemoryLocker
}

// OpenAzure stores files as block blobs, using tusd's azurestore: each PATCH
// is staged as a block, and the block list is committed when the last byte
// lands, so a blob only appears once it is whole.
//
// Without an account key it signs in with DefaultAzureCredential, which is the
// app's managed identity in Azure. tusd creates the container if it is
// missing, so the identity needs Storage Blob Data Contributor.
//
// Locks are in memory, so every request for one upload has to reach the same
// process: this backend is for a single replica (see DEPLOYMENT.md).
func OpenAzure(cfg Config) (Store, error) {
	if cfg.AzureEndpoint == "" || cfg.AzureContainer == "" {
		return nil, errors.New("storage: the azure backend needs a blob endpoint and a container")
	}
	service, err := azurestore.NewAzureService(&azurestore.AzConfig{
		Endpoint:      strings.TrimSuffix(cfg.AzureEndpoint, "/"),
		ContainerName: cfg.AzureContainer,
		AccountName:   cfg.AzureAccountName,
		AccountKey:    cfg.AzureAccountKey,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: connecting to blob container %s: %w", cfg.AzureContainer, err)
	}
	tus := azurestore.New(service)
	tus.ObjectPrefix = prefix
	tus.Container = cfg.AzureContainer
	return &azure{service: service, tus: tus, locker: memorylocker.New()}, nil
}

func (s *azure) UseIn(composer *tushandler.StoreComposer) {
	s.tus.UseIn(composer)
	s.locker.UseIn(composer)
}

func (s *azure) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	blob, err := s.service.NewBlob(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	r, err := blob.Download(ctx)
	var tusErr tushandler.Error
	if errors.As(err, &tusErr) && tusErr.ErrorCode == tushandler.ErrNotFound.ErrorCode {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return r, err
}
