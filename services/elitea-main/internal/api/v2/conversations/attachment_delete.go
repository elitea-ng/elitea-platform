package conversations

// Per-file attachment delete (client contract 1.13, deleteConversationAttachment):
// DELETE /attachments/prompt_lib/{projectID}/{conversationUUID}/{name}.
//
// The conversation-wide DELETE on the parent path removes every file the
// conversation stored. This one removes ONE: the file a user uploaded in the
// composer and then took back before sending. It is addressed exactly like
// the download (the conversation UUID and the last segment of `filepath`), is
// gated like the conversation-wide delete (`models.chat.attachments.delete`
// plus the chat authority's visibility check), and deletes the way
// deleteStoredAttachments does: bytes first, then the metadata row, so a
// failed byte delete leaves a row the caller can retry against.
//
// A FILE A MESSAGE CARRIES IS REFUSED WITH 409 attachment_in_use. Once a
// message was sent with the file, its attachment item names the object and
// the transcript, the download and every later turn read it from there.
// Deleting the bytes under it would leave a message that names a file
// nobody can open; the message delete (with ?delete_attachment) is the path
// that removes such a file together with what names it. Any attachment row
// in the project counts — a fork that shares the object names it too.
//
// The check and the delete are two statements, so a send that admits the
// file between them still names deleted bytes. The client deletes a file it
// has not sent, so the window is the client racing itself.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// AttachmentReferenceReader reports whether any message item in the project
// names the stored object (bucket, name). *repos.ConversationsRepo
// implements it.
type AttachmentReferenceReader interface {
	AttachmentReferenced(ctx context.Context, projectID, bucket, name string) (bool, error)
}

// DeleteAttachment deletes one stored attachment of a conversation.
func (h *Handler) DeleteAttachment(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	references, ok := h.repo.(AttachmentReferenceReader)
	if h.store == nil || h.attachments == nil || !ok {
		apierr.Write(w, apierr.NotImplemented("chat attachments are not stored on this deployment"))
		return
	}
	projectIDText := chi.URLParam(r, "projectID")
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}
	conversationID := chi.URLParam(r, "conversationID")
	if _, numeric := strconv.ParseUint(conversationID, 10, 64); numeric == nil {
		apierr.Write(w, apierr.BadRequest("conversation id must be the conversation uuid"))
		return
	}
	name, ok := attachmentDownloadName(chi.URLParam(r, "name"), r.URL.RawPath != "")
	if !ok {
		apierr.Write(w, apierr.BadRequest("invalid attachment name"))
		return
	}

	ctx := r.Context()
	bucketName, _, _, err := h.attachments.AttachmentPolicy(ctx, projectID)
	if err != nil {
		apierr.Write(w, apierr.Internal("get project storage policy: "+err.Error()))
		return
	}
	if bucketName == "" {
		bucketName = storage.DefaultAttachmentBucket
	}
	key := conversationID + "/" + name
	// The same three gates as the download: the project's own SYSTEM bucket
	// and a metadata row for exactly this key. Anything else is not a file
	// this route stored, so it is not this route's to delete.
	if _, err := h.attachments.AttachmentObject(ctx, projectID, bucketName, key); errors.Is(err, storage.ErrNotFound) {
		apierr.Write(w, apierr.NotFound("attachment not found"))
		return
	} else if err != nil {
		apierr.Write(w, apierr.Internal("read attachment metadata: "+err.Error()))
		return
	}
	inUse, err := references.AttachmentReferenced(ctx, projectIDText, bucketName, key)
	if err != nil {
		apierr.Write(w, apierr.Internal("read attachment references: "+err.Error()))
		return
	}
	if inUse {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "attachment_in_use",
			"message": "A message carries this file. Delete the message with its attachments instead.",
		})
		return
	}
	bucketID, err := h.attachments.LookupAttachmentBucket(ctx, projectID, bucketName)
	if errors.Is(err, storage.ErrNotFound) {
		apierr.Write(w, apierr.NotFound("attachment not found"))
		return
	}
	if err != nil {
		apierr.Write(w, apierr.Internal("lookup attachment bucket: "+err.Error()))
		return
	}
	ref, err := storage.NewObjectRef(projectIDText, bucketName, key)
	if err != nil {
		apierr.Write(w, apierr.BadRequest("invalid attachment name"))
		return
	}

	// Bytes BEFORE the row, for deleteStoredAttachments' reason: the row is
	// what this route, the conversation delete and the retention sweeper
	// find the bytes by.
	result, err := h.store.DeleteBatch(ctx, []storage.ObjectRef{ref})
	if err != nil {
		apierr.Write(w, apierr.Internal("delete attachment bytes: "+err.Error()))
		return
	}
	if len(result.Failed) > 0 {
		apierr.Write(w, apierr.Internal("delete attachment bytes: "+result.Failed[0].Err.Error()))
		return
	}
	if err := h.attachments.DeleteAttachmentObjects(ctx, bucketID, []string{key}); err != nil {
		apierr.Write(w, apierr.Internal("delete attachment metadata: "+err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
