package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
)

// UNTESTED: this backend compiles against the real SDK but has not yet been run
// against an Azure account or the emulator. The local backend and its tests
// define the behaviour this one has to match.

// Containers and their partition keys. DEPLOYMENT.md lists the same five; the
// app never creates them (its data-plane role can't, and Terraform owns them).
const (
	containerUsers      = "users"      // partition key /id
	containerRecorders  = "recorders"  // partition key /id
	containerUploads    = "uploads"    // partition key /id
	containerAudioFiles = "audioFiles" // partition key /uploadId
	containerDetections = "detections" // partition key /uploadId
)

const (
	// maxUpdateAttempts bounds the ETag retry loop in updateDoc.
	maxUpdateAttempts = 5
	// batchWorkers is how many writes a batch runs at once.
	batchWorkers = 8
)

type cosmosStore struct {
	users, recorders, uploads, audioFiles, detections *azcosmos.ContainerClient
}

var _ Store = (*cosmosStore)(nil)

// OpenCosmos connects to a Cosmos DB for NoSQL account and checks that every
// container exists, so a misconfigured deployment fails at startup rather than
// on its first request.
//
// Authentication is Microsoft Entra ID through DefaultAzureCredential: the
// container app's managed identity in Azure, `az login` on a laptop. Setting
// Config.CosmosKey uses an account key instead, for the emulator.
func OpenCosmos(ctx context.Context, cfg Config) (Store, error) {
	if cfg.CosmosEndpoint == "" {
		return nil, errors.New("db: the cosmos backend needs an account endpoint")
	}
	if cfg.CosmosDatabase == "" {
		return nil, errors.New("db: the cosmos backend needs a database name")
	}

	var client *azcosmos.Client
	if cfg.CosmosKey != "" {
		cred, err := azcosmos.NewKeyCredential(cfg.CosmosKey)
		if err != nil {
			return nil, fmt.Errorf("db: cosmos key: %w", err)
		}
		if client, err = azcosmos.NewClientWithKey(cfg.CosmosEndpoint, cred, nil); err != nil {
			return nil, fmt.Errorf("db: cosmos client: %w", err)
		}
	} else {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("db: azure credential: %w", err)
		}
		if client, err = azcosmos.NewClient(cfg.CosmosEndpoint, cred, nil); err != nil {
			return nil, fmt.Errorf("db: cosmos client: %w", err)
		}
	}

	s := &cosmosStore{}
	for name, dst := range map[string]**azcosmos.ContainerClient{
		containerUsers:      &s.users,
		containerRecorders:  &s.recorders,
		containerUploads:    &s.uploads,
		containerAudioFiles: &s.audioFiles,
		containerDetections: &s.detections,
	} {
		c, err := client.NewContainer(cfg.CosmosDatabase, name)
		if err != nil {
			return nil, fmt.Errorf("db: cosmos container %s/%s: %w", cfg.CosmosDatabase, name, err)
		}
		if _, err := c.Read(ctx, nil); err != nil {
			return nil, fmt.Errorf("db: cosmos container %s/%s: %w", cfg.CosmosDatabase, name, err)
		}
		*dst = c
	}
	return s, nil
}

// Close is a no-op: the SDK client holds no resources that need releasing.
func (s *cosmosStore) Close() error { return nil }

// --- users ---

func (s *cosmosStore) GetUser(ctx context.Context, id string) (User, error) {
	return readDoc[User](ctx, s.users, id, id)
}

func (s *cosmosStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	users, err := queryDocs[User](ctx, s.users, "", "SELECT * FROM c WHERE c.email = @email",
		param("@email", normalizeEmail(email)))
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrNotFound
	}
	return users[0], nil
}

func (s *cosmosStore) ListUsers(ctx context.Context) ([]User, error) {
	users, err := queryDocs[User](ctx, s.users, "", "SELECT * FROM c")
	sortUsers(users)
	return users, err
}

// CreateUser checks the address with a query before inserting. Two admins
// adding the same address in the same instant could both pass; the roster is
// small and admin-only, so that race is accepted rather than paying for a
// unique-key container layout.
func (s *cosmosStore) CreateUser(ctx context.Context, u User) (User, error) {
	if err := prepareUser(&u, now()); err != nil {
		return User{}, err
	}
	if err := s.checkEmail(ctx, u.Email, u.ID); err != nil {
		return User{}, err
	}
	return u, createDoc(ctx, s.users, u.ID, u)
}

func (s *cosmosStore) UpdateUser(ctx context.Context, id string, mutate func(*User) error) (User, error) {
	return updateDoc(ctx, s.users, id, id, func(u *User, old User) error {
		if err := mutate(u); err != nil {
			return err
		}
		u.keep(old, now())
		if u.Email != old.Email {
			return s.checkEmail(ctx, u.Email, u.ID)
		}
		return nil
	})
}

func (s *cosmosStore) checkEmail(ctx context.Context, email, exceptID string) error {
	switch existing, err := s.GetUserByEmail(ctx, email); {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case existing.ID != exceptID:
		return ErrConflict
	}
	return nil
}

// --- recorders ---

func (s *cosmosStore) GetRecorder(ctx context.Context, id string) (Recorder, error) {
	return readDoc[Recorder](ctx, s.recorders, id, id)
}

func (s *cosmosStore) ListRecorders(ctx context.Context) ([]Recorder, error) {
	recorders, err := queryDocs[Recorder](ctx, s.recorders, "", "SELECT * FROM c")
	sortRecorders(recorders)
	return recorders, err
}

func (s *cosmosStore) CreateRecorder(ctx context.Context, r Recorder) (Recorder, error) {
	if err := prepareRecorder(&r, now()); err != nil {
		return Recorder{}, err
	}
	return r, createDoc(ctx, s.recorders, r.ID, r)
}

func (s *cosmosStore) UpdateRecorder(ctx context.Context, id string, mutate func(*Recorder) error) (Recorder, error) {
	return updateDoc(ctx, s.recorders, id, id, func(r *Recorder, old Recorder) error {
		if err := mutate(r); err != nil {
			return err
		}
		r.keep(old, now())
		return nil
	})
}

// --- uploads ---

func (s *cosmosStore) GetUpload(ctx context.Context, id string) (Upload, error) {
	return readDoc[Upload](ctx, s.uploads, id, id)
}

func (s *cosmosStore) ListUploads(ctx context.Context, f UploadFilter) ([]Upload, error) {
	var w where
	if f.UserID != "" {
		w.add("c.userId = @userId", "@userId", f.UserID)
	}
	if f.Status != "" {
		w.add("c.status = @status", "@status", f.Status)
	}
	uploads, err := queryDocs[Upload](ctx, s.uploads, "", "SELECT * FROM c"+w.sql(), w.params...)
	sortUploads(uploads)
	return uploads, err
}

func (s *cosmosStore) CreateUpload(ctx context.Context, u Upload) (Upload, error) {
	if err := prepareUpload(&u, now()); err != nil {
		return Upload{}, err
	}
	return u, createDoc(ctx, s.uploads, u.ID, u)
}

func (s *cosmosStore) UpdateUpload(ctx context.Context, id string, mutate func(*Upload) error) (Upload, error) {
	return updateDoc(ctx, s.uploads, id, id, func(u *Upload, old Upload) error {
		if err := mutate(u); err != nil {
			return err
		}
		u.keep(old, now())
		return nil
	})
}

// DeleteUpload is not atomic: there is no transaction across containers. Audio
// files go before detections, because the analysis queue treats its file going
// missing as the card being deleted, and sweeps up detections it stored after
// this had listed them (internal/analysis).
func (s *cosmosStore) DeleteUpload(ctx context.Context, id string) error {
	_, err := readDoc[Upload](ctx, s.uploads, id, id)
	missing := errors.Is(err, ErrNotFound)
	if err != nil && !missing {
		return err
	}
	for _, c := range []*azcosmos.ContainerClient{s.audioFiles, s.detections} {
		if err := deletePartition(ctx, c, id); err != nil {
			return err
		}
	}
	if missing {
		return ErrNotFound
	}
	_, err = s.uploads.DeleteItem(ctx, azcosmos.NewPartitionKeyString(id), id, nil)
	return cosmosErr(err)
}

// --- audio files ---

func (s *cosmosStore) GetAudioFile(ctx context.Context, uploadID, id string) (AudioFile, error) {
	return readDoc[AudioFile](ctx, s.audioFiles, uploadID, id)
}

func (s *cosmosStore) ListAudioFiles(ctx context.Context, uploadID string) ([]AudioFile, error) {
	files, err := queryDocs[AudioFile](ctx, s.audioFiles, uploadID, "SELECT * FROM c")
	sortAudioFiles(files)
	return files, err
}

func (s *cosmosStore) UpsertAudioFiles(ctx context.Context, uploadID string, files []AudioFile) error {
	t := now()
	docs := make([]AudioFile, len(files))
	for i, f := range files {
		if err := prepareAudioFile(uploadID, &f, t); err != nil {
			return err
		}
		docs[i] = f
	}
	return upsertAll(ctx, s.audioFiles, uploadID, docs)
}

func (s *cosmosStore) UpdateAudioFile(ctx context.Context, uploadID, id string, mutate func(*AudioFile) error) (AudioFile, error) {
	return updateDoc(ctx, s.audioFiles, uploadID, id, func(f *AudioFile, old AudioFile) error {
		if err := mutate(f); err != nil {
			return err
		}
		f.keep(old, now())
		return nil
	})
}

// --- detections ---

// ListDetections is a single-partition query when f.UploadID is set and a
// cross-partition one otherwise (the public overview). Timestamps are compared
// as RFC 3339 strings, which only order correctly to the whole second, so the
// query widens Since by a second and the shared filter trims the result.
func (s *cosmosStore) ListDetections(ctx context.Context, f DetectionFilter) ([]Detection, error) {
	var w where
	if f.AudioFileID != "" {
		w.add("c.audioFileId = @audioFileId", "@audioFileId", f.AudioFileID)
	}
	if f.ReviewStatus != "" {
		w.add("c.reviewStatus = @reviewStatus", "@reviewStatus", f.ReviewStatus)
	}
	if !f.Since.IsZero() {
		since := f.Since.UTC().Truncate(time.Second).Add(-time.Second)
		w.add("c.detectedAt >= @since", "@since", since.Format(time.RFC3339))
	}
	found, err := queryDocs[Detection](ctx, s.detections, f.UploadID, "SELECT * FROM c"+w.sql(), w.params...)
	if err != nil {
		return nil, err
	}
	out := found[:0]
	for _, d := range found {
		if f.match(d) {
			out = append(out, d)
		}
	}
	sortDetections(out)
	return out, nil
}

func (s *cosmosStore) UpsertDetections(ctx context.Context, uploadID string, detections []Detection) error {
	t := now()
	docs := make([]Detection, len(detections))
	for i, d := range detections {
		if err := prepareDetection(uploadID, &d, t); err != nil {
			return err
		}
		docs[i] = d
	}
	return upsertAll(ctx, s.detections, uploadID, docs)
}

func (s *cosmosStore) UpdateDetection(ctx context.Context, uploadID, id string, mutate func(*Detection) error) (Detection, error) {
	return updateDoc(ctx, s.detections, uploadID, id, func(d *Detection, old Detection) error {
		if err := mutate(d); err != nil {
			return err
		}
		d.keep(old, now())
		return nil
	})
}

// --- plumbing ---

func readDoc[T any](ctx context.Context, c *azcosmos.ContainerClient, partition, id string) (T, error) {
	var v T
	resp, err := c.ReadItem(ctx, azcosmos.NewPartitionKeyString(partition), id, nil)
	if err != nil {
		return v, cosmosErr(err)
	}
	return v, decodeDoc(resp.Value, &v)
}

func createDoc[T any](ctx context.Context, c *azcosmos.ContainerClient, partition string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.CreateItem(ctx, azcosmos.NewPartitionKeyString(partition), b, nil)
	return cosmosErr(err)
}

// updateDoc is read, mutate, replace-if-unchanged. A 412 means someone else
// wrote the document in between, so it reads again and reapplies.
func updateDoc[T any](ctx context.Context, c *azcosmos.ContainerClient, partition, id string, apply func(v *T, old T) error) (T, error) {
	var zero T
	pk := azcosmos.NewPartitionKeyString(partition)
	for range maxUpdateAttempts {
		resp, err := c.ReadItem(ctx, pk, id, nil)
		if err != nil {
			return zero, cosmosErr(err)
		}
		var old, v T
		if err := decodeDoc(resp.Value, &old); err != nil {
			return zero, err
		}
		if err := decodeDoc(resp.Value, &v); err != nil {
			return zero, err
		}
		if err := apply(&v, old); err != nil {
			return zero, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return zero, err
		}
		etag := resp.ETag
		_, err = c.ReplaceItem(ctx, pk, id, b, &azcosmos.ItemOptions{IfMatchEtag: &etag})
		if hasStatus(err, http.StatusPreconditionFailed) {
			continue
		}
		if err != nil {
			return zero, cosmosErr(err)
		}
		return v, nil
	}
	return zero, fmt.Errorf("%w: %s kept changing underneath the update", ErrConflict, id)
}

// upsertAll writes a batch a few documents at a time. It is not atomic:
// documents written before a failure stay written, which is safe because every
// batch id is deterministic and a retry overwrites.
func upsertAll[T any](ctx context.Context, c *azcosmos.ContainerClient, partition string, docs []T) error {
	pk := azcosmos.NewPartitionKeyString(partition)
	return inParallel(ctx, docs, func(ctx context.Context, doc T) error {
		b, err := json.Marshal(doc)
		if err == nil {
			_, err = c.UpsertItem(ctx, pk, b, nil)
		}
		return cosmosErr(err)
	})
}

type docID struct {
	ID string `json:"id"`
}

// deletePartition deletes every document in one logical partition, a few at a
// time. A document that is already gone counts as deleted.
func deletePartition(ctx context.Context, c *azcosmos.ContainerClient, partition string) error {
	docs, err := queryDocs[docID](ctx, c, partition, "SELECT c.id FROM c")
	if err != nil {
		return err
	}
	pk := azcosmos.NewPartitionKeyString(partition)
	return inParallel(ctx, docs, func(ctx context.Context, doc docID) error {
		_, err := c.DeleteItem(ctx, pk, doc.ID, nil)
		if hasStatus(err, http.StatusNotFound) {
			return nil
		}
		return cosmosErr(err)
	})
}

// inParallel runs do over items, batchWorkers at a time, and stops at the
// first failure.
func inParallel[T any](ctx context.Context, items []T, do func(context.Context, T) error) error {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan T)
	firstErr := make(chan error, 1)
	var wg sync.WaitGroup
	for range batchWorkers {
		wg.Go(func() {
			for item := range jobs {
				if err := do(workCtx, item); err != nil {
					select {
					case firstErr <- err:
						cancel()
					default:
					}
				}
			}
		})
	}
feed:
	for _, item := range items {
		select {
		case jobs <- item:
		case <-workCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-firstErr:
		return err
	default:
		return ctx.Err()
	}
}

// queryDocs runs a query in one partition, or across all of them when
// partition is "". Cross-partition queries in the Go SDK only support what the
// gateway can serve: keep them to SELECT * ... WHERE, and sort, count and
// de-duplicate in Go.
func queryDocs[T any](ctx context.Context, c *azcosmos.ContainerClient, partition, query string, params ...azcosmos.QueryParameter) ([]T, error) {
	pk := azcosmos.NewPartitionKey()
	if partition != "" {
		pk = azcosmos.NewPartitionKeyString(partition)
	}
	pager := c.NewQueryItemsPager(query, pk, &azcosmos.QueryOptions{QueryParameters: params})
	out := []T{}
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, cosmosErr(err)
		}
		for _, item := range page.Items {
			var v T
			if err := decodeDoc(item, &v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// decodeDoc reads a stored document. Cosmos adds system properties (_rid,
// _etag, _ts, ...) that the structs don't declare; json.Unmarshal drops them.
func decodeDoc(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("db: decoding %T: %w", v, err)
	}
	return nil
}

func param(name string, value any) azcosmos.QueryParameter {
	return azcosmos.QueryParameter{Name: name, Value: value}
}

// where builds a parameterized WHERE clause. Values are always parameters,
// never spliced into the SQL.
type where struct {
	clauses []string
	params  []azcosmos.QueryParameter
}

func (w *where) add(clause, name string, value any) {
	w.clauses = append(w.clauses, clause)
	w.params = append(w.params, param(name, value))
}

func (w *where) sql() string {
	if len(w.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.clauses, " AND ")
}

func hasStatus(err error, status int) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == status
}

// cosmosErr turns the SDK's HTTP errors into the package's sentinel errors.
func cosmosErr(err error) error {
	switch {
	case err == nil:
		return nil
	case hasStatus(err, http.StatusNotFound):
		return ErrNotFound
	case hasStatus(err, http.StatusConflict), hasStatus(err, http.StatusPreconditionFailed):
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return err
}
