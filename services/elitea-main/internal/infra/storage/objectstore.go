package storage

import (
	"context"
	"io"
	"time"
)

// UploadID identifies a backend-native multipart upload session.
type UploadID string

// Part describes one completed part of a multipart upload.
type Part struct {
	Number int32
	ETag   string
	Size   int64
}

// BatchError pairs a key from a batch operation with the error it produced.
type BatchError struct {
	Key string
	Err error
}

// PutOptions configures a Put or presigned-PUT call.
type PutOptions struct {
	ContentType   string
	ContentLength int64 // -1 when unknown
	Metadata      map[string]string
	// ChecksumSHA256 is the raw SHA-256 digest the backend must verify on
	// the PUT. A backend with Capabilities.ChecksumSHA256 refuses a body
	// that does not match it; a presigned PUT signs it, so the client must
	// send it as ChecksumSHA256Header. Other backends ignore it.
	ChecksumSHA256 []byte
}

// ChecksumSHA256Header is the request header that carries the base64 SHA-256
// digest of a presigned PUT on a backend with Capabilities.ChecksumSHA256.
const ChecksumSHA256Header = "x-amz-checksum-sha256"

// ByteRange requests a byte-range read from Get. End -1 means "to end of
// object".
type ByteRange struct {
	Start, End int64
}

// ListQuery selects a page of objects within a bucket.
type ListQuery struct {
	Bucket            ObjectRef // bucket ref; its Key() is always empty
	KeyPrefix         string    // validated by ValidateKeyPrefix; may end in "/"
	Delimiter         string
	MaxKeys           int32 // 0 means backend default; hard cap 1000
	ContinuationToken string
}

// ListPage is one page of a List call's results.
type ListPage struct {
	Objects               []ObjectInfo
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
}

// BatchResult is the outcome of a DeleteBatch call.
type BatchResult struct {
	Deleted []string
	Failed  []BatchError
}

// Capabilities describes which optional operations a backend actually
// supports. Callers must check this rather than assume every backend
// supports every method.
type Capabilities struct {
	Presign         bool
	NativeMultipart bool
	ServerSideCopy  bool
	// ChecksumSHA256: a single-part PUT with PutOptions.ChecksumSHA256 is
	// refused unless the body matches it, and Stat reports the stored
	// full-object digest in ObjectInfo.DigestSHA256.
	ChecksumSHA256 bool
}

// ObjectStore is the target storage abstraction: S3/Azure/GCS-compatible
// object storage, scoped by ObjectRef. Implementations must honour the
// semantic rules documented on each method's package (see the storage
// migration plan): Delete is idempotent, not-found mapping to ErrNotFound
// applies only to Get and Stat, List returns exactly one page per call, and
// an unsupported operation returns ErrNotSupported rather than panicking.
type ObjectStore interface {
	Put(ctx context.Context, ref ObjectRef, body io.Reader, opts PutOptions) (ObjectInfo, error)
	// Get returns the bytes the range selects. On a ranged read
	// ObjectInfo.Size is the length of that range and ObjectInfo.TotalSize
	// is the size of the whole object. A caller that declares
	// Content-Length must use Size, never TotalSize.
	Get(ctx context.Context, ref ObjectRef, rng *ByteRange) (io.ReadCloser, ObjectInfo, error)
	Stat(ctx context.Context, ref ObjectRef) (ObjectInfo, error)
	Delete(ctx context.Context, ref ObjectRef) error
	DeleteBatch(ctx context.Context, refs []ObjectRef) (BatchResult, error)
	List(ctx context.Context, q ListQuery) (ListPage, error)

	PresignGet(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error)
	PresignPut(ctx context.Context, ref ObjectRef, ttl time.Duration, opts PutOptions) (string, error)

	StartMultipart(ctx context.Context, ref ObjectRef, opts PutOptions) (UploadID, error)
	PresignPart(ctx context.Context, ref ObjectRef, id UploadID, part int32, ttl time.Duration) (string, error)
	CompleteMultipart(ctx context.Context, ref ObjectRef, id UploadID, parts []Part) (ObjectInfo, error)
	AbortMultipart(ctx context.Context, ref ObjectRef, id UploadID) error

	Capabilities() Capabilities
}
