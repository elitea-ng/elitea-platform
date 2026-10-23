package conversations_test

// Issue 1062: per-file attachment DELETE and the upload-time extraction
// warm-up.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// referencingRepo answers the in-use check the per-file delete makes.
type referencingRepo struct {
	*mockRepo
	referenced map[string]bool // "bucket/name"
	err        error
	asked      []string
}

func (r *referencingRepo) AttachmentReferenced(_ context.Context, projectID, bucket, name string) (bool, error) {
	r.asked = append(r.asked, projectID+":"+bucket+"/"+name)
	return r.referenced[bucket+"/"+name], r.err
}

const deleteConversationUUID = "8d5b6f0e-8f62-4c1f-9a51-0c7b5e8f7a10"

func newAttachmentDeleteFixture(t *testing.T, repo conversations.Repository, keys ...string) (http.Handler, *fakeAttachmentStore, *deleteFakeObjectStore, *[]string) {
	t.Helper()
	ops := &[]string{}
	attStore := newFakeAttachmentStore()
	attStore.seedBucket(1, storage.DefaultAttachmentBucket, 7)
	for _, key := range keys {
		attStore.seedObject(7, key)
	}
	objStore := newDeleteFakeObjectStore(ops, keys...)
	h := conversations.NewHandler(repo).
		WithObjectStore(objStore).
		WithAttachmentStore(&opsLoggingAttachmentStore{fakeAttachmentStore: attStore, ops: ops})
	router := chi.NewRouter()
	router.Delete("/projects/{projectID}/conversations/{conversationID}/attachments/{name}", h.DeleteAttachment)
	return router, attStore, objStore, ops
}

func deleteAttachmentRequest(router http.Handler, conversationID, name string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		"/projects/1/conversations/"+conversationID+"/attachments/"+name, nil))
	return rec
}

func TestDeleteAttachmentRemovesOneFileBytesFirst(t *testing.T) {
	keep := deleteConversationUUID + "/keep.txt"
	drop := deleteConversationUUID + "/drop.txt"
	repo := &referencingRepo{mockRepo: &mockRepo{}}
	router, attStore, objStore, ops := newAttachmentDeleteFixture(t, repo, keep, drop)

	rec := deleteAttachmentRequest(router, deleteConversationUUID, "drop.txt")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := objStore.remaining(); !slices.Equal(got, []string{keep}) {
		t.Fatalf("remaining bytes = %v, want only %q", got, keep)
	}
	if got := attStore.recordedKeys(); !slices.Equal(got, []string{keep}) {
		t.Fatalf("remaining rows = %v, want only %q", got, keep)
	}
	if !slices.Equal(*ops, []string{"bytes", "rows"}) {
		t.Fatalf("ops = %v, want bytes before rows", *ops)
	}
	if !slices.Equal(repo.asked, []string{"1:" + storage.DefaultAttachmentBucket + "/" + drop}) {
		t.Fatalf("in-use check = %v", repo.asked)
	}
}

func TestDeleteAttachmentRefusesAFileAMessageCarries(t *testing.T) {
	key := deleteConversationUUID + "/sent.pdf"
	repo := &referencingRepo{mockRepo: &mockRepo{}, referenced: map[string]bool{storage.DefaultAttachmentBucket + "/" + key: true}}
	router, attStore, objStore, ops := newAttachmentDeleteFixture(t, repo, key)

	rec := deleteAttachmentRequest(router, deleteConversationUUID, "sent.pdf")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"error":"attachment_in_use"`) {
		t.Fatalf("status = %d body=%s, want 409 attachment_in_use", rec.Code, rec.Body.String())
	}
	if len(*ops) != 0 || len(objStore.remaining()) != 1 || len(attStore.recordedKeys()) != 1 {
		t.Fatalf("a refused delete touched storage: ops=%v", *ops)
	}
}

func TestDeleteAttachmentAnswers404ForAFileItDidNotStore(t *testing.T) {
	repo := &referencingRepo{mockRepo: &mockRepo{}}
	router, _, _, ops := newAttachmentDeleteFixture(t, repo, deleteConversationUUID+"/other.txt")

	rec := deleteAttachmentRequest(router, deleteConversationUUID, "missing.txt")
	if rec.Code != http.StatusNotFound || len(*ops) != 0 || len(repo.asked) != 0 {
		t.Fatalf("status = %d ops=%v asked=%v, want 404 and nothing touched", rec.Code, *ops, repo.asked)
	}
}

func TestDeleteAttachmentRefusesBadAddresses(t *testing.T) {
	repo := &referencingRepo{mockRepo: &mockRepo{}}
	router, _, _, _ := newAttachmentDeleteFixture(t, repo)
	for _, tc := range []struct{ conversation, name string }{
		{"42", "a.txt"},                    // a numeric id
		{deleteConversationUUID, "..%2Fx"}, // climbs out of the conversation
		{deleteConversationUUID, "a%5Cb"},  // a backslash
	} {
		if rec := deleteAttachmentRequest(router, tc.conversation, tc.name); rec.Code != http.StatusBadRequest {
			t.Errorf("%s/%s: status = %d, want 400", tc.conversation, tc.name, rec.Code)
		}
	}
}

func TestDeleteAttachmentHidesAConversationTheCallerCannotSee(t *testing.T) {
	key := deleteConversationUUID + "/x.txt"
	repo := &referencingRepo{mockRepo: &mockRepo{authorizeFn: func(context.Context, string, string) error {
		return apierr.NotFound("conversation not found")
	}}}
	router, _, objStore, _ := newAttachmentDeleteFixture(t, repo, key)
	if rec := deleteAttachmentRequest(router, deleteConversationUUID, "x.txt"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(objStore.remaining()) != 1 {
		t.Fatal("an invisible conversation's file was deleted")
	}
}

func TestDeleteAttachmentWithoutAReferenceReaderIsNotImplemented(t *testing.T) {
	router, _, _, _ := newAttachmentDeleteFixture(t, &mockRepo{}, deleteConversationUUID+"/x.txt")
	if rec := deleteAttachmentRequest(router, deleteConversationUUID, "x.txt"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: an unchecked delete could remove a file a message carries", rec.Code)
	}
}

func TestDeleteAttachmentReportsAFailedReferenceCheck(t *testing.T) {
	repo := &referencingRepo{mockRepo: &mockRepo{}, err: errors.New("db down")}
	router, _, objStore, _ := newAttachmentDeleteFixture(t, repo, deleteConversationUUID+"/x.txt")
	if rec := deleteAttachmentRequest(router, deleteConversationUUID, "x.txt"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(objStore.remaining()) != 1 {
		t.Fatal("a failed check deleted the file")
	}
}

// --- the upload-time warm-up ---------------------------------------------

type recordingWarmer struct {
	mu    sync.Mutex
	calls []string
}

func (w *recordingWarmer) WarmAttachmentExtraction(projectID int64, bucket, name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, bucket+"/"+name)
	_ = projectID
}

func TestUploadWarmsTheExtractionOfADocumentOnly(t *testing.T) {
	warmer := &recordingWarmer{}
	attStore := newFakeAttachmentStore()
	objStore := newFakeAttachmentObjectStore()
	h := newAttachmentTestHandler(t, attStore, objStore).WithAttachmentExtractionWarmer(warmer)

	for _, name := range []string{"report.pdf", "photo.png", "report.pdf"} {
		if rec := doAttachmentUpload(t, h, nil, name, []byte("x")); rec.Code != http.StatusCreated {
			t.Fatalf("%s: status = %d", name, rec.Code)
		}
	}
	want := []string{"chat-attachments/conv-abc/report.pdf", "chat-attachments/conv-abc/report (1).pdf"}
	if !slices.Equal(warmer.calls, want) {
		t.Fatalf("warmed = %v, want %v (the stored keys of the documents, no image)", warmer.calls, want)
	}
}

func TestUploadSucceedsWithoutAWarmerAndIgnoresATypedNil(t *testing.T) {
	var typedNil *recordingWarmer
	h := newAttachmentTestHandler(t, newFakeAttachmentStore(), newFakeAttachmentObjectStore()).
		WithAttachmentExtractionWarmer(typedNil)
	if rec := doAttachmentUpload(t, h, nil, "report.pdf", []byte("x")); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}
