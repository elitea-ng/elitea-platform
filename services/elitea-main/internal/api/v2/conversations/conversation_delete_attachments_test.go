package conversations_test

// Attachment bytes on a CONVERSATION delete.
//
// DELETE /chat/prompt_lib/{projectID}/{conversationID} removed the whole
// message graph — participants, groups, items, the conversation row — and left
// every attachment's bytes in the object store, where they survived until the
// retention sweeper expired them
// (internal/runtimecomposition/artifact_retention_sweep.go). Unlike a message
// delete there is nothing left that could ever name those files again: the
// conversation, its groups, its items and their chat_messages_attachment rows
// all went in the same transaction.
//
// The ordering property these tests pin is the one Handler.DeleteMessage
// already keeps, and the opposite of the legacy one: the repository collects
// the refs INSIDE its transaction and commits, and only then are the bytes
// touched. A delete that refuses must destroy nothing.
//
// The fakes come from delete_attachments_test.go, so this suite runs
// unconditionally — no Postgres, no object-store infrastructure.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

func deleteConversationHandler(att *fakeAttachmentStore, obj *deleteFakeObjectStore, refs []conversations.AttachmentRef) *conversations.Handler {
	return conversations.NewHandler(&mockRepo{deleteConversationAttachments: refs}).
		WithObjectStore(obj).
		WithAttachmentStore(att)
}

func doDeleteConversation(t *testing.T, h *conversations.Handler, projectID, conversationID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/projects/%s/conversations/%s", projectID, conversationID), nil)
	rec := httptest.NewRecorder()
	newRouter(h).ServeHTTP(rec, req)
	return rec
}

// The OBJECT is gone — not merely "the request answered 204". Asserting the
// status alone is what let this gap sit unnoticed for as long as it did: the
// old handler answered 204 for every conversation whose files it left behind.
func TestDeleteConversation_DeletesAttachmentBytesThenRows(t *testing.T) {
	var ops []string
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/report.pdf")
	att.seedObject(7, "other-conv/keep.pdf")
	obj := newDeleteFakeObjectStore(&ops, "conv-abc/report.pdf", "other-conv/keep.pdf")

	h := conversations.NewHandler(&mockRepo{
		deleteConversationAttachments: []conversations.AttachmentRef{
			{Bucket: "chat-attachments", Name: "conv-abc/report.pdf"},
		},
	}).WithObjectStore(obj).
		WithAttachmentStore(&opsLoggingAttachmentStore{fakeAttachmentStore: att, ops: &ops})

	rec := doDeleteConversation(t, h, "1", "conv-abc")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv-abc/report.pdf"})
	assertStrings(t, "surviving objects", obj.remaining(), []string{"other-conv/keep.pdf"})
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"other-conv/keep.pdf"})
	assertStrings(t, "operation order", ops, []string{"bytes", "rows"})
}

// Attachments can name different buckets, so the byte delete groups by bucket
// rather than assuming one — the same property the message-delete path has.
func TestDeleteConversation_DeletesAcrossSeveralBuckets(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedBucket(1, "legacy-attachments", 8)
	att.seedObject(7, "conv-abc/new.pdf")
	att.seedObject(8, "conv-abc/old.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv-abc/new.pdf", "conv-abc/old.pdf")
	h := deleteConversationHandler(att, obj, []conversations.AttachmentRef{
		{Bucket: "chat-attachments", Name: "conv-abc/new.pdf"},
		{Bucket: "legacy-attachments", Name: "conv-abc/old.pdf"},
	})

	rec := doDeleteConversation(t, h, "1", "conv-abc")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv-abc/new.pdf", "conv-abc/old.pdf"})
	assertStrings(t, "surviving objects", obj.remaining(), nil)
}

// A MISSING elitea_storage.objects record is not an error. The row and the
// bytes are independent — an upload from before the byte path, or a row the
// legacy runtime wrote, has no record at all — so the bytes still go and the
// request still succeeds.
func TestDeleteConversation_MissingObjectRecordIsNotAnError(t *testing.T) {
	att := newFakeAttachmentStore() // no bucket seeded: LookupAttachmentBucket answers ErrNotFound
	obj := newDeleteFakeObjectStore(nil, "conv-abc/report.pdf")
	h := deleteConversationHandler(att, obj,
		[]conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv-abc/report.pdf"}})

	rec := doDeleteConversation(t, h, "1", "conv-abc")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, []string{"conv-abc/report.pdf"})
}

// A byte-delete failure surfaces as a 500. The conversation rows are already
// gone by this point, so a 204 would claim a cleanup that did not happen and
// the caller would never retry.
func TestDeleteConversation_AttachmentByteFailureIs500(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/report.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv-abc/report.pdf")
	obj.batchErr = errors.New("object store down")
	h := deleteConversationHandler(att, obj,
		[]conversations.AttachmentRef{{Bucket: "chat-attachments", Name: "conv-abc/report.pdf"}})

	rec := doDeleteConversation(t, h, "1", "conv-abc")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"conv-abc/report.pdf"})
}

// A REFUSED delete destroys nothing — the legacy ordering bug stated for the
// conversation route. The repository reports refs only after its own row
// delete has committed, so a 404 leaves every byte in place.
func TestDeleteConversation_RefusalLeavesTheBytes(t *testing.T) {
	att := newFakeAttachmentStore()
	att.seedBucket(1, "chat-attachments", 7)
	att.seedObject(7, "conv-abc/report.pdf")
	obj := newDeleteFakeObjectStore(nil, "conv-abc/report.pdf")
	repo := &mockRepo{
		deleteFn: func(context.Context, string, string) error { return errRepo },
		deleteConversationAttachments: []conversations.AttachmentRef{
			{Bucket: "chat-attachments", Name: "conv-abc/report.pdf"},
		},
	}
	h := conversations.NewHandler(repo).WithObjectStore(obj).WithAttachmentStore(att)

	rec := doDeleteConversation(t, h, "1", "conv-abc")

	if rec.Code == http.StatusNoContent {
		t.Fatalf("a refused delete answered 204: %s", rec.Body.String())
	}
	assertStrings(t, "deleted object keys", obj.deleted, nil)
	assertStrings(t, "surviving objects", obj.remaining(), []string{"conv-abc/report.pdf"})
	assertStrings(t, "surviving metadata rows", att.recordedKeys(), []string{"conv-abc/report.pdf"})
}
