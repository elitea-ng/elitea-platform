package gcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gcstorage "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// Config holds GCS-specific configuration.
type Config struct {
	Bucket          string // single GCS bucket used as root
	CredentialsFile string // path to service account JSON

	// Endpoint overrides the default public GCS API host for reaching an
	// emulator (e.g. fake-gcs-server). It must be a full base URL including
	// the "/storage/v1/" path, for example "http://localhost:4443/storage/v1/"
	// — this is a complete base-URL override, not a host override, so a bare
	// host value silently routes every call to the wrong path and 404s on
	// every Stat, List, and Delete. Mutually exclusive with CredentialsFile.
	Endpoint string

	// KeyPrefix namespaces every ObjectStore key under Bucket; passed
	// through to ObjectRef.StorageKey/BucketPrefix. Optional, empty by
	// default.
	KeyPrefix string
}

// Backend implements storage.ObjectStore using Google Cloud Storage.
// All objects live in a single GCS bucket with key prefix: {projectID}/{bucketName}/{key}
type Backend struct {
	client    *gcstorage.Client
	bucket    string
	keyPrefix string

	// hasSigningMaterial is true when New was given a service-account
	// credentials file, which BucketHandle.SignedURL can auto-detect a
	// signing key from. Ambient/ADC credentials (the CredentialsFile-empty
	// case) do not expose local signing material. Capabilities() reads this.
	hasSigningMaterial bool
}

var _ storage.ObjectStore = (*Backend)(nil)

// clientOptions validates cfg and builds the client.ClientOption slice New
// passes to gcstorage.NewClient. Split out from New so the validation and
// option-selection logic can be tested without constructing a real client.
func clientOptions(cfg Config) ([]option.ClientOption, error) {
	if cfg.Endpoint != "" && cfg.CredentialsFile != "" {
		return nil, fmt.Errorf("storage/gcs: GCS_ENDPOINT and GOOGLE_APPLICATION_CREDENTIALS are mutually exclusive")
	}

	var opts []option.ClientOption
	switch {
	case cfg.CredentialsFile != "":
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, cfg.CredentialsFile))
	case cfg.Endpoint != "":
		opts = append(opts, option.WithEndpoint(cfg.Endpoint), option.WithoutAuthentication())
	}
	return opts, nil
}

// New creates a GCS Backend.
func New(ctx context.Context, cfg Config) (*Backend, error) {
	opts, err := clientOptions(cfg)
	if err != nil {
		return nil, err
	}

	client, err := gcstorage.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("storage/gcs: new client: %w", err)
	}
	return &Backend{
		client:             client,
		bucket:             cfg.Bucket,
		keyPrefix:          cfg.KeyPrefix,
		hasSigningMaterial: cfg.CredentialsFile != "",
	}, nil
}

// ConfigureRetentionLifecycle sets a coarse, one-time, provider-native
// bucket lifecycle rule aborting incomplete multipart uploads left dangling
// by a crashed/abandoned client after 7 days (actual object/bucket
// retention stays a Go-owned ledger — internal/runtimecomposition's
// artifact retention sweep — not a provider lifecycle rule; this is the one
// deliberate exception). This backend's own Capabilities().NativeMultipart
// is false — Put uses GCS's JSON-API resumable upload, not the XML-API
// multipart upload this lifecycle action targets — so today this is
// defense-in-depth against a mechanism this backend does not itself
// create, not an active leak closed. Kept for parity with S3/Azure and in
// case that changes.
//
// Deliberately not called from New(): New() must stay a cheap,
// non-network-dialing constructor — cmd/elitea-main's
// TestArtifactNewObjectStoreDispatchesPerBackend asserts exactly that for
// every backend. This is a separate, explicit startup step, alongside
// main.go's own objectStoreReadinessProbe, not folded into construction.
//
// Empirically confirmed against fake-gcs-server (this stack's GCS
// emulator): the call succeeds without error, but the rule does not
// actually round-trip (Bucket.Attrs().Lifecycle stays empty afterward) — a
// known emulator limitation, not evidence the rule is a no-op against real
// GCS. Returning an error here on a real failure (e.g. the service account
// lacking storage.buckets.update) is intentional, matching this backend's
// fail-closed posture elsewhere.
func (b *Backend) ConfigureRetentionLifecycle(ctx context.Context) error {
	if _, err := b.client.Bucket(b.bucket).Update(ctx, gcstorage.BucketAttrsToUpdate{
		Lifecycle: &gcstorage.Lifecycle{
			Rules: []gcstorage.LifecycleRule{
				{
					Action:    gcstorage.LifecycleAction{Type: gcstorage.AbortIncompleteMPUAction},
					Condition: gcstorage.LifecycleCondition{AgeInDays: 7},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("storage/gcs: configure incomplete-multipart-upload lifecycle rule: %w", err)
	}
	return nil
}

func (b *Backend) Capabilities() storage.Capabilities {
	return storage.Capabilities{Presign: b.hasSigningMaterial, NativeMultipart: false, ServerSideCopy: true}
}

func (b *Backend) Put(ctx context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions) (storage.ObjectInfo, error) {
	obj := b.client.Bucket(b.bucket).Object(ref.StorageKey(b.keyPrefix))
	w := obj.NewWriter(ctx)
	w.ChunkSize = 8 << 20
	if opts.ContentType != "" {
		w.ContentType = opts.ContentType
	}
	if len(opts.Metadata) > 0 {
		w.Metadata = opts.Metadata
	}
	if _, err := io.Copy(w, body); err != nil {
		_ = w.Close()
		return storage.ObjectInfo{}, fmt.Errorf("storage/gcs: put object: %w", err)
	}
	if err := w.Close(); err != nil {
		return storage.ObjectInfo{}, mapGCSError(err, "put object")
	}
	attrs := w.Attrs()
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         attrs.Size,
		LastModified: attrs.Updated,
		ContentType:  attrs.ContentType,
		ETag:         attrs.Etag,
	}, nil
}

// Get issues a single call — NewRangeReader when rng != nil, NewReader
// otherwise — and reads attributes off the reader itself rather than a
// separate Attrs call, avoiding a TOCTOU window. ObjectInfo.ETag is
// deliberately empty on this path: ReaderObjectAttrs carries no ETag. Only
// Stat and List populate it, from ObjectAttrs.Etag.
func (b *Backend) Get(ctx context.Context, ref storage.ObjectRef, rng *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	obj := b.client.Bucket(b.bucket).Object(ref.StorageKey(b.keyPrefix))

	var reader *gcstorage.Reader
	var err error
	if rng != nil {
		length := int64(-1)
		if rng.End >= 0 {
			length = rng.End - rng.Start + 1
		}
		reader, err = obj.NewRangeReader(ctx, rng.Start, length)
	} else {
		reader, err = obj.NewReader(ctx)
	}
	if err != nil {
		return nil, storage.ObjectInfo{}, mapGCSError(err, "get object")
	}

	// reader.Attrs.Size is the size of the WHOLE object, even for a range
	// read — unlike the S3 and Azure responses, which report the range
	// length. Declaring it as the body length made every ranged download
	// short-write against its own Content-Length and the client saw an
	// unexpected EOF. Remain() gives the length of the range; it returns -1
	// for an unfinalized or gzip-transcoded object, so fall back to the
	// whole size there.
	size := reader.Attrs.Size
	if rng != nil {
		if remaining := reader.Remain(); remaining >= 0 {
			size = remaining
		}
	}
	info := storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         size,
		TotalSize:    reader.Attrs.Size,
		LastModified: reader.Attrs.LastModified,
		ContentType:  reader.Attrs.ContentType,
	}
	return reader, info, nil
}

func (b *Backend) Stat(ctx context.Context, ref storage.ObjectRef) (storage.ObjectInfo, error) {
	obj := b.client.Bucket(b.bucket).Object(ref.StorageKey(b.keyPrefix))
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return storage.ObjectInfo{}, mapGCSError(err, "stat object")
	}
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         attrs.Size,
		LastModified: attrs.Updated,
		ContentType:  attrs.ContentType,
		ETag:         attrs.Etag,
	}, nil
}

// Delete explicitly checks for ErrObjectNotExist and returns nil — unlike
// S3, GCS returns an error for a missing key, so the interface's
// idempotency rule needs an explicit check here.
func (b *Backend) Delete(ctx context.Context, ref storage.ObjectRef) error {
	obj := b.client.Bucket(b.bucket).Object(ref.StorageKey(b.keyPrefix))
	if err := obj.Delete(ctx); err != nil {
		if errors.Is(err, gcstorage.ErrObjectNotExist) {
			return nil
		}
		return mapGCSError(err, "delete object")
	}
	return nil
}

// DeleteBatch has no bulk API in this client library — issue Delete calls
// sequentially and report each outcome individually.
func (b *Backend) DeleteBatch(ctx context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	var result storage.BatchResult
	for _, ref := range refs {
		if err := b.Delete(ctx, ref); err != nil {
			result.Failed = append(result.Failed, storage.BatchError{Key: ref.Key(), Err: err})
			continue
		}
		result.Deleted = append(result.Deleted, ref.Key())
	}
	return result, nil
}

func (b *Backend) List(ctx context.Context, q storage.ListQuery) (storage.ListPage, error) {
	if err := storage.ValidateKeyPrefix(q.KeyPrefix); err != nil {
		return storage.ListPage{}, err
	}
	basePrefix := q.Bucket.BucketPrefix(b.keyPrefix)

	pageSize := int(q.MaxKeys)
	if pageSize <= 0 || pageSize > 1000 {
		pageSize = 1000
	}

	it := b.client.Bucket(b.bucket).Objects(ctx, &gcstorage.Query{
		Prefix:    basePrefix + q.KeyPrefix,
		Delimiter: q.Delimiter,
	})
	pager := iterator.NewPager(it, pageSize, q.ContinuationToken)

	var attrs []*gcstorage.ObjectAttrs
	nextPageToken, err := pager.NextPage(&attrs)
	if err != nil {
		return storage.ListPage{}, mapGCSError(err, "list objects")
	}

	page := storage.ListPage{
		NextContinuationToken: nextPageToken,
		IsTruncated:           nextPageToken != "",
	}
	for _, a := range attrs {
		// A Delimiter query interleaves real objects with "directory"
		// pseudo-entries in the same stream — a pseudo-entry carries Prefix
		// instead of Name, matching the old Backend's ListBuckets check.
		if a.Prefix != "" {
			page.CommonPrefixes = append(page.CommonPrefixes, strings.TrimPrefix(a.Prefix, basePrefix))
			continue
		}
		page.Objects = append(page.Objects, storage.ObjectInfo{
			Key:          strings.TrimPrefix(a.Name, basePrefix),
			Size:         a.Size,
			LastModified: a.Updated,
			ContentType:  a.ContentType,
			ETag:         a.Etag,
		})
	}
	return page, nil
}

func (b *Backend) PresignGet(ctx context.Context, ref storage.ObjectRef, ttl time.Duration) (string, error) {
	if !b.hasSigningMaterial {
		return "", storage.ErrNotSupported
	}
	signedURL, err := b.client.Bucket(b.bucket).SignedURL(ref.StorageKey(b.keyPrefix), &gcstorage.SignedURLOptions{
		Method:  http.MethodGet,
		Expires: time.Now().Add(ttl),
	})
	if err != nil {
		return "", fmt.Errorf("storage/gcs: presign get: %w", err)
	}
	return signedURL, nil
}

// PresignPut's opts.ContentType is intentionally not passed to
// SignedURLOptions.ContentType: doing so would require every subsequent PUT
// to send back that exact Content-Type header or fail signature validation,
// which is a stricter contract than S3/Azure's presigned PUT (S15 verifies
// media type on commit instead, uniformly across all three backends).
func (b *Backend) PresignPut(ctx context.Context, ref storage.ObjectRef, ttl time.Duration, opts storage.PutOptions) (string, error) {
	if !b.hasSigningMaterial {
		return "", storage.ErrNotSupported
	}
	signedURL, err := b.client.Bucket(b.bucket).SignedURL(ref.StorageKey(b.keyPrefix), &gcstorage.SignedURLOptions{
		Method:  http.MethodPut,
		Expires: time.Now().Add(ttl),
	})
	if err != nil {
		return "", fmt.Errorf("storage/gcs: presign put: %w", err)
	}
	return signedURL, nil
}

// The four multipart methods return ErrNotSupported. The pinned client
// library has no multipart upload API: no upload identifier, no part list,
// no per-part signing. Do not attempt to emulate one.

func (b *Backend) StartMultipart(ctx context.Context, ref storage.ObjectRef, opts storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (b *Backend) PresignPart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, part int32, ttl time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}

func (b *Backend) CompleteMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID, parts []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (b *Backend) AbortMultipart(ctx context.Context, ref storage.ObjectRef, id storage.UploadID) error {
	return storage.ErrNotSupported
}

// mapGCSError maps ErrObjectNotExist/ErrBucketNotExist to storage.ErrNotFound,
// wrapping the original error so errors.Is still finds it.
func mapGCSError(err error, op string) error {
	if errors.Is(err, gcstorage.ErrObjectNotExist) || errors.Is(err, gcstorage.ErrBucketNotExist) {
		return fmt.Errorf("storage/gcs: %s: %w", op, storage.ErrNotFound)
	}
	return fmt.Errorf("storage/gcs: %s: %w", op, err)
}

// DefaultEncryptionConfirmed: Cloud Storage encrypts every object at rest by
// default and the encryption cannot be disabled (ADR-0034 decision 4).
func (b *Backend) DefaultEncryptionConfirmed(context.Context) (bool, error) { return true, nil }
