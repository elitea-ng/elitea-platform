package repos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/extract"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// attachmentObjectTestStore is an in-memory ObjectStore that serves ONE key.
//
// The bytes are faked deliberately and the metadata is not: what this file
// exists to prove is the set of gates that live in Postgres — which project
// owns the bucket, which bucket_type it is, whether a metadata row exists at
// all, and whether its declared length agrees with what came back. A real S3
// backend would add a second service to the run and prove none of those.
type attachmentObjectTestStore struct {
	key  string
	body []byte
	gets int
}

func (store *attachmentObjectTestStore) Get(
	_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange,
) (io.ReadCloser, storage.ObjectInfo, error) {
	store.gets++
	if ref.Key() != store.key {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(store.body)), storage.ObjectInfo{
		Key:  ref.Key(),
		Size: int64(len(store.body)),
	}, nil
}

func (*attachmentObjectTestStore) Put(
	context.Context, storage.ObjectRef, io.Reader, storage.PutOptions,
) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (*attachmentObjectTestStore) Stat(
	context.Context, storage.ObjectRef,
) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (*attachmentObjectTestStore) Delete(context.Context, storage.ObjectRef) error {
	return storage.ErrNotSupported
}

func (*attachmentObjectTestStore) DeleteBatch(
	context.Context, []storage.ObjectRef,
) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}

func (*attachmentObjectTestStore) List(
	context.Context, storage.ListQuery,
) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}

func (*attachmentObjectTestStore) PresignGet(
	context.Context, storage.ObjectRef, time.Duration,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*attachmentObjectTestStore) PresignPut(
	context.Context, storage.ObjectRef, time.Duration, storage.PutOptions,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*attachmentObjectTestStore) StartMultipart(
	context.Context, storage.ObjectRef, storage.PutOptions,
) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (*attachmentObjectTestStore) PresignPart(
	context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*attachmentObjectTestStore) CompleteMultipart(
	context.Context, storage.ObjectRef, storage.UploadID, []storage.Part,
) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (*attachmentObjectTestStore) AbortMultipart(
	context.Context, storage.ObjectRef, storage.UploadID,
) error {
	return storage.ErrNotSupported
}

func (*attachmentObjectTestStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

// TestCurrentAttachmentObjectRepositoryEnforcesItsPostgresGates walks the four
// conditions ReadAttachmentObject documents, each against the real schema.
func TestCurrentAttachmentObjectRepositoryEnforcesItsPostgresGates(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	const (
		project      = int64(90211)
		otherProject = int64(90212)
		bucketName   = "chat-attachments"
		conversation = "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1"
	)
	key := conversation + "/report.txt"
	body := []byte("The secret word is ATTACHTOKEN.\n")

	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}

	// The reserved system bucket the chat upload path creates, and a
	// user-facing bucket in the SAME project holding the SAME key. The second
	// is the interesting one: everything about it satisfies the conversation
	// prefix, and it must still not be readable through this route.
	systemBucket, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: bucketName, DisplayName: bucketName,
		BucketType: "system",
	})
	if err != nil {
		t.Fatalf("create system bucket: %v", err)
	}
	userBucket, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: "user-uploads", DisplayName: "user-uploads",
		BucketType: "local",
	})
	if err != nil {
		t.Fatalf("create user bucket: %v", err)
	}
	// Another tenant's bucket, same name. It exists to prove the project scope
	// is a lookup key and not a filter applied afterwards.
	if _, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: otherProject, Name: bucketName, DisplayName: bucketName,
		BucketType: "system",
	}); err != nil {
		t.Fatalf("create other tenant bucket: %v", err)
	}

	for _, bucketID := range []int64{systemBucket.ID, userBucket.ID} {
		if _, err := objects.UpsertObject(ctx, NewObjectInput{
			BucketID: bucketID, Key: key,
			ByteLength: int64(len(body)), MediaType: "text/plain",
		}); err != nil {
			t.Fatalf("record attachment object: %v", err)
		}
	}

	store := &attachmentObjectTestStore{key: key, body: body}
	repository, err := NewCurrentAttachmentObjectRepository(pool, store)
	if err != nil {
		t.Fatalf("NewCurrentAttachmentObjectRepository: %v", err)
	}

	record, err := repository.ReadAttachmentObject(ctx, project, bucketName, key, 128*1024)
	if err != nil {
		t.Fatalf("ReadAttachmentObject: %v", err)
	}
	if string(record.Content) != string(body) {
		t.Fatalf("ReadAttachmentObject content = %q, want %q", record.Content, body)
	}
	if record.MediaType != "text/plain" || record.ByteLength != int64(len(body)) {
		t.Fatalf("ReadAttachmentObject metadata wrong: %+v", record)
	}

	gets := store.gets
	for name, call := range map[string]func() (storage.AttachmentObjectRecord, error){
		// (1) the bucket belongs to another project — same name, different
		// tenant, and the row this project owns is the only one reachable.
		"another tenant's bucket": func() (storage.AttachmentObjectRecord, error) {
			return repository.ReadAttachmentObject(ctx, otherProject, bucketName, key, 128*1024)
		},
		// (2) a user-facing bucket in the caller's OWN project, holding the
		// very same conversation-prefixed key.
		"a non-system bucket": func() (storage.AttachmentObjectRecord, error) {
			return repository.ReadAttachmentObject(ctx, project, "user-uploads", key, 128*1024)
		},
		// (3) no metadata row: an object nothing recorded was not written by
		// the upload path.
		"no metadata row": func() (storage.AttachmentObjectRecord, error) {
			return repository.ReadAttachmentObject(
				ctx, project, bucketName, conversation+"/never-uploaded.txt", 128*1024,
			)
		},
		"a bucket that does not exist": func() (storage.AttachmentObjectRecord, error) {
			return repository.ReadAttachmentObject(ctx, project, "no-such-bucket", key, 128*1024)
		},
	} {
		if _, err := call(); !errors.Is(err, storage.ErrContentNotFound) {
			t.Fatalf("%s: err = %v, want ErrContentNotFound", name, err)
		}
	}
	if store.gets != gets {
		t.Fatalf("a refused reference reached object storage %d times", store.gets-gets)
	}

	// (4) the declared length is the ceiling's subject, and it is checked
	// BEFORE the bytes are fetched.
	if _, err := repository.ReadAttachmentObject(
		ctx, project, bucketName, key, int64(len(body)-1),
	); !errors.Is(err, storage.ErrContentRejected) || attachmentRefusalReason(err) != extract.ReasonTooLarge {
		t.Fatalf("over-cap read err = %v, want a too_large refusal", err)
	}
	if store.gets != gets {
		t.Fatalf("an over-cap object was fetched before its length was checked")
	}

	// …and a metadata row that disagrees with the bytes is refused rather than
	// resolved in favour of either side. It is an error, not a property of
	// the file: in particular it is not "too large".
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: systemBucket.ID, Key: key,
		ByteLength: int64(len(body)) + 10, MediaType: "text/plain",
	}); err != nil {
		t.Fatalf("rewrite attachment object row: %v", err)
	}
	if _, err := repository.ReadAttachmentObject(
		ctx, project, bucketName, key, 128*1024,
	); err == nil || errors.Is(err, storage.ErrContentRejected) || errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("length-drift read err = %v, want a server error", err)
	}

	// An empty upload is "empty", never "too large".
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: systemBucket.ID, Key: key, ByteLength: 0, MediaType: "text/plain",
	}); err != nil {
		t.Fatalf("rewrite attachment object row: %v", err)
	}
	gets = store.gets
	if _, err := repository.ReadAttachmentObject(
		ctx, project, bucketName, key, 128*1024,
	); attachmentRefusalReason(err) != extract.ReasonEmpty {
		t.Fatalf("empty read err = %v, want an empty refusal", err)
	}
	if store.gets != gets {
		t.Fatalf("an empty object was fetched")
	}
}

func attachmentRefusalReason(err error) extract.Reason {
	var refusal *storage.AttachmentUnreadableError
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}
