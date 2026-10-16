package supportassistant

// Attachment upload and read-back — issue #625 item 2.
//
// `MessageInput.tsx` (the vendored widget, apps/elitea-web/src/widgets/
// support-assistant/vendor/components/chat) removed the paperclip with a
// comment claiming this platform's agent-execution start contract "has no
// [attachments] field" — true when it was written, false now: #606 gave
// `CurrentApplicationStartRequest` an `Attachments []CurrentTurnAttachmentRef`
// field, and `StartCurrentApplication` (predict.go's own use case) already
// writes them onto the question group like any other current-agent turn. So
// the byte path this file adds is not a dead end: `Predict`'s `attachments`
// field (predict.go) reaches the SAME admission code the main chat composer's
// `payload.attachments` does, prefix-checked against the conversation UUID by
// `currentTurnAttachments` (internal/application/agentexecution/
// attachments.go) — support gets that check for free by calling the same
// function, not by re-deriving it.
//
// # No second store
//
// Both routes below are a thin authorization wrapper around the EXISTING
// handlers, not a second implementation of either:
//
//   - UploadAttachment delegates to `conversations.Handler.AddAttachments`
//     (internal/api/v2/conversations/attachments.go's S20a byte path): same
//     object store, same elitea_storage.buckets/objects metadata, same
//     bucket-policy resolution, same chunked-upload protocol.
//   - GetAttachment delegates to `artifacts.Handler.DownloadObject`
//     (internal/api/v2/artifacts/objects.go): the SAME generic object-read
//     route regular chat attachments are read back through today
//     (apps/elitea-web/src/entities/attachment/lib/download.ts's
//     `downloadAttachmentFromArtifact`, via `fetchArtifactBlob`).
//
// Neither delegate takes a `{projectID}` path segment from a support caller —
// every other route in this package makes the same choice, for the same
// reason (the hidden support project is resolved server-side, never named by
// the request). Both handlers below resolve it from `settingsFromContext` and
// write it into chi's OWN route context before delegating —
// `internal/api/v2/configurations/global_providers.go`'s `pinPublicProject`
// is the existing precedent for this exact rewrite, pinning ITS OWN hidden
// project (the platform-wide public project) the same way.
//
// GetAttachment's route pattern, `/attachments/{bucket}/*`, is deliberately
// shaped like `artifacts.Handler.DownloadObject`'s own
// (`/objects/{projectID}/{bucket}/*`): the client already has the exact
// `filepath` UploadAttachment answered with (`/{bucket}/{conversationUUID}/
// {name}`), splits it into bucket + key with the SAME
// `parseAttachmentFilepath` helper the main chat attachment cards use
// (apps/elitea-web/src/features/chat-messages/ui/attachments/
// attachmentDownload.helpers.ts), and the wildcard `*` therefore arrives here
// as EXACTLY the key `DownloadObject` expects — no rewrite needed for it,
// only for `projectID`.
import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// AttachmentUploader is `conversations.Handler.AddAttachments`, narrowed to an
// interface so this package need not import the conversations package for
// wiring beyond the Option below — the same reuse-not-reimplement shape
// `ChatStore` already uses for the conversation routes.
type AttachmentUploader interface {
	AddAttachments(w http.ResponseWriter, r *http.Request)
}

// AttachmentDownloader is `artifacts.Handler.DownloadObject`.
type AttachmentDownloader interface {
	DownloadObject(w http.ResponseWriter, r *http.Request)
}

// WithAttachmentUploader supplies the reused chat-attachment byte path.
// Without it UploadAttachment answers 503 rather than a nil-pointer panic.
func WithAttachmentUploader(uploader AttachmentUploader) Option {
	return func(h *Handler) { h.attachmentUploader = uploader }
}

// WithAttachmentDownloader supplies the reused artifact object-read path.
// Without it GetAttachment answers 503 rather than a nil-pointer panic.
func WithAttachmentDownloader(downloader AttachmentDownloader) Option {
	return func(h *Handler) { h.attachmentDownloader = downloader }
}

// pinProjectID rewrites the request so a delegated handler reads the resolved
// support project instead of the (absent) one a support caller could name.
//
// Mirrors `internal/api/v2/configurations/global_providers.go`'s
// `pinPublicProject` exactly, including WHY the rewrite goes through chi's
// route context rather than a fabricated new path: `chi.URLParam` is the one
// place `AddAttachments`/`DownloadObject` read a path parameter from, and it
// reads the route context, not the request's actual matched pattern.
func pinProjectID(w http.ResponseWriter, r *http.Request, projectID int64) bool {
	routeCtx := chi.RouteContext(r.Context())
	if routeCtx == nil {
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	routeCtx.URLParams.Add("projectID", strconv.FormatInt(projectID, 10))
	return true
}

// UploadAttachment stores one file for a support conversation.
//
// `{conversationID}` is deliberately the SAME parameter NAME
// `conversations.Handler.AddAttachments` itself reads via
// `chi.URLParam(r, "conversationID")` — its value has to be the conversation
// UUID, never the numeric row id (that handler refuses a numeric one
// outright), which is exactly the shape `{conversationUUID}` already has on
// every OTHER route in this package; the two names address the same value.
func (h *Handler) UploadAttachment(w http.ResponseWriter, r *http.Request) {
	if h.attachmentUploader == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "attachment storage is not configured")
		return
	}
	projectID, userID, ok := h.requestContext(w, r)
	if !ok {
		return
	}
	conversationUUID := chi.URLParam(r, "conversationID")
	if _, err := h.store.conversationOwnedByCaller(r.Context(), projectID, userID, conversationUUID); err != nil {
		h.writeConversationError(w, err, "resolve conversation for attachment upload")
		return
	}
	if !pinProjectID(w, r, projectID) {
		return
	}
	h.attachmentUploader.AddAttachments(w, r)
}

// GetAttachment serves back one previously uploaded file's bytes.
//
// The wildcard key's own first path segment must name a support conversation
// this caller owns — the SAME authorization `currentTurnAttachments` applies
// a second time, independently, when the file is later attached to a turn
// (see this file's package doc). Read-back is refused here even though a
// turn was never started, because the upload alone already put readable
// bytes in a bucket a same-project stranger could otherwise guess a key for.
func (h *Handler) GetAttachment(w http.ResponseWriter, r *http.Request) {
	if h.attachmentDownloader == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "attachment storage is not configured")
		return
	}
	projectID, userID, ok := h.requestContext(w, r)
	if !ok {
		return
	}
	key := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	conversationUUID, _, found := strings.Cut(key, "/")
	if !found || !validTurnUUID(conversationUUID) {
		// Not-found, not a 400: a malformed key names nothing this caller
		// could ever have uploaded, and the package's own rule (store.go's
		// conversationOwnedByCaller doc) is that "not yours" and "does not
		// exist" answer the same way.
		apierr.WriteStatus(w, http.StatusNotFound, "attachment not found")
		return
	}
	if _, err := h.store.conversationOwnedByCaller(r.Context(), projectID, userID, conversationUUID); err != nil {
		h.writeConversationError(w, err, "resolve conversation for attachment download")
		return
	}
	if !pinProjectID(w, r, projectID) {
		return
	}
	h.attachmentDownloader.DownloadObject(w, r)
}
