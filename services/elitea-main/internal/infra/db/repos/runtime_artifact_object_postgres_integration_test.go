package repos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// runtimeArtifactTestStore is an in-memory ObjectStore with the four verbs the
// runtime artifact plane uses.
//
// The BYTES are faked and the metadata is not: what this file exists to prove
// lives in Postgres — which project owns the bucket, what the actor's
// per-bucket access list says, and that a write records a metadata row the
// quota accounting can then see. A real S3 backend would add a service to the
// run and prove none of those.
type runtimeArtifactTestStore struct {
	objects map[string][]byte
	types   map[string]string
	puts    int
	deletes int
}

func newRuntimeArtifactTestStore() *runtimeArtifactTestStore {
	return &runtimeArtifactTestStore{
		objects: map[string][]byte{},
		types:   map[string]string{},
	}
}

// key is PROJECT-scoped, exactly as a real backend's path is
// (ObjectRef.StorageKey). Keying by bucket and object alone would make the
// double answer for any tenant that happens to use the same bucket name, and
// the cross-tenant case below would then pass without the product doing
// anything.
func (store *runtimeArtifactTestStore) key(ref storage.ObjectRef) string {
	return ref.ProjectID() + "/" + ref.Bucket() + "/" + ref.Key()
}

func (store *runtimeArtifactTestStore) Put(
	_ context.Context, ref storage.ObjectRef, body io.Reader, opts storage.PutOptions,
) (storage.ObjectInfo, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	store.puts++
	store.objects[store.key(ref)] = content
	store.types[store.key(ref)] = opts.ContentType
	return storage.ObjectInfo{
		Key:          ref.Key(),
		Size:         int64(len(content)),
		ContentType:  opts.ContentType,
		LastModified: time.Now().UTC(),
	}, nil
}

func (store *runtimeArtifactTestStore) Get(
	_ context.Context, ref storage.ObjectRef, _ *storage.ByteRange,
) (io.ReadCloser, storage.ObjectInfo, error) {
	content, ok := store.objects[store.key(ref)]
	if !ok {
		return nil, storage.ObjectInfo{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(content)), storage.ObjectInfo{
		Key:       ref.Key(),
		Size:      int64(len(content)),
		TotalSize: int64(len(content)),
	}, nil
}

func (store *runtimeArtifactTestStore) Stat(
	_ context.Context, ref storage.ObjectRef,
) (storage.ObjectInfo, error) {
	content, ok := store.objects[store.key(ref)]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(content))}, nil
}

func (store *runtimeArtifactTestStore) Delete(_ context.Context, ref storage.ObjectRef) error {
	if _, ok := store.objects[store.key(ref)]; !ok {
		return storage.ErrNotFound
	}
	store.deletes++
	delete(store.objects, store.key(ref))
	return nil
}

func (*runtimeArtifactTestStore) DeleteBatch(
	context.Context, []storage.ObjectRef,
) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}

func (store *runtimeArtifactTestStore) List(
	_ context.Context, query storage.ListQuery,
) (storage.ListPage, error) {
	page := storage.ListPage{}
	bucketPrefix := query.Bucket.ProjectID() + "/" + query.Bucket.Bucket() + "/"
	prefix := bucketPrefix + query.KeyPrefix
	for key, content := range store.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		page.Objects = append(page.Objects, storage.ObjectInfo{
			Key:          strings.TrimPrefix(key, bucketPrefix),
			Size:         int64(len(content)),
			ContentType:  store.types[key],
			LastModified: time.Now().UTC(),
		})
	}
	return page, nil
}

func (*runtimeArtifactTestStore) PresignGet(
	context.Context, storage.ObjectRef, time.Duration,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) PresignPut(
	context.Context, storage.ObjectRef, time.Duration, storage.PutOptions,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) StartMultipart(
	context.Context, storage.ObjectRef, storage.PutOptions,
) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) PresignPart(
	context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration,
) (string, error) {
	return "", storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) CompleteMultipart(
	context.Context, storage.ObjectRef, storage.UploadID, []storage.Part,
) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) AbortMultipart(
	context.Context, storage.ObjectRef, storage.UploadID,
) error {
	return storage.ErrNotSupported
}

func (*runtimeArtifactTestStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

// runtimeArtifactAuthorizer stands in for the durable claim: the service takes
// the project and the actor from what THIS returns, never from the request,
// which is the property every case below depends on.
type runtimeArtifactAuthorizer struct {
	projectID int64
	actorID   string
	err       error
}

func (authorizer runtimeArtifactAuthorizer) AuthorizeAgentRuntimeContext(
	context.Context, storage.ContentClaim,
) (storage.RuntimeContextAuthorization, error) {
	if authorizer.err != nil {
		return storage.RuntimeContextAuthorization{}, authorizer.err
	}
	return storage.RuntimeContextAuthorization{
		ResourceProjectID: authorizer.projectID,
		ActorID:           authorizer.actorID,
		Initiator:         "user",
		ConversationID:    "5f5a1ad4-2b30-4a54-9b7f-2d05a0d3f6c1",
	}, nil
}

// TestRuntimeArtifactRoutesAgainstPostgres walks all four claim-authorized
// artifact routes (#906) against the real schema: write, read, list, delete,
// plus the per-bucket access list and the project scope that gate every one of
// them.
func TestRuntimeArtifactRoutesAgainstPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	const (
		project      = int64(90411)
		otherProject = int64(90412)
		actor        = int64(4711)
		blockedActor = int64(4712)
		adminActor   = int64(4713)
		bucketName   = "agent-artifacts"
	)

	// The project-admin bypass reads the auth tables, which this service does
	// not own and the migration corpus therefore does not create.
	seedAuthCoreTables(t, pool)
	seedProjectAdmin(t, pool, project, adminActor)

	buckets, err := NewArtifactBucketsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactBucketsRepository: %v", err)
	}
	objects, err := NewArtifactObjectsRepository(pool)
	if err != nil {
		t.Fatalf("NewArtifactObjectsRepository: %v", err)
	}
	bucketRow, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: project, Name: bucketName, DisplayName: bucketName, BucketType: "local",
	})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// Another tenant's bucket with the SAME name, so the project scope is
	// proven to be a lookup key rather than a filter applied afterwards.
	if _, err := buckets.CreateBucket(ctx, NewBucketInput{
		ProjectID: otherProject, Name: bucketName, DisplayName: bucketName, BucketType: "local",
	}); err != nil {
		t.Fatalf("create other tenant bucket: %v", err)
	}
	// The access list: one member explicitly blocked on this bucket ([] means
	// "no access", which is NOT the same as having no row at all).
	for _, blocked := range []int64{blockedActor, adminActor} {
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_storage.bucket_permissions (project_id, user_id, bucket, permissions)
VALUES ($1, $2, $3, '{}'::text[])`, project, blocked, bucketName); err != nil {
			t.Fatalf("seed blocked bucket permission: %v", err)
		}
	}

	store := newRuntimeArtifactTestStore()
	source, err := NewCurrentRuntimeArtifactRepository(pool, store)
	if err != nil {
		t.Fatalf("NewCurrentRuntimeArtifactRepository: %v", err)
	}
	service, err := storage.NewRuntimeArtifactObjectService(
		runtimeArtifactAuthorizer{projectID: project, actorID: "4711"}, source,
	)
	if err != nil {
		t.Fatalf("NewRuntimeArtifactObjectService: %v", err)
	}
	claim := storage.ContentClaim{ExecutionID: "execution/one", Generation: 1, ClaimID: "claim-one"}

	// ── WRITE ────────────────────────────────────────────────────────────
	const fileName = "reports/summary.txt"
	const fileBody = "# Summary\n\nThe token is ARTIFACTTOKEN.\n"
	written, err := service.Write(ctx, claim, storage.RuntimeArtifactWriteRequest{
		Bucket: bucketName, Name: fileName, Content: fileBody,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written.Name != fileName || written.ByteLength != int64(len(fileBody)) {
		t.Fatalf("Write returned %+v", written)
	}
	// The content type is derived from the extension by the SHARED rule, not
	// taken from the request — what keeps an agent-written file and a
	// person-uploaded one of the same name indistinguishable to a browser.
	// `.txt` is asserted rather than `.md` because Go's built-in table answers
	// for it on every host, while `.md` depends on the image's /etc/mime.types
	// — a difference that belongs to the shared helper, not to this route.
	if written.MediaType != storage.MediaTypeFromKey(fileName) ||
		!strings.HasPrefix(written.MediaType, "text/plain") {
		t.Fatalf("Write media type = %q, want text/plain", written.MediaType)
	}
	// The metadata row is what makes the project quota mean anything; a write
	// that stored bytes and recorded nothing would leave SumProjectBytes at 0
	// forever.
	total, err := objects.SumProjectBytes(ctx, project)
	if err != nil {
		t.Fatalf("SumProjectBytes: %v", err)
	}
	if total != int64(len(fileBody)) {
		t.Fatalf("recorded project bytes = %d, want %d", total, len(fileBody))
	}

	// ── READ ─────────────────────────────────────────────────────────────
	read, err := service.Read(ctx, claim, storage.RuntimeArtifactReadRequest{
		Bucket: bucketName, Name: fileName,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read.Content != fileBody || read.OverLimit {
		t.Fatalf("Read returned %+v", read)
	}
	if read.MaxChars != 200_000 || read.CharLength != int64(len([]rune(fileBody))) {
		t.Fatalf("Read measurement wrong: %+v", read)
	}

	// A file past the agent-path cap comes back as the MEASUREMENT, not as
	// content and not as an error: the tool has to be able to tell the model
	// the limit and the actual size.
	const bigName = "reports/big.txt"
	bigBody := strings.Repeat("0123456789", 30_000) // 300k characters
	if _, err := service.Write(ctx, claim, storage.RuntimeArtifactWriteRequest{
		Bucket: bucketName, Name: bigName, Content: bigBody,
	}); !errors.Is(err, storage.ErrContentRejected) {
		t.Fatalf("a 300k-character WRITE err = %v, want ErrContentRejected", err)
	}
	// Written behind the route, the way a person's upload would be: the read
	// cap is about what reaches a prompt, not about who put the file there.
	bigRef, err := storage.NewObjectRef("90411", bucketName, bigName)
	if err != nil {
		t.Fatalf("NewObjectRef: %v", err)
	}
	if _, err := store.Put(ctx, bigRef, strings.NewReader(bigBody), storage.PutOptions{}); err != nil {
		t.Fatalf("seed the oversized object: %v", err)
	}
	if _, err := objects.UpsertObject(ctx, NewObjectInput{
		BucketID: bucketRow.ID, Key: bigName,
		ByteLength: int64(len(bigBody)), MediaType: "text/plain",
	}); err != nil {
		t.Fatalf("record the oversized object: %v", err)
	}
	over, err := service.Read(ctx, claim, storage.RuntimeArtifactReadRequest{
		Bucket: bucketName, Name: bigName,
	})
	if err != nil {
		t.Fatalf("over-cap Read: %v", err)
	}
	if !over.OverLimit || over.Content != "" || over.MaxChars != 200_000 {
		t.Fatalf("over-cap Read returned %+v", over)
	}
	if over.CharLength != int64(len(bigBody)) {
		t.Fatalf("over-cap Read char length = %d, want %d", over.CharLength, len(bigBody))
	}

	// ── LIST ─────────────────────────────────────────────────────────────
	listed, err := service.List(ctx, claim, storage.RuntimeArtifactListRequest{
		Bucket: bucketName, Prefix: "reports/", Recursive: true,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed.Files) != 2 {
		t.Fatalf("List returned %d files, want 2: %+v", len(listed.Files), listed.Files)
	}

	// ── DELETE ───────────────────────────────────────────────────────────
	if _, err := service.Delete(ctx, claim, storage.RuntimeArtifactDeleteRequest{
		Bucket: bucketName, Name: fileName,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := service.Read(ctx, claim, storage.RuntimeArtifactReadRequest{
		Bucket: bucketName, Name: fileName,
	}); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("Read after Delete err = %v, want ErrContentNotFound", err)
	}
	// …and the metadata row went with the bytes, or the project's accounted
	// usage would drift upward on every agent-authored file.
	remaining, err := objects.SumProjectBytes(ctx, project)
	if err != nil {
		t.Fatalf("SumProjectBytes after delete: %v", err)
	}
	if remaining != int64(len(bigBody)) {
		t.Fatalf("recorded project bytes after delete = %d, want %d", remaining, len(bigBody))
	}

	// ── THE ACCESS LIST, AND THE PROJECT SCOPE ───────────────────────────
	blocked, err := storage.NewRuntimeArtifactObjectService(
		runtimeArtifactAuthorizer{projectID: project, actorID: "4712"}, source,
	)
	if err != nil {
		t.Fatalf("NewRuntimeArtifactObjectService(blocked): %v", err)
	}
	writes := store.puts
	deletes := store.deletes
	for name, call := range map[string]func() error{
		"a blocked member listing": func() error {
			_, err := blocked.List(ctx, claim, storage.RuntimeArtifactListRequest{Bucket: bucketName})
			return err
		},
		"a blocked member reading": func() error {
			_, err := blocked.Read(ctx, claim, storage.RuntimeArtifactReadRequest{
				Bucket: bucketName, Name: bigName,
			})
			return err
		},
		"a blocked member writing": func() error {
			_, err := blocked.Write(ctx, claim, storage.RuntimeArtifactWriteRequest{
				Bucket: bucketName, Name: "sneaky.txt", Content: "no",
			})
			return err
		},
		"a blocked member deleting": func() error {
			_, err := blocked.Delete(ctx, claim, storage.RuntimeArtifactDeleteRequest{
				Bucket: bucketName, Name: bigName,
			})
			return err
		},
	} {
		if err := call(); !errors.Is(err, storage.ErrContentUnauthorized) {
			t.Fatalf("%s: err = %v, want ErrContentUnauthorized", name, err)
		}
	}
	if store.puts != writes || store.deletes != deletes {
		t.Fatalf("a refused caller reached object storage")
	}

	// …and the project-admin bypass is the same one the HTTP routes apply: an
	// exception written against an administrator's own account does not lock
	// them out of their own project's bucket.
	administrator, err := storage.NewRuntimeArtifactObjectService(
		runtimeArtifactAuthorizer{projectID: project, actorID: "4713"}, source,
	)
	if err != nil {
		t.Fatalf("NewRuntimeArtifactObjectService(admin): %v", err)
	}
	if _, err := administrator.List(ctx, claim, storage.RuntimeArtifactListRequest{
		Bucket: bucketName,
	}); err != nil {
		t.Fatalf("a blocked project ADMIN was refused: %v", err)
	}

	// The claim's project is the only one addressable: a claim for another
	// tenant cannot read this bucket even though the NAME exists there too.
	foreign, err := storage.NewRuntimeArtifactObjectService(
		runtimeArtifactAuthorizer{projectID: otherProject, actorID: "4711"}, source,
	)
	if err != nil {
		t.Fatalf("NewRuntimeArtifactObjectService(foreign): %v", err)
	}
	if _, err := foreign.Read(ctx, claim, storage.RuntimeArtifactReadRequest{
		Bucket: bucketName, Name: bigName,
	}); !errors.Is(err, storage.ErrContentNotFound) {
		t.Fatalf("a foreign project's read err = %v, want ErrContentNotFound", err)
	}

	// A refused CLAIM never reaches the access list at all.
	refused, err := storage.NewRuntimeArtifactObjectService(
		runtimeArtifactAuthorizer{err: storage.ErrContentUnauthorized}, source,
	)
	if err != nil {
		t.Fatalf("NewRuntimeArtifactObjectService(refused): %v", err)
	}
	if _, err := refused.List(ctx, claim, storage.RuntimeArtifactListRequest{
		Bucket: bucketName,
	}); !errors.Is(err, storage.ErrContentUnauthorized) {
		t.Fatalf("a refused claim's list err = %v, want ErrContentUnauthorized", err)
	}
}
