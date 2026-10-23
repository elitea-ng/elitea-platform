package artifacts

// The read and delete half of the artifact API, added for DeepWiki's
// wiki_query family (ADR-0022 parity): a wiki lives in the bucket as a set
// of keys under `{wiki_id}/`, so listing wikis and deleting one are bucket
// operations, not engine ones.
//
// They are ArtifactObjectService's ListObjects, GetObject and DeleteBatch,
// which elitea-main runs through the modern /artifacts/objects/{project}/
// {bucket}[…] routes — the ones the wiki browser already reads and deletes
// through (the legacy plugin's /artifacts/artifact(s)/default/… shape is
// served by nothing).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// Object is one listed object.
type Object struct {
	Key        string
	SizeBytes  int64
	MediaType  string
	ModifiedAt string
}

// Failure is one key a batch delete could not remove.
type Failure struct {
	Key     string
	Code    string
	Message string
}

// Store is the whole artifact surface: uploading, plus the reads and the
// batch delete a sub-application needs to manage what it produced. The
// upload-only Client stays the narrow interface a composer depends on; a
// caller that needs more asserts to this.
type Store interface {
	Client
	// List returns every object whose key starts with prefix, following the
	// route's cursor until it is exhausted.
	List(ctx context.Context, bucket, prefix string) ([]Object, error)
	// Download returns one object's bytes.
	Download(ctx context.Context, bucket, key string) ([]byte, error)
	// DeleteBatch removes keys in ONE request and reports what survived.
	// The failures are returned rather than folded into a count: a partial
	// delete leaves a half-removed wiki, and the operator has to be told
	// which keys remain.
	DeleteBatch(ctx context.Context, bucket string, keys []string) ([]string, []Failure, error)
}

const (
	// maxListPages bounds the cursor loop. A wiki bucket holds thousands of
	// keys, not millions; a listing that will not end is a server fault, and
	// looping forever on it would hang the invocation instead of failing it.
	maxListPages = 200
	// listPageSize is what one page asks for. The route's own cap is what
	// actually applies; this only keeps the number of round trips down.
	listPageSize = 1000
	// maxReadBytes bounds one read. A manifest or a registry is kilobytes;
	// anything of this size is not one, and reading it into memory to
	// discover that is the failure mode worth avoiding.
	maxReadBytes = MaxDownloadBytes
)

// MaxDownloadBytes is the largest object Download returns. A larger one is
// refused with ErrTooLarge, never returned cut short: half a JSON document
// is not a shorter one.
const MaxDownloadBytes = 32 << 20

var (
	// ErrNotFound is a Download of a key the bucket does not hold.
	ErrNotFound = errors.New("artifact not found")
	// ErrTooLarge is a Download of an object over MaxDownloadBytes.
	ErrTooLarge = errors.New("artifact is too large to read")
)

// List pages through the object listing in one server stream: the service
// follows the cursor itself, up to maxListPages pages.
func (c *GRPCClient) List(ctx context.Context, bucket, prefix string) ([]Object, error) {
	ctx, cancel := context.WithCancel(c.authorized(ctx))
	defer cancel()
	stream, err := c.service.ListObjects(ctx, &runtimev1.ArtifactListObjectsRequestV1{
		ProjectId: c.project, Bucket: strings.ToLower(bucket), Prefix: prefix,
		PageSize: listPageSize, MaxPages: maxListPages,
	})
	var objects []Object
	next := ""
	for err == nil {
		var page *runtimev1.ArtifactListObjectsResponseV1
		if page, err = stream.Recv(); err != nil {
			break
		}
		for _, object := range page.GetObjects() {
			objects = append(objects, Object{
				Key: object.GetKey(), SizeBytes: object.GetSizeBytes(),
				MediaType: object.GetMediaType(), ModifiedAt: object.GetModifiedAt(),
			})
		}
		next = page.GetNextCursor()
	}
	if !errors.Is(err, io.EOF) {
		return nil, failure("list artifacts", err, stream)
	}
	if next != "" {
		return nil, fmt.Errorf("listing %s did not end after %d pages", bucket, maxListPages)
	}
	return objects, nil
}

// Download reads one object.
func (c *GRPCClient) Download(ctx context.Context, bucket, key string) ([]byte, error) {
	return c.DownloadUpTo(ctx, bucket, key, maxReadBytes)
}

// DownloadUpTo is Download with the caller's own bound, for the one reader
// whose objects are legitimately larger than a manifest: an Inventory
// graph.json carries an embedding per entity (a 50-file repository with
// 2560-dimension embeddings exported to 43 MB, measured). Over limit is
// ErrTooLarge, never a short read: the stream is cancelled the moment the
// object announces, or reaches, more than limit bytes.
func (c *GRPCClient) DownloadUpTo(ctx context.Context, bucket, key string, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = maxReadBytes
	}
	ctx, cancel := context.WithCancel(c.authorized(ctx))
	defer cancel()
	stream, err := c.service.GetObject(ctx, &runtimev1.ArtifactGetObjectRequestV1{Object: &runtimev1.ArtifactObjectRefV1{
		ProjectId: c.project, Bucket: strings.ToLower(bucket), Key: key,
	}})
	data := []byte{}
	for err == nil {
		var frame *runtimev1.ArtifactGetObjectResponseV1
		if frame, err = stream.Recv(); err != nil {
			break
		}
		if info := frame.GetInfo(); info != nil && info.GetSizeBytes() > int64(limit) {
			return nil, fmt.Errorf("%w: %s/%s is over %d bytes", ErrTooLarge, bucket, key, limit)
		}
		if len(data)+len(frame.GetData()) > limit {
			return nil, fmt.Errorf("%w: %s/%s is over %d bytes", ErrTooLarge, bucket, key, limit)
		}
		data = append(data, frame.GetData()...)
	}
	if errors.Is(err, io.EOF) {
		return data, nil
	}
	if refusal := refusalOf(err, stream); refusal != nil && refusal.GetHttpStatus() == 404 {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, bucket, key)
	}
	return nil, failure("download artifact", err, stream)
}

// DeleteBatch removes every key in one call.
func (c *GRPCClient) DeleteBatch(ctx context.Context, bucket string, keys []string) ([]string, []Failure, error) {
	if len(keys) == 0 {
		// Refused rather than sent, as the wiki browser's own delete does:
		// the route answers 400 for an empty batch, and a caller that sent
		// one would read that as a failed delete rather than as nothing to do.
		return nil, nil, fmt.Errorf("no keys were named for deletion")
	}
	var trailer metadata.MD
	answer, err := c.service.DeleteBatch(c.authorized(ctx), &runtimev1.ArtifactDeleteBatchRequestV1{
		ProjectId: c.project, Bucket: strings.ToLower(bucket), Keys: keys,
	}, grpc.Trailer(&trailer))
	if err != nil {
		return nil, nil, failure("delete artifacts", err, nil, trailer)
	}
	failed := make([]Failure, 0, len(answer.GetFailed()))
	for _, f := range answer.GetFailed() {
		failed = append(failed, Failure{Key: f.GetKey(), Code: f.GetCode(), Message: f.GetMessage()})
	}
	return answer.GetDeleted(), failed, nil
}

// failure is a call's error: the platform's refusal with its HTTP status and
// text when a route answered, the hop's own error when none did.
func failure(what string, err error, stream trailered, md ...metadata.MD) error {
	if refusal := refusalOf(err, stream, md...); refusal != nil {
		return fmt.Errorf("failed to %s: HTTP %d — %s", what, refusal.GetHttpStatus(), clip(describe(refusal), 500))
	}
	return fmt.Errorf("failed to %s: %w", what, err)
}
