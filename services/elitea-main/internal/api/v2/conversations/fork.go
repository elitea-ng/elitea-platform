package conversations

// Conversation fork (client contract 1.11, forkConversation):
// POST /conversation_fork/prompt_lib/{projectID}/{conversationID}.
//
// The repository copies the rows in one transaction (ConversationForker).
// This file owns what the database cannot do: the attachment BYTES. An
// attachment is stored under `{conversationUUID}/{file}` and the download
// route derives that key from the conversation it authorized
// (attachment_download.go), so the fork cannot read the source's objects.
// The repository rewrites the copied attachment names to the new
// conversation's UUID and calls CopyAttachments with the objects to copy,
// before its transaction begins (and, for an object a racing reply added,
// once more inside it). A copy failure rolls the fork back, and the bytes
// copied so far are deleted again.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// ForkedFromMetaKey is the server-owned `chat_conversations.meta` key that
// names the source of a fork: {conversation_id, conversation_uuid,
// message_id, message_uuid, mode, cut_message_uuid, fork_id,
// branch_summary} (fork_rewind.go has the last four). Create drops a client-supplied value and Update
// keeps the stored one (PreserveForkedFrom).
const ForkedFromMetaKey = "forked_from"

// ForkNameSuffix is added to the source name when the request names no fork.
const ForkNameSuffix = " (fork)"

// DefaultConversationName stands in for a blank source name in a fork's
// default name (the legacy `conversations.py` default).
const DefaultConversationName = "New conversation"

// ForkedMessagesKey is the forkConversation answer's map of the copied
// message groups: source UUID to the copy's UUID. Only that answer has it.
const ForkedMessagesKey = "forked_messages"

// ErrForkPointUnsettled: the fork point is still streaming, or it waits for
// a tool approval. Answered 409 `fork_point_unsettled`.
var ErrForkPointUnsettled = errors.New("conversations: fork point is not settled")

// ErrForkNotSupported: the source cannot be forked (an editor Test
// conversation). Answered 409 `fork_not_supported`.
var ErrForkNotSupported = errors.New("conversations: conversation cannot be forked")

// AttachmentCopy is one stored attachment object the fork copies: the same
// bucket, from the source conversation's key to the fork's key.
type AttachmentCopy struct {
	Bucket string
	From   string
	To     string
}

// ForkRequest is one fork. NewUUID is the fork's conversation UUID, chosen
// before the transaction so the attachment keys can be named.
type ForkRequest struct {
	// MessageID is the fork point (a message group's numeric id or UUID);
	// empty forks the whole conversation.
	MessageID string
	// Name is the fork's name, already validated; empty takes the default.
	Name    string
	NewUUID string
	// CopyAttachments copies the attachment objects. It runs after the
	// access and fork point checks, before the repository's transaction
	// begins; it runs again inside it only for an object the transaction's
	// own plan names that the first one did not. Nil copies nothing.
	CopyAttachments func(ctx context.Context, copies []AttachmentCopy) error
	// Mode is ForkModeFork or ForkModeRewind (fork_rewind.go).
	Mode string
	// BranchSummary is a rewind's v1 record, already validated; nil on a
	// rewind has the repository build one from the abandoned turns.
	BranchSummary map[string]any
	// ForkID is the client's idempotency key (a UUID), or empty. A request
	// whose ForkID made a fork of this caller in this project before
	// answers that fork (Replayed); one made for another source, fork point
	// or mode is ErrForkIDReused.
	ForkID string
}

// ForkResult names the new conversation. MessageUUIDs maps each copied
// message group's source UUID to its copy's UUID.
type ForkResult struct {
	ID           int64
	UUID         string
	MessageUUIDs map[string]string
	// Replayed: the ForkID made this fork before; nothing was copied now
	// and MessageUUIDs is empty.
	Replayed bool
}

// ConversationForker is the optional repository half of the fork. Optional
// so test doubles of Repository keep compiling; the production repository
// implements it, and a composition without it answers 501.
//
// ForkConversation resolves the source under the chat authority's detail
// rule: a source the caller cannot see is apierr 404, as is a MessageID that
// is not a message of the source. It answers ErrForkPointUnsettled and
// ErrForkNotSupported as documented on them.
type ConversationForker interface {
	ForkConversation(ctx context.Context, projectID, conversationID string, request ForkRequest) (ForkResult, error)
	// ForkSourceName is the source conversation's name when the caller can
	// see it under the detail rule; found is false otherwise.
	ForkSourceName(ctx context.Context, projectID string, sourceID int64) (name string, found bool, err error)
}

// forkBodyLimit bounds the request body: short strings and a branch
// summary of at most 32 KiB, whose JSON escapes may take more.
const forkBodyLimit = 128 << 10

// Fork serves forkConversation.
func (h *Handler) Fork(w http.ResponseWriter, r *http.Request) {
	forker, ok := h.repo.(ConversationForker)
	pool, _ := h.pool.(*pgxpool.Pool)
	if !ok || pool == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "conversation fork is not supported by this composition"})
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, forkBodyLimit+1))
	if err != nil || len(raw) > forkBodyLimit {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	body := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil || body == nil {
			apierr.Write(w, apierr.BadRequest("the body must be a JSON object"))
			return
		}
	}
	request := ForkRequest{NewUUID: uuid.NewString()}
	if value, present := body["message_id"]; present && value != nil {
		messageID, ok := value.(string)
		if !ok {
			apierr.Write(w, apierr.BadRequest("message_id must be a string"))
			return
		}
		request.MessageID = messageID
		if messageID == "" {
			apierr.Write(w, apierr.NotFound("message not found"))
			return
		}
	}
	if request.Name, err = conversationNameFromBody(body); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := forkOptions(body, &request); err != nil {
		apierr.Write(w, err)
		return
	}

	var copied []AttachmentRef
	request.CopyAttachments = func(ctx context.Context, copies []AttachmentCopy) error {
		return h.copyForkAttachments(ctx, projectID, copies, &copied)
	}
	result, err := forker.ForkConversation(r.Context(), projectID, conversationID, request)
	if err != nil || result.Replayed {
		if len(copied) > 0 {
			// The fork was rolled back: its objects belong to nothing.
			_ = h.deleteAttachmentObjects(context.WithoutCancel(r.Context()), projectID, copied)
		}
		if err != nil {
			writeForkError(w, err)
			return
		}
	}

	row, err := conversationRow(r.Context(), pool, s, projectID, result.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	forkRowID := result.ID
	action := "conversation.fork"
	if request.Mode == ForkModeRewind {
		action = "conversation.rewind"
	}
	audit.Annotate(r.Context(), audit.Annotation{Action: action, EntityType: "conversation", EntityID: &forkRowID})
	if h.events != nil && !result.Replayed {
		payload := map[string]any{"conversation_id": strconv.FormatInt(result.ID, 10), "name": row["name"], "forked_from": row["forked_from"]}
		if created, ok := row["source"].(string); ok && created == LocalWorkSource {
			payload["source"] = LocalWorkSource
		}
		if actor, err := chatauthority.Actor(r.Context()); err == nil {
			payload["created_by"] = strconv.FormatInt(actor, 10)
		}
		h.events.Emit(r.Context(), projectID, "conversation.created", payload)
	}
	forked := result.MessageUUIDs
	if forked == nil {
		forked = map[string]string{}
	}
	row[ForkedMessagesKey] = forked
	writeJSON(w, http.StatusCreated, row)
}

func writeForkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrForkPointUnsettled):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     "fork_point_unsettled",
			"message":   "This message is still in progress. Wait for the reply to finish, then fork again.",
			"retryable": true,
		})
	case errors.Is(err, ErrThreadExecutorIsCloud):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     "thread_executor_is_cloud",
			"message":   "This thread is running in the cloud. Take it back to this computer, then fork it.",
			"retryable": false,
		})
	case errors.Is(err, ErrForkTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error":     "fork_too_large",
			"message":   "This conversation is too long to fork there. Fork from an earlier message.",
			"retryable": false,
		})
	case errors.Is(err, ErrForkIDReused):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     "fork_id_conflict",
			"message":   "This fork_id was used for another fork.",
			"retryable": false,
		})
	case errors.Is(err, ErrForkNotSupported):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":     "fork_not_supported",
			"message":   "This conversation cannot be forked.",
			"retryable": false,
		})
	default:
		apierr.Write(w, err)
	}
}

// copyForkAttachments copies each object, records its metadata row with a
// retention deadline that starts now (as an upload does), and appends what
// it stored to copied so a rollback can delete it. An object with no
// metadata row or no bytes is skipped: the source cannot serve it either.
func (h *Handler) copyForkAttachments(ctx context.Context, projectIDText string, copies []AttachmentCopy, copied *[]AttachmentRef) error {
	if h.store == nil || h.attachments == nil || len(copies) == 0 {
		return nil
	}
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		return apierr.BadRequest("invalid project id")
	}
	_, _, policyRetention, err := h.attachments.AttachmentPolicy(ctx, projectID)
	if err != nil {
		return apierr.Internal("get project storage policy")
	}
	retentionDays := attachmentRetentionDays()
	if policyRetention != nil {
		retentionDays = *policyRetention
	}
	expiresAt := time.Now().AddDate(0, 0, int(retentionDays))
	buckets := map[string]int64{}
	for _, item := range copies {
		info, err := h.attachments.AttachmentObject(ctx, projectID, item.Bucket, item.From)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return apierr.Internal("read attachment metadata")
		}
		from, err := storage.NewObjectRef(projectIDText, item.Bucket, item.From)
		if err != nil {
			continue
		}
		to, err := storage.NewObjectRef(projectIDText, item.Bucket, item.To)
		if err != nil {
			return apierr.Internal("name the copied attachment")
		}
		bucketID, known := buckets[item.Bucket]
		if !known {
			bucketID, err = h.attachments.LookupAttachmentBucket(ctx, projectID, item.Bucket)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return apierr.Internal("lookup attachment bucket")
			}
			buckets[item.Bucket] = bucketID
		}
		body, _, err := h.store.Get(ctx, from, nil)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return apierr.Internal("read attachment")
		}
		stored, err := h.store.Put(ctx, to, body, storage.PutOptions{ContentType: info.MediaType, ContentLength: info.ByteLength})
		_ = body.Close()
		if err != nil {
			return apierr.Internal("copy attachment")
		}
		key := stored.Key
		if key == "" {
			key = item.To
		}
		// The key the store wrote is the one the metadata names and a
		// rollback deletes.
		*copied = append(*copied, AttachmentRef{Bucket: item.Bucket, Name: key})
		if err := h.attachments.RecordAttachmentObject(ctx, bucketID, key, stored.Size, info.MediaType, &expiresAt); err != nil {
			return apierr.Internal("record attachment metadata")
		}
	}
	return nil
}

// conversationRow reads one conversation in the listConversations row
// shape. The caller has already authorized the read.
func conversationRow(ctx context.Context, pool *pgxpool.Pool, s, projectID string, id int64) (map[string]any, error) {
	rows, err := pool.Query(ctx, conversationSelectSQL(s, projectID, false)+` WHERE c.id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		row, _, _, err := scanConversationRow(rows, false)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(result) == 0 {
		return nil, apierr.NotFound("conversation not found")
	}
	annotateWorkspaceExecutors(ctx, pool, s, projectID, result)
	annotateArchivers(ctx, pool, result)
	reader, _ := chatauthority.Actor(ctx)
	if err := annotateActivity(ctx, pool, s, reader, result); err != nil {
		return nil, err
	}
	return result[0], nil
}

// forkedFrom reads `meta.forked_from` into its wire shape, or nil when the
// conversation is not a fork or the value is malformed. Only well-formed
// keys are kept, so the row never re-emits arbitrary client text here.
func forkedFrom(meta map[string]any) map[string]any {
	value, ok := meta[ForkedFromMetaKey].(map[string]any)
	if !ok {
		return nil
	}
	conversationID, ok := forkID(value["conversation_id"])
	if !ok {
		return nil
	}
	out := map[string]any{"conversation_id": conversationID}
	if text, ok := value["conversation_uuid"].(string); ok && validUUID(text) {
		out["conversation_uuid"] = text
	}
	if messageID, ok := forkID(value["message_id"]); ok {
		out["message_id"] = messageID
		if text, ok := value["message_uuid"].(string); ok && validUUID(text) {
			out["message_uuid"] = text
		}
	}
	forkMarkerOptions(value, out)
	return out
}

func forkID(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if v >= 1 && v <= 1<<53 && v == float64(int64(v)) {
			return int64(v), true
		}
	case int64:
		return v, v > 0
	case int:
		return int64(v), v > 0
	case json.Number:
		n, err := v.Int64()
		return n, err == nil && n > 0
	}
	return 0, false
}

func validUUID(text string) bool {
	_, err := uuid.Parse(text)
	return err == nil
}

// PreserveForkedFrom keeps the stored `meta.forked_from` through a meta
// write: the client PUTs the whole document, and the key is the server's.
func PreserveForkedFrom(current Conversation, update *Conversation) {
	if update.Meta == nil {
		return
	}
	stored, present := current.Meta[ForkedFromMetaKey]
	if _, supplied := update.Meta[ForkedFromMetaKey]; !present && !supplied {
		return
	}
	preserved := make(map[string]any, len(update.Meta)+1)
	for key, value := range update.Meta {
		preserved[key] = value
	}
	if present {
		preserved[ForkedFromMetaKey] = stored
	} else {
		delete(preserved, ForkedFromMetaKey)
	}
	update.Meta = preserved
}

// annotateForkSource adds the source name to a getConversation answer's
// `forked_from` when the caller can still see the source. A failure leaves
// the name out: the back-link is an enrichment.
func (h *Handler) annotateForkSource(ctx context.Context, projectID string, ff map[string]any) {
	forker, ok := h.repo.(ConversationForker)
	if !ok {
		return
	}
	id, _ := ff["conversation_id"].(int64)
	name, found, err := forker.ForkSourceName(ctx, projectID, id)
	if err == nil && found {
		ff["name"] = name
	}
}
