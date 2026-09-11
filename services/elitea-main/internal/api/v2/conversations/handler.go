package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

type Conversation struct {
	InstructionsUpdate *string   `json:"-"`
	ID                 string    `json:"id"`
	UUID               string    `json:"uuid,omitempty"`
	ProjectID          string    `json:"project_id"`
	Name               string    `json:"name"`
	Description        string    `json:"description,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	CreatedBy          string    `json:"created_by"`
	MessageCount       int       `json:"message_count"`
	FolderID           *string   `json:"folder_id,omitempty"`
	// `chat_conversations.is_private` — the flag the rail's "Make public"
	// item writes.
	//
	// A POINTER because absent and false are different requests: the rename
	// path PUTs `name` alone and must not publish the conversation, while
	// "Make public" PUTs `is_private: false` and must. Read back on GET for
	// the same reason the menu needs it — the item is offered only while the
	// conversation is private, and a client that cannot read the flag cannot
	// stop offering it.
	IsPrivate *bool `json:"is_private,omitempty"`
	// The conversation's own settings document (`chat_conversations.meta`).
	//
	// Pylon's PUT accepted it and its GET returned it
	// (legacy/plugins/elitea_core/api/v2/conversation.py:123), and the web
	// client is written against that contract on both sides: the chat page
	// reads `detailsQuery.data.meta` (apps/elitea-web/src/pages/chat/
	// useChatPageData.ts:142) and the composer's Modules switch PUTs
	// `meta.internal_tools` (widgets/chat-box/ui/hooks/
	// useChatBoxInternalTools.ts:59-62). This server read neither, so every
	// module a user switched on answered 200 and stored nothing.
	//
	// A nil map is "the caller states no meta" — Update writes the column
	// only when the body carried the key, so a rename cannot silently blank
	// a conversation's settings.
	Meta         map[string]any `json:"meta,omitempty"`
	Source       string         `json:"source,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
	Participants []Participant  `json:"participants,omitempty"`
}

type Message struct {
	ID             string `json:"id"`
	UUID           string `json:"uid"`
	ConversationID string `json:"conversation_id"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	// The author's chat_participants row id, so a reloaded transcript can
	// attribute each question through the conversation's own participants
	// payload (the lookup `normaliseUserMessage` already performs on the
	// message-groups shape). Without it every reloaded user bubble was
	// anonymous — and, before the web dropped its reader-name fallback,
	// captioned with whoever happened to be LOOKING at the conversation.
	// Pointers with omitempty: a NULL column must read as "states no
	// author", never as an author the participants list cannot resolve.
	AuthorParticipantID *int           `json:"author_participant_id,omitempty"`
	SentToID            *int           `json:"sent_to_id,omitempty"`
	ReplyToID           *int           `json:"reply_to_id,omitempty"`
	ContentType         string         `json:"content_type,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	// The `attachment_message` items of this group, in the shape the
	// conversation-DETAILS route already embeds (`ListMessageGroups`): one
	// `{id, item_type, order_index, item_details}` entry per file, whose
	// item_details is `attachmentItemDetails`' seven keys.
	//
	// The chat page reads THIS route, not the details one
	// (apps/elitea-web/src/pages/chat/useChatPageData.ts hands these rows to
	// ChatBox as `message_groups`), so without the field a reloaded
	// conversation rendered the question and dropped the file that rode it —
	// everything #606 built on the read side had no producer on the page that
	// needed it.
	//
	// OMITTED, not `[]`, for a group with no files: `entities/message`'s
	// normaliser sets `messageItems` only when the key is present and its unit
	// tests pin that ("omits messageItems entirely when the wire does not send
	// message_items"), so an always-present empty array would make every
	// message claim an items list it does not have.
	MessageItems []map[string]any `json:"message_items,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
}

type ListResponse struct {
	Items      []Conversation `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	TotalPages int            `json:"total_pages"`
}

type MessagesListResponse struct {
	Items      []Message `json:"items"`
	Total      int       `json:"total"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	TotalPages int       `json:"total_pages"`
}

// MessagesQuery is the transcript window one GET
// /messages/prompt_lib/{projectID}/{conversationID} asked for, resolved from
// the query string before it reaches the repository.
//
// DEFECT #603: this route read `page`/`page_size` and nothing else, and no
// caller has ever sent either. The web client builds the query string as
// `{...params, limit, offset: page * pageSize}`
// (apps/elitea-web/src/entities/conversation/api/messageApi.ts:59-63); the SPA
// it was ported from does the same (frontends/EliteaUI/.../chat.api.js:24-32);
// and pylon, the contract both were written against, read `limit` (default 10),
// `offset` (default 0), `sort_by` (default created_at) and `sort_order`
// (default desc) — legacy/plugins/elitea_core/api/v2/messages.py:71-107. So
// every request collapsed onto this server's own defaults: page 1, size 50,
// created_at DESC. Scrolling back re-fetched the same newest 50 groups
// forever (useLoadMoreMessages.ts:96 sends offset=10,20,30…), and a caller
// that asked for `sort_order=asc` (useChatPageData.ts:66,
// usePlaybackConversation.ts:66) was served DESC.
//
// SortBy crosses this boundary UNVALIDATED on purpose: it names a column, so it
// is concatenated into SQL rather than bound, and the allow-list that decides
// what may be interpolated lives at that interpolation site
// (repos.ConversationsRepo.ListMessages) where it cannot be bypassed by a
// second caller of the interface.
type MessagesQuery struct {
	Limit     int
	Offset    int
	SortBy    string
	SortOrder string

	// Query is the free-text term a caller is searching the transcript for,
	// matched against the text content of a group's items. Empty means "no
	// filter" — NOT "match the empty string", which every group would satisfy.
	//
	// It is passed through raw. The repository decides what a match means and
	// escapes the term for the pattern operator it uses; doing that here would
	// bake one storage layer's pattern syntax into the HTTP boundary.
	Query string
}

// AttachmentRef locates one stored attachment: the bucket it lives in and the
// object name within it. Both come straight from the `chat_messages_attachment`
// row (#606), where `name` already carries the `{conversationUUID}/` prefix the
// upload path keys objects by — so this pair addresses the object directly,
// with no reconstruction.
type AttachmentRef struct {
	Bucket string
	Name   string
}

// DeleteMessageResult is what one message delete removed.
//
// Attachments are reported rather than deleted by the repository because the
// bytes do not live in the database: the repository removes the ROWS, and only
// the handler holds the object store. Reporting them is also what lets the
// byte delete happen strictly AFTER the authorisation and last-message guards
// have passed — see Handler.DeleteMessage for why that ordering is the point.
type DeleteMessageResult struct {
	// Deleted holds the UUIDs of every message group that went, newest first.
	Deleted []string
	// Attachments holds every stored attachment those groups carried. Empty
	// unless the groups had attachment items.
	Attachments []AttachmentRef
}

// MessageFeedback is one user's like/dislike + optional comment on a chat
// message (#880). `Rating` is `1` (like) or `-1` (dislike) — never `0`, which
// is not a state a stored row can hold (tenant/0135's CHECK constraint
// refuses it), only the absence of one (`MessageFeedbackSummary.Mine == nil`).
type MessageFeedback struct {
	Rating  int    `json:"rating"`
	Comment string `json:"comment,omitempty"`
}

// MessageFeedbackSummary is what every feedback endpoint answers: the
// aggregate like/dislike counts across every user who has rated this
// message, plus the CALLING user's own feedback — `nil` when they have not
// rated it. The client's hover tooltip reads the counts; the thumbs
// buttons' highlighted state reads `Mine`.
type MessageFeedbackSummary struct {
	Likes    int              `json:"likes"`
	Dislikes int              `json:"dislikes"`
	Mine     *MessageFeedback `json:"mine,omitempty"`
}

type Participant struct {
	ID             int            `json:"id"`
	EntityName     string         `json:"entity_name"`
	EntityMeta     map[string]any `json:"entity_meta"`
	EntitySettings map[string]any `json:"entity_settings"`
	Meta           map[string]any `json:"meta"`
}

type Repository interface {
	AuthorizeChatResource(ctx context.Context, projectID, resourceKind, resourceID string) error
	List(ctx context.Context, projectID string, page, pageSize int) (ListResponse, error)
	Get(ctx context.Context, projectID, conversationID string) (Conversation, error)
	Create(ctx context.Context, projectID string, conv Conversation) (Conversation, error)
	Update(ctx context.Context, projectID, conversationID string, conv Conversation) (Conversation, error)
	Delete(ctx context.Context, projectID, conversationID string) ([]AttachmentRef, error)
	ListMessages(ctx context.Context, projectID, conversationID string, query MessagesQuery) (MessagesListResponse, error)
	ListMessageGroups(ctx context.Context, projectID, conversationID string, limit int, sortOrder string) ([]map[string]any, error)
	ListParticipants(ctx context.Context, projectID, conversationID string) ([]Participant, error)
	AddParticipant(ctx context.Context, projectID, conversationID string, body map[string]any) error
	AddParticipants(ctx context.Context, projectID, conversationID string, bodies []map[string]any) error
	RemoveParticipant(ctx context.Context, projectID, conversationID, participantID string) error
	UpdateEntitySettings(ctx context.Context, projectID, conversationID, participantID string, settings map[string]any) error
	BatchUpdateEntitySettings(ctx context.Context, projectID, conversationID string, settings []map[string]any) error
	SelectConversation(ctx context.Context, projectID, conversationID, userID string) error
	DeselectConversation(ctx context.Context, projectID, userID string) error
	CreateCanvas(ctx context.Context, projectID string, body map[string]any) (map[string]any, error)
	GetCanvas(ctx context.Context, projectID, canvasID string) (map[string]any, error)
	UpdateCanvas(ctx context.Context, projectID, canvasID string, body map[string]any) error
	// ResolveCanvas proves a canvas id names a canvas INSIDE schema(projectID)
	// and returns its uuid plus its message group's uuid. No handler in THIS
	// package calls it: it is the cross-project control for the presence
	// heartbeat (internal/api/v2/canvaspresence, #622), which is registered
	// beside the canvas routes above and is handed this same repository.
	//
	// It is declared HERE rather than asserted from the concrete type at the
	// call site so the compiler is what proves the wiring. A type assertion
	// that quietly fails leaves a route that answers and does nothing, which is
	// the #126/#128 class this repository keeps re-finding.
	ResolveCanvas(ctx context.Context, projectID, canvasID string) (canvasUUID, messageGroupUUID string, err error)
	UpdateAttachmentStorage(ctx context.Context, projectID, conversationID string, body map[string]any) error
	AddAttachments(ctx context.Context, projectID, conversationID string, body map[string]any) error
	DeleteAttachments(ctx context.Context, projectID, conversationID string) error
	GetContextState(ctx context.Context, projectID, conversationID string) (contextsettings.ConversationState, error)
	UpdateContextStrategy(ctx context.Context, projectID, conversationID string, strategy contextsettings.Strategy) error
	GetMessageByUUID(ctx context.Context, projectID, messageUUID string) (map[string]any, error)
	DeleteMessages(ctx context.Context, projectID, conversationID string) error
	DeleteMessage(ctx context.Context, projectID, groupUID, userID string) (DeleteMessageResult, error)
	// GetMessageFeedback, SetMessageFeedback and DeleteMessageFeedback are
	// #880's like/dislike/comment control (tenant/0135_chat_message_feedback.sql).
	// SetMessageFeedback upserts: a second call from the same user on the
	// same message REPLACES their rating and comment rather than adding a
	// second row (the table's own UNIQUE(message_group_uuid, user_id)
	// constraint is what makes that safe under a concurrent write).
	GetMessageFeedback(ctx context.Context, projectID, messageUUID, userID string) (MessageFeedbackSummary, error)
	SetMessageFeedback(ctx context.Context, projectID, messageUUID, userID string, rating int, comment string) (MessageFeedbackSummary, error)
	DeleteMessageFeedback(ctx context.Context, projectID, messageUUID, userID string) (MessageFeedbackSummary, error)
	// ListMessageFeedbackBatch answers every message's summary in ONE query
	// rather than one per message — Export (export.go) is the caller, and a
	// transcript export walking up to exportMessageCap messages one feedback
	// read at a time would be an N+1 query the size of the conversation.
	// Messages with no feedback at all are simply absent from the map.
	ListMessageFeedbackBatch(ctx context.Context, projectID string, messageUUIDs []string, userID string) (map[string]MessageFeedbackSummary, error)
}

type Handler struct {
	contextGate     ContextManagementGate
	reasoningModels ReasoningModelReader
	repo            Repository
	pool            any
	store           storage.ObjectStore
	attachments     AttachmentStore
	userDefaults    UserContextDefaults
	events          EventEmitter
}

// EventEmitter is the seam to internal/events.Publisher — declared locally,
// like AttachmentStore and UserContextDefaults above, so this package (which
// the composition root imports) does not import internal/events and close a
// cycle. *events.Publisher already satisfies this structurally.
type EventEmitter interface {
	Emit(ctx context.Context, projectID, eventType string, payload any)
}

// WithEvents wires the conversation.created producer (#876's second half).
// Left nil, Create still works exactly as before — no webhook fires on a
// new conversation and no project SSE notice is sent for it, which is the
// same "degraded, not wrong" shape every other optional dependency here
// uses.
//
// Guarded by eventEmitterPresent: the composition root passes a
// *events.Publisher through this interface-typed parameter, and Go boxes a
// nil *events.Publisher into a NON-NIL EventEmitter — the #86 trap this
// service has met at four other composition roots — which would otherwise
// turn a deployment with no domain-events publisher into a panic on the
// first conversation Create.
func (h *Handler) WithEvents(emitter EventEmitter) *Handler {
	if eventEmitterPresent(emitter) {
		h.events = emitter
	}
	return h
}

// eventEmitterPresent reports whether emitter holds something usable — false
// for a nil interface AND for an interface boxing a nil pointer. Same check
// internal/api/v2/pipelinetriggers/job.go's present() performs, duplicated
// locally rather than shared to avoid a new cross-package dependency for one
// six-line function.
func eventEmitterPresent(emitter EventEmitter) bool {
	if emitter == nil {
		return false
	}
	v := reflect.ValueOf(emitter)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return !v.IsNil()
	default:
		return true
	}
}

// UserContextDefaults reads the caller's own context-management defaults —
// the middle tier of the resolution rule (conversation, then user, then the
// contract's constants).
//
// Declared here rather than imported from internal/infra/db/repos for the same
// reason AttachmentStore is: this package is imported BY the composition root,
// so depending on the repository package directly would close an import cycle.
type UserContextDefaults interface {
	ContextDefaults(ctx context.Context, userID int64) (contextsettings.UserDefaults, error)
}

func NewHandler(repo Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) WithPool(pool any) *Handler {
	h.pool = pool
	return h
}

// WithObjectStore wires the S20a chat-attachment byte path — see
// attachments.go. Left nil, AddAttachments falls back to its pre-S20a
// JSON-only behavior for a multipart request too (writeAttachmentBytes
// reports a 500 rather than silently accepting bytes it can't store).
func (h *Handler) WithObjectStore(store storage.ObjectStore) *Handler {
	h.store = store
	return h
}

// WithAttachmentStore wires S20a's Postgres metadata dependency — see
// attachments.go's AttachmentStore doc comment for why this is a
// locally-defined interface rather than a direct internal/infra/db/repos
// import (an import cycle).
func (h *Handler) WithAttachmentStore(store AttachmentStore) *Handler {
	h.attachments = store
	return h
}

// WithUserContextDefaults wires the middle tier of the context-strategy
// resolution rule.
//
// Left nil, the context routes still work and simply skip that tier: a
// conversation with a stored strategy is unaffected, and one without falls
// straight through to the contract's constants. That is a degraded answer, not
// a wrong one, which is why it is an option rather than a constructor
// argument — a composition with no database pool can build neither this nor
// the repository behind it.
func (h *Handler) WithUserContextDefaults(defaults UserContextDefaults) *Handler {
	h.userDefaults = defaults
	return h
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{conversationID}", h.Get)
	r.Put("/{conversationID}", h.Update)
	r.Delete("/{conversationID}", h.Delete)
	r.Get("/{conversationID}/messages", h.ListMessages)
	return r
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	// Run-history style: limit/offset with entity filtering
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	source := r.URL.Query().Get("source")
	entityName := r.URL.Query().Get("entity_name")
	entityMetaID := r.URL.Query().Get("entity_meta_id")

	if limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	pool, _ := h.pool.(*pgxpool.Pool)
	if pool == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "rows": []any{}})
		return
	}

	s, schemaOK := tenantSchema(w, projectID)
	if !schemaOK {
		return
	}
	ctx := r.Context()
	access, err := chatauthority.Load(ctx, pool, projectID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// Build the query with optional filtering by source & participant entity
	visibility, args := access.Predicate(s, "c", 1, chatauthority.Listing)
	baseWhere := "WHERE " + visibility
	argIdx := len(args) + 1

	if source != "" {
		baseWhere += fmt.Sprintf(" AND c.source = $%d", argIdx)
		args = append(args, source)
		argIdx++
	}

	// Filter by entity_meta_id + entity_name via conversation.meta->'single_participant'
	if entityMetaID != "" {
		baseWhere += fmt.Sprintf(" AND c.meta->'single_participant'->'entity_meta'->>'id' = $%d", argIdx)
		args = append(args, entityMetaID)
		argIdx++
	}
	if entityName != "" {
		baseWhere += fmt.Sprintf(" AND c.meta->'single_participant'->>'entity_name' = $%d", argIdx)
		args = append(args, entityName)
		argIdx++
	}

	// The optional mine filter further narrows actor-visible conversations.
	if r.URL.Query().Get("mine") == "true" {
		user, ok := auth.UserFromContext(ctx)
		callerID, hasCaller := int64(0), false
		if ok {
			callerID, hasCaller = user.OwningUserID()
		}
		if !hasCaller {
			writeJSON(w, http.StatusOK, map[string]any{"total": 0, "rows": []any{}})
			return
		}
		baseWhere += fmt.Sprintf(" AND c.author_id = $%d", argIdx)
		args = append(args, callerID)
		argIdx++
	}

	// Hidden conversations.
	//
	// The default is UNCHANGED — they are excluded, which is what keeps a
	// support transcript and a DeepWiki wiki chat out of a user's ordinary
	// chat list. `?hidden=only` asks for exactly the opposite set, and it
	// exists because a surface that OWNS hidden conversations has to be able
	// to list its own: the wiki chat drawer files its conversations hidden
	// precisely so they do not surface in the chat list, and then needs them
	// back, filtered by `source` and `entity_name` the way this route already
	// filters everything else.
	//
	// Two values and not a boolean: "only" is a different question from
	// "including", and answering the second by accident would put every
	// hidden conversation into the ordinary list. Anything else — including
	// the absent parameter every existing caller sends — takes the default.
	if r.URL.Query().Get("hidden") == "only" {
		baseWhere += " AND c.meta->>'is_hidden' = 'true'"
	} else {
		// Exclude hidden conversations
		baseWhere += " AND (c.meta->>'is_hidden' IS NULL OR c.meta->>'is_hidden' = 'false')"
	}

	// Count total
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_conversations c %s`, s, baseWhere)
	var total int
	if err := pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		apierr.Write(w, err)
		return
	}

	// Fetch conversations
	args = append(args, limit, offset)
	q := fmt.Sprintf(`
		SELECT c.id, c.name, c.created_at, COALESCE(c.updated_at, c.created_at), c.meta,
			(SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
		FROM %s.chat_conversations c
		%s
		ORDER BY c.created_at DESC, c.id DESC
		LIMIT $%d OFFSET $%d`, s, s, baseWhere, argIdx, argIdx+1)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	defer rows.Close()

	result := []map[string]any{}
	for rows.Next() {
		var id int
		var name string
		var createdAt, updatedAt time.Time
		var metaBytes []byte
		var msgCount int
		if err := rows.Scan(&id, &name, &createdAt, &updatedAt, &metaBytes, &msgCount); err != nil {
			apierr.Write(w, err)
			return
		}

		var meta map[string]any
		if metaBytes != nil {
			_ = json.Unmarshal(metaBytes, &meta) // internal DB column; nil map on error is handled below
		}
		if meta == nil {
			meta = map[string]any{}
		}

		row := map[string]any{
			"id":                   id,
			"name":                 name,
			"created_at":           createdAt.Format("2006-01-02T15:04:05.000000"),
			"updated_at":           updatedAt.Format("2006-01-02T15:04:05.000000"),
			"meta":                 meta,
			"duration":             -1,
			"message_groups_count": msgCount,
		}
		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": result})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	conv, err := h.repo.Get(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// Every downstream lookup keys off `conv.ID`, not the path segment: the
	// path may carry a UUID (see the repo's idPredicate), and participants
	// and message groups are joined on the numeric conversation id only.
	participants, err := h.repo.ListParticipants(r.Context(), projectID, conv.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if participants == nil {
		participants = []Participant{}
	}

	resp := map[string]any{
		"id":            conv.ID,
		"uuid":          conv.UUID,
		"project_id":    conv.ProjectID,
		"name":          conv.Name,
		"description":   conv.Description,
		"created_at":    conv.CreatedAt,
		"updated_at":    conv.UpdatedAt,
		"created_by":    conv.CreatedBy,
		"message_count": conv.MessageCount,
		"participants":  participants,
		"source":        conv.Source,
		"instructions":  conv.Instructions,
		// Null when the conversation sits outside every folder. Absent
		// entirely before #128, so a client could not tell which folder a
		// conversation belonged to from its own details response.
		"folder_id": conv.FolderID,
		// Always an object, never absent: the client reads
		// `meta.internal_tools` off this response to decide which module
		// switches are on, and an absent key is indistinguishable there from
		// a conversation whose modules are all off.
		"meta": metaOrEmpty(conv.Meta),
		// Always present, and defaulting to PRIVATE when the row cannot say:
		// the sidebar menu offers "Make public" while this is true, and an
		// absent key read as "public" would withdraw the control from every
		// conversation.
		"is_private": conv.IsPrivate == nil || *conv.IsPrivate,
	}

	// UI passes messages_limit to embed message_groups in the conversation response
	messagesLimit, _ := strconv.Atoi(r.URL.Query().Get("messages_limit"))
	if messagesLimit > 0 {
		sortOrder := r.URL.Query().Get("sort_order")
		groups, err := h.repo.ListMessageGroups(r.Context(), projectID, conv.ID, messagesLimit, sortOrder)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		if groups == nil {
			groups = []map[string]any{}
		}
		resp["message_groups"] = groups
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	var conv Conversation
	if name, ok := body["name"].(string); ok {
		conv.Name = name
	}
	// The create path carries settings too: the composer sends
	// `meta.steps_limit` with the very first send (apps/elitea-web/src/
	// widgets/chat-box/ui/hooks/useChatBoxSend.ts:310-313), and pylon stored
	// it (legacy/plugins/elitea_core/api/v2/conversations.py:153-164).
	if meta, ok := body["meta"].(map[string]any); ok {
		conv.Meta = meta
	}
	actorID, err := chatauthority.Actor(r.Context())
	if err != nil {
		apierr.Write(w, err)
		return
	}
	conv.CreatedBy = strconv.FormatInt(actorID, 10)
	if raw, present := body["is_private"]; present {
		value, ok := raw.(bool)
		if !ok {
			apierr.Write(w, apierr.BadRequest("is_private must be a boolean"))
			return
		}
		conv.IsPrivate = &value
	}
	conv.Source, _ = body["source"].(string)
	conv.Instructions, _ = body["instructions"].(string)
	if raw, present := body["participants"]; present {
		values, ok := raw.([]any)
		if !ok || len(values) > 100 {
			apierr.Write(w, apierr.BadRequest("participants must be an array with at most 100 items"))
			return
		}
		for _, raw := range values {
			value, ok := raw.(map[string]any)
			if !ok {
				apierr.Write(w, apierr.BadRequest("invalid participant"))
				return
			}
			defaultEntityProjectID(value, projectID)
			encoded, _ := json.Marshal(value)
			var participant Participant
			if err := json.Unmarshal(encoded, &participant); err != nil || participant.EntityName == "" || participant.EntityMeta == nil {
				apierr.Write(w, apierr.BadRequest("invalid participant"))
				return
			}
			if err := participant.Validate(); err != nil {
				apierr.Write(w, apierr.BadRequest(err.Error()))
				return
			}
			conv.Participants = append(conv.Participants, participant)
		}
	}

	if defaults, ok := h.userDefaults.(interface {
		Personalization(context.Context, int64) (map[string]any, error)
	}); ok {
		if personalization, err := defaults.Personalization(r.Context(), actorID); err == nil {
			applyCreatePersonalization(&conv, personalization)
		}
	}

	if err := h.applyCreateContextDefaults(r, projectID, &conv); err != nil {
		apierr.Write(w, apierr.Internal("read chat creation defaults"))
		return
	}
	created, err := h.repo.Create(r.Context(), projectID, conv)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if h.events != nil {
		h.events.Emit(r.Context(), projectID, "conversation.created", map[string]any{
			"conversation_id": created.ID,
			"name":            created.Name,
			"created_by":      created.CreatedBy,
		})
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	var conv Conversation
	if name, ok := body["name"].(string); ok {
		conv.Name = name
	}
	if folderID, exists := body["folder_id"]; exists {
		if folderID == nil {
			nullStr := ""
			conv.FolderID = &nullStr
		} else {
			fid := fmt.Sprintf("%v", folderID)
			conv.FolderID = &fid
		}
	}
	// Present-and-a-boolean only, for the reason `meta` is present-and-an-
	// object only: every other PUT this client makes (rename, move to folder,
	// settings) sends no `is_private`, and reading an absent key as `false`
	// would publish a conversation on every rename.
	//
	// This is the whole of "Make public". The route used to discard the key:
	// the menu item confirmed, the client patched its own list, the column
	// kept the `true` the INSERT gave it, and a reload put the conversation
	// back to private with nothing anywhere reporting a failure.
	if isPrivate, ok := body["is_private"].(bool); ok {
		conv.IsPrivate = &isPrivate
	}
	// Present-and-an-object only. `"meta": null` and a body with no `meta`
	// key both leave the stored document alone, so the rename path (which
	// sends `name` alone) cannot erase the conversation's settings.
	if meta, ok := body["meta"].(map[string]any); ok {
		conv.Meta = meta
	}

	if raw, present := body["instructions"]; present {
		value, ok := raw.(string)
		if !ok {
			apierr.Write(w, apierr.BadRequest("instructions must be a string"))
			return
		}
		conv.InstructionsUpdate = &value
	}
	updated, err := h.repo.Update(r.Context(), projectID, conversationID, conv)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// Delete removes a conversation and, with it, the stored bytes of every
// attachment its messages carried.
//
// The byte cleanup is NOT opt-in here, and that is the one place this route
// deliberately differs from DeleteMessage's `delete_attachment` flag. A message
// delete leaves the rest of the conversation standing, so keeping the files is
// a defensible choice a client can make; deleting the conversation removes
// every row that named them — the groups, the items, the
// chat_messages_attachment rows — so there is nothing left in the product that
// could ever show, download or delete those files again. Leaving them was not
// "keeping" them, it was leaking them until the retention sweeper
// (internal/runtimecomposition/artifact_retention_sweep.go) happened to expire
// them.
//
// The ORDER is the one Handler.DeleteMessage explains at length: the repository
// applies its guards, collects the refs inside its transaction and commits, and
// only then are the bytes touched. Pylon's opposite order destroys files for
// requests that go on to refuse; the cost of this one — rows gone, bytes not —
// is bounded, because the objects are still recorded in
// `elitea_storage.objects` and the sweeper still finds them.
//
// KNOWN RESIDUE, stated rather than implied: this collects the attachments
// reachable from message items. Bytes uploaded into a conversation that were
// never sent with a message have no item row, so they are not among the refs
// and still fall to the sweeper. `DeleteAttachments` (the explicit
// per-conversation attachment route) is the path that sweeps those by key
// prefix.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	attachments, err := h.repo.Delete(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	if len(attachments) > 0 {
		if err := h.deleteAttachmentObjects(r.Context(), projectID, attachments); err != nil {
			// 500, not 204. The conversation rows are already gone, so
			// answering success would claim a cleanup that did not happen and
			// nothing would ever retry it.
			apierr.Write(w, err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// NOTE(#126, #93): `PostMessage` stood here — a synchronous predict shim that
// resolved a model, called an LLM over plain HTTP itself, and INSERTed the
// resulting pair of message groups. It was deleted rather than routed, and
// `predict.go` (its only caller's only dependency) went with it.
//
// It was never mounted by any router, and the reason is that the route it
// implements is already served by something else. pylon declares the message
// POST at `/api/v2/elitea_core/messages/prompt_lib/<project_id>/
// <conversation_uuid>` under `models.chat.messages.create`
// (elitea_core/api/v2/messages.py:220-228,377-385). That is character for
// character `agentexecution.CurrentApplicationStartPath` with
// `CurrentApplicationStartPermission` — the live agent-execution start route
// #93 ported the dispatch onto. Mounting this shim at its own baseline URL
// would collide with that registration; mounting it anywhere else would invent
// a URL pylon never served.
//
// The two implementations were also not interchangeable. The runtime plane
// dispatches a real turn and streams it. The shim built its own HTTP request
// to the credential's api_base, so it bypassed the gateway outright — no
// budget enforcement, no governance, no request-log row — and where it could
// not resolve a model it fabricated a reply and stored it as the assistant's
// answer ("Message received", "Tool %s executed successfully"), falling back to
// a hardcoded `gpt-4o-mini` no deployment here serves.

const (
	// Pylon's default page of a transcript is 10 groups
	// (messages.py:73). The 50 this handler used instead was never observable
	// — no caller sends page_size — so restoring the legacy number changes what
	// a parameterless request returns, and nothing else.
	defaultMessagesLimit = 10

	// The cap the page_size branch already enforced, carried over onto `limit`.
	// Pylon has no cap at all, but the ceiling matters more here than parity
	// does: ListMessages runs one correlated string_agg subquery per group, so
	// an uncapped limit lets an anonymous query string decide how much work the
	// database does. 100 is not arbitrary either — it is exactly what the
	// largest real caller asks for (usePlaybackConversation.ts:66 requests
	// pageSize 100), so the cap binds nothing that exists today.
	maxMessagesLimit = 100
)

// parseMessagesQuery resolves the transcript window from the query string.
//
// PRECEDENCE, and why it is this way round (#603): `limit`/`offset` are the
// primary pair because they are the pair pylon defined and the pair every
// client sends. `page`/`page_size` are read FIRST and then overwritten, which
// makes limit/offset win whenever both are present, while leaving the
// page/page_size pair fully functional for a caller that sends only it — this
// server has advertised that pair for long enough that dropping it would be a
// second silent break, and the sibling List handler on the same resource takes
// limit/offset, so a client mixing the two is plausible.
//
// `page` is converted through the limit that is already resolved, so
// page=3&limit=25 means offset 50, not offset 50-derived-from-some-other-size.
func parseMessagesQuery(values url.Values) MessagesQuery {
	query := MessagesQuery{
		Limit:     defaultMessagesLimit,
		SortBy:    "created_at",
		SortOrder: "desc",
	}

	if pageSize, err := strconv.Atoi(values.Get("page_size")); err == nil && pageSize > 0 {
		query.Limit = pageSize
	}
	if limit, err := strconv.Atoi(values.Get("limit")); err == nil && limit > 0 {
		query.Limit = limit
	}
	if query.Limit > maxMessagesLimit {
		query.Limit = maxMessagesLimit
	}

	if page, err := strconv.Atoi(values.Get("page")); err == nil && page > 1 {
		query.Offset = (page - 1) * query.Limit
	}
	if offset, err := strconv.Atoi(values.Get("offset")); err == nil && offset > 0 {
		query.Offset = offset
	}

	if sortBy := values.Get("sort_by"); sortBy != "" {
		query.SortBy = sortBy
	}
	// Pylon read this as `request.args.get('query')` and applied it only when
	// truthy (messages.py:71,86), so an explicit `query=` was the same as
	// sending nothing. Same here.
	query.Query = values.Get("query")
	// Only the exact token `asc` flips the order. Pylon wrote this as
	// `desc if sort_order == 'desc' else asc`, so under pylon ANY unrecognised
	// value — a typo, an empty explicit `sort_order=` — silently reversed the
	// transcript. The documented default is desc; an unrecognised value gets
	// the documented default rather than the opposite of it.
	if values.Get("sort_order") == "asc" {
		query.SortOrder = "asc"
	}

	return query
}

func (h *Handler) ListMessages(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	resp, err := h.repo.ListMessages(r.Context(), projectID, conversationID, parseMessagesQuery(r.URL.Query()))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) DeleteMessages(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	if err := h.repo.DeleteMessages(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteMessage removes one message group and the user input it answers, and —
// when the caller asks for it — the bytes of any attachment those groups
// carried.
//
// The caller's identity is part of the request, not decoration: deleting a
// message is authorised against the conversation's author and the group's own
// author, so the repository cannot decide it without knowing who is asking.
// An unauthenticated context yields an empty id, which the repository refuses —
// the route already sits behind `models.chat.messages.delete`, so that state
// should be unreachable, and failing closed is right if it ever is not.
//
// WHY THIS ANSWERS 200 WITH A BODY RATHER THAN 204. One request can remove TWO
// groups — the reply named in the URL and the question it answers — and a
// client that prunes only the id it asked for would leave the other message on
// screen until a reload, which is worse than not pairing at all. Pylon told the
// client through a per-group socket event (message.py:154-171); there is no
// such channel on this route, so the response carries the fact instead. The
// body names every group that is really gone, newest first, so a client can
// prune exactly what the server removed rather than guessing at the pairing
// rule — the pairing lives in one place, on the server.
//
// # `delete_attachment`, and the pylon ordering bug NOT reproduced
//
// Pylon removes an attachment's stored bytes only when the request carries a
// `delete_attachment` query flag (message.py:103-107), and this keeps that
// opt-in: a delete that silently destroyed uploaded files would be a bigger
// surprise than one that leaves them for the retention sweeper.
//
// But pylon runs that loop BEFORE its "summarized message cannot be deleted"
// and "only the last message" guards (message.py:103 vs :108-122), so a request
// that goes on to answer 400 has ALREADY deleted the files. That is a defect,
// not a contract, and it is not ported. Here the repository applies every guard
// and commits first; the bytes are touched only once the delete is a fact.
//
// The cost of that ordering is the opposite failure: rows gone, bytes not. It
// is the cheaper one, and it is bounded — the objects are still recorded in
// `elitea_storage.objects`, so the retention sweeper still finds and expires
// them. Deleting the bytes first and refusing afterwards, pylon's order, is
// unrecoverable: the file is gone and the message still claims it.
func (h *Handler) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageID := chi.URLParam(r, "messageID")
	user, _ := auth.UserFromContext(r.Context())

	result, err := h.repo.DeleteMessage(r.Context(), projectID, messageID, user.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// Presence, not value — pylon tests `'delete_attachment' in request.args`,
	// so `?delete_attachment` with no value counts, and so does `=false`.
	// Matching that literally matters more than tidiness: a client that has
	// been sending the bare flag at pylon for years must keep working.
	if _, asked := r.URL.Query()["delete_attachment"]; asked && len(result.Attachments) > 0 {
		if err := h.deleteAttachmentObjects(r.Context(), projectID, result.Attachments); err != nil {
			apierr.Write(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": result.Deleted})
}

// deleteAttachmentObjects removes the stored bytes of attachments whose rows
// have already gone, and then their `elitea_storage.objects` records.
//
// Bytes before records, for the same reason DeleteAttachments does it that way:
// the record is what the retention sweeper walks, so dropping it first would
// orphan the bytes from the only thing that could ever find them again.
//
// A `chat_messages_attachment` row and an `elitea_storage.objects` row are
// independent — the first is the message's claim on a file, the second is the
// storage layer's record of it — so an attachment may legitimately have no
// object record here (an upload from before the S20a byte path, or a row pylon
// wrote). A missing record is not an error: the bytes are deleted by ref, and
// the record delete simply matches nothing.
func (h *Handler) deleteAttachmentObjects(ctx context.Context, projectIDStr string, attachments []AttachmentRef) error {
	if h.store == nil || h.attachments == nil {
		// Nothing could have been stored in a deployment with no object store;
		// same degradation writeAttachmentBytes and DeleteAttachments apply.
		return nil
	}
	projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
	if err != nil || projectID <= 0 {
		return apierr.BadRequest("invalid project id")
	}

	// Group by bucket: the object store addresses a bucket at a time, and an
	// attachment row carries its own bucket, so one message group's items are
	// not guaranteed to share one.
	byBucket := map[string][]string{}
	refs := map[string][]storage.ObjectRef{}
	for _, attachment := range attachments {
		if attachment.Bucket == "" || attachment.Name == "" {
			continue
		}
		ref, err := storage.NewObjectRef(projectIDStr, attachment.Bucket, attachment.Name)
		if err != nil {
			// SURFACED, not skipped. Skipping here reported 200 — "the
			// attachment was deleted" — for a file that is still stored, with
			// nothing anywhere recording that the cleanup was incomplete, which
			// is the one answer a delete must never give.
			//
			// Admission now refuses a name that cannot address an object
			// (application/agentexecution/attachments.go,
			// addressableObjectKey), so reaching this is a row written before
			// that check or by another writer — a real inconsistency worth a
			// 500 rather than a silent leak. The message rows are already gone
			// by this point; the caller learns the bytes are not.
			return apierr.Internal("stored attachment name " + strconv.Quote(attachment.Name) +
				" cannot address an object: " + err.Error())
		}
		byBucket[attachment.Bucket] = append(byBucket[attachment.Bucket], attachment.Name)
		refs[attachment.Bucket] = append(refs[attachment.Bucket], ref)
	}

	// Sorted, so the deletes happen in a defined order rather than whatever
	// order Go's map iteration produces. A caller reading the failure message
	// for "which object refused" gets the same answer twice for the same input.
	buckets := make([]string, 0, len(refs))
	for bucket := range refs {
		buckets = append(buckets, bucket)
	}
	sort.Strings(buckets)
	for _, bucket := range buckets {
		bucketRefs := refs[bucket]
		result, err := h.store.DeleteBatch(ctx, bucketRefs)
		if err != nil {
			return apierr.Internal("delete attachment bytes: " + err.Error())
		}
		if len(result.Failed) > 0 {
			return apierr.Internal(fmt.Sprintf("delete attachment bytes: %d of %d objects failed, first %q: %v",
				len(result.Failed), len(bucketRefs), result.Failed[0].Key, result.Failed[0].Err))
		}
		bucketID, err := h.attachments.LookupAttachmentBucket(ctx, projectID, bucket)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return apierr.Internal("lookup attachment bucket: " + err.Error())
		}
		if err := h.attachments.DeleteAttachmentObjects(ctx, bucketID, byBucket[bucket]); err != nil {
			return apierr.Internal("delete attachment metadata: " + err.Error())
		}
	}
	return nil
}

// GetMessage is the per-message read pylon declares as `message.py`'s `get`,
// alongside the `delete` DeleteMessage already serves — one module, one URL,
// two verbs (elitea_core/api/v2/message.py:176-183).
//
// The path param is `{messageID}`, shared with that DELETE sibling, and it
// carries pylon's `message_group_uid`: a message-GROUP uuid STRING, not the
// numeric row id the name suggests. The name is the sibling's because chi
// resolves one param per path segment and the two verbs share the segment.
//
// DEFECT this fixes: the method read `chi.URLParam(r, "messageUUID")`, a name
// no route has ever declared. Registering it under the baseline URL without
// this change would have handed the repository the empty string for every
// request, so a freshly mounted route would have answered 404 for every
// message that exists. Nothing caught it because nothing routed the method —
// it is the never-run half of the dead wiring, not a regression.
func (h *Handler) GetMessage(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageUUID := chi.URLParam(r, "messageID")
	msg, err := h.repo.GetMessageByUUID(r.Context(), projectID, messageUUID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// messageFeedbackCommentMaxLen bounds the optional comment a thumbs-down (or
// thumbs-up) can carry — matching the popover's own limit
// (features/chat-messages' MessageFeedbackPopover), so a body that passed the
// client's own check never bounces off the server's.
const messageFeedbackCommentMaxLen = 2000

// GetMessageFeedback answers the aggregate like/dislike counts for one
// message plus the caller's own rating, if any (#880). Read-only, so it sits
// behind the same permission GetMessage does: reading a message's feedback is
// not a wider claim than reading the message itself.
func (h *Handler) GetMessageFeedback(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageUUID := chi.URLParam(r, "messageID")
	user, _ := auth.UserFromContext(r.Context())
	summary, err := h.repo.GetMessageFeedback(r.Context(), projectID, messageUUID, user.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// SetMessageFeedback records the caller's like/dislike (+ optional comment)
// on one message, replacing whatever they had recorded before (#880).
//
// An unauthenticated caller is refused outright rather than silently
// recorded against an empty user id — the route sits behind
// `models.chat.messages.details`, so this should be unreachable, and failing
// closed is right if it ever is not (same reasoning DeleteMessage's own empty
// userID guard states).
func (h *Handler) SetMessageFeedback(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageUUID := chi.URLParam(r, "messageID")
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		apierr.Write(w, apierr.Unauthorized("authentication required"))
		return
	}

	var body struct {
		Rating  int    `json:"rating"`
		Comment string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if body.Rating != 1 && body.Rating != -1 {
		apierr.Write(w, apierr.BadRequest("rating must be 1 (like) or -1 (dislike)"))
		return
	}
	if len(body.Comment) > messageFeedbackCommentMaxLen {
		apierr.Write(w, apierr.BadRequest(fmt.Sprintf("comment exceeds %d characters", messageFeedbackCommentMaxLen)))
		return
	}

	summary, err := h.repo.SetMessageFeedback(r.Context(), projectID, messageUUID, user.ID, body.Rating, body.Comment)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// DeleteMessageFeedback retracts the caller's own like/dislike on one
// message (#880). Retracting is not "the same as never having voted" to
// anybody but the caller — the aggregate the response carries reflects the
// retraction immediately, same as SetMessageFeedback's response reflects the
// new vote.
func (h *Handler) DeleteMessageFeedback(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	messageUUID := chi.URLParam(r, "messageID")
	user, ok := auth.UserFromContext(r.Context())
	if !ok || user.ID == "" {
		apierr.Write(w, apierr.Unauthorized("authentication required"))
		return
	}

	summary, err := h.repo.DeleteMessageFeedback(r.Context(), projectID, messageUUID, user.ID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// defaultEntityProjectID fills entity_meta.project_id from the request path
// when the caller left it out. It changes nothing for a model participant,
// which carries no project id at all.
//
// The early return for `llm` and `dummy` matches what legacy STORES. legacy
// writes the project id into every request entry (participants.py:63-65), but
// pydantic then validates entity_meta against a per-type model.
// ParticipantEntityLlm keeps `model_name` only, and ParticipantEntityDummy
// keeps nothing (models/pd/participant.py:21-30). The extra key never reaches
// the INSERT.
//
// One difference remains. legacy validates a `user` entity against
// ParticipantEntityUser, which keeps `id` only, so the stored document holds
// no project id. This helper adds one. The identity key for a user is `id`
// alone, so the extra key splits no participant row.
func defaultEntityProjectID(body map[string]any, projectID string) {
	entityName, _ := body["entity_name"].(string)
	if entityName == "llm" || entityName == "dummy" {
		return
	}
	entityMeta, ok := body["entity_meta"].(map[string]any)
	if !ok {
		return
	}
	if existing, present := entityMeta["project_id"]; present && existing != nil {
		return
	}
	if numeric, err := strconv.Atoi(projectID); err == nil {
		entityMeta["project_id"] = numeric
	}
}

func (h *Handler) AddParticipant(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	var payload json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	var bodyList []map[string]any
	if err := json.Unmarshal(payload, &bodyList); err != nil {
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil || body == nil {
			apierr.Write(w, apierr.BadRequest("invalid request body"))
			return
		}
		bodyList = []map[string]any{body}
	}

	if len(bodyList) > 100 {
		apierr.Write(w, apierr.BadRequest("at most 100 participants are allowed"))
		return
	}
	for _, body := range bodyList {
		defaultEntityProjectID(body, projectID)
		raw, err := json.Marshal(body)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("invalid participant"))
			return
		}
		var participant Participant
		if err := json.Unmarshal(raw, &participant); err != nil {
			apierr.Write(w, apierr.BadRequest("invalid participant"))
			return
		}
		if err := participant.Validate(); err != nil {
			apierr.Write(w, apierr.BadRequest(err.Error()))
			return
		}
	}
	if err := h.repo.AddParticipants(r.Context(), projectID, conversationID, bodyList); err != nil {
		apierr.Write(w, err)
		return
	}

	// Return the participants list from DB
	participants, err := h.repo.ListParticipants(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, participants)
}

// GetParticipant reads only a participant mapped to an actor-visible conversation.
func (h *Handler) GetParticipant(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	participants, err := h.repo.ListParticipants(r.Context(), chi.URLParam(r, "projectID"), chi.URLParam(r, "conversationID"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	for _, participant := range participants {
		if strconv.Itoa(participant.ID) == chi.URLParam(r, "participantID") {
			writeJSON(w, http.StatusOK, participant)
			return
		}
	}
	apierr.Write(w, apierr.NotFound("participant not found"))
}

func (h *Handler) RemoveParticipant(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	participantID := chi.URLParam(r, "participantID")
	if err := h.repo.RemoveParticipant(r.Context(), projectID, conversationID, participantID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) UpdateEntitySettings(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	participantID := chi.URLParam(r, "participantID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	if err := normalizeParticipantSettings(body); err != nil {
		apierr.Write(w, apierr.BadRequest(err.Error()))
		return
	}
	var participant *Participant
	if pool, _ := h.pool.(*pgxpool.Pool); pool != nil {
		participants, err := h.repo.ListParticipants(r.Context(), projectID, conversationID)
		if err != nil {
			apierr.Write(w, err)
			return
		}
		for i := range participants {
			if strconv.Itoa(participants[i].ID) == participantID {
				participant = &participants[i]
				break
			}
		}
		if participant == nil {
			apierr.Write(w, apierr.NotFound("participant not found"))
			return
		}
	}
	emptyLLM := false
	if settings, ok := body["llm_settings"].(map[string]any); ok && len(settings) == 0 {
		emptyLLM = true
	}
	if raw, present := body["llm_settings"]; present && raw != nil && !emptyLLM {
		settings, err := normalizeParticipantLLM(raw, false)
		if err != nil {
			apierr.Write(w, apierr.BadRequest(err.Error()))
			return
		}
		if participant != nil && participant.EntityName == "application" && fmt.Sprint(participant.EntityMeta["project_id"]) != publicproject.IDString() {
			versionID := body["version_id"]
			if versionID == nil {
				versionID = body["id"]
			}
			pool, _ := h.pool.(*pgxpool.Pool)
			if h.llmSettingsDiffer(r.Context(), pool, projectID, versionID, settings) {
				apierr.Write(w, apierr.BadRequest("LLM settings override is only allowed for published agents from agent studio"))
				return
			}
			delete(body, "llm_settings")
		} else {
			if h.reasoningModels != nil && (settings["temperature"] != nil || settings["reasoning_effort"] == nil) {
				name, _ := settings["model_name"].(string)
				modelProject := projectID
				if value := settings["model_project_id"]; value != nil {
					modelProject = fmt.Sprint(value)
				}
				if modelProject != projectID && modelProject != publicproject.IDString() {
					apierr.Write(w, apierr.BadRequest("model project must be current or public"))
					return
				}
				if name != "" {
					if reasoning, err := h.reasoningModels.SupportsReasoning(r.Context(), modelProject, name); err == nil && reasoning {
						apierr.Write(w, apierr.BadRequest("a reasoning-capable model requires a reasoning_effort (low/medium/high) and no temperature"))
						return
					}
				}
			}
			body["llm_settings"] = settings
		}
	}

	if err := h.repo.UpdateEntitySettings(r.Context(), projectID, conversationID, participantID, body); err != nil {
		apierr.Write(w, err)
		return
	}
	if participant != nil {
		participant.EntitySettings = body
		writeJSON(w, http.StatusOK, participant)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entity_settings": body})
}

func (h *Handler) llmSettingsDiffer(ctx context.Context, pool *pgxpool.Pool, projectID string, versionID, llmSettings any) bool {
	incoming, err := normalizeParticipantLLM(llmSettings, false)
	if err != nil {
		return true
	}
	baseline := map[string]any{}
	if versionID != nil {
		vid, err := participantPositiveID(versionID)
		if err != nil {
			return true
		}
		s, err := tenantschema.Quote(projectID)
		if err != nil || pool == nil {
			return true
		}
		var raw []byte
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT llm_settings FROM %s.application_versions WHERE id=$1`, s), vid).Scan(&raw); err != nil {
			return true
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &baseline); err != nil {
				return true
			}
		}
	}
	if len(baseline) > 0 {
		baseline, err = normalizeParticipantLLM(baseline, true)
		if err != nil {
			return true
		}
	}
	// Both models inject chat_history_template=all. An absent baseline is
	// deliberately empty, matching the legacy private-agent restriction.
	return !reflect.DeepEqual(participantLLMComparable(incoming), participantLLMComparable(baseline))
}

func (h *Handler) BatchUpdateEntitySettings(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var body []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.repo.BatchUpdateEntitySettings(r.Context(), projectID, conversationID, body); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) SelectConversation(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	user, _ := auth.UserFromContext(r.Context())
	if err := h.repo.SelectConversation(r.Context(), projectID, conversationID, user.ID); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) DeselectConversation(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	user, _ := auth.UserFromContext(r.Context())
	if err := h.repo.DeselectConversation(r.Context(), projectID, user.ID); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) Regenerate(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) CreateCanvas(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.repo.AuthorizeChatResource(r.Context(), projectID, "message", fmt.Sprint(body["message_group_id"])); err != nil {
		apierr.Write(w, err)
		return
	}
	canvas, err := h.repo.CreateCanvas(r.Context(), projectID, body)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, canvas)
}

func (h *Handler) GetCanvas(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	canvasID := chi.URLParam(r, "canvasID")
	canvas, err := h.repo.GetCanvas(r.Context(), projectID, canvasID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, canvas)
}

func (h *Handler) UpdateCanvas(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	canvasID := chi.URLParam(r, "canvasID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.repo.UpdateCanvas(r.Context(), projectID, canvasID, body); err != nil {
		apierr.Write(w, err)
		return
	}
	// Answer the SAVED canvas, not `{"ok": true}`. The client normalises this
	// response into its canvas cache (`useEditCanvasMutation` invalidates the
	// details query with it), so an acknowledgement with no document left the
	// editor holding the text it sent rather than the text the server stored —
	// which is indistinguishable from a save that never happened.
	saved, err := h.repo.GetCanvas(r.Context(), projectID, canvasID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *Handler) UpdateAttachmentStorage(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.repo.UpdateAttachmentStorage(r.Context(), projectID, conversationID, body); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// AddAttachments serves two different resource types at the one legacy URL,
// exactly like legacy itself does (legacy/plugins/elitea_core/api/v2/
// attachments.py's PromptLibAPI.post handles both a plain multipart upload
// and a chunked one at the same route, distinguishing by form fields, not a
// separate path): a JSON body updates chat_conversations.meta (pre-existing,
// unchanged below); a multipart/form-data body is S20a's byte path,
// writeAttachmentBytes.
func (h *Handler) AddAttachments(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mediaType == "multipart/form-data" {
		h.writeAttachmentBytes(w, r)
		return
	}

	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	if err := h.repo.AddAttachments(r.Context(), projectID, conversationID, body); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DeleteAttachments removes a conversation's attachments: the stored bytes,
// their elitea_storage.objects metadata rows, and finally the
// chat_conversations.meta.attachments list that named them.
//
// DEFECT #599: this route used to strip the meta and nothing else. The
// uploaded bytes and their metadata rows survived until the retention
// sweeper eventually expired them, so "delete my attachments" forgot the
// attachments without deleting them. Pylon's equivalent route removes the
// bytes at the same moment (legacy/plugins/elitea_core/api/v2/
// attachments.py:240, `mc.remove_file(bucket_name, filename)`); this is that
// parity baseline.
func (h *Handler) DeleteAttachments(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	// Bytes first, meta last. If the byte delete fails we must NOT strip the
	// meta: the meta is the only thing left in the product that names those
	// files, so stripping it after a failed delete produces exactly the state
	// this defect is about — stored bytes nobody can see or retry deleting.
	if err := h.deleteStoredAttachments(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	if err := h.repo.DeleteAttachments(r.Context(), projectID, conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteStoredAttachments removes the object bytes and metadata rows
// finalizeAttachment wrote for one conversation, and resolves the bucket the
// same way finalizeAttachment does (policy first, defaultAttachmentBucketName
// fallback) so the delete reads the same bucket the upload wrote.
//
// A returned error is meant to reach the client as a 500 rather than being
// logged and swallowed: answering `{"ok": true}` for an attachment that is
// still stored is the precise failure shape this change exists to remove.
func (h *Handler) deleteStoredAttachments(ctx context.Context, projectIDStr, conversationID string) error {
	// No database or object store configured: degrade to the historical
	// metadata-only behaviour instead of failing the request, matching how
	// writeAttachmentBytes degrades on the same two nil dependencies (see
	// WithObjectStore/WithAttachmentStore). There are no bytes to delete in
	// a deployment that could never have stored any.
	if h.store == nil || h.attachments == nil {
		return nil
	}

	projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
	if err != nil || projectID <= 0 {
		return apierr.BadRequest("invalid project id")
	}

	bucketName, _, _, err := h.attachments.AttachmentPolicy(ctx, projectID)
	if err != nil {
		return apierr.Internal("get project storage policy: " + err.Error())
	}
	if bucketName == "" {
		bucketName = defaultAttachmentBucketName
	}

	// Lookup, never create — a delete that mints a bucket row as a side
	// effect is a worse outcome than the no-op it is standing in for. No
	// bucket means nothing was ever stored for this project, which is a
	// normal outcome here (meta-only attachments, or a conversation that
	// never had an upload), not an error: skip cleanup, still strip the meta.
	bucketID, err := h.attachments.LookupAttachmentBucket(ctx, projectID, bucketName)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return apierr.Internal("lookup attachment bucket: " + err.Error())
	}

	// REJECT LIKE METACHARACTERS IN THE PREFIX.
	//
	// ListAttachmentObjectKeys resolves to `key LIKE $prefix || '%'`
	// (internal/db/queries/artifact_storage.sql:117), so `%` and `_` in the
	// route parameter are WILDCARDS, not literals. A caller authorised for the
	// project — this route sits behind models.chat.attachments.delete — could
	// pass `%` as the conversation id and have the prefix become `%/`, which
	// matches every key containing a slash: every conversation's attachments in
	// the project, deleted in one request. `\` is rejected with them because it
	// is the escape character the pattern would otherwise consume.
	//
	// Rejecting rather than escaping, because no legitimate identifier contains
	// any of the three: finalizeAttachment builds the key from this same route
	// parameter, and the values that reach it are conversation UUIDs and
	// numeric ids. An escape would silently accept an identifier that cannot
	// name a real conversation and then quietly match nothing.
	if strings.ContainsAny(conversationID, `%_\`) {
		return apierr.BadRequest("invalid conversation id")
	}

	// The recorded metadata rows are the source of truth for what to delete,
	// NOT a listing of the object store. The bucket is shared by every
	// conversation in the project, and an object in it with no metadata row
	// was not written by finalizeAttachment — deriving the delete set from a
	// store listing would let this route reach bytes it never recorded.
	// finalizeAttachment keys every attachment `{conversationID}/{filename}`,
	// so that prefix selects exactly this conversation's own objects.
	keys, err := h.attachments.ListAttachmentObjectKeys(ctx, bucketID, conversationID+"/")
	if err != nil {
		return apierr.Internal("list attachment objects: " + err.Error())
	}
	if len(keys) == 0 {
		return nil
	}

	refs := make([]storage.ObjectRef, 0, len(keys))
	for _, key := range keys {
		ref, err := storage.NewObjectRef(projectIDStr, bucketName, key)
		if err != nil {
			return apierr.Internal("invalid stored attachment key " + strconv.Quote(key) + ": " + err.Error())
		}
		refs = append(refs, ref)
	}

	// Bytes BEFORE rows. If this fails, the rows must still name what is
	// stored — dropping the rows first would orphan the bytes with nothing
	// left pointing at them, and neither this route nor the retention sweeper
	// (which walks the same rows) could ever find them again.
	result, err := h.store.DeleteBatch(ctx, refs)
	if err != nil {
		return apierr.Internal("delete attachment bytes: " + err.Error())
	}
	if len(result.Failed) > 0 {
		return apierr.Internal(fmt.Sprintf("delete attachment bytes: %d of %d objects failed, first %q: %v",
			len(result.Failed), len(refs), result.Failed[0].Key, result.Failed[0].Err))
	}

	if err := h.attachments.DeleteAttachmentObjects(ctx, bucketID, keys); err != nil {
		return apierr.Internal("delete attachment metadata: " + err.Error())
	}
	return nil
}

// GetContextStatus serves pylon's ContextStatus for one conversation:
// `GET /elitea_core/context_analytics/prompt_lib/{projectID}/{conversationID}`.
//
// The `max_tokens` it reports is the RESOLVED strategy's, so a conversation
// that has never been configured reports the budget its owner's Settings >
// Memory defaults set, not a constant nobody chose.
//
// What it does NOT do is invent the token count. See
// contextsettings.AnalyticsUnavailableReason for what is refused and why.
func (h *Handler) GetContextStatus(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	state, err := h.repo.GetContextState(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	strategy := contextsettings.Resolve(state.Strategy, h.contextDefaults(r))
	writeJSON(w, http.StatusOK, contextsettings.BuildStatus(strategy, state.Analytics, state.MessageGroupsTotal))
}

// GetContextStrategy serves the resolved strategy itself, so a client can show
// what applies to a conversation before anything has been written to it.
//
// pylon had no such route - its UI only ever saw a strategy in the response to
// its own PUT, which meant a conversation nobody had configured could not be
// displayed without writing to it first. The response is the same document the
// PUT returns under `updated_strategy`.
func (h *Handler) GetContextStrategy(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	state, err := h.repo.GetContextState(r.Context(), projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contextsettings.Resolve(state.Strategy, h.contextDefaults(r)))
}

// UpdateContextStrategy is pylon's context_strategy PUT
// (legacy/plugins/elitea_core/api/v2/context_strategy.py), response shape
// included: the context status, plus `message` and `updated_strategy`.
//
// THE MERGE IS THE POINT. The body is a partial update; it is applied over the
// RESOLVED strategy (conversation, else user defaults, else constants) and the
// complete result is stored. The previous implementation wrote the raw body
// into `meta.context_strategy` wholesale, so a form that sent two fields
// erased the rest - including `summary_llm_settings`.
func (h *Handler) UpdateContextStrategy(w http.ResponseWriter, r *http.Request) {
	if !h.authorizeConversation(w, r) {
		return
	}
	projectID := chi.URLParam(r, "projectID")
	conversationID := chi.URLParam(r, "conversationID")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}
	update, fieldErr := contextsettings.DecodeStrategyUpdate(body)
	if fieldErr != nil {
		writeContextFieldError(w, fieldErr)
		return
	}

	ctx := r.Context()
	state, err := h.repo.GetContextState(ctx, projectID, conversationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	merged, fieldErr := contextsettings.Resolve(state.Strategy, h.contextDefaults(r)).Apply(update)
	if fieldErr != nil {
		writeContextFieldError(w, fieldErr)
		return
	}

	if err := h.repo.UpdateContextStrategy(ctx, projectID, conversationID, merged); err != nil {
		apierr.Write(w, err)
		return
	}

	writeJSON(w, http.StatusOK, contextStrategyUpdateResponse(merged, state))
}

// contextStrategyUpdateResponse is pylon's PUT body: the context status with
// `message` and `updated_strategy` laid on top, one flat object.
func contextStrategyUpdateResponse(merged contextsettings.Strategy, state contextsettings.ConversationState) map[string]any {
	response := map[string]any{}
	// Marshal-then-unmarshal so the status fields keep the JSON names their
	// struct tags give them, in one place, instead of being spelled a second
	// time here where they could drift from the served status document.
	if encoded, err := json.Marshal(contextsettings.BuildStatus(merged, state.Analytics, state.MessageGroupsTotal)); err == nil {
		_ = json.Unmarshal(encoded, &response)
	}
	response["message"] = "Strategy updated successfully"
	response["updated_strategy"] = merged
	return response
}

// contextDefaults reads the CALLER's defaults, never the conversation author's.
//
// That is pylon's behaviour and the only one that is safe here: the defaults
// live on the author record of whoever is asking, and a conversation can be
// read by any member of its project. Resolving someone else's Settings >
// Memory preferences would leak them across a shared conversation.
//
// A failure is answered with "no defaults" rather than an error: the tier is a
// convenience, and losing it degrades to the contract's constants.
func (h *Handler) contextDefaults(r *http.Request) contextsettings.UserDefaults {
	if h.userDefaults == nil {
		return contextsettings.UserDefaults{}
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return contextsettings.UserDefaults{}
	}
	userID, ok := user.OwningUserID()
	if !ok {
		return contextsettings.UserDefaults{}
	}
	defaults, err := h.userDefaults.ContextDefaults(r.Context(), userID)
	if err != nil {
		return contextsettings.UserDefaults{}
	}
	return defaults
}

// writeContextFieldError answers with this API's validation shape, the same
// `{"error": ..., "field": ...}` body internal/api/v2/configurations writes.
func writeContextFieldError(w http.ResponseWriter, fieldErr *contextsettings.FieldError) {
	writeJSON(w, http.StatusBadRequest, struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}{Error: fieldErr.Message, Field: fieldErr.Field})
}

// metaOrEmpty keeps a details response's `meta` an object even for a
// conversation that has none. `null` there would reach the web client as
// `undefined`, and its own reader (`useChatPageData.ts`) only forwards the key
// when it is present — so the composer would fall back to "no modules" for a
// conversation it could not read rather than for one that has none.
func metaOrEmpty(meta map[string]any) map[string]any {
	if meta == nil {
		return map[string]any{}
	}
	return meta
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // connection already committed; ignore write error
}

func (h *Handler) authorizeConversation(w http.ResponseWriter, r *http.Request) bool {
	kind, id := "conversation", chi.URLParam(r, "conversationID")
	if id == "" {
		kind, id = "message", chi.URLParam(r, "messageID")
	}
	if id == "" {
		kind, id = "canvas", chi.URLParam(r, "canvasID")
	}
	err := h.repo.AuthorizeChatResource(r.Context(), chi.URLParam(r, "projectID"), kind, id)
	if err != nil {
		apierr.Write(w, err)
		return false
	}
	return true
}

func applyCreatePersonalization(conv *Conversation, personalization map[string]any) {
	if len(personalization) == 0 {
		return
	}
	if conv.Meta == nil {
		conv.Meta = map[string]any{}
	}
	persona, _ := personalization["persona"].(string)
	if persona != "" {
		conv.Meta["persona"] = persona
	}
	instructions, _ := personalization["default_instructions"].(string)
	if choices, ok := personalization["personality_instructions"].(map[string]any); ok {
		instructions = ""
		if persona != "" {
			instructions, _ = choices[persona].(string)
		}
	}
	if instructions != "" {
		conv.Meta["default_instructions"] = instructions
		if conv.Instructions == "" {
			conv.Instructions = instructions
		}
	}
}
