package workspacestore

import (
	"context"
	"time"
)

// ThreadRow is what the store reads of a thread's handoff row.
type ThreadRow struct {
	State              State
	Epoch              int64
	OwnerUserID        int64
	RunningExecutionID string
	HasUpstream        bool
	LastSeq            int64
}

// PublishPrecheck is the one read before a publish transaction.
type PublishPrecheck struct {
	Thread ThreadRow
	// Live is the newest live segment with the publish's digest, nil if none.
	Live         *SegmentRow
	ProjectBytes int64
	ThreadBytes  int64
}

// WorkspaceBucket is the project's hidden workspace bucket row.
type WorkspaceBucket struct {
	ID   int64
	Name string
}

// SegmentRow is one segment of the log.
type SegmentRow struct {
	ID             int64
	Seq            int64
	Epoch          int64
	Kind           string
	SHA256         string
	Bytes          int64
	Prerequisites  []string
	Refs           map[string]string
	Writer         Writer
	ObjectKey      string
	PhysicalBucket string
	CreatedAt      time.Time
	SupersededAt   *time.Time
}

// GrantRecord is one upload grant (an elitea_storage.transfer_grants row on the
// project's workspace bucket).
type GrantRecord struct {
	ID          string
	ProjectID   int64
	BucketID    int64
	Key         string
	MaxBytes    int64
	Digest      []byte
	UploadID    *string
	ExpiresAt   time.Time
	ContentType string
}

// GrantRow is a stored upload grant.
type GrantRow struct {
	GrantRecord
	ConsumedAt *time.Time
}

// PublishRecord is the publish transaction's input. The object was checked
// before it was built.
type PublishRecord struct {
	Thread         Thread
	Caller         Caller
	Epoch          int64
	Kind           string
	SHA256         string
	Bytes          int64
	Prerequisites  []string
	Refs           []RefUpdate
	ObjectKey      string
	PhysicalBucket string
	// MaxProjectBytes is the project's workspace ceiling. The transaction
	// re-checks it under a per-project lock (the check before the
	// transaction is only a fast path); 0 skips the re-check.
	MaxProjectBytes int64
}

// PublishResult answers a publish.
type PublishResult struct {
	Seq        int64
	Refs       map[string]string
	Superseded []int64
	// Replayed is true when the same segment and refs were already applied.
	Replayed bool
}

// FetchRows is what a fetch plan reads.
type FetchRows struct {
	HeadSeq int64
	Refs    map[string]string
	// Newest is the newest live COMPACT at or after since_seq, nil when none.
	Newest *SegmentRow
}

// ObjectSweep is a segment the sweeper claimed for object deletion. Shared is
// true when a live segment of the same thread still needs the same object (a
// COMPACT with identical bytes), so only the row is retired.
type ObjectSweep struct {
	ID             int64
	ProjectID      int64
	ObjectKey      string
	PhysicalBucket string
	Shared         bool
}

// Repository is the store's persistence (repos.WorkspaceStoreRepository).
type Repository interface {
	ReadThread(ctx context.Context, thread Thread) (ThreadRow, error)
	// PublishPrecheck answers, in ONE read, what a publish checks before its
	// transaction: the thread row (ErrNotFound when absent), the newest live
	// segment with the digest, and the project's and the thread's live bytes.
	PublishPrecheck(ctx context.Context, thread Thread, sha256 string) (PublishPrecheck, error)
	// ThreadRefs answers the thread's refs as committed, without a lock.
	ThreadRefs(ctx context.Context, thread Thread) (map[string]string, error)
	// EnsureWorkspaceBucket answers the project's workspace bucket, creating
	// it on first use. It is found by type; its name is WorkspaceBucketName,
	// or a suffixed name when a user bucket already holds that one.
	EnsureWorkspaceBucket(ctx context.Context, projectID int64) (WorkspaceBucket, error)
	// WorkspaceBucket answers the project's workspace bucket, if any.
	WorkspaceBucket(ctx context.Context, projectID int64) (WorkspaceBucket, bool, error)
	// LiveSegment answers the thread's newest live (not superseded) segment
	// with this digest.
	LiveSegment(ctx context.Context, thread Thread, sha256 string) (SegmentRow, bool, error)
	// StoredSegment answers the thread's newest segment with this digest whose
	// object is not deleted (live or within its superseded grace).
	StoredSegment(ctx context.Context, thread Thread, sha256 string) (SegmentRow, bool, error)
	// ProjectWorkspaceBytes sums the live segments of a project; ThreadWorkspaceBytes
	// those of one thread.
	ProjectWorkspaceBytes(ctx context.Context, projectID int64) (int64, error)
	ThreadWorkspaceBytes(ctx context.Context, thread Thread) (int64, error)
	CreateGrant(ctx context.Context, grant GrantRecord) error
	// OpenGrant answers the newest unconsumed, unexpired upload grant for a
	// workspace key of the project.
	OpenGrant(ctx context.Context, projectID int64, key string) (GrantRow, bool, error)
	// ReadGrant answers one workspace grant of the project.
	ReadGrant(ctx context.Context, projectID int64, grantID string) (GrantRow, bool, error)
	Publish(ctx context.Context, record PublishRecord) (PublishResult, error)
	FetchRows(ctx context.Context, thread Thread, sinceSeq int64) (FetchRows, error)

	// ClaimSupersededObjects marks a batch of superseded segments (superseded
	// before `before`) object-deleted and answers them, FOR UPDATE SKIP
	// LOCKED. The caller deletes each non-shared object and calls
	// ReleaseObjectSweep for one it could not delete.
	ClaimSupersededObjects(ctx context.Context, before time.Time, limit int) ([]ObjectSweep, error)
	ReleaseObjectSweep(ctx context.Context, ids []int64) error
	// KeyHasLiveObject reports whether a segment row still needs the object
	// at key (its object is not deleted).
	KeyHasLiveObject(ctx context.Context, projectID int64, key string) (bool, error)
	// RetireThreadSegments marks every segment of the thread superseded and
	// object-deleted and removes its refs (the thread erasure's row half).
	RetireThreadSegments(ctx context.Context, thread Thread) error
	// WorkspaceProjects answers projects that own a workspace bucket, after
	// `after`, at most limit.
	WorkspaceProjects(ctx context.Context, after int64, limit int) ([]int64, error)
}

// Audit is the store's audit sink.
type Audit interface {
	StoreEvent(ctx context.Context, event AuditEvent)
}

// AuditEvent is one store audit event (decision 12): counts, sizes, shas,
// writers and reasons, never file contents.
type AuditEvent struct {
	Name      string // store.publish, store.publish_refused, store.integrity_failed, store.gc
	Thread    Thread
	UserID    int64
	Writer    Writer
	Epoch     int64
	Seq       int64
	SHA256    string
	Bytes     int64
	Reason    string
	Count     int
	Started   time.Time
	Succeeded bool
}
