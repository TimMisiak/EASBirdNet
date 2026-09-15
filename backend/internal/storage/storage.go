// Package storage keeps card audio. Browsers send files with the tus resumable
// upload protocol, so a backend is a tusd data store plus a way to read a
// finished file back: a directory for local development, and Azure Blob
// Storage in production. Config.Backend picks between them.
//
// Both lay files out the same way. An upload's bytes are at Name(id), and tusd
// keeps its own record of the upload beside them, at Name(id) + ".info". Files
// the server cuts from that audio, detection clips, are under ClipName.
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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
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
	// Open reads a stored file by name, as Name or ClipName gave it.
	Open(ctx context.Context, name string) (io.ReadCloser, error)
	// Put stores a file the server made itself, a detection clip, replacing
	// anything under that name. Card audio only ever arrives over tus.
	Put(ctx context.Context, name string, body io.ReadSeeker) error
	// DeleteAll removes everything stored for the card with that prefix: every
	// upload whose id starts with prefix + "/", finished or not, with tusd's
	// records of them, and the card's clips. A prefix with nothing stored under
	// it is not an error.
	DeleteAll(ctx context.Context, prefix string) error
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

// under is the start of every name stored for the card with that prefix: its
// uploads, whose ids start with prefix + "/", and its clips. The prefix is one
// path element, so it can't reach outside either.
func under(prefix string) ([]string, error) {
	if strings.Contains(prefix, "/") || !fs.ValidPath(prefix) || prefix == "." {
		return nil, fmt.Errorf("storage: invalid prefix %q", prefix)
	}
	return []string{Name(prefix) + "/", clipPrefix + "/" + prefix + "/"}, nil
}

// clipPrefix is where detection clips go: beside the card audio rather than
// inside it, so a lifecycle rule that tiers or deletes originals leaves the
// clips reviewers listen to alone.
const clipPrefix = "clips"

// ClipName is where a detection's clip is stored. A card's clips share a
// prefix, like its audio.
func ClipName(uploadID, detectionID string) string {
	return clipPrefix + "/" + segment(uploadID) + "/" + segment(detectionID) + ".wav"
}

// segment keeps an id to letters, digits, "-", "_" and ".", so it can't add a
// level to a name or climb out of one.
func segment(id string) string {
	id = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)
	if strings.Trim(id, ".") == "" {
		return strings.Repeat("_", max(len(id), 1))
	}
	return id
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

func (s *local) DeleteAll(_ context.Context, prefix string) error {
	names, err := under(prefix)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(s.dir, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("storage: %w", err)
		}
	}
	return nil
}

func (s *local) Put(_ context.Context, name string, body io.ReadSeeker) error {
	if !fs.ValidPath(name) {
		return fmt.Errorf("storage: invalid name %q", name)
	}
	dst := filepath.Join(s.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o775); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	// Write beside it and rename, so a reader never gets half a file.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".put-*")
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o664)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		return fmt.Errorf("storage: writing %s: %w", name, err)
	}
	return nil
}

// --- Azure Blob Storage ---

type azure struct {
	service azurestore.AzService
	// container lists and deletes blobs, which tusd's AzService can't.
	container *container.Client
	tus       *azurestore.AzureStore
	locker    *memorylocker.MemoryLocker
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
	client, err := containerClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("storage: connecting to blob container %s: %w", cfg.AzureContainer, err)
	}
	tus := azurestore.New(service)
	tus.ObjectPrefix = prefix
	tus.Container = cfg.AzureContainer
	return &azure{service: service, container: client, tus: tus, locker: memorylocker.New()}, nil
}

// containerClient signs in the same way tusd's AzService does.
func containerClient(cfg Config) (*container.Client, error) {
	url := strings.TrimSuffix(cfg.AzureEndpoint, "/") + "/" + cfg.AzureContainer
	if cfg.AzureAccountKey != "" {
		cred, err := container.NewSharedKeyCredential(cfg.AzureAccountName, cfg.AzureAccountKey)
		if err != nil {
			return nil, err
		}
		return container.NewClientWithSharedKeyCredential(url, cred, nil)
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	return container.NewClient(url, cred, nil)
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

// DeleteAll deletes one blob at a time: a card is a few hundred files, which
// is seconds of requests inside Azure.
func (s *azure) DeleteAll(ctx context.Context, prefix string) error {
	names, err := under(prefix)
	if err != nil {
		return err
	}
	for _, name := range names {
		// A file that was never finished is uncommitted blocks, which are only
		// listed when asked for.
		pager := s.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
			Prefix:  &name,
			Include: container.ListBlobsInclude{UncommittedBlobs: true},
		})
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return fmt.Errorf("storage: listing %s: %w", name, err)
			}
			for _, item := range page.Segment.BlobItems {
				_, err := s.container.NewBlobClient(*item.Name).Delete(ctx, &blob.DeleteOptions{
					DeleteSnapshots: to.Ptr(blob.DeleteSnapshotsOptionTypeInclude),
				})
				if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
					return fmt.Errorf("storage: deleting %s: %w", *item.Name, err)
				}
			}
		}
	}
	return nil
}

// Put writes a block blob in one block: tusd's blob stages what it is given as
// a block, and committing the list makes the blob. A name ending in ".info"
// would be taken for tusd's own record, which ClipName never gives.
func (s *azure) Put(ctx context.Context, name string, body io.ReadSeeker) error {
	blob, err := s.service.NewBlob(ctx, name)
	if err == nil {
		err = blob.Upload(ctx, body)
	}
	if err == nil {
		err = blob.Commit(ctx)
	}
	if err != nil {
		return fmt.Errorf("storage: writing %s: %w", name, err)
	}
	return nil
}
