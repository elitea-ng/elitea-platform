package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// CurrentRuntimeArtifactRepository is the claim-bound `artifact` toolkit plane
// (#906): the four operations the native worker reaches over the private mTLS
// content listener, executed against the SAME rows and the SAME object store
// the Artifacts page drives.
//
// It reuses the artifact repositories rather than querying storage itself, and
// that is the point: a second way to write an object would be a second set of
// rules about buckets, metadata rows, retention and quota, and the second set
// is the one that would be wrong. What this type adds is the part the HTTP
// handlers get from their request context and a claim-bound caller cannot —
// WHICH PERSON the access list is about — and it takes that from the caller
// (the claim's actor), never from anything on the wire.
type CurrentRuntimeArtifactRepository struct {
	buckets     *ArtifactBucketsRepository
	objects     *ArtifactObjectsRepository
	permissions *ArtifactBucketPermissionsRepository
	store       storage.ObjectStore
}

func NewCurrentRuntimeArtifactRepository(
	pool *pgxpool.Pool,
	store storage.ObjectStore,
) (*CurrentRuntimeArtifactRepository, error) {
	if store == nil {
		return nil, errors.New("current runtime artifact store is required")
	}
	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		return nil, err
	}
	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		return nil, err
	}
	permissions, err := NewArtifactBucketPermissionsRepository(pool)
	if err != nil {
		return nil, err
	}
	return &CurrentRuntimeArtifactRepository{
		buckets:     buckets,
		objects:     objects,
		permissions: permissions,
		store:       store,
	}, nil
}

// AttachmentBucket names the project's chat attachment bucket from its storage
// policy, or "" when the project has no policy row or the policy names none
// (the caller then uses storage.DefaultAttachmentBucket). It reads the same
// policy field the upload path reads, through the same repository method, so a
// listing and an upload cannot disagree.
func (repository *CurrentRuntimeArtifactRepository) AttachmentBucket(
	ctx context.Context,
	projectID int64,
) (string, error) {
	if repository == nil || repository.objects == nil || projectID <= 0 {
		return "", errors.New("current runtime artifact repository is unavailable")
	}
	policy, err := repository.objects.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get runtime attachment bucket policy: %w", err)
	}
	if policy.AttachmentBucket == nil {
		return "", nil
	}
	return strings.TrimSpace(*policy.AttachmentBucket), nil
}

// maxAttachmentListingPages bounds how many store pages one attachment listing
// reads to order a conversation's files by time (1,000 keys per page).
const maxAttachmentListingPages = 20

// ListAttachmentsNewestFirst lists one conversation's attachment objects,
// NEWEST UPLOAD FIRST, at most `limit` of them. The store lists by key, so the
// whole prefix (up to maxAttachmentListingPages pages) is read and ordered
// here: cutting a key-ordered page and calling it "newest" would be false.
// `truncated` is true when files were left out, whether by `limit` or by the
// page bound. An absent bucket is not an error: nothing has been uploaded yet,
// so the listing is empty.
func (repository *CurrentRuntimeArtifactRepository) ListAttachmentsNewestFirst(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	prefix string,
	limit int32,
) ([]storage.RuntimeArtifactRecord, bool, error) {
	if _, err := repository.authorizeBucket(
		ctx, projectID, actorID, bucket, BucketAccessRead,
	); err != nil {
		if errors.Is(err, storage.ErrContentNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if err := storage.ValidateKeyPrefix(prefix); err != nil {
		return nil, false, storage.ErrContentRejected
	}
	bucketRef, err := storage.NewBucketRef(strconv.FormatInt(projectID, 10), bucket)
	if err != nil {
		return nil, false, nil
	}
	var all []storage.ObjectInfo
	token := ""
	more := false
	for page := 0; page < maxAttachmentListingPages; page++ {
		result, err := repository.store.List(ctx, storage.ListQuery{
			Bucket:            bucketRef,
			KeyPrefix:         prefix,
			Delimiter:         "/",
			MaxKeys:           1000,
			ContinuationToken: token,
		})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("list runtime attachments: %w", err)
		}
		all = append(all, result.Objects...)
		more = result.IsTruncated
		if !result.IsTruncated || result.NextContinuationToken == "" {
			more = result.IsTruncated && result.NextContinuationToken == ""
			break
		}
		token = result.NextContinuationToken
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].LastModified.Equal(all[j].LastModified) {
			return all[i].LastModified.After(all[j].LastModified)
		}
		return all[i].Key < all[j].Key
	})
	truncated := more
	if limit > 0 && int32(len(all)) > limit {
		all = all[:limit]
		truncated = true
	}
	records := make([]storage.RuntimeArtifactRecord, 0, len(all))
	for _, object := range all {
		mediaType := object.ContentType
		if mediaType == "" {
			mediaType = storage.MediaTypeFromKey(object.Key)
		}
		records = append(records, storage.RuntimeArtifactRecord{
			Name:       object.Key,
			MediaType:  mediaType,
			ByteLength: object.Size,
			ModifiedAt: object.LastModified,
		})
	}
	return records, truncated, nil
}

// authorizeBucket resolves one bucket inside the claimed project and applies
// the claimed ACTOR's own per-bucket access list.
//
// It is the exact order internal/api/v2/artifacts.requireBucket uses, and the
// order is load-bearing in both places: THE ACCESS LIST IS CHECKED BEFORE THE
// BUCKET IS FETCHED, so a refused caller cannot learn from the answer whether
// a bucket they may not touch exists. The bypass for project administrators is
// the same one the HTTP routes apply, for the same reason — an exception
// written against an administrator's own account must not lock them out.
//
// The refusal is ErrContentUnauthorized (403 on the wire) and a missing bucket
// is ErrContentNotFound (404): the worker turns both into a tool result the
// model reads, and an operator can tell "not allowed" from "not there".
func (repository *CurrentRuntimeArtifactRepository) authorizeBucket(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	need string,
) (BucketRow, error) {
	if repository == nil || repository.buckets == nil || repository.objects == nil ||
		repository.permissions == nil || repository.store == nil || ctx == nil {
		return BucketRow{}, errors.New("current runtime artifact repository is unavailable")
	}
	// The hidden workspace bucket (ADR-0034 decision 3) is absent, before the
	// access list is read.
	if projectID <= 0 || actorID <= 0 || bucket == "" || storage.ReservedBucketName(bucket) {
		return BucketRow{}, storage.ErrContentNotFound
	}
	permissions, found, err := repository.permissions.GetBucketPermission(
		ctx, projectID, actorID, bucket,
	)
	if err != nil {
		return BucketRow{}, fmt.Errorf("get runtime artifact bucket permission: %w", err)
	}
	if !BucketAccessPermitted(permissions, found, need) {
		isAdmin, adminErr := repository.permissions.IsProjectAdmin(ctx, projectID, actorID)
		if adminErr != nil {
			return BucketRow{}, fmt.Errorf("check runtime artifact project admin: %w", adminErr)
		}
		if !isAdmin {
			return BucketRow{}, storage.ErrContentUnauthorized
		}
	}
	row, err := repository.buckets.GetBucket(ctx, projectID, bucket)
	if errors.Is(err, storage.ErrNotFound) {
		return BucketRow{}, storage.ErrContentNotFound
	}
	if err != nil {
		return BucketRow{}, fmt.Errorf("get runtime artifact bucket: %w", err)
	}
	if row.ProjectID != projectID {
		return BucketRow{}, storage.ErrContentNotFound
	}
	return row, nil
}

// ListRuntimeArtifacts lists one bucket, from the OBJECT STORE rather than the
// metadata table.
//
// The store is what the Artifacts page itself lists (internal/api/v2/artifacts
// ListObjects), and an agent that saw a different set of files from the page
// its user is looking at would be worse than one that could not list at all.
// A non-recursive listing uses the "/" delimiter, the same way the page's
// folder view does.
func (repository *CurrentRuntimeArtifactRepository) ListRuntimeArtifacts(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	prefix string,
	recursive bool,
	limit int32,
) ([]storage.RuntimeArtifactRecord, bool, error) {
	if _, err := repository.authorizeBucket(
		ctx, projectID, actorID, bucket, BucketAccessRead,
	); err != nil {
		return nil, false, err
	}
	if err := storage.ValidateKeyPrefix(prefix); err != nil {
		return nil, false, storage.ErrContentRejected
	}
	bucketRef, err := storage.NewBucketRef(strconv.FormatInt(projectID, 10), bucket)
	if err != nil {
		return nil, false, storage.ErrContentNotFound
	}
	delimiter := ""
	if !recursive {
		delimiter = "/"
	}
	page, err := repository.store.List(ctx, storage.ListQuery{
		Bucket:    bucketRef,
		KeyPrefix: prefix,
		Delimiter: delimiter,
		MaxKeys:   limit,
	})
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, false, storage.ErrContentNotFound
		}
		return nil, false, fmt.Errorf("list runtime artifacts: %w", err)
	}
	records := make([]storage.RuntimeArtifactRecord, 0, len(page.Objects))
	for _, object := range page.Objects {
		mediaType := object.ContentType
		if mediaType == "" {
			mediaType = storage.MediaTypeFromKey(object.Key)
		}
		records = append(records, storage.RuntimeArtifactRecord{
			Name:       object.Key,
			MediaType:  mediaType,
			ByteLength: object.Size,
			ModifiedAt: object.LastModified,
		})
	}
	return records, page.IsTruncated, nil
}

// ReadRuntimeArtifact returns one object's bytes, or — when its metadata row
// already says it is past the caller's ceiling — the measurement alone.
//
// The row is consulted FIRST so an oversized object is refused without being
// fetched. It is not treated as authoritative about the bytes, though: an
// object with no row at all is still READ (the store is the thing the page
// lists, and a row can be missing for an object written before S12 recorded
// them), and the caller checks what actually came back against the ceiling a
// second time. A store that reports a small size and streams a large body
// therefore cannot outgrow the caller's budget.
func (repository *CurrentRuntimeArtifactRepository) ReadRuntimeArtifact(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	name string,
	maxBytes int64,
) (storage.RuntimeArtifactContentRecord, error) {
	bucketRow, err := repository.authorizeBucket(
		ctx, projectID, actorID, bucket, BucketAccessRead,
	)
	if err != nil {
		return storage.RuntimeArtifactContentRecord{}, err
	}
	if maxBytes <= 0 {
		return storage.RuntimeArtifactContentRecord{}, storage.ErrContentRejected
	}
	row, hasRow, err := repository.objectRow(ctx, bucketRow.ID, name)
	if err != nil {
		return storage.RuntimeArtifactContentRecord{}, err
	}
	mediaType := storage.MediaTypeFromKey(name)
	if hasRow {
		if row.MediaType != "" {
			mediaType = row.MediaType
		}
		if row.ByteLength > maxBytes {
			return storage.RuntimeArtifactContentRecord{
				Name:       name,
				MediaType:  mediaType,
				ByteLength: row.ByteLength,
				OverLimit:  true,
			}, nil
		}
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucket, name)
	if err != nil {
		return storage.RuntimeArtifactContentRecord{}, storage.ErrContentNotFound
	}
	body, info, err := repository.store.Get(ctx, ref, nil)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.RuntimeArtifactContentRecord{}, storage.ErrContentNotFound
	}
	if err != nil {
		return storage.RuntimeArtifactContentRecord{}, fmt.Errorf("get runtime artifact: %w", err)
	}
	defer func() {
		_ = body.Close()
	}()
	if info.TotalSize > maxBytes || info.Size > maxBytes {
		return storage.RuntimeArtifactContentRecord{
			Name:       name,
			MediaType:  mediaType,
			ByteLength: max(info.TotalSize, info.Size),
			OverLimit:  true,
		}, nil
	}
	content, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return storage.RuntimeArtifactContentRecord{}, fmt.Errorf("read runtime artifact: %w", err)
	}
	if int64(len(content)) > maxBytes {
		// The store under-reported. Refused as over-limit rather than served
		// as a prefix: the caller's cap is about what reaches a prompt, and a
		// truncated file presented as the file is the failure it exists to
		// prevent.
		return storage.RuntimeArtifactContentRecord{
			Name:       name,
			MediaType:  mediaType,
			ByteLength: int64(len(content)),
			OverLimit:  true,
		}, nil
	}
	return storage.RuntimeArtifactContentRecord{
		Name:       name,
		MediaType:  mediaType,
		ByteLength: int64(len(content)),
		Content:    content,
	}, nil
}

// WriteRuntimeArtifact stores one object and records its metadata row.
//
// It performs the whole write internal/api/v2/artifacts.storeObject performs,
// in the same order and with the same rollback — the streaming Put, the
// metadata row, then the project-wide quota check that deletes BOTH halves on
// violation. The quota check runs after the write because an object's exact
// length is only known once it is stored; the bytes here are already in memory
// and bounded by the caller, so nothing unbounded is buffered to get there.
//
// The media type is derived from the key's extension by the shared rule
// (storage.MediaTypeFromKey), never taken from the request: a caller that
// could name the content type could make a script download as text or a text
// file execute.
func (repository *CurrentRuntimeArtifactRepository) WriteRuntimeArtifact(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	name string,
	content []byte,
) (storage.RuntimeArtifactRecord, error) {
	bucketRow, err := repository.authorizeBucket(
		ctx, projectID, actorID, bucket, BucketAccessWrite,
	)
	if err != nil {
		return storage.RuntimeArtifactRecord{}, err
	}
	if len(content) == 0 {
		return storage.RuntimeArtifactRecord{}, storage.ErrContentRejected
	}
	policy, err := repository.objects.GetProjectStoragePolicy(ctx, projectID)
	if err != nil {
		return storage.RuntimeArtifactRecord{}, fmt.Errorf("get runtime artifact policy: %w", err)
	}
	if policy.MaxObjectBytes != nil && int64(len(content)) > *policy.MaxObjectBytes {
		return storage.RuntimeArtifactRecord{}, storage.ErrContentRejected
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucket, name)
	if err != nil {
		return storage.RuntimeArtifactRecord{}, storage.ErrContentRejected
	}
	mediaType := storage.MediaTypeFromKey(name)
	info, err := repository.store.Put(ctx, ref, bytes.NewReader(content), storage.PutOptions{
		ContentType:   mediaType,
		ContentLength: int64(len(content)),
	})
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) || errors.Is(err, storage.ErrInvalidKey) {
			return storage.RuntimeArtifactRecord{}, storage.ErrContentRejected
		}
		return storage.RuntimeArtifactRecord{}, fmt.Errorf("put runtime artifact: %w", err)
	}
	// The object's own retention window starts now, exactly as an overwrite
	// through the upload route restarts it.
	var expiresAt *time.Time
	if bucketRow.RetentionDays != nil {
		expiry := time.Now().AddDate(0, 0, int(*bucketRow.RetentionDays))
		expiresAt = &expiry
	}
	if _, err := repository.objects.UpsertObject(ctx, NewObjectInput{
		BucketID:   bucketRow.ID,
		Key:        info.Key,
		ByteLength: info.Size,
		MediaType:  mediaType,
		ExpiresAt:  expiresAt,
	}); err != nil {
		return storage.RuntimeArtifactRecord{}, fmt.Errorf("record runtime artifact: %w", err)
	}
	if policy.MaxTotalBytes != nil {
		total, sumErr := repository.objects.SumProjectBytes(ctx, projectID)
		if sumErr != nil {
			return storage.RuntimeArtifactRecord{}, fmt.Errorf("sum runtime artifact bytes: %w", sumErr)
		}
		if total > *policy.MaxTotalBytes {
			_ = repository.objects.DeleteObjects(ctx, bucketRow.ID, []string{info.Key})
			_ = repository.store.Delete(ctx, ref)
			return storage.RuntimeArtifactRecord{}, storage.ErrContentRejected
		}
	}
	return storage.RuntimeArtifactRecord{
		Name:       info.Key,
		MediaType:  mediaType,
		ByteLength: info.Size,
		ModifiedAt: time.Now().UTC(),
	}, nil
}

// DeleteRuntimeArtifact removes one object and its metadata row.
//
// The bytes go first and the row second, which is the order the HTTP delete
// uses: a row left behind by a failed second step overstates the project's
// usage, while bytes left behind by the reverse order are unreachable and
// unaccounted — the worse of the two. A missing object is NOT an error: the
// model asked for the file to be gone and it is.
func (repository *CurrentRuntimeArtifactRepository) DeleteRuntimeArtifact(
	ctx context.Context,
	projectID int64,
	actorID int64,
	bucket string,
	name string,
) error {
	bucketRow, err := repository.authorizeBucket(
		ctx, projectID, actorID, bucket, BucketAccessWrite,
	)
	if err != nil {
		return err
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(projectID, 10), bucket, name)
	if err != nil {
		return storage.ErrContentNotFound
	}
	if err := repository.store.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("delete runtime artifact: %w", err)
	}
	if err := repository.objects.DeleteObjects(ctx, bucketRow.ID, []string{name}); err != nil {
		return fmt.Errorf("delete runtime artifact row: %w", err)
	}
	return nil
}

// objectRow reads one object's metadata row by exact key.
//
// ListObjects with the full key as its prefix, for the reason
// CurrentAttachmentObjectRepository gives: this repository has no
// single-object query, and the exact-match filter below is what makes the
// prefix query safe — `key LIKE $2 || '%'` treats `%` and `_` in the key as
// wildcards, so the prefix alone could match a neighbour.
func (repository *CurrentRuntimeArtifactRepository) objectRow(
	ctx context.Context,
	bucketID int64,
	name string,
) (ObjectRow, bool, error) {
	rows, err := repository.objects.ListObjects(ctx, bucketID, name)
	if err != nil {
		return ObjectRow{}, false, fmt.Errorf("list runtime artifact row: %w", err)
	}
	for _, row := range rows {
		if row.Key == name {
			return row, true, nil
		}
	}
	return ObjectRow{}, false, nil
}

var _ storage.RuntimeArtifactSource = (*CurrentRuntimeArtifactRepository)(nil)
