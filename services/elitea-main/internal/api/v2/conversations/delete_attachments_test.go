package conversations_test

// DEFECT #599: DELETE /attachments/prompt_lib/{projectID}/{conversationID}
// stripped chat_conversations.meta.attachments and nothing else — the uploaded
// bytes and their elitea_storage.objects rows survived until the retention
// sweeper expired them. The legacy equivalent route removes the bytes at the
// same moment (legacy/plugins/elitea_core/api/v2/attachments.py:240,
// `mc.remove_file(bucket_name, filename)`), which is the parity baseline these
// tests encode.
//
// Handler-level, in-memory fakes only (no Postgres, no object-store infra), so
// this suite runs unconditionally — matching attachments_test.go's convention.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// --- AttachmentStore delete surface on the shared fake ----------------------
//
// These three methods complete fakeAttachmentStore (attachments_test.go) for
// the widened conversations.AttachmentStore interface. They are defined here,
// beside the tests that exercise them, rather than in attachments_test.go,
// which covers the upload path and never touches them.

// LookupAttachmentBucket is lookup-only, mirroring the real adapter: unlike
// RequireAttachmentBucket it must NOT create the bucket, and must report a
// missing one as storage.ErrNotFound so the handler can treat "nothing was
// ever stored" as a normal delete outcome.
func (f *fakeAttachmentStore) LookupAttachmentBucket(_ context.Context, projectID int64, bucketName string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.buckets[fmt.Sprintf("%d/%s", projectID, bucketName)]; ok {
		return id, nil
	}
	return 0, fmt.Errorf("%w: bucket %q", storage.ErrNotFound, bucketName)
}

func (f *fakeAttachmentStore) ListAttachmentObjectKeys(_ context.Context, bucketID int64, keyPrefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, row := range f.recorded {
		if row.BucketID == bucketID && strings.HasPrefix(row.Key, keyPrefix) {
			keys = append(keys, row.Key)
		}
	}
	return keys, nil
}

func (f *fakeAttachmentStore) DeleteAttachmentObjects(_ context.Context, bucketID int64, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
		delete(f.reserved, fmt.Sprintf("%d/%s", bucketID, k))
	}
	kept := f.recorded[:0:0]
	for _, row := range f.recorded {
		if row.BucketID == bucketID && drop[row.Key] {
			continue
		}
		kept = append(kept, row)
	}
	f.recorded = kept
	return nil
}

// recordedKeys is the metadata rows still present, for assertions.
func (f *fakeAttachmentStore) recordedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.recorded))
	for _, row := range f.recorded {
		keys = append(keys, row.Key)
	}
	return keys
}

// seedBucket registers a bucket without going through the create path.
func (f *fakeAttachmentStore) seedBucket(projectID int64, name string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buckets[fmt.Sprintf("%d/%s", projectID, name)] = id
}

func (f *fakeAttachmentStore) seedObject(bucketID int64, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, recordedAttachmentObject{BucketID: bucketID, Key: key, ByteLength: 3})
}

// --- a delete-capable object store fake ------------------------------------
//
// fakeAttachmentObjectStore (attachments_test.go) answers ErrNotSupported to
// everything but Put, because the upload path never deletes. This one records
// what was deleted and, sharing an ops log with the metadata fake, in what
// ORDER relative to the metadata rows.

type deleteFakeObjectStore struct {
	fakeAttachmentObjectStore
	mu           sync.Mutex
	objects      map[string]bool // key -> present
	deleted      []string
	batchErr     error
	failKeys     map[string]bool // keys DeleteBatch reports in BatchResult.Failed
	ops          *[]string
	lastBucket   string
	lastProjects []string
}

func newDeleteFakeObjectStore(ops *[]string, keys ...string) *deleteFakeObjectStore {
	objects := map[string]bool{}
	for _, k := range keys {
		objects[k] = true
	}
	return &deleteFakeObjectStore{objects: objects, ops: ops}
}

func (f *deleteFakeObjectStore) DeleteBatch(_ context.Context, refs []storage.ObjectRef) (storage.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ops != nil {
		*f.ops = append(*f.ops, "bytes")
	}
	if f.batchErr != nil {
		return storage.BatchResult{}, f.batchErr
	}
	var res storage.BatchResult
	for _, ref := range refs {
		f.lastBucket = ref.Bucket()
		f.lastProjects = append(f.lastProjects, ref.ProjectID())
		if f.failKeys[ref.Key()] {
			res.Failed = append(res.Failed, storage.BatchError{Key: ref.Key(), Err: errors.New("backend refused")})
			continue
		}
		delete(f.objects, ref.Key())
		f.deleted = append(f.deleted, ref.Key())
		res.Deleted = append(res.Deleted, ref.Key())
	}
	return res, nil
}

func (f *deleteFakeObjectStore) remaining() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	return out
}

var _ storage.ObjectStore = (*deleteFakeObjectStore)(nil)

// opsLoggingAttachmentStore wraps the metadata fake so the row delete lands in
// the same ops log as the byte delete — the ordering assertion is the whole
// point: bytes must go first, or a failed byte delete orphans them with no row
// left naming them.
type opsLoggingAttachmentStore struct {
	*fakeAttachmentStore
	ops *[]string
}

func (s *opsLoggingAttachmentStore) DeleteAttachmentObjects(ctx context.Context, bucketID int64, keys []string) error {
	if s.ops != nil {
		*s.ops = append(*s.ops, "rows")
	}
	return s.fakeAttachmentStore.DeleteAttachmentObjects(ctx, bucketID, keys)
}

var _ conversations.AttachmentStore = (*opsLoggingAttachmentStore)(nil)

// --- helpers ---------------------------------------------------------------

func doDeleteAttachments(t *testing.T, h *conversations.Handler, projectID, conversationID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/projects/%s/conversations/%s/attachments", projectID, conversationID), nil)
	rec := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rec, req)
	return rec
}

// metaStrippingRepo records whether the conversation-meta strip ran.
func metaStrippingRepo(stripped *bool) *mockRepo {
	return &mockRepo{deleteAttachmentsFn: func(context.Context, string, string) error {
		*stripped = true
		return nil
	}}
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", label, got, want)
		}
	}
}

// --- scenarios -------------------------------------------------------------

// The happy path: exactly the keys recorded under this conversation's prefix
// lose their bytes and then their rows, and the meta is still stripped.
func TestDeleteAttachments_DeletesRecordedBytesThenRows(t *testing.T) {
	var ops []string
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/one.txt")
	att.seedObject(7, "conv-abc/two.txt")
	obj := newDeleteFakeObjectStore(&ops, "conv-abc/one.txt", "conv-abc/two.txt")

	stripped := false
	h := conversations.NewHandler(metaStrippingRepo(&stripped)).
		WithObjectStore(obj).
		WithAttachmentStore(&opsLoggingAttachmentStore{fakeAttachmentStore: att, ops: &ops})

	rec := doDeleteAttachments(t, h, "1", "conv-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok body, got %s", rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv-abc/one.txt", "conv-abc/two.txt"})
	assertStrings(t, "remaining metadata rows", att.recordedKeys(), nil)
	assertStrings(t, "operation order", ops, []string{"bytes", "rows"})
	if !stripped {
		t.Fatal("conversation meta was not stripped")
	}
	if obj.lastBucket != "chat-attachments" {
		t.Fatalf("deleted from bucket %q, want the upload path's default bucket", obj.lastBucket)
	}
	for _, p := range obj.lastProjects {
		if p != "1" {
			t.Fatalf("deleted under project %q, want %q", p, "1")
		}
	}
}

// The bucket is shared by every conversation in the project, so the prefix is
// what scopes the delete. Another conversation's object must survive.
func TestDeleteAttachments_LeavesOtherConversationsObjects(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/mine.txt")
	att.seedObject(7, "conv-other/theirs.txt")
	obj := newDeleteFakeObjectStore(nil, "conv-abc/mine.txt", "conv-other/theirs.txt")

	stripped := false
	h := conversations.NewHandler(metaStrippingRepo(&stripped)).
		WithObjectStore(obj).
		WithAttachmentStore(att)

	rec := doDeleteAttachments(t, h, "1", "conv-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv-abc/mine.txt"})
	assertStrings(t, "surviving objects", obj.remaining(), []string{"conv-other/theirs.txt"})
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"conv-other/theirs.txt"})
}

// No bucket means nothing was ever stored for this project. That is a normal
// delete, not an error — and the lookup must not have created one either.
func TestDeleteAttachments_BucketNotFoundStripsMetaOnly(t *testing.T) {
	att := newFakeAttachmentStore()
	obj := newDeleteFakeObjectStore(nil, "conv-abc/one.txt")

	stripped := false
	h := conversations.NewHandler(metaStrippingRepo(&stripped)).
		WithObjectStore(obj).
		WithAttachmentStore(att)

	rec := doDeleteAttachments(t, h, "1", "conv-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, nil)
	if !stripped {
		t.Fatal("conversation meta was not stripped")
	}
	att.mu.Lock()
	nBuckets := len(att.buckets)
	att.mu.Unlock()
	if nBuckets != 0 {
		t.Fatalf("delete path created %d bucket(s); it must be lookup-only", nBuckets)
	}
}

// Nil dependencies degrade to the historical metadata-only behaviour, exactly
// as writeAttachmentBytes degrades on the same two.
func TestDeleteAttachments_NilStoresDegradeToMetadataOnly(t *testing.T) {
	t.Run("nil object store", func(t *testing.T) {
		att := newFakeAttachmentStore()
		att.seedBucket(1, "chat-attachments", 7)
		att.seedObject(7, "conv-abc/one.txt")

		stripped := false
		h := conversations.NewHandler(metaStrippingRepo(&stripped)).WithAttachmentStore(att)

		rec := doDeleteAttachments(t, h, "1", "conv-abc")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !stripped {
			t.Fatal("conversation meta was not stripped")
		}
		assertStrings(t, "metadata rows", att.recordedKeys(), []string{"conv-abc/one.txt"})
	})

	t.Run("nil attachment store", func(t *testing.T) {
		obj := newDeleteFakeObjectStore(nil, "conv-abc/one.txt")

		stripped := false
		h := conversations.NewHandler(metaStrippingRepo(&stripped)).WithObjectStore(obj)

		rec := doDeleteAttachments(t, h, "1", "conv-abc")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if !stripped {
			t.Fatal("conversation meta was not stripped")
		}
		assertStrings(t, "deleted object keys", obj.deleted, nil)
	})
}

// A byte delete that fails must surface as a 500 and must NOT strip the meta:
// answering {"ok": true} over a still-stored attachment is the exact failure
// shape #599 is about, and dropping the meta would leave nothing naming it.
func TestDeleteAttachments_ByteDeleteFailureIs500AndKeepsMeta(t *testing.T) {
	t.Run("DeleteBatch errors", func(t *testing.T) {
		att := newFakeAttachmentStore()
		att.seedBucket(1, "chat-attachments", 7)
		att.seedObject(7, "conv-abc/one.txt")
		obj := newDeleteFakeObjectStore(nil, "conv-abc/one.txt")
		obj.batchErr = errors.New("backend unreachable")

		stripped := false
		h := conversations.NewHandler(metaStrippingRepo(&stripped)).
			WithObjectStore(obj).
			WithAttachmentStore(att)

		rec := doDeleteAttachments(t, h, "1", "conv-abc")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
		}
		if stripped {
			t.Fatal("conversation meta was stripped despite the byte delete failing")
		}
		assertStrings(t, "metadata rows", att.recordedKeys(), []string{"conv-abc/one.txt"})
	})

	t.Run("BatchResult reports a failed key", func(t *testing.T) {
		att := newFakeAttachmentStore()
		att.seedBucket(1, "chat-attachments", 7)
		att.seedObject(7, "conv-abc/one.txt")
		att.seedObject(7, "conv-abc/two.txt")
		obj := newDeleteFakeObjectStore(nil, "conv-abc/one.txt", "conv-abc/two.txt")
		obj.failKeys = map[string]bool{"conv-abc/two.txt": true}

		stripped := false
		h := conversations.NewHandler(metaStrippingRepo(&stripped)).
			WithObjectStore(obj).
			WithAttachmentStore(att)

		rec := doDeleteAttachments(t, h, "1", "conv-abc")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
		}
		if stripped {
			t.Fatal("conversation meta was stripped despite a failed object delete")
		}
		assertStrings(t, "metadata rows", att.recordedKeys(),
			[]string{"conv-abc/one.txt", "conv-abc/two.txt"})
	})
}

// A `%` or `_` in the conversation id is a LIKE WILDCARD, not a literal.
//
// ListAttachmentObjectKeys resolves to `key LIKE $prefix || '%'`
// (internal/db/queries/artifact_storage.sql:117). Passing `%` as the
// conversation id makes the prefix `%/`, which matches every key containing a
// slash — every conversation's attachments in the project, deleted by one
// request from a caller who only ever had permission to delete their own.
//
// The fake store below matches by plain Go prefix, so it CANNOT reproduce the
// over-match; that is exactly why this asserts the handler refuses the
// identifier rather than asserting on what the fake deleted. A test written
// against the fake's matching would pass with the guard removed.
func TestDeleteAttachments_RejectsLikeWildcardsInTheConversationID(t *testing.T) {
	// Percent-encoded, because a bare `%` in a path is an invalid escape that
	// net/http rejects before any handler sees it — `%25` is how a `%` actually
	// reaches the route parameter. `_` needs no encoding and arrives literally.
	for _, conversationID := range []string{"%25", "conv%25abc", "conv_abc", "conv%5Cabc"} {
		att := newFakeAttachmentStore()
		att.seedBucket(1, "chat-attachments", 7)
		att.seedObject(7, "conv-abc/mine.txt")
		att.seedObject(7, "conv-other/theirs.txt")
		obj := newDeleteFakeObjectStore(nil, "conv-abc/mine.txt", "conv-other/theirs.txt")

		stripped := false
		h := conversations.NewHandler(metaStrippingRepo(&stripped)).
			WithObjectStore(obj).
			WithAttachmentStore(att)

		rec := doDeleteAttachments(t, h, "1", conversationID)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("conversation id %q: expected 400, got %d: %s", conversationID, rec.Code, rec.Body.String())
		}
		if len(obj.deleted) != 0 {
			t.Errorf("conversation id %q deleted %v", conversationID, obj.deleted)
		}
		// A refused delete must not strip the meta either: the files are still
		// there, and the meta is the only thing naming them.
		if stripped {
			t.Errorf("conversation id %q stripped the conversation meta", conversationID)
		}
	}
}

// --- `delete_attachment` on a message delete (#606 option 1) ---------------

func doDeleteMessage(t *testing.T, h *conversations.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, target, nil)
	rec := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rec, req)
	return rec
}

func deleteMessageHandler(att *fakeAttachmentStore, obj *deleteFakeObjectStore, refs []conversations.AttachmentRef) *conversations.Handler {
	repo := &mockRepo{deleteMessageResult: []string{"answer"}, deleteMessageAttachments: refs}
	return conversations.NewHandler(repo).WithObjectStore(obj).WithAttachmentStore(att)
}

// Without the flag the bytes stay. The legacy runtime makes this opt-in too — a
// delete that silently destroyed uploaded files would be a bigger surprise than
// one that leaves them for the retention sweeper.
func TestDeleteMessage_LeavesAttachmentBytesWithoutTheFlag(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv/report.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv/report.pdf")
	h := deleteMessageHandler(att, obj, []conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv/report.pdf"}})

	rec := doDeleteMessage(t, h, "/projects/1/conversations/c1/messages/answer")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, nil)
	assertStrings(t, "surviving objects", obj.remaining(), []string{"conv/report.pdf"})
}

// With the flag the bytes and their storage records go.
func TestDeleteMessage_DeletesAttachmentBytesWithTheFlag(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv/report.pdf")
	att.seedObject(7, "conv/keep.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv/report.pdf", "conv/keep.pdf")
	h := deleteMessageHandler(att, obj, []conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv/report.pdf"}})

	rec := doDeleteMessage(t, h, "/projects/1/conversations/c1/messages/answer?delete_attachment")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv/report.pdf"})
	assertStrings(t, "surviving objects", obj.remaining(), []string{"conv/keep.pdf"})
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"conv/keep.pdf"})
}

// PRESENCE, not value. The legacy runtime tests `'delete_attachment' in
// request.args`, so a bare flag counts and so does `=false`. A legacy client
// that has been sending the bare form for years must keep working, which a
// `== "true"` check would silently break.
func TestDeleteMessage_AttachmentFlagIsPresenceNotValue(t *testing.T) {
	for _, query := range []string{"?delete_attachment", "?delete_attachment=", "?delete_attachment=false", "?delete_attachment=1"} {
		att := newFakeAttachmentStore()
		att.seedBucket(1, "chat-attachments", 7)
		att.seedObject(7, "conv/report.pdf")
		obj := newDeleteFakeObjectStore(nil, "conv/report.pdf")
		h := deleteMessageHandler(att, obj, []conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv/report.pdf"}})

		rec := doDeleteMessage(t, h, "/projects/1/conversations/c1/messages/answer"+query)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", query, rec.Code)
		}
		if len(obj.deleted) != 1 {
			t.Errorf("%s: deleted %v, want the one attachment", query, obj.deleted)
		}
	}
}

// A byte-delete failure surfaces. The message rows are already gone by this
// point — the repository committed before returning — so reporting 200 would
// claim a cleanup that did not happen, and the caller would never retry.
func TestDeleteMessage_AttachmentByteFailureIs500(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv/report.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv/report.pdf")
	obj.batchErr = errors.New("object store down")
	h := deleteMessageHandler(att, obj, []conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv/report.pdf"}})

	rec := doDeleteMessage(t, h, "/projects/1/conversations/c1/messages/answer?delete_attachment")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"conv/report.pdf"})
}

// Attachments can name different buckets, so the byte delete groups by bucket
// rather than assuming one. Collapse that and a second bucket's objects are
// addressed against the wrong bucket.
func TestDeleteMessage_DeletesAcrossSeveralBuckets(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedBucket(1, "legacy-attachments", 8)
	att.seedObject(7, "conv/new.pdf")
	att.seedObject(8, "conv/old.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv/new.pdf", "conv/old.pdf")
	h := deleteMessageHandler(att, obj, []conversations.AttachmentRef{
		{Bucket: "chat-attachments", Name: "conv/new.pdf"},
		{Bucket: "legacy-attachments", Name: "conv/old.pdf"},
	})

	rec := doDeleteMessage(t, h, "/projects/1/conversations/c1/messages/answer?delete_attachment")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv/new.pdf", "conv/old.pdf"})
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), nil)
}

// heldRepo is a repository that reports an active legal hold.
type heldRepo struct{ *mockRepo }

func (heldRepo) ConversationHeld(context.Context, string, string) (bool, error) { return true, nil }

// A legal hold (ADR-0042 decision 4) keeps a conversation's files: the
// conversation-wide delete answers 409 BEFORE any byte is removed, because the
// bytes go first and the repository's own refusal would come too late.
func TestDeleteAttachments_LegalHoldRefusesBeforeBytes(t *testing.T) {
	var ops []string
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/one.txt")
	obj := newDeleteFakeObjectStore(&ops, "conv-abc/one.txt")

	stripped := false
	h := conversations.NewHandler(heldRepo{metaStrippingRepo(&stripped)}).
		WithObjectStore(obj).
		WithAttachmentStore(&opsLoggingAttachmentStore{fakeAttachmentStore: att, ops: &ops})

	rec := doDeleteAttachments(t, h, "1", "conv-abc")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(obj.deleted) != 0 || len(ops) != 0 || stripped {
		t.Fatalf("a held conversation lost attachments: deleted %v ops %v stripped %v", obj.deleted, ops, stripped)
	}
	assertStrings(t, "remaining metadata rows", att.recordedKeys(), []string{"conv-abc/one.txt"})
}
