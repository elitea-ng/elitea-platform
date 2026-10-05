package conversations_test

// S20a: TestArtifactAttachment* — the chat-attachment byte path
// (attachments.go). Handler-level, backed by in-memory fakes (no real
// Postgres/object-store infra) so this suite runs unconditionally, never
// t.Skip — matching the plan's own Verify command, which requires zero
// --- SKIP lines.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// fakeAttachmentStore implements conversations.AttachmentStore entirely
// in-memory — no shared testutil exists in this codebase (established
// convention: each test file writes its own fake).
type fakeAttachmentStore struct {
	mu            sync.Mutex
	bucketName    string
	maxFileBytes  *int64
	retentionDays *int32
	nextBucketID  int64
	buckets       map[string]int64 // "projectID/name" -> id
	chunks        map[string][]conversations.AttachmentChunk
	recorded      []recordedAttachmentObject
}

type recordedAttachmentObject struct {
	BucketID   int64
	Key        string
	ByteLength int64
	MediaType  string
	ExpiresAt  *time.Time
}

func newFakeAttachmentStore() *fakeAttachmentStore {
	return &fakeAttachmentStore{
		buckets: map[string]int64{},
		chunks:  map[string][]conversations.AttachmentChunk{},
	}
}

func (f *fakeAttachmentStore) AttachmentPolicy(context.Context, int64) (string, *int64, *int32, error) {
	return f.bucketName, f.maxFileBytes, f.retentionDays, nil
}

func (f *fakeAttachmentStore) RequireAttachmentBucket(_ context.Context, projectID int64, bucketName string, _ int32) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("%d/%s", projectID, bucketName)
	if id, ok := f.buckets[key]; ok {
		return id, nil
	}
	f.nextBucketID++
	f.buckets[key] = f.nextBucketID
	return f.nextBucketID, nil
}

func (f *fakeAttachmentStore) RecordAttachmentObject(_ context.Context, bucketID int64, key string, byteLength int64, mediaType string, expiresAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, recordedAttachmentObject{
		BucketID: bucketID, Key: key, ByteLength: byteLength, MediaType: mediaType, ExpiresAt: expiresAt,
	})
	return nil
}

func chunkKey(projectID int64, conversationID, fileID string) string {
	return fmt.Sprintf("%d/%s/%s", projectID, conversationID, fileID)
}

func (f *fakeAttachmentStore) UpsertAttachmentChunk(_ context.Context, projectID int64, conversationID, fileID string, chunkIndex, _ int32, _, _ string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := chunkKey(projectID, conversationID, fileID)
	stored := append([]byte(nil), body...)
	for i, c := range f.chunks[key] {
		if c.ChunkIndex == chunkIndex {
			f.chunks[key][i].Bytes = stored
			return nil
		}
	}
	f.chunks[key] = append(f.chunks[key], conversations.AttachmentChunk{ChunkIndex: chunkIndex, Bytes: stored})
	return nil
}

func (f *fakeAttachmentStore) CountAttachmentChunks(_ context.Context, projectID int64, conversationID, fileID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.chunks[chunkKey(projectID, conversationID, fileID)])), nil
}

func (f *fakeAttachmentStore) ListAttachmentChunksOrdered(_ context.Context, projectID int64, conversationID, fileID string) ([]conversations.AttachmentChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	chunks := append([]conversations.AttachmentChunk(nil), f.chunks[chunkKey(projectID, conversationID, fileID)]...)
	for i := 0; i < len(chunks); i++ {
		for j := i + 1; j < len(chunks); j++ {
			if chunks[j].ChunkIndex < chunks[i].ChunkIndex {
				chunks[i], chunks[j] = chunks[j], chunks[i]
			}
		}
	}
	return chunks, nil
}

func (f *fakeAttachmentStore) DeleteAttachmentChunks(_ context.Context, projectID int64, conversationID, fileID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.chunks, chunkKey(projectID, conversationID, fileID))
	return nil
}

func (f *fakeAttachmentStore) AttachmentObject(_ context.Context, projectID int64, bucketName, key string) (conversations.AttachmentObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucketID, ok := f.buckets[fmt.Sprintf("%d/%s", projectID, bucketName)]
	if !ok {
		return conversations.AttachmentObjectInfo{}, storage.ErrNotFound
	}
	for index := len(f.recorded) - 1; index >= 0; index-- {
		if row := f.recorded[index]; row.BucketID == bucketID && row.Key == key {
			return conversations.AttachmentObjectInfo{MediaType: row.MediaType, ByteLength: row.ByteLength}, nil
		}
	}
	return conversations.AttachmentObjectInfo{}, storage.ErrNotFound
}

var _ conversations.AttachmentStore = (*fakeAttachmentStore)(nil)

// fakeAttachmentObjectStore implements storage.ObjectStore — only Put is
// exercised by attachments.go; every other method is unused by this path.
type fakeAttachmentObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
}

func newFakeAttachmentObjectStore() *fakeAttachmentObjectStore {
	return &fakeAttachmentObjectStore{objects: map[string][]byte{}}
}

func (f *fakeAttachmentObjectStore) Put(_ context.Context, ref storage.ObjectRef, body io.Reader, _ storage.PutOptions) (storage.ObjectInfo, error) {
	if f.putErr != nil {
		return storage.ObjectInfo{}, f.putErr
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	f.mu.Lock()
	f.objects[ref.Key()] = data
	f.mu.Unlock()
	return storage.ObjectInfo{Key: ref.Key(), Size: int64(len(data))}, nil
}

func (f *fakeAttachmentObjectStore) Get(context.Context, storage.ObjectRef, *storage.ByteRange) (io.ReadCloser, storage.ObjectInfo, error) {
	return nil, storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) Stat(context.Context, storage.ObjectRef) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) Delete(context.Context, storage.ObjectRef) error {
	return storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) DeleteBatch(context.Context, []storage.ObjectRef) (storage.BatchResult, error) {
	return storage.BatchResult{}, storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) List(context.Context, storage.ListQuery) (storage.ListPage, error) {
	return storage.ListPage{}, storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) PresignGet(context.Context, storage.ObjectRef, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) PresignPut(context.Context, storage.ObjectRef, time.Duration, storage.PutOptions) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) StartMultipart(context.Context, storage.ObjectRef, storage.PutOptions) (storage.UploadID, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) PresignPart(context.Context, storage.ObjectRef, storage.UploadID, int32, time.Duration) (string, error) {
	return "", storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) CompleteMultipart(context.Context, storage.ObjectRef, storage.UploadID, []storage.Part) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) AbortMultipart(context.Context, storage.ObjectRef, storage.UploadID) error {
	return storage.ErrNotSupported
}
func (f *fakeAttachmentObjectStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{}
}

var _ storage.ObjectStore = (*fakeAttachmentObjectStore)(nil)

// --- test helpers ----------------------------------------------------------

func newAttachmentTestHandler(t *testing.T, attStore *fakeAttachmentStore, objStore *fakeAttachmentObjectStore) *conversations.Handler {
	t.Helper()
	return conversations.NewHandler(&mockRepo{}).
		WithObjectStore(objStore).
		WithAttachmentStore(attStore)
}

func multipartAttachmentBody(t *testing.T, fields map[string]string, fileName string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	part, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func doAttachmentUpload(t *testing.T, h *conversations.Handler, fields map[string]string, fileName string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartAttachmentBody(t, fields, fileName, content)
	router := newRouter(h)
	req := httptest.NewRequest(http.MethodPost, "/projects/1/conversations/conv-abc/attachments", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// --- scenarios ---------------------------------------------------------

func TestArtifactAttachmentPlainUploadStoresBytesAndReturnsFilepath(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	content := []byte("hello attachment")
	rec := doAttachmentUpload(t, h, nil, "notes.txt", content)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Filepath string `json:"filepath"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].FileSize != int64(len(content)) {
		t.Fatalf("response = %+v, want one item with file_size=%d", got, len(content))
	}
	if len(attStore.recorded) != 1 || attStore.recorded[0].ByteLength != int64(len(content)) {
		t.Fatalf("RecordAttachmentObject calls = %+v, want one matching call", attStore.recorded)
	}
}

func TestArtifactAttachmentChunkedUploadReceivesThenMerges(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	part1, part2 := []byte("first-half-"), []byte("second-half")
	fields := func(index int) map[string]string {
		return map[string]string{
			"file_id":      "abc123",
			"chunk_index":  fmt.Sprint(index),
			"total_chunks": "2",
			"file_name":    "big.txt",
		}
	}

	// Sent out of index order (1 before 0) deliberately: this is what
	// actually distinguishes index-order merging from arrival-order
	// merging below — sending them in index order too would make this
	// test pass even if the merge silently regressed to arrival order.
	rec1 := doAttachmentUpload(t, h, fields(1), "chunk", part2)
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("chunk 1 status = %d, want 202; body=%s", rec1.Code, rec1.Body.String())
	}
	var chunkResp map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &chunkResp); err != nil {
		t.Fatalf("decode chunk response: %v", err)
	}
	if chunkResp["status"] != "chunk_received" {
		t.Fatalf("chunk response = %+v, want status=chunk_received", chunkResp)
	}

	rec2 := doAttachmentUpload(t, h, fields(0), "chunk", part1)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("final chunk status = %d, want 201; body=%s", rec2.Code, rec2.Body.String())
	}
	var final []struct {
		FileSize int64 `json:"file_size"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &final); err != nil {
		t.Fatalf("decode final response: %v", err)
	}
	wantSize := int64(len(part1) + len(part2))
	if len(final) != 1 || final[0].FileSize != wantSize {
		t.Fatalf("final response = %+v, want file_size=%d", final, wantSize)
	}

	// Chunks were merged in index order (0 then 1), not arrival order (1
	// then 0, per the out-of-order sends above) — this assertion would fail
	// if the merge regressed to arrival order.
	var merged []byte
	for _, data := range objStore.objects {
		merged = data
	}
	if string(merged) != "first-half-second-half" {
		t.Fatalf("merged bytes = %q, want %q", merged, "first-half-second-half")
	}

	remaining, err := attStore.CountAttachmentChunks(context.Background(), 1, "conv-abc", "abc123")
	if err != nil || remaining != 0 {
		t.Fatalf("chunks remaining after merge = %d (err=%v), want 0 (cleaned up)", remaining, err)
	}
}

func TestArtifactAttachmentChunkOutOfRangeIsRejected(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	rec := doAttachmentUpload(t, h, map[string]string{
		"file_id":      "abc123",
		"chunk_index":  "5",
		"total_chunks": "2",
		"file_name":    "big.txt",
	}, "chunk", []byte("x"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestArtifactAttachmentExcessiveTotalChunksIsRejected closes a real gap an
// adversarial review found: without this cap, a caller could declare an
// arbitrarily large total_chunks and drip-feed distinct chunk_index values
// forever, growing elitea_storage.attachment_chunks unbounded (never
// reached by S12's SumProjectBytes quota, which only sums
// elitea_storage.objects) — and, separately, would have made the merge
// step's buffer pre-allocation attempt a multi-gigabyte allocation from a
// modest real payload.
func TestArtifactAttachmentExcessiveTotalChunksIsRejected(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	rec := doAttachmentUpload(t, h, map[string]string{
		"file_id":      "abc123",
		"chunk_index":  "0",
		"total_chunks": "100000",
		"file_name":    "big.txt",
	}, "chunk", []byte("x"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	count, err := attStore.CountAttachmentChunks(context.Background(), 1, "conv-abc", "abc123")
	if err != nil || count != 0 {
		t.Fatalf("chunk count after rejected upload = %d (err=%v), want 0 — the chunk must never be stored", count, err)
	}
}

func TestArtifactAttachmentInvalidFileIDIsRejected(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	rec := doAttachmentUpload(t, h, map[string]string{
		"file_id":      "not a valid id!",
		"chunk_index":  "0",
		"total_chunks": "1",
		"file_name":    "big.txt",
	}, "chunk", []byte("x"))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactAttachmentFileOverLimitIsRejected(t *testing.T) {
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_FILE_MB", "1")
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	oversized := bytes.Repeat([]byte("x"), 2<<20)
	rec := doAttachmentUpload(t, h, nil, "big.bin", oversized)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if len(attStore.recorded) != 0 {
		t.Fatalf("RecordAttachmentObject was called for an oversized upload: %+v", attStore.recorded)
	}
}

func TestArtifactAttachmentImageOverLimitIsRejectedAtTheLowerImageCeiling(t *testing.T) {
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_IMAGE_MB", "1")
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_FILE_MB", "150")
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	// Under the file ceiling but over the (lower) image ceiling.
	oversizedImage := bytes.Repeat([]byte("x"), 2<<20)
	rec := doAttachmentUpload(t, h, nil, "photo.png", oversizedImage)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactAttachmentSVGIsNotTreatedAsAnImage(t *testing.T) {
	// .svg is image/* by MIME type but legacy explicitly excludes it from
	// the lower image ceiling — see isImageAttachment's own doc comment.
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_IMAGE_MB", "1")
	t.Setenv("ARTIFACT_ATTACHMENT_MAX_FILE_MB", "150")
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	overImageLimitButUnderFileLimit := bytes.Repeat([]byte("x"), 2<<20)
	rec := doAttachmentUpload(t, h, nil, "diagram.svg", overImageLimitButUnderFileLimit)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (svg uses the file ceiling, not the image one); body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactAttachmentMissingFilePartIsRejected(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("note", "no file field here"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	router := newRouter(h)
	req := httptest.NewRequest(http.MethodPost, "/projects/1/conversations/conv-abc/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactAttachmentUsesProjectStoragePolicyBucketName(t *testing.T) {
	attStore := newFakeAttachmentStore()
	attStore.bucketName = "custom-attachments"
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	rec := doAttachmentUpload(t, h, nil, "notes.txt", []byte("hi"))
	requireAttachmentCreatedFilepathContains(t, rec, "/custom-attachments/")
}

// TestArtifactAttachmentUsesProjectStoragePolicyFileLimit proves
// project_storage_policy.max_object_bytes overrides the env/const per-file
// ceiling for non-image uploads — matching objects.go's own established
// policy-first precedent for the identical concept (S12).
func TestArtifactAttachmentUsesProjectStoragePolicyFileLimit(t *testing.T) {
	attStore := newFakeAttachmentStore()
	tinyLimit := int64(10)
	attStore.maxFileBytes = &tinyLimit
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	rec := doAttachmentUpload(t, h, nil, "notes.txt", []byte("this is more than ten bytes"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (policy's 10-byte limit should apply); body=%s", rec.Code, rec.Body.String())
	}
}

func requireAttachmentCreatedFilepathContains(t *testing.T, rec *httptest.ResponseRecorder, substr string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Filepath string `json:"filepath"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || !bytes.Contains([]byte(got[0].Filepath), []byte(substr)) {
		t.Fatalf("response = %+v, want filepath containing %q", got, substr)
	}
}

func TestArtifactAttachmentJSONBodyStillUpdatesConversationMeta(t *testing.T) {
	// Proves AddAttachments' Content-Type dispatch (attachments.go) did not
	// disturb the pre-existing JSON-metadata branch — same assertion as
	// TestAddAttachments_Success, exercised again here under the
	// TestArtifactAttachment* name this stage's Verify command greps for.
	var called bool
	repo := &mockRepo{
		addAttachmentsFn: func(_ context.Context, _, _ string, _ map[string]any) error {
			called = true
			return nil
		},
	}
	h := conversations.NewHandler(repo)
	router := newRouter(h)

	body, _ := json.Marshal(map[string]any{"files": []string{"file1.txt"}})
	req := httptest.NewRequest(http.MethodPost, "/projects/1/conversations/conv-abc/attachments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("repo.AddAttachments was never called for a JSON request")
	}
}

// TestArtifactAttachmentUploadStampsItsOwnExpiry pins the retention rule for
// a chat attachment.
//
// DEFECT: RequireAttachmentBucket returned the system bucket's expires_at,
// and the upload handler stamped that one value onto every attachment it
// ever recorded. The bucket's expires_at is computed once, when the bucket
// row is created on the project's FIRST attachment. So
// ARTIFACT_ATTACHMENT_RETENTION_DAYS days after that first attachment, every
// new attachment was recorded with a deadline in the past. The artifact
// retention sweeper then deleted it within 15 minutes of a successful 201.
//
// The fake store returns a bucket id only, which is the shape that makes the
// defect unrepresentable: there is no frozen deadline left to copy.
func TestArtifactAttachmentUploadStampsItsOwnExpiry(t *testing.T) {
	attStore := newFakeAttachmentStore()
	retention := int32(7)
	attStore.retentionDays = &retention
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	before := time.Now()
	rec := doAttachmentUpload(t, h, nil, "notes.txt", []byte("hello attachment"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	if len(attStore.recorded) != 1 {
		t.Fatalf("RecordAttachmentObject calls = %+v, want one", attStore.recorded)
	}
	got := attStore.recorded[0].ExpiresAt
	if got == nil {
		t.Fatal("attachment recorded with a nil ExpiresAt, want now + retention_days")
	}
	// The sweeper deletes anything already past its deadline. A fresh
	// attachment must never be born in that set.
	if !got.After(time.Now()) {
		t.Fatalf("attachment ExpiresAt = %v is not in the future; the sweeper deletes it on the next tick", *got)
	}
	wantLow := before.AddDate(0, 0, int(retention))
	wantHigh := time.Now().AddDate(0, 0, int(retention))
	if got.Before(wantLow) || got.After(wantHigh) {
		t.Errorf("attachment ExpiresAt = %v, want between %v and %v", *got, wantLow, wantHigh)
	}
}

// A chat attachment is stored under `{conversationUUID}/{filename}` and the
// admission gate refuses any reference that is not prefixed by the
// conversation's UUID (internal/application/agentexecution/attachments.go,
// currentTurnAttachments) — an authorisation check, not tidiness. So an object
// keyed by the conversation's NUMERIC id is unusable the moment it is written:
// the upload answers 201, the send is refused 400 before admissions.Submit
// runs, the question is lost, and the bytes stay in the bucket until retention
// expires them. That is what the web composer really did (#606's client half).
//
// The refusal is asserted together with "nothing was stored": a 400 that had
// already written the object would leave exactly the orphan this guard exists
// to prevent, and only the store assertions can tell the two apart.
func TestArtifactAttachmentUploadRefusesNumericConversationID(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	body, contentType := multipartAttachmentBody(t, nil, "notes.txt", []byte("hello attachment"))
	req := httptest.NewRequest(http.MethodPost, "/projects/1/conversations/4242/attachments", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a numeric conversation id; body=%s", rec.Code, rec.Body.String())
	}
	// The message has to name the identifier, not just say "invalid": the
	// caller holds both and the whole defect is that they are interchangeable
	// everywhere else on this resource.
	if !strings.Contains(rec.Body.String(), "uuid") {
		t.Errorf("body = %s, want a message naming the conversation uuid", rec.Body.String())
	}
	if len(objStore.objects) != 0 {
		t.Errorf("object store holds %v after a refused upload, want nothing written", objStore.objects)
	}
	if len(attStore.recorded) != 0 {
		t.Errorf("RecordAttachmentObject calls = %+v after a refused upload, want none", attStore.recorded)
	}
}

// The UUID path is the one the fixed composer takes, and it stays keyed
// exactly as `finalizeAttachment`'s own comment says it is — the guard above
// is what makes that comment true, so it is asserted here rather than left
// implied.
func TestArtifactAttachmentUploadKeysObjectByConversationUUID(t *testing.T) {
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore)

	const conversationUUID = "8f14e45f-ceea-467a-9ba6-2e3a3d1e9b21"
	body, contentType := multipartAttachmentBody(t, nil, "notes.txt", []byte("hello attachment"))
	req := httptest.NewRequest(http.MethodPost, "/projects/1/conversations/"+conversationUUID+"/attachments", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Filepath string `json:"filepath"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 || got[0].Filepath != "/chat-attachments/"+conversationUUID+"/notes.txt" {
		t.Fatalf("filepath = %+v, want /chat-attachments/%s/notes.txt", got, conversationUUID)
	}
	if _, ok := objStore.objects[conversationUUID+"/notes.txt"]; !ok {
		t.Errorf("stored keys = %v, want the object keyed %q", objStore.objects, conversationUUID+"/notes.txt")
	}
}
