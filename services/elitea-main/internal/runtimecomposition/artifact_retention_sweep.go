package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	artifactRetentionSweepCapability        = "artifact.retention.sweep.v1"
	artifactRetentionSweepRevision          = "artifact-retention-sweep-r1"
	artifactRetentionSweepCadence           = "*/15 * * * *"
	artifactRetentionSweepHandlerTimeout    = 25 * time.Second
	artifactRetentionSweepBatchSize         = int32(500)
	artifactRetentionSweepMaxBatchesPerTick = 20
	artifactRetentionSweepNotifyWithin      = 24 * time.Hour
	artifactRetentionSweepNotifyLimit       = int32(100)
	// artifactRetentionStaleChunkTTL matches legacy's cleanup_stale_chunks —
	// see cleanupStaleAttachmentChunks's own doc comment.
	artifactRetentionStaleChunkTTL = 12 * time.Hour
	// artifactRetentionGrantGrace keeps the sweeper away from a grant that
	// has only just expired. A commit that started before the deadline
	// streams the whole object through a SHA-256 hasher, so it can still be
	// running when expires_at passes. One sweep cadence of margin means the
	// sweeper never claims a grant a live commit is working on.
	artifactRetentionGrantGrace = 15 * time.Minute
)

var errArtifactRetentionSweepInvalidOccurrence = errors.New("invalid artifact retention sweep occurrence")

// artifactRetentionObjectsRepository is the S6 dependency slice this handler
// needs from ArtifactObjectsRepository — see S14's plan text on why deletion
// closes the metadata-cleanup gap S8/S12 deliberately deferred.
// SumProjectBytes is S18's addition, backing the per-project byte-usage
// gauge.
type artifactRetentionObjectsRepository interface {
	ListExpiredObjects(ctx context.Context, olderThan time.Time, limit int32) ([]repos.ObjectRow, error)
	DeleteObjectRows(ctx context.Context, ids []int64) error
	SumProjectBytes(ctx context.Context, projectID int64) (int64, error)
}

// artifactRetentionBucketsRepository is the S6 dependency slice this handler
// needs from ArtifactBucketsRepository. ListProjectIDsWithBuckets is S18's
// addition, backing the per-project byte-usage gauge.
type artifactRetentionBucketsRepository interface {
	GetBucketByID(ctx context.Context, id int64) (repos.BucketRow, error)
	ListBucketsNeedingExpiryNotice(ctx context.Context, within time.Duration, limit int32) ([]repos.BucketRow, error)
	MarkBucketNotified(ctx context.Context, bucketID int64) error
	ListProjectIDsWithBuckets(ctx context.Context) ([]int64, error)
}

// artifactRetentionNotifier is the S14 notification dependency —
// repos.ArtifactRetentionNotificationRepository satisfies it.
type artifactRetentionNotifier interface {
	ProjectOwnerUserID(ctx context.Context, projectID int64) (int64, error)
	NotifyBucketExpiring(ctx context.Context, projectID, userID, bucketID int64, expiresAt time.Time) error
}

// artifactRetentionGrantsRepository is the transfer-grant dependency slice
// (repos.ArtifactTransferGrantsRepository satisfies it). It is
// constructor-required, not optional: a grant sweep that silently never runs
// is the defect it exists to fix.
type artifactRetentionGrantsRepository interface {
	ClaimExpiredTransferGrants(ctx context.Context, olderThan time.Time, limit int32) ([]repos.ExpiredTransferGrantRow, error)
	ListExpiredGrantStaging(ctx context.Context, olderThan time.Time, limit int32) ([]repos.GrantStagingRow, error)
	ClearGrantStaging(ctx context.Context, id, key string) error
}

// artifactRetentionAttachmentChunksRepository is S20a's cleanup dependency
// (repos.AttachmentChunksRepository satisfies it) — optional, unlike the
// four constructor-required dependencies above: an installation with no
// attachment-chunk cleanup wired simply skips that step rather than failing
// to construct the whole sweeper over an unrelated feature's dependency.
type artifactRetentionAttachmentChunksRepository interface {
	DeleteStaleChunks(ctx context.Context, olderThan time.Time) (int64, error)
}

// artifactRetentionSweep is the typed retention adapter behind the generic
// platform scheduler, following the shape of currentIndexScheduleDueWork:
// it owns product-aware sweeping (expired object purge, expiry
// notification), not a clock, replica claim, or occurrence ledger.
//
// Mode is ModeLocalBounded, not ModeDurableAdmission: every effect (delete
// bytes, delete metadata, insert a notification, mark notified) completes
// synchronously within Execute — there is no downstream durable system this
// handler merely admits work into, unlike currentIndexScheduleDueWork.
type artifactRetentionSweep struct {
	objects          artifactRetentionObjectsRepository
	buckets          artifactRetentionBucketsRepository
	notifier         artifactRetentionNotifier
	grants           artifactRetentionGrantsRepository
	store            storage.ObjectStore
	attachmentChunks artifactRetentionAttachmentChunksRepository
}

func newArtifactRetentionSweep(
	objects artifactRetentionObjectsRepository,
	buckets artifactRetentionBucketsRepository,
	notifier artifactRetentionNotifier,
	grants artifactRetentionGrantsRepository,
	store storage.ObjectStore,
) (*artifactRetentionSweep, error) {
	if objects == nil || buckets == nil || notifier == nil || grants == nil || store == nil {
		return nil, errors.New("artifact retention sweep dependencies are required")
	}
	return &artifactRetentionSweep{
		objects: objects, buckets: buckets, notifier: notifier, grants: grants, store: store,
	}, nil
}

// WithAttachmentChunks activates S20a's stale-chunk cleanup step. Optional:
// left unset, Execute simply skips it — see
// artifactRetentionAttachmentChunksRepository's own doc comment for why this
// one dependency is not constructor-required like the other four.
func (s *artifactRetentionSweep) WithAttachmentChunks(repo artifactRetentionAttachmentChunksRepository) *artifactRetentionSweep {
	s.attachmentChunks = repo
	return s
}

func (*artifactRetentionSweep) Name() string {
	return artifactRetentionSweepCapability
}

func (s *artifactRetentionSweep) Execute(
	ctx context.Context,
	occurrence schedulingapp.Occurrence,
) (schedulingapp.Outcome, error) {
	if s == nil || s.objects == nil || s.buckets == nil || s.notifier == nil || s.grants == nil || s.store == nil || ctx == nil ||
		occurrence.InvocationID == "" ||
		occurrence.JobID != artifactRetentionSweepCapability ||
		occurrence.ScheduleRevision != artifactRetentionSweepRevision ||
		occurrence.DueAt.IsZero() ||
		occurrence.LeaseEpoch <= 0 ||
		occurrence.ClaimFence == "" {
		return "", errArtifactRetentionSweepInvalidOccurrence
	}
	if err := s.sweepExpiredObjects(ctx); err != nil {
		return "", err
	}
	if err := s.sweepExpiredGrants(ctx); err != nil {
		return "", err
	}
	if err := s.sweepGrantStaging(ctx); err != nil {
		return "", err
	}
	if err := s.notifyExpiringBuckets(ctx); err != nil {
		return "", err
	}
	if err := s.updateProjectByteUsageGauges(ctx); err != nil {
		return "", err
	}
	if s.attachmentChunks != nil {
		if err := s.cleanupStaleAttachmentChunks(ctx); err != nil {
			return "", err
		}
	}
	return schedulingapp.OutcomeLocalCompleted, nil
}

// cleanupStaleAttachmentChunks reclaims S20a chunk rows for chunked
// attachment uploads abandoned before their final chunk arrived — the
// completed-merge path (DeleteAttachmentChunks) never runs for those, so
// nothing else in this service ever deletes them. Matches legacy's
// cleanup_stale_chunks 12-hour TTL.
func (s *artifactRetentionSweep) cleanupStaleAttachmentChunks(ctx context.Context) error {
	if _, err := s.attachmentChunks.DeleteStaleChunks(ctx, time.Now().Add(-artifactRetentionStaleChunkTTL)); err != nil {
		return fmt.Errorf("delete stale attachment chunks: %w", err)
	}
	return nil
}

// sweepExpiredGrants reclaims the bytes behind an expired transfer grant
// that was never committed.
//
// THE GAP THIS CLOSES. CreateTransferGrant issues a presigned PUT for a
// server-derived key and writes only a transfer_grants row. The
// elitea_storage.objects row is written by the commit and nowhere else. So a
// caller who uploads to the signed URL and never commits leaves an object
// that:
//
//   - no quota counts, because SumProjectBytes reads only the objects table;
//   - no sweep finds, because ListExpiredObjects reads only the objects
//     table;
//   - nobody can name, because the caller never told the server it uploaded.
//
// S3 does not enforce the grant's max_bytes at the edge either: a SigV4
// presigned PUT can sign an exact Content-Length only, and the server does
// not know the size in advance. Commit-time TooLarge stays the enforcement
// point, so reclamation is what bounds the damage. Repeating the loop
// otherwise fills the storage account with unbilled, unreclaimable data.
//
// A GET grant reserves no bytes: its key is a grant id nothing was ever
// written to. Claiming the row is the whole cleanup for one.
//
// One storage failure does not stop the tick. The remaining grants are
// swept, and the first error is returned afterwards, so one unreachable
// bucket cannot stall object retention behind it.
func (s *artifactRetentionSweep) sweepExpiredGrants(ctx context.Context) error {
	olderThan := time.Now().Add(-artifactRetentionGrantGrace)
	bucketCache := map[int64]repos.BucketRow{}
	var firstErr error
	for i := 0; i < artifactRetentionSweepMaxBatchesPerTick; i++ {
		claimed, err := s.grants.ClaimExpiredTransferGrants(ctx, olderThan, artifactRetentionSweepBatchSize)
		if err != nil {
			return fmt.Errorf("claim expired artifact transfer grants: %w", err)
		}
		if len(claimed) == 0 {
			return firstErr
		}
		for _, grant := range claimed {
			if err := s.reclaimGrantBytes(ctx, grant, bucketCache); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if int32(len(claimed)) < artifactRetentionSweepBatchSize {
			return firstErr
		}
	}
	return firstErr
}

// reclaimGrantBytes deletes whatever the caller may have uploaded against
// one claimed grant. An absent object is the common case — most grants are
// simply never used — so storage.ErrNotFound counts as success.
func (s *artifactRetentionSweep) reclaimGrantBytes(
	ctx context.Context,
	grant repos.ExpiredTransferGrantRow,
	bucketCache map[int64]repos.BucketRow,
) error {
	if grant.Method != "PUT" {
		return nil
	}
	bucket, ok := bucketCache[grant.BucketID]
	if !ok {
		var err error
		bucket, err = s.buckets.GetBucketByID(ctx, grant.BucketID)
		if errors.Is(err, storage.ErrNotFound) {
			// The bucket is gone, so its objects went with it. The claim
			// alone completes this grant.
			return nil
		}
		if err != nil {
			return fmt.Errorf("get bucket %d for expired grant %s: %w", grant.BucketID, grant.ID, err)
		}
		bucketCache[grant.BucketID] = bucket
	}

	ref, err := storage.NewObjectRef(strconv.FormatInt(grant.ProjectID, 10), bucket.Name, grant.Key)
	if err != nil {
		return fmt.Errorf("build object ref for expired grant %s: %w", grant.ID, err)
	}

	if grant.UploadID != nil {
		// A native multipart session holds uploaded parts that are billed
		// and that no completed object references.
		if err := s.store.AbortMultipart(ctx, ref, storage.UploadID(*grant.UploadID)); err != nil &&
			!errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("abort multipart for expired grant %s: %w", grant.ID, err)
		}
		return nil
	}

	if err := s.store.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("delete bytes for expired grant %s: %w", grant.ID, err)
	}
	if repos.IsGrantStagingKey(grant.ID, grant.Key) {
		// A staged grant's commit copies to the grant id before it verifies.
		// A commit that failed after the copy, and was never retried, leaves
		// that copy behind with no metadata row. The grant was never
		// consumed, so nothing was committed there.
		final, err := storage.NewObjectRef(strconv.FormatInt(grant.ProjectID, 10), bucket.Name, grant.ID)
		if err != nil {
			return fmt.Errorf("build object ref for expired grant %s: %w", grant.ID, err)
		}
		if err := s.store.Delete(ctx, final); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("delete copied bytes for expired grant %s: %w", grant.ID, err)
		}
	}
	return nil
}

// sweepGrantStaging reclaims the staging object of a presigned single-shot
// grant once its URL has expired (api/v2/artifacts grantStagingKey).
//
// A commit deletes the staging object, but the URL stays valid until the
// grant expires, so a PUT after the commit recreates it: bytes no metadata
// row counts against the quota and no object sweep finds, under a grant that
// is already consumed and so never claimed by sweepExpiredGrants. After
// expires_at plus the grace nothing can write there again, so one delete is
// final. The marker is cleared only after the delete, so a failed delete is
// retried next tick.
func (s *artifactRetentionSweep) sweepGrantStaging(ctx context.Context) error {
	olderThan := time.Now().Add(-artifactRetentionGrantGrace)
	bucketCache := map[int64]repos.BucketRow{}
	var firstErr error
	for i := 0; i < artifactRetentionSweepMaxBatchesPerTick; i++ {
		staged, err := s.grants.ListExpiredGrantStaging(ctx, olderThan, artifactRetentionSweepBatchSize)
		if err != nil {
			return fmt.Errorf("list expired artifact grant staging: %w", err)
		}
		cleared := 0
		for _, grant := range staged {
			if err := s.reclaimGrantStaging(ctx, grant, bucketCache); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			cleared++
		}
		// A batch that cleared nothing would be listed again unchanged.
		if int32(len(staged)) < artifactRetentionSweepBatchSize || cleared == 0 {
			return firstErr
		}
	}
	return firstErr
}

func (s *artifactRetentionSweep) reclaimGrantStaging(
	ctx context.Context,
	grant repos.GrantStagingRow,
	bucketCache map[int64]repos.BucketRow,
) error {
	bucket, ok := bucketCache[grant.BucketID]
	if !ok {
		var err error
		bucket, err = s.buckets.GetBucketByID(ctx, grant.BucketID)
		if errors.Is(err, storage.ErrNotFound) {
			// The bucket's objects went with it.
			return s.grants.ClearGrantStaging(ctx, grant.ID, grant.Key)
		}
		if err != nil {
			return fmt.Errorf("get bucket %d for grant staging %s: %w", grant.BucketID, grant.ID, err)
		}
		bucketCache[grant.BucketID] = bucket
	}
	ref, err := storage.NewObjectRef(strconv.FormatInt(grant.ProjectID, 10), bucket.Name, grant.Key)
	if err != nil {
		return fmt.Errorf("build object ref for grant staging %s: %w", grant.ID, err)
	}
	if err := s.store.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("delete staging bytes for grant %s: %w", grant.ID, err)
	}
	return s.grants.ClearGrantStaging(ctx, grant.ID, grant.Key)
}

var _ schedulingapp.Handler = (*artifactRetentionSweep)(nil)

// sweepExpiredObjects deletes every object whose expires_at has passed, in
// bounded batches. Each successful DeleteObjectRows call removes the rows
// ListExpiredObjects would otherwise return again, so no continuation token
// is needed — a batch smaller than artifactRetentionSweepBatchSize means the
// backlog is exhausted for this tick.
func (s *artifactRetentionSweep) sweepExpiredObjects(ctx context.Context) error {
	now := time.Now()
	bucketCache := map[int64]repos.BucketRow{}
	for i := 0; i < artifactRetentionSweepMaxBatchesPerTick; i++ {
		expired, err := s.objects.ListExpiredObjects(ctx, now, artifactRetentionSweepBatchSize)
		if err != nil {
			return fmt.Errorf("list expired artifact objects: %w", err)
		}
		if len(expired) == 0 {
			return nil
		}
		if err := s.deleteExpiredBatch(ctx, expired, bucketCache); err != nil {
			return err
		}
		if int32(len(expired)) < artifactRetentionSweepBatchSize {
			return nil
		}
	}
	return nil
}

// deleteExpiredBatch groups expired objects by bucket (ListExpiredObjects
// scans across every project, so a batch can span many buckets), resolves
// each bucket once via GetBucketByID, and deletes both the physical bytes
// (ObjectStore.DeleteBatch) and the metadata rows (DeleteObjectRows) for
// each group.
//
// On any DeleteBatch failure (top-level error or a partial per-object
// failure), metadata is still cleaned up for whatever DeleteBatch actually
// reports as deleted before the error is returned — mirroring the fix S13's
// adversarial review found in artifactbootstrap.purgeObjects: a partial
// failure must not orphan metadata rows for objects whose physical bytes
// are already gone, since a retry's List/ListExpiredObjects call can never
// rediscover a key that no longer exists in the backend.
func (s *artifactRetentionSweep) deleteExpiredBatch(
	ctx context.Context,
	expired []repos.ObjectRow,
	bucketCache map[int64]repos.BucketRow,
) error {
	byBucket := map[int64][]repos.ObjectRow{}
	for _, obj := range expired {
		byBucket[obj.BucketID] = append(byBucket[obj.BucketID], obj)
	}
	for bucketID, objs := range byBucket {
		bucket, ok := bucketCache[bucketID]
		if !ok {
			var err error
			bucket, err = s.buckets.GetBucketByID(ctx, bucketID)
			if errors.Is(err, storage.ErrNotFound) {
				// The bucket itself is already gone — there is no physical
				// object to resolve a ref for, only an orphaned metadata
				// row. Delete the rows directly rather than failing the
				// whole tick over a bucket that no longer exists.
				if err := s.deleteObjectRowsOnly(ctx, objs); err != nil {
					return fmt.Errorf("delete orphaned expired object rows for missing bucket %d: %w", bucketID, err)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("get bucket %d for expired objects: %w", bucketID, err)
			}
			bucketCache[bucketID] = bucket
		}

		if err := s.deleteExpiredBucketGroup(ctx, bucket, objs); err != nil {
			return err
		}
	}
	return nil
}

func (s *artifactRetentionSweep) deleteObjectRowsOnly(ctx context.Context, objs []repos.ObjectRow) error {
	ids := make([]int64, 0, len(objs))
	for _, obj := range objs {
		ids = append(ids, obj.ID)
	}
	return s.objects.DeleteObjectRows(ctx, ids)
}

func (s *artifactRetentionSweep) deleteExpiredBucketGroup(ctx context.Context, bucket repos.BucketRow, objs []repos.ObjectRow) error {
	bucketRef, err := storage.NewBucketRef(strconv.FormatInt(bucket.ProjectID, 10), bucket.Name)
	if err != nil {
		return fmt.Errorf("build bucket ref for bucket %d: %w", bucket.ID, err)
	}
	refs := make([]storage.ObjectRef, 0, len(objs))
	for _, obj := range objs {
		ref, err := storage.NewObjectRef(bucketRef.ProjectID(), bucketRef.Bucket(), obj.Key)
		if err != nil {
			return fmt.Errorf("build object ref for %q: %w", obj.Key, err)
		}
		refs = append(refs, ref)
	}
	result, batchErr := s.store.DeleteBatch(ctx, refs)

	if len(result.Deleted) > 0 {
		deletedKeys := make(map[string]bool, len(result.Deleted))
		for _, key := range result.Deleted {
			deletedKeys[key] = true
		}
		ids := make([]int64, 0, len(result.Deleted))
		for _, obj := range objs {
			if deletedKeys[obj.Key] {
				ids = append(ids, obj.ID)
			}
		}
		if err := s.objects.DeleteObjectRows(ctx, ids); err != nil {
			return fmt.Errorf("delete expired object metadata rows for bucket %d: %w", bucket.ID, err)
		}
	}
	if batchErr != nil {
		return fmt.Errorf("delete batch for bucket %d: %w", bucket.ID, batchErr)
	}
	if len(result.Failed) > 0 {
		first := result.Failed[0]
		return fmt.Errorf("delete batch for bucket %d: %d object(s) failed (first: %s: %v)", bucket.ID, len(result.Failed), first.Key, first.Err)
	}
	return nil
}

// updateProjectByteUsageGauges refreshes S18's per-project byte-usage gauge
// for every project known to own a bucket, sourced from the metadata table
// (SumProjectBytes) — deliberately called once per sweeper tick, here, and
// nowhere on a per-request path. A failure to list projects or sum one
// project's bytes fails the whole tick (matching sweepExpiredObjects'/
// notifyExpiringBuckets' own error handling above) rather than silently
// skipping a project's gauge update, which would leave a stale reading with
// no indication it stopped refreshing.
func (s *artifactRetentionSweep) updateProjectByteUsageGauges(ctx context.Context) error {
	projectIDs, err := s.buckets.ListProjectIDsWithBuckets(ctx)
	if err != nil {
		return fmt.Errorf("list artifact project ids with buckets: %w", err)
	}
	for _, projectID := range projectIDs {
		total, err := s.objects.SumProjectBytes(ctx, projectID)
		if err != nil {
			return fmt.Errorf("sum artifact project bytes for project %d: %w", projectID, err)
		}
		// Argument order matters and the compiler cannot catch a swap here —
		// both parameters are int64. See RecordProjectByteUsage's own
		// signature: (ctx, projectID, totalBytes), in that order.
		storage.RecordProjectByteUsage(ctx, projectID, total)
	}
	return nil
}

// notifyExpiringBuckets emits one centry.notifications row per bucket within
// artifactRetentionSweepNotifyWithin of expires_at that has not already been
// notified, addressed to the owning project's owner_id (see
// GetArtifactBucketOwningProjectUserID's comment for why: elitea_storage.
// buckets has no user-identifying column of its own). A failure partway
// through leaves already-notified buckets correctly marked — MarkBucketNotified
// runs immediately after each successful insert, not in a batch at the end —
// so a retried tick picks up only the buckets it didn't reach.
func (s *artifactRetentionSweep) notifyExpiringBuckets(ctx context.Context) error {
	buckets, err := s.buckets.ListBucketsNeedingExpiryNotice(ctx, artifactRetentionSweepNotifyWithin, artifactRetentionSweepNotifyLimit)
	if err != nil {
		return fmt.Errorf("list artifact buckets needing expiry notice: %w", err)
	}
	for _, bucket := range buckets {
		if bucket.ExpiresAt == nil {
			// ListBucketsNeedingExpiryNotice's own WHERE clause already
			// excludes a nil expires_at; defensive, not reachable.
			continue
		}
		ownerID, err := s.notifier.ProjectOwnerUserID(ctx, bucket.ProjectID)
		if err != nil {
			return fmt.Errorf("resolve owner for project %d: %w", bucket.ProjectID, err)
		}
		if err := s.notifier.NotifyBucketExpiring(ctx, bucket.ProjectID, ownerID, bucket.ID, *bucket.ExpiresAt); err != nil {
			return fmt.Errorf("notify bucket %d expiring: %w", bucket.ID, err)
		}
		if err := s.buckets.MarkBucketNotified(ctx, bucket.ID); err != nil {
			return fmt.Errorf("mark bucket %d notified: %w", bucket.ID, err)
		}
	}
	return nil
}
