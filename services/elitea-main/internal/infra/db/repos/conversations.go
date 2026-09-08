package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type ConversationsRepo struct {
	pool *pgxpool.Pool
}

func NewConversationsRepo(pool *pgxpool.Pool) *ConversationsRepo {
	return &ConversationsRepo{pool: pool}
}

func (r *ConversationsRepo) List(ctx context.Context, projectID string, page, pageSize int) (conversations.ListResponse, error) {
	s := schema(projectID)

	var total int
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_conversations`, s)
	if err := r.pool.QueryRow(ctx, countQ).Scan(&total); err != nil {
		return conversations.ListResponse{Items: []conversations.Conversation{}, Total: 0, Page: page, PageSize: pageSize}, nil
	}

	offset := (page - 1) * pageSize
	q := fmt.Sprintf(`
		SELECT c.id::text, c.name, COALESCE(c.uuid::text, ''), c.author_id, c.created_at, COALESCE(c.updated_at, c.created_at),
			(SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
		FROM %s.chat_conversations c
		ORDER BY c.updated_at DESC NULLS LAST, c.created_at DESC
		LIMIT $1 OFFSET $2`, s, s)

	rows, err := r.pool.Query(ctx, q, pageSize, offset)
	if err != nil {
		return conversations.ListResponse{Items: []conversations.Conversation{}, Total: 0, Page: page, PageSize: pageSize}, nil
	}
	defer rows.Close()

	var items []conversations.Conversation
	for rows.Next() {
		var c conversations.Conversation
		var authorID int
		if err := rows.Scan(&c.ID, &c.Name, &c.UUID, &authorID, &c.CreatedAt, &c.UpdatedAt, &c.MessageCount); err != nil {
			continue
		}
		c.ProjectID = projectID
		c.CreatedBy = fmt.Sprintf("%d", authorID)
		items = append(items, c)
	}
	if items == nil {
		items = []conversations.Conversation{}
	}

	totalPages := total / pageSize
	if total%pageSize > 0 {
		totalPages++
	}

	return conversations.ListResponse{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}

// idPredicate picks the column a conversation identifier addresses.
//
// `chat_conversations.id` is a SERIAL, so handing it a UUID made Postgres
// raise a type error and the route answered 500 — #128 defect 5, a status
// that says "the server is broken" for what is really "look this up
// differently". A conversation's UUID is a real, unique identity for the same
// row, so it resolves against `uuid` instead; an identifier that is neither
// is a 404, because no row can ever carry it.
func idPredicate(conversationID string) (predicate string, ok bool) {
	if conversationID == "" {
		return "", false
	}
	numeric := true
	for i := 0; i < len(conversationID); i++ {
		if conversationID[i] < '0' || conversationID[i] > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		return "c.id = $1::bigint", true
	}
	if _, err := uuid.Parse(conversationID); err == nil {
		return "c.uuid = $1::uuid", true
	}
	return "", false
}

// resolveConversationID maps whichever identifier form a route carries onto the
// numeric primary key the child tables actually hold.
//
// DEFECT #599: every conversation child table — chat_message_group,
// chat_messages, chat_participant_mapping, chat_selected_conversations — keys
// on `conversation_id integer`, a FK to chat_conversations.id. The repository
// passed the raw route parameter into those comparisons, but the routes are
// not uniform: /messages/prompt_lib/{projectID}/{conversationID} is called
// with the conversation UUID (apps/elitea-web .../messageApi.ts), while
// /participants/... is called with the numeric id. Comparing an integer column
// to a UUID makes Postgres raise `invalid input syntax for type integer`, so
// ListMessages returned an empty transcript for every conversation and
// DeleteMessages answered 500. Resolving here, once, means both forms address
// the same row and no caller has to know which form its route uses.
//
// An identifier that is neither form, or that names no conversation, is a 404:
// no row can ever carry it.
func (r *ConversationsRepo) resolveConversationID(ctx context.Context, projectID, conversationID string) (int64, error) {
	predicate, ok := idPredicate(conversationID)
	if !ok {
		return 0, apierr.NotFound("conversation not found")
	}
	q := fmt.Sprintf(`SELECT c.id FROM %s.chat_conversations c WHERE %s`, schema(projectID), predicate)
	var id int64
	if err := r.pool.QueryRow(ctx, q, conversationID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, apierr.NotFound("conversation not found")
		}
		return 0, fmt.Errorf("conversations: resolve conversation id: %w", err)
	}
	return id, nil
}

func (r *ConversationsRepo) Get(ctx context.Context, projectID, conversationID string) (conversations.Conversation, error) {
	s := schema(projectID)
	predicate, ok := idPredicate(conversationID)
	if !ok {
		return conversations.Conversation{}, apierr.NotFound("conversation not found")
	}

	q := fmt.Sprintf(`
		SELECT c.id::text, c.name, COALESCE(c.uuid::text, ''), c.author_id, c.folder_id::text, c.created_at, COALESCE(c.updated_at, c.created_at),
			c.meta, c.is_private,
			(SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
		FROM %s.chat_conversations c WHERE %s`, s, s, predicate)

	var c conversations.Conversation
	var authorID int
	var folderID *string
	var metaBytes []byte
	var isPrivate bool
	err := r.pool.QueryRow(ctx, q, conversationID).Scan(
		&c.ID, &c.Name, &c.UUID, &authorID, &folderID, &c.CreatedAt, &c.UpdatedAt, &metaBytes, &isPrivate, &c.MessageCount,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return conversations.Conversation{}, apierr.NotFound("conversation not found")
		}
		return conversations.Conversation{}, fmt.Errorf("conversations: get: %w", err)
	}
	c.ProjectID = projectID
	c.CreatedBy = fmt.Sprintf("%d", authorID)
	c.FolderID = folderID
	c.Meta = decodeConversationMeta(metaBytes)
	c.IsPrivate = &isPrivate
	return c, nil
}

// decodeConversationMeta reads the `meta` jsonb column.
//
// A document this repository cannot parse reads as "no settings", not as an
// error: the column is written by this service and by pylon before it, and a
// details request must still answer with the conversation's name and
// participants when one legacy row holds something unexpected.
func decodeConversationMeta(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	return meta
}

func (r *ConversationsRepo) ListParticipants(ctx context.Context, projectID, conversationID string) ([]conversations.Participant, error) {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return nil, err
	}
	// The auth_core__user join resolves a DISPLAY NAME for user participants.
	// The REST attach path stores only `entity_meta.id` (the numeric user id
	// the resolver's author join needs), and the participant writer records
	// its own disclosed gap: "Legacy also resolves a user name ... this
	// repository does not perform that lookup" (participantDisplayMeta). The
	// transcript's author captions read `meta.user_name` off THIS payload, so
	// without the lookup every author renders as "User <n>".
	//
	// Probe-guarded, not assumed: auth_core__user is BOOTSTRAP-owned
	// (001_initial.sql), not part of the shared migration corpus this
	// repository's RunMigrations applies — a corpus-only database (the
	// integration suite is one) has chat_participants and no auth table, and
	// an unguarded join would turn every participants read into 42P01 there.
	// Same arrangement as the application_tools probe in applications.go: a
	// display name is an enrichment, and its absence must never take the
	// payload down with it.
	authorIdentityJoin := ""
	authorIdentityColumns := "'', ''"
	var authTable *string
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('auth_core__user')::text`).Scan(&authTable); err == nil && authTable != nil {
		authorIdentityColumns = "COALESCE(au.name, ''), COALESCE(au.email, '')"
		authorIdentityJoin = `
		LEFT JOIN auth_core__user au
			ON p.entity_name = 'user'
			AND p.entity_meta->>'id' ~ '^[0-9]+$'
			AND au.id = (p.entity_meta->>'id')::integer`
	}
	q := fmt.Sprintf(`
		SELECT p.id, p.entity_name, p.entity_meta, p.meta, pm.entity_settings,
			%s
		FROM %s.chat_participant_mapping pm
		JOIN %s.chat_participants p ON p.id = pm.participant_id%s
		WHERE pm.conversation_id = $1
		ORDER BY pm.id`, authorIdentityColumns, s, s, authorIdentityJoin)

	rows, err := r.pool.Query(ctx, q, id)
	if err != nil {
		return []conversations.Participant{}, nil
	}
	defer rows.Close()

	var items []conversations.Participant
	for rows.Next() {
		var p conversations.Participant
		var entityMeta, meta, entitySettings []byte
		var authorName, authorEmail string
		if err := rows.Scan(&p.ID, &p.EntityName, &entityMeta, &meta, &entitySettings, &authorName, &authorEmail); err != nil {
			continue
		}
		if entityMeta != nil {
			_ = json.Unmarshal(entityMeta, &p.EntityMeta) // best-effort: DB column is trusted JSON
		}
		if p.EntityMeta == nil {
			p.EntityMeta = map[string]any{}
		}
		if meta != nil {
			_ = json.Unmarshal(meta, &p.Meta) // best-effort: DB column is trusted JSON
		}
		if p.Meta == nil {
			p.Meta = map[string]any{}
		}
		// Overlay, never overwrite: a participant that already carries a
		// user_name (the socket-era writer resolved one) keeps it, and a user
		// the auth table no longer holds stays as stored — the reader then
		// renders its own "no longer available" state.
		if p.EntityName == "user" {
			if existing, _ := p.Meta["user_name"].(string); existing == "" {
				if authorName != "" {
					p.Meta["user_name"] = authorName
				} else if authorEmail != "" {
					p.Meta["user_name"] = authorEmail
				}
			}
		}
		if entitySettings != nil {
			_ = json.Unmarshal(entitySettings, &p.EntitySettings) // best-effort: DB column is trusted JSON
		}
		if p.EntitySettings == nil {
			p.EntitySettings = map[string]any{}
		}
		items = append(items, p)
	}
	if items == nil {
		items = []conversations.Participant{}
	}
	return items, nil
}

func (r *ConversationsRepo) Create(ctx context.Context, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
	s := schema(projectID)

	authorID := conv.CreatedBy
	if authorID == "" {
		authorID = "1"
	}

	// The settings document the caller sent, or an empty one. Marshalled
	// rather than interpolated: it is caller data.
	meta := conv.Meta
	if meta == nil {
		meta = map[string]any{}
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return conversations.Conversation{}, fmt.Errorf("conversations: create: encode meta: %w", err)
	}

	q := fmt.Sprintf(`
		INSERT INTO %s.chat_conversations (name, author_id, is_private, meta, source)
		VALUES ($1, $2, true, $3::jsonb, 'api')
		RETURNING id::text, name, uuid::text, created_at, COALESCE(updated_at, created_at), meta`, s)

	var c conversations.Conversation
	var metaBytes []byte
	if err := r.pool.QueryRow(ctx, q, conv.Name, authorID, encodedMeta).Scan(&c.ID, &c.Name, &c.UUID, &c.CreatedAt, &c.UpdatedAt, &metaBytes); err != nil {
		return conversations.Conversation{}, fmt.Errorf("conversations: create: %w", err)
	}
	c.ProjectID = projectID
	c.CreatedBy = authorID
	c.Meta = decodeConversationMeta(metaBytes)
	return c, nil
}

func (r *ConversationsRepo) Update(ctx context.Context, projectID, conversationID string, conv conversations.Conversation) (conversations.Conversation, error) {
	s := schema(projectID)

	setClauses := "updated_at = now()"
	args := []any{}
	argIdx := 1

	if conv.Name != "" {
		setClauses += fmt.Sprintf(", name = $%d", argIdx)
		args = append(args, conv.Name)
		argIdx++
	}
	if conv.FolderID != nil {
		if *conv.FolderID == "" {
			setClauses += ", folder_id = NULL"
		} else {
			setClauses += fmt.Sprintf(", folder_id = $%d", argIdx)
			args = append(args, *conv.FolderID)
			argIdx++
		}
	}
	// Set only when the caller stated it. The INSERT hardcodes `true`, so
	// without this clause the column could never change and "Make public"
	// wrote nothing at all.
	if conv.IsPrivate != nil {
		setClauses += fmt.Sprintf(", is_private = $%d", argIdx)
		args = append(args, *conv.IsPrivate)
		argIdx++
	}
	// Written whole, and only when the caller stated one: the handler sets
	// this field exactly when the request body carried a `meta` object, so a
	// rename never touches the column while a settings write replaces the
	// whole document — which is what the client sends (it PUTs the previous
	// document plus its one change).
	if conv.Meta != nil {
		encodedMeta, err := json.Marshal(conv.Meta)
		if err != nil {
			return conversations.Conversation{}, fmt.Errorf("conversations: update: encode meta: %w", err)
		}
		setClauses += fmt.Sprintf(", meta = $%d::jsonb", argIdx)
		args = append(args, encodedMeta)
		argIdx++
	}

	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return conversations.Conversation{}, err
	}
	args = append(args, id)
	// `author_id` and `folder_id` are returned so the PUT response describes
	// the same conversation the GET does. Omitting author_id was #128 defect
	// 6: every update answered with `"created_by": ""`, so a client that
	// refreshed its cache from the mutation response lost the owner.
	q := fmt.Sprintf(`UPDATE %s.chat_conversations SET %s WHERE id = $%d
		RETURNING id::text, name, COALESCE(uuid::text, ''), author_id, folder_id::text, created_at, COALESCE(updated_at, created_at), meta, is_private`,
		s, setClauses, argIdx)

	var c conversations.Conversation
	var authorID int
	var folderID *string
	var metaBytes []byte
	var isPrivate bool
	if err := r.pool.QueryRow(ctx, q, args...).Scan(
		&c.ID, &c.Name, &c.UUID, &authorID, &folderID, &c.CreatedAt, &c.UpdatedAt, &metaBytes, &isPrivate,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return conversations.Conversation{}, apierr.NotFound("conversation not found")
		}
		return conversations.Conversation{}, fmt.Errorf("conversations: update: %w", err)
	}
	c.ProjectID = projectID
	c.CreatedBy = fmt.Sprintf("%d", authorID)
	c.FolderID = folderID
	c.Meta = decodeConversationMeta(metaBytes)
	c.IsPrivate = &isPrivate
	return c, nil
}

// attachmentQuerier is the part of pgx.Tx the attachment collection needs.
// Stated as an interface so the two delete paths share one scan without either
// of them handing the helper a transaction it could end.
type attachmentQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// scanAttachmentRefs collects (bucket, name) pairs from a two-column query.
//
// Both delete paths REPORT the attachments whose rows they are about to remove
// rather than removing the bytes themselves: the bytes are not in the database,
// only the HTTP handler holds the object store, and reporting is what lets the
// byte delete happen strictly after the row delete has committed. Sharing the
// scan states that contract once — a second copy is where the two paths would
// quietly drift on what counts as an attachment.
func scanAttachmentRefs(ctx context.Context, q attachmentQuerier, sql string, args ...any) ([]conversations.AttachmentRef, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("conversations: read message attachments: %w", err)
	}
	defer rows.Close()

	var refs []conversations.AttachmentRef
	for rows.Next() {
		var ref conversations.AttachmentRef
		if err := rows.Scan(&ref.Bucket, &ref.Name); err != nil {
			return nil, fmt.Errorf("conversations: scan message attachment: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversations: read message attachments: %w", err)
	}
	return refs, nil
}

// collectGroupAttachments reports every stored attachment the named message
// groups carry.
func collectGroupAttachments(ctx context.Context, q attachmentQuerier, s string, groupIDs []int64) ([]conversations.AttachmentRef, error) {
	return scanAttachmentRefs(ctx, q, fmt.Sprintf(`
		SELECT att.bucket, att.name
		FROM %s.chat_message_items mi
		JOIN %s.chat_messages_attachment att ON att.id = mi.id
		WHERE mi.message_group_id = ANY($1) AND mi.item_type = 'attachment_message'
		ORDER BY mi.message_group_id, mi.order_index`, s, s), groupIDs)
}

// collectConversationAttachments reports every stored attachment any message in
// one conversation carries. Scoped by conversation_id through the group join:
// the tenant schema holds every conversation in the project, so an unscoped
// read would hand the handler other conversations' files to destroy.
func collectConversationAttachments(ctx context.Context, q attachmentQuerier, s string, conversationID int64) ([]conversations.AttachmentRef, error) {
	return scanAttachmentRefs(ctx, q, fmt.Sprintf(`
		SELECT att.bucket, att.name
		FROM %s.chat_message_items mi
		JOIN %s.chat_message_group g ON g.id = mi.message_group_id
		JOIN %s.chat_messages_attachment att ON att.id = mi.id
		WHERE g.conversation_id = $1 AND mi.item_type = 'attachment_message'
		ORDER BY mi.message_group_id, mi.order_index`, s, s, s), conversationID)
}

func (r *ConversationsRepo) Delete(ctx context.Context, projectID, conversationID string) ([]conversations.AttachmentRef, error) {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return nil, err
	}

	// DEFECT #602: two of the six statements here named tables that exist in NO
	// schema anywhere, so deleting a conversation answered 500 on every
	// deployment.
	//
	//   * `chat_messages` (the bare name) is a dead legacy artifact — an older
	//     flat one-row-per-message representation with an inline `content`
	//     column. Its successor is the chat_message_group -> chat_message_items
	//     -> chat_messages_text graph that migrations/tenant/0123 creates and
	//     ListMessages reads. Pylon does not declare it either.
	//   * `chat_conversation_summaries` appeared exactly once in this
	//     repository — in the statement below. It is not in pylon, not in any
	//     migration, and not in the pg-catalog dump of the live legacy database
	//     (testdata/postgres/legacy-centry-catalog.json).
	//
	// Neither is recoverable, so both statements are gone rather than repaired.
	// What replaces them is the item level of the real graph: chat_message_items
	// does NOT cascade from chat_message_group, so deleting groups without
	// deleting their items first violates the FK.
	//
	// One transaction, because the previous sequential Execs could leave a
	// conversation with its participants detached and its messages still
	// present if any statement in the middle failed.
	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("conversations: delete: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err := transaction.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_participant_mapping WHERE conversation_id = $1`, s), id); err != nil {
		return nil, fmt.Errorf("conversations: delete participant mapping: %w", err)
	}

	// Read the attachments BEFORE anything that names them is deleted, for the
	// same reason DeleteMessage does: chat_messages_attachment cascades from
	// chat_message_items (tenant migration 0127), so the item DELETE below takes
	// these rows with it, and after this transaction nothing anywhere names the
	// stored files. A message delete at least leaves the rest of the
	// conversation behind; here the whole graph goes, so an uncollected ref is
	// bytes no caller can ever reach again — they sit in the object store until
	// the retention sweeper expires them
	// (internal/runtimecomposition/artifact_retention_sweep.go).
	//
	// Reported rather than deleted, and only once the commit below has
	// succeeded: the bytes are not in the database, only the handler holds the
	// object store, and a refused delete must destroy nothing. See
	// Handler.Delete and Handler.deleteAttachmentObjects.
	//
	// Selected by `item_type` as well as by the join, the same way DeleteMessage
	// does — the discriminator and the payload table state the same fact twice,
	// and a disagreement between them would show up here as silent data loss.
	attachments, err := collectConversationAttachments(ctx, transaction, s, id)
	if err != nil {
		return nil, err
	}

	if _, err := transaction.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_message_items
		WHERE message_group_id IN (SELECT id FROM %s.chat_message_group WHERE conversation_id = $1)`, s, s), id); err != nil {
		return nil, fmt.Errorf("conversations: delete message items: %w", err)
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_message_group WHERE conversation_id = $1`, s), id); err != nil {
		return nil, fmt.Errorf("conversations: delete message groups: %w", err)
	}
	if _, err := transaction.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_selected_conversations WHERE conversation_id = $1`, s), id); err != nil {
		return nil, fmt.Errorf("conversations: delete selected conversations: %w", err)
	}

	q := fmt.Sprintf(`DELETE FROM %s.chat_conversations WHERE id = $1`, s)
	ct, err := transaction.Exec(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("conversations: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return nil, apierr.NotFound("conversation not found")
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, fmt.Errorf("conversations: delete commit: %w", err)
	}
	return attachments, nil
}

// participantIdentityQuery finds the participant row that already describes the
// same entity.
//
// The identity of a participant is NOT the whole entity_meta document. The web
// client puts a `name` inside it. A renamed agent therefore produces a
// different document for the same agent. Whole-row equality would create a
// second row. The key is a per-type subset, the same subset legacy matches on
// (`make_query_filter_for_entity`):
//
//	llm    -> model_name        (a model participant carries no id)
//	dummy  -> nothing; one row serves every conversation
//	user   -> id
//	other  -> id and project_id (application, prompt, toolkit)
//
// IS NOT DISTINCT FROM, not `=`: a missing key reads as SQL NULL, and `=`
// answers NULL for it, which the WHERE clause drops.
//
// THE COMPARISON IS TEXTUAL, and legacy's is numeric. legacy casts both sides
// to Integer (`make_query_filter_for_entity`, participant_utils.py:79-94).
// This query compares the `->>` text on both sides. A caller that sends
// `"id": "42"` and a caller that sends `"id": 42` therefore get two rows for
// one entity. The web client sends a number, so the split does not happen
// today. A numeric cast here would fail on a non-numeric id, which this
// endpoint does not reject.
const participantIdentityQuery = `SELECT id FROM %s.chat_participants
	WHERE entity_name = $1::text
	  AND CASE $1::text
	        WHEN 'llm'   THEN entity_meta->>'model_name' IS NOT DISTINCT FROM $2::jsonb->>'model_name'
	        WHEN 'dummy' THEN TRUE
	        WHEN 'user'  THEN entity_meta->>'id' IS NOT DISTINCT FROM $2::jsonb->>'id'
	        ELSE entity_meta->>'id'         IS NOT DISTINCT FROM $2::jsonb->>'id'
	         AND entity_meta->>'project_id' IS NOT DISTINCT FROM $2::jsonb->>'project_id'
	      END
	ORDER BY id
	LIMIT 1`

// participantDisplayMeta builds the `meta` document the chat turn builder
// reads.
//
// internal/db/queries/agent_chat.sql reads `meta->>'name'` when it assembles
// the adhoc tools payload. The previous INSERT hardcoded `'{}'::json`, so every
// participant this repository created contributed a NULL name there.
//
// The name comes from the request, which is what the web client sends
// (features/chat-participants/lib/helpers.ts). Legacy also resolves an
// `agent_type` for an application through an entity lookup. Legacy also
// resolves a user name and avatar for a user through that lookup. This
// repository does not perform that lookup.
func participantDisplayMeta(entityName string, entityMeta map[string]any) []byte {
	meta := map[string]any{}
	switch entityName {
	case "llm":
		if modelName, ok := entityMeta["model_name"].(string); ok && modelName != "" {
			meta["name"] = modelName
		}
	case "dummy":
		meta["name"] = "EliteA"
	case "user":
		if userName, ok := entityMeta["user_name"].(string); ok && userName != "" {
			meta["user_name"] = userName
		}
	default:
		if name, ok := entityMeta["name"].(string); ok && name != "" {
			meta["name"] = name
		}
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// AddParticipant attaches one entity to a conversation.
//
// DEFECT this shape corrects: the previous version always INSERTed a new
// chat_participants row with its own gen_random_uuid(), and only looked for an
// existing row if that INSERT failed. The table has no unique key on
// (entity_name, entity_meta), so the INSERT could not fail and the lookup was
// unreachable. The mapping insert's
// `ON CONFLICT ON CONSTRAINT _participant_conversation_uc` guards
// (participant_id, conversation_id), and participant_id was always new, so the
// guard never fired either. Adding the same agent twice left two participant
// rows and two mapping rows in one conversation. A double click does that,
// and so does a retry after a failed first attempt.
//
// The order is now find, then create, which is what legacy's `get_or_create_one`
// does and what predict.go's findOrCreateUserParticipant already did.
//
// One transaction covers the lookup, the insert and the mapping. Two
// concurrent first-time adds can still both miss the lookup; the mapping
// ON CONFLICT does not catch that, because the two transactions hold different
// participant ids.
func (r *ConversationsRepo) AddParticipant(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	s := schema(projectID)
	entityName, _ := body["entity_name"].(string)
	entityMetaMap, _ := body["entity_meta"].(map[string]any)
	entityMeta, _ := json.Marshal(body["entity_meta"])
	entitySettings, _ := json.Marshal(body["entity_settings"])
	if string(entitySettings) == "null" {
		entitySettings = []byte("{}")
	}

	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}

	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("conversations: add participant: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var participantID int
	err = transaction.QueryRow(ctx,
		fmt.Sprintf(participantIdentityQuery, s), entityName, entityMeta).Scan(&participantID)
	if errors.Is(err, pgx.ErrNoRows) {
		insert := fmt.Sprintf(`INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
			VALUES (gen_random_uuid(), $1, $2::jsonb, $3::json) RETURNING id`, s)
		if err = transaction.QueryRow(ctx, insert,
			entityName, entityMeta, participantDisplayMeta(entityName, entityMetaMap),
		).Scan(&participantID); err != nil {
			return fmt.Errorf("conversations: create participant: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("conversations: add participant lookup: %w", err)
	}

	// ON CONFLICT names the COLUMNS, not the constraint.
	//
	// DEFECT: the clause used to read
	// `ON CONFLICT ON CONSTRAINT _participant_conversation_uc`. Only the legacy
	// bootstrap schema (internal/infra/db/migrations/001_initial.sql:511) gives
	// the unique key that name. The ledgered tenant history that every real
	// deployment runs — migrations/tenant/0123_agent_chat_message_tables.sql:87
	// — declares an anonymous `UNIQUE (participant_id, conversation_id)`, whose
	// generated name is different. On such a database the statement failed with
	// SQLSTATE 42704 (undefined_object), so adding a participant answered 500.
	// Column inference matches the unique key under either name.
	mapping := fmt.Sprintf(`INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings)
		VALUES ($1, $2, $3::jsonb) ON CONFLICT (participant_id, conversation_id) DO NOTHING`, s)
	if _, err := transaction.Exec(ctx, mapping, id, participantID, entitySettings); err != nil {
		return fmt.Errorf("conversations: add participant mapping: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("conversations: add participant commit: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) RemoveParticipant(ctx context.Context, projectID, conversationID, participantID string) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`DELETE FROM %s.chat_participant_mapping WHERE conversation_id = $1 AND participant_id = $2`, s)
	if _, err := r.pool.Exec(ctx, q, id, participantID); err != nil {
		return fmt.Errorf("conversations: remove participant: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) UpdateEntitySettings(ctx context.Context, projectID, conversationID, participantID string, settings map[string]any) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(settings)
	q := fmt.Sprintf(`UPDATE %s.chat_participant_mapping SET entity_settings = $1 WHERE conversation_id = $2 AND participant_id = $3`, s)
	if _, err := r.pool.Exec(ctx, q, data, id, participantID); err != nil {
		return fmt.Errorf("conversations: update entity settings: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) BatchUpdateEntitySettings(ctx context.Context, projectID, conversationID string, settings []map[string]any) error {
	for _, s := range settings {
		pid, _ := s["participant_id"].(string)
		delete(s, "participant_id")
		if err := r.UpdateEntitySettings(ctx, projectID, conversationID, pid, s); err != nil {
			return err
		}
	}
	return nil
}

func (r *ConversationsRepo) SelectConversation(ctx context.Context, projectID, conversationID, userID string) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	// Schema: id, user_id, conversation_id (no unique on user_id, so delete+insert)
	delQ := fmt.Sprintf(`DELETE FROM %s.chat_selected_conversations WHERE user_id = $1`, s)
	if _, err := r.pool.Exec(ctx, delQ, userID); err != nil {
		return fmt.Errorf("conversations: select conversation delete old: %w", err)
	}
	insQ := fmt.Sprintf(`INSERT INTO %s.chat_selected_conversations (conversation_id, user_id) VALUES ($1, $2)`, s)
	if _, err := r.pool.Exec(ctx, insQ, id, userID); err != nil {
		return fmt.Errorf("conversations: select conversation insert: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) DeselectConversation(ctx context.Context, projectID, userID string) error {
	s := schema(projectID)
	q := fmt.Sprintf(`DELETE FROM %s.chat_selected_conversations WHERE user_id = $1`, s)
	if _, err := r.pool.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("conversations: deselect conversation: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) CreateCanvas(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	s := schema(projectID)

	messageGroupID := intFromAny(body["message_group_id"])
	messageItemID := intFromAny(body["message_item_id"])
	name, _ := body["name"].(string)
	canvasType, _ := body["canvas_type"].(string)
	if canvasType == "" {
		canvasType = "code"
	}
	meta, _ := body["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	startsAt := intFromAny(body["canvas_content_starts_at"])
	endsAt := intFromAny(body["canvas_content_ends_at"])
	codeLang, _ := body["code_language"].(string)

	if startsAt > endsAt {
		return nil, apierr.BadRequest("canvas_content_starts_at must be <= canvas_content_ends_at")
	}

	// 1. Fetch the existing text message item
	q := fmt.Sprintf(`SELECT mi.id, mi.order_index, mt.content
		FROM %s.chat_message_items mi
		JOIN %s.chat_messages_text mt ON mt.id = mi.id
		WHERE mi.id = $1 AND mi.message_group_id = $2`, s, s)

	var oldItemID, orderIndex int
	var oldContent string
	err := r.pool.QueryRow(ctx, q, messageItemID, messageGroupID).Scan(&oldItemID, &orderIndex, &oldContent)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, apierr.BadRequest("No such message in message group")
		}
		return nil, fmt.Errorf("fetch message item: %w", err)
	}

	// The selection must be INSIDE the message, and it is refused here rather
	// than clamped.
	//
	// The offsets are byte positions the CLIENT computes over text it holds a
	// copy of, so a selection the stored message cannot satisfy is ordinary
	// caller input: a message edited or regenerated between the read and the
	// selection produces one, and so does an off-by-one in a client. Without
	// this the slice below indexed the string out of range and took the whole
	// request down — the caller read a server error for a selection it made,
	// and the panic said nothing about which of the two ends was wrong.
	//
	// Clamping was the other option and is worse: silently carving a
	// DIFFERENT range than the user selected splits their message somewhere
	// they did not choose, and the split is destructive (the original text
	// item is deleted). A refusal leaves the message as it was.
	if startsAt < 0 || endsAt > len(oldContent) {
		return nil, apierr.BadRequest("canvas_content_starts_at and canvas_content_ends_at must lie inside the message content")
	}

	// 2. Slice content
	preContent := ""
	canvasContent := oldContent[startsAt:endsAt]
	postContent := ""
	if startsAt > 0 {
		preContent = oldContent[:startsAt]
	}
	if endsAt < len(oldContent) {
		postContent = oldContent[endsAt:]
	}

	// 3. Delete old text item (cascades from chat_messages_text)
	delTextQ := fmt.Sprintf(`DELETE FROM %s.chat_messages_text WHERE id = $1`, s)
	if _, err := r.pool.Exec(ctx, delTextQ, oldItemID); err != nil {
		return nil, fmt.Errorf("delete old text item: %w", err)
	}
	delItemQ := fmt.Sprintf(`DELETE FROM %s.chat_message_items WHERE id = $1`, s)
	if _, err := r.pool.Exec(ctx, delItemQ, oldItemID); err != nil {
		return nil, fmt.Errorf("delete old message item: %w", err)
	}

	// 4. Insert new items with proper ordering
	newOrder := orderIndex
	insertItemQ := fmt.Sprintf(`INSERT INTO %s.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
		VALUES (gen_random_uuid(), $1, $2, '{}'::jsonb, $3) RETURNING id, uuid::text`, s)
	insertTextQ := fmt.Sprintf(`INSERT INTO %s.chat_messages_text (id, content) VALUES ($1, $2)`, s)

	if preContent != "" {
		var preID int
		var preUUID string
		err = r.pool.QueryRow(ctx, insertItemQ, "text_message", newOrder, messageGroupID).Scan(&preID, &preUUID)
		if err != nil {
			return nil, fmt.Errorf("insert pre-canvas text: %w", err)
		}
		_, err = r.pool.Exec(ctx, insertTextQ, preID, preContent)
		if err != nil {
			return nil, fmt.Errorf("insert pre-canvas text content: %w", err)
		}
		newOrder++
	}

	// Insert canvas message item
	var canvasItemID int
	var canvasUUID string
	err = r.pool.QueryRow(ctx, insertItemQ, "canvas_message", newOrder, messageGroupID).Scan(&canvasItemID, &canvasUUID)
	if err != nil {
		return nil, fmt.Errorf("insert canvas item: %w", err)
	}
	newOrder++

	// Insert canvas record
	insertCanvasQ := fmt.Sprintf(`INSERT INTO %s.chat_messages_canvas (id, name, canvas_type) VALUES ($1, $2, $3)`, s)
	_, err = r.pool.Exec(ctx, insertCanvasQ, canvasItemID, name, canvasType)
	if err != nil {
		return nil, fmt.Errorf("insert canvas record: %w", err)
	}

	// Insert canvas version
	insertVersionQ := fmt.Sprintf(`INSERT INTO %s.chat_canvas_versions (canvas_content, code_language, canvas_item_id)
		VALUES ($1, $2, $3) RETURNING id, created_at`, s)
	var versionID int
	var versionCreatedAt time.Time
	err = r.pool.QueryRow(ctx, insertVersionQ, canvasContent, nilIfEmpty(codeLang), canvasItemID).Scan(&versionID, &versionCreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert canvas version: %w", err)
	}

	if postContent != "" {
		var postID int
		var postUUID string
		err = r.pool.QueryRow(ctx, insertItemQ, "text_message", newOrder, messageGroupID).Scan(&postID, &postUUID)
		if err != nil {
			return nil, fmt.Errorf("insert post-canvas text: %w", err)
		}
		_, err = r.pool.Exec(ctx, insertTextQ, postID, postContent)
		if err != nil {
			return nil, fmt.Errorf("insert post-canvas text content: %w", err)
		}
	}

	// 5. Re-order any existing items that were before the old item
	reorderQ := fmt.Sprintf(`UPDATE %s.chat_message_items
		SET order_index = order_index + 100
		WHERE message_group_id = $1 AND id != $2 AND order_index < $3`, s)
	if _, err := r.pool.Exec(ctx, reorderQ, messageGroupID, canvasItemID, orderIndex); err != nil {
		return nil, fmt.Errorf("reorder message items: %w", err)
	}

	// 6. Build response matching CanvasItemDetail
	result := map[string]any{
		"id":          canvasItemID,
		"uuid":        canvasUUID,
		"name":        name,
		"canvas_type": canvasType,
		"item_type":   "canvas_message",
		"meta":        meta,
		"editors":     []any{},
		"created_at":  versionCreatedAt,
		"latest_version": map[string]any{
			"id":             versionID,
			"canvas_content": canvasContent,
			"code_language":  codeLang,
			"created_at":     versionCreatedAt,
		},
	}

	return result, nil
}

// canvasItemPredicate matches a canvas message item by whichever identifier
// the route carries.
//
// `CreateCanvas` answers BOTH — a numeric `id` and a `uuid` — and the web
// client stores the uuid (`entities/canvas`'s `canvasUUID`), while the
// message item embeds the numeric one. Accepting only one of them would make
// the read and the write address different canvases.
func canvasItemPredicate(canvasID string) (predicate string, ok bool) {
	if canvasID == "" {
		return "", false
	}
	numeric := true
	for i := 0; i < len(canvasID); i++ {
		if canvasID[i] < '0' || canvasID[i] > '9' {
			numeric = false
			break
		}
	}
	if numeric {
		return "mi.id = $1::bigint", true
	}
	if _, err := uuid.Parse(canvasID); err == nil {
		return "mi.uuid = $1::uuid", true
	}
	return "", false
}

// canvasRow is one canvas message item with the newest version on it.
type canvasRow struct {
	itemID     int
	itemUUID   string
	name       string
	canvasType string
	// The newest `chat_canvas_versions` row. `versionID` is 0 when the canvas
	// carries none, which is a real state: `CreateCanvas` writes one, but a
	// row imported from elsewhere need not have.
	versionID int
	content   string
	language  string
	createdAt time.Time
	groupUUID string
}

// readCanvas loads a canvas item and its newest version.
//
// The version is picked by `created_at DESC, id DESC` and not by `id` alone:
// the column is the ordering the deployed schema indexes, and the id
// tiebreaker keeps two versions written inside one clock tick deterministic.
func (r *ConversationsRepo) readCanvas(ctx context.Context, s, canvasID string) (canvasRow, error) {
	predicate, ok := canvasItemPredicate(canvasID)
	if !ok {
		return canvasRow{}, apierr.NotFound("canvas not found")
	}
	q := fmt.Sprintf(`
		SELECT mi.id, COALESCE(mi.uuid::text, ''), cv.name, cv.canvas_type,
			COALESCE(ver.id, 0), COALESCE(ver.canvas_content, ''), COALESCE(ver.code_language, ''),
			COALESCE(ver.created_at, now()), COALESCE(mg.uuid::text, '')
		FROM %s.chat_message_items mi
		JOIN %s.chat_messages_canvas cv ON cv.id = mi.id
		LEFT JOIN %s.chat_message_group mg ON mg.id = mi.message_group_id
		LEFT JOIN LATERAL (
			SELECT id, canvas_content, code_language, created_at
			FROM %s.chat_canvas_versions v
			WHERE v.canvas_item_id = mi.id
			ORDER BY v.created_at DESC, v.id DESC
			LIMIT 1
		) ver ON true
		WHERE %s`, s, s, s, s, predicate)

	var row canvasRow
	if err := r.pool.QueryRow(ctx, q, canvasID).Scan(
		&row.itemID, &row.itemUUID, &row.name, &row.canvasType,
		&row.versionID, &row.content, &row.language, &row.createdAt, &row.groupUUID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return canvasRow{}, apierr.NotFound("canvas not found")
		}
		return canvasRow{}, fmt.Errorf("conversations: read canvas: %w", err)
	}
	return row, nil
}

// canvasResponse is the one shape both canvas reads answer.
//
// FLAT AND NESTED AT ONCE, deliberately: the REST client normalises the flat
// keys (`entities/canvas/lib/normalise.ts` reads `canvas_content` and
// `code_language` off the top level), while a message item embeds the nested
// `latest_version` (the baseline's chat history patches
// `item_details.latest_version.canvas_content`). Answering one of the two
// would leave the other reader with an undefined document and no error.
func canvasResponse(row canvasRow) map[string]any {
	latest := map[string]any{
		"id":             row.versionID,
		"canvas_content": row.content,
		"code_language":  row.language,
		"created_at":     row.createdAt,
	}
	response := map[string]any{
		"id":             row.itemID,
		"uuid":           row.itemUUID,
		"name":           row.name,
		"canvas_type":    row.canvasType,
		"item_type":      "canvas_message",
		"editors":        []any{},
		"created_at":     row.createdAt,
		"canvas_content": row.content,
		"code_language":  row.language,
		"latest_version": latest,
	}
	if row.groupUUID != "" {
		response["message_group_uuid"] = row.groupUUID
	}
	return response
}

// canvasItemDetailsInput is what one `canvas_message` row carries.
type canvasItemDetailsInput struct {
	itemID    int
	itemUUID  string
	name      string
	canvasT   string
	versionID *int
	content   *string
	language  *string
	createdAt *time.Time
}

// canvasItemDetails shapes a canvas message item the way the chat history
// reads it: `item_details.latest_version.canvas_content`, with the flat keys
// beside it for the REST normaliser (see canvasResponse for why both).
func canvasItemDetails(in canvasItemDetailsInput) map[string]any {
	content := ""
	if in.content != nil {
		content = *in.content
	}
	language := ""
	if in.language != nil {
		language = *in.language
	}
	versionID := 0
	if in.versionID != nil {
		versionID = *in.versionID
	}
	latest := map[string]any{
		"id":             versionID,
		"canvas_content": content,
		"code_language":  language,
	}
	if in.createdAt != nil {
		latest["created_at"] = *in.createdAt
	}
	details := map[string]any{
		"id":             in.itemID,
		"item_type":      "canvas_message",
		"name":           in.name,
		"canvas_type":    in.canvasT,
		"canvas_content": content,
		"code_language":  language,
		"editors":        []any{},
		"latest_version": latest,
	}
	if in.itemUUID != "" {
		details["uuid"] = in.itemUUID
	}
	return details
}

// GetCanvas serves `GET /elitea_core/canvas/prompt_lib/{p}/{canvasID}`.
//
// It used to read `chat_conversations` by that id and answer the
// CONVERSATION's name — a different table, a different id space and a
// document with no canvas content in it at all. A caller asking for a canvas
// got either somebody's conversation or a 404, and never the text it was
// editing.
func (r *ConversationsRepo) GetCanvas(ctx context.Context, projectID, canvasID string) (map[string]any, error) {
	row, err := r.readCanvas(ctx, schema(projectID), canvasID)
	if err != nil {
		return nil, err
	}
	return canvasResponse(row), nil
}

// UpdateCanvas serves `PUT /elitea_core/canvas/prompt_lib/{p}/{canvasID}` —
// the only route that can persist an edit made in the canvas editor.
//
// WHAT IT USED TO DO, because the failure was silent and destructive in two
// directions at once: `UPDATE chat_conversations SET name = $1 WHERE id = $2`.
// The canvas id was read as a CONVERSATION id, so a save either matched
// nothing (uuid against an integer column — an error the handler reported as a
// 500) or RENAMED an unrelated conversation to the canvas's title. The edited
// text was never written anywhere: `chat_canvas_versions` had exactly one
// writer, `CreateCanvas`, so every edit after the first was lost the moment
// the editor closed.
//
// A new VERSION row, not an update in place: the table is a version history
// (`chat_canvas_versions`, `chat_canvas_version_authors`) and the reads above
// take the newest. Overwriting the current row would make the history a lie
// and lose the previous text.
func (r *ConversationsRepo) UpdateCanvas(ctx context.Context, projectID, canvasID string, body map[string]any) error {
	s := schema(projectID)
	row, err := r.readCanvas(ctx, s, canvasID)
	if err != nil {
		return err
	}

	// Name and type are updated only when stated, for the reason the
	// conversation Update states its own: the editor PUTs a language change
	// alone, and reading an absent key as "" would blank the canvas's title.
	if name, ok := body["name"].(string); ok && name != "" {
		if _, err := r.pool.Exec(ctx,
			fmt.Sprintf(`UPDATE %s.chat_messages_canvas SET name = $1 WHERE id = $2`, s), name, row.itemID); err != nil {
			return fmt.Errorf("conversations: update canvas name: %w", err)
		}
	}
	if canvasType, ok := body["canvas_type"].(string); ok && canvasType != "" {
		if _, err := r.pool.Exec(ctx,
			fmt.Sprintf(`UPDATE %s.chat_messages_canvas SET canvas_type = $1 WHERE id = $2`, s), canvasType, row.itemID); err != nil {
			return fmt.Errorf("conversations: update canvas type: %w", err)
		}
	}

	content, hasContent := body["canvas_content"].(string)
	language, hasLanguage := body["code_language"].(string)
	if !hasContent && !hasLanguage {
		return nil
	}
	// A language-only PUT carries the text forward rather than blanking it:
	// `canvas_content` is NOT NULL, and a version holding "" would read as a
	// canvas the user emptied.
	if !hasContent {
		content = row.content
	}
	if !hasLanguage {
		language = row.language
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.chat_canvas_versions (canvas_content, code_language, canvas_item_id)
		VALUES ($1, $2, $3)`, s), content, nilIfEmpty(language), row.itemID); err != nil {
		return fmt.Errorf("conversations: write canvas version: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) GetMessageByUUID(ctx context.Context, projectID, messageUUID string) (map[string]any, error) {
	s := schema(projectID)

	// Get message group by UUID
	q := fmt.Sprintf(`SELECT mg.id, mg.uuid::text, mg.author_participant_id, mg.sent_to_id, mg.reply_to_id, mg.meta
		FROM %s.chat_message_group mg WHERE mg.uuid::text = $1`, s)

	var groupID int
	var groupUUID string
	var authorPID *int
	var sentToID, replyToID *int
	var metaBytes []byte
	err := r.pool.QueryRow(ctx, q, messageUUID).Scan(&groupID, &groupUUID, &authorPID, &sentToID, &replyToID, &metaBytes)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, apierr.NotFound("message not found")
		}
		return nil, fmt.Errorf("get message: %w", err)
	}

	var meta map[string]any
	_ = json.Unmarshal(metaBytes, &meta) // best-effort: DB column is trusted JSON

	// Get message items with their content, ordered
	qi := fmt.Sprintf(`SELECT mi.id, mi.uuid::text, mi.item_type, mi.order_index, mi.meta,
		COALESCE(mt.content, '') as text_content,
		COALESCE(mc.name, '') as canvas_name,
		COALESCE(mc.canvas_type, '') as canvas_type
		FROM %s.chat_message_items mi
		LEFT JOIN %s.chat_messages_text mt ON mt.id = mi.id
		LEFT JOIN %s.chat_messages_canvas mc ON mc.id = mi.id
		WHERE mi.message_group_id = $1
		ORDER BY mi.order_index ASC`, s, s, s)

	rows, err := r.pool.Query(ctx, qi, groupID)
	if err != nil {
		return nil, fmt.Errorf("query message items: %w", err)
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var itemID int
		var itemUUID, itemType string
		var oIdx int
		var itemMetaBytes []byte
		var textContent, canvasName, canvasTypeStr string
		if err := rows.Scan(&itemID, &itemUUID, &itemType, &oIdx, &itemMetaBytes, &textContent, &canvasName, &canvasTypeStr); err != nil {
			return nil, fmt.Errorf("scan message item: %w", err)
		}

		item := map[string]any{
			"id":        itemID,
			"uuid":      itemUUID,
			"item_type": itemType,
		}

		switch itemType {
		case "text_message":
			item["item_details"] = map[string]any{"content": textContent}
		case "canvas_message":
			item["item_details"] = map[string]any{
				"name":        canvasName,
				"canvas_type": canvasTypeStr,
			}
		}

		items = append(items, item)
	}

	result := map[string]any{
		"id":                    groupID,
		"uuid":                  groupUUID,
		"author_participant_id": authorPID,
		"sent_to_id":            sentToID,
		"reply_to_id":           replyToID,
		"meta":                  meta,
		"message_items":         items,
	}

	return result, nil
}

func (r *ConversationsRepo) UpdateAttachmentStorage(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(body)
	q := fmt.Sprintf(`UPDATE %s.chat_conversations SET meta = jsonb_set(COALESCE(meta, '{}')::jsonb, '{attachment_storage}', $1::jsonb) WHERE id = $2`, s)
	if _, err := r.pool.Exec(ctx, q, data, id); err != nil {
		return fmt.Errorf("conversations: update attachment storage: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) AddAttachments(ctx context.Context, projectID, conversationID string, body map[string]any) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(body)
	q := fmt.Sprintf(`UPDATE %s.chat_conversations SET meta = jsonb_set(COALESCE(meta, '{}')::jsonb, '{attachments}', $1::jsonb) WHERE id = $2`, s)
	if _, err := r.pool.Exec(ctx, q, data, id); err != nil {
		return fmt.Errorf("conversations: add attachments: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) DeleteAttachments(ctx context.Context, projectID, conversationID string) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(`UPDATE %s.chat_conversations SET meta = (COALESCE(meta, '{}')::jsonb - 'attachments') WHERE id = $1`, s)
	if _, err := r.pool.Exec(ctx, q, id); err != nil {
		return fmt.Errorf("conversations: delete attachments: %w", err)
	}
	return nil
}

// GetContextState reads the two `meta` documents the context routes work
// with, plus the transcript size, in one query.
//
// It replaces GetContextAnalytics, which assembled the whole status document
// inside the repository AND invented the number at its centre:
//
//	currentTokens := msgCount * 500
//
// Every conversation therefore reported a token usage that was a function of
// its message count and of nothing else. Token counting belongs to the runtime
// that assembles a turn with the model's own tokenizer; this service does not
// run it. The repository now returns what the database actually holds and lets
// contextsettings.BuildStatus report the absence — see
// contextsettings.AnalyticsUnavailableReason.
func (r *ConversationsRepo) GetContextState(ctx context.Context, projectID, conversationID string) (contextsettings.ConversationState, error) {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return contextsettings.ConversationState{}, err
	}

	q := fmt.Sprintf(`
		SELECT COALESCE(c.meta, '{}'::jsonb) -> 'context_strategy',
			COALESCE(c.meta, '{}'::jsonb) -> 'context_analytics',
			(SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = c.id)
		FROM %s.chat_conversations c WHERE c.id = $1`, s, s)

	var state contextsettings.ConversationState
	if err := r.pool.QueryRow(ctx, q, id).Scan(&state.Strategy, &state.Analytics, &state.MessageGroupsTotal); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return contextsettings.ConversationState{}, apierr.NotFound("conversation not found")
		}
		return contextsettings.ConversationState{}, fmt.Errorf("conversations: get context state: %w", err)
	}
	return state, nil
}

// UpdateContextStrategy replaces `meta.context_strategy` with one RESOLVED
// strategy document.
//
// It takes the typed value rather than the request body on purpose. The route
// used to write the raw body straight into the column, so a partial form —
// which is the only kind the UI sends — silently dropped every key it did not
// mention (apps/elitea-web's context-budget widget carries a comment warning
// its future editor about exactly that). Merging happens once, in the handler,
// against the resolved strategy; what reaches the column is always complete.
func (r *ConversationsRepo) UpdateContextStrategy(ctx context.Context, projectID, conversationID string, strategy contextsettings.Strategy) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(strategy)
	if err != nil {
		return fmt.Errorf("conversations: encode context strategy: %w", err)
	}
	q := fmt.Sprintf(`UPDATE %s.chat_conversations SET meta = jsonb_set(COALESCE(meta, '{}')::jsonb, '{context_strategy}', $1::jsonb) WHERE id = $2`, s)
	if _, err := r.pool.Exec(ctx, q, data, id); err != nil {
		return fmt.Errorf("conversations: update context strategy: %w", err)
	}
	return nil
}

func (r *ConversationsRepo) DeleteMessages(ctx context.Context, projectID, conversationID string) error {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return err
	}

	// DEFECT #599: this used to delete from `chat_messages`, which no
	// migration in this repository creates — the transcript lives in
	// chat_message_group -> chat_message_items -> chat_messages_text
	// (migrations/tenant/0123_agent_chat_message_tables.sql), the same graph
	// ListMessages reads. On a clean install the statement raised 42P01, so
	// clearing a conversation answered 500; where pylon had created the legacy
	// table it succeeded and cleared nothing the user could see. Deleting the
	// group graph is what "clear this conversation's messages" means.
	//
	// chat_messages_text and chat_messages_context cascade from
	// chat_message_items, and chat_message_trace_step cascades from
	// chat_message_group, so two statements cover the whole subtree.
	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("conversations: delete messages: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	items := fmt.Sprintf(`DELETE FROM %s.chat_message_items
		WHERE message_group_id IN (SELECT id FROM %s.chat_message_group WHERE conversation_id = $1)`, s, s)
	if _, err := transaction.Exec(ctx, items, id); err != nil {
		return fmt.Errorf("conversations: delete message items: %w", err)
	}
	groups := fmt.Sprintf(`DELETE FROM %s.chat_message_group WHERE conversation_id = $1`, s)
	if _, err := transaction.Exec(ctx, groups, id); err != nil {
		return fmt.Errorf("conversations: delete message groups: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("conversations: delete messages commit: %w", err)
	}
	return nil
}

// DeleteMessage removes one message group and the user input it answers,
// addressed by the group's UUID, on behalf of userID. It reports the UUIDs of
// every group it actually removed, newest first.
//
// DEFECT #602: this used to read
//
//	DELETE FROM %s.chat_messages WHERE group_uid = $1
//
// which could not work on ANY deployment. `chat_messages` does not exist on a
// database this repository provisioned (42P01), and on a legacy pylon database,
// where it does exist, it has no `group_uid` column (42703) — see the catalog
// dump at testdata/postgres/legacy-centry-catalog.json. The identifier the
// route carries is a chat_message_group UUID; pylon's own handler
// (legacy/plugins/elitea_core/api/v2/message.py:80-83) resolves it that way.
//
// # Why the pair goes together
//
// A turn is two groups: the user's question, and the reply that points back at
// it through reply_to_id. Deleting only the reply leaves a question the model
// will be re-sent on the next turn with no answer beneath it — a transcript
// that reads as though the assistant ignored the user. Pylon deletes both
// (message.py:129-146) and that is the behaviour the chat UI was built around.
//
// The pair is followed in ONE direction only: from this group to the group it
// replies to. Deleting a user question does NOT take its answer, because
// reply_to_id points backwards and the answer is the later group — which the
// last-only rule already refuses to skip past.
//
// # The three rules, ported from pylon
//
// Restoring the delete without them would have been worse than leaving it
// broken: an unconditional delete-by-uuid lets any member of a project with the
// `models.chat.messages.delete` permission remove anyone's message from anyone's
// conversation, and lets a message be removed from the middle of a transcript.
//
//  1. AUTHOR (message.py:91-101). The caller must own the conversation, or be
//     the user who authored this group. A group's author is a participant row,
//     and a participant is only a person when entity_name is 'user' — an
//     'application' participant's entity_meta.id is an AGENT id, so comparing a
//     user id against it would let user 42 delete the messages of agent 42.
//     That is why the entity_name check is part of the predicate rather than an
//     optimisation. Pylon authorises on the named group only, not on its pair,
//     and so does this.
//
//  2. SUMMARIZED (message.py:108-112, and again at :135-139 for the pair). A
//     group whose meta says context.included is false has been folded into a
//     summary; its text is already represented elsewhere in the context window,
//     so deleting the row would not remove the content and would desynchronise
//     the summary from the transcript it summarises. Checked on BOTH groups: if
//     the paired question is summarized, the whole delete is refused rather
//     than half-done, because removing the answer alone is the outcome the
//     pairing exists to prevent.
//
//  3. LAST ONLY (message.py:114-122). Deleting from the middle of a transcript
//     leaves the model with a conversation that never happened. Pylon ordered
//     this check by created_at alone, which is nondeterministic here for the
//     same reason ListMessages needs its id tiebreaker: created_at defaults to
//     a transaction-scoped now(), so a turn's two groups share a timestamp to
//     the microsecond and "the last message" was whichever row Postgres
//     happened to return. This orders by (created_at, id) so the answer is
//     stable, and so it agrees with the order ListMessages renders. The rule
//     applies to the NAMED group; its pair is by construction the group before
//     it, which is exactly what the rule would otherwise forbid.
//
// STATUS CODES DIVERGE FROM PYLON DELIBERATELY. Pylon answered 400 for all
// three (and for "not found"). Here they are 403, 400 and 400, with 404 for a
// group that does not exist. Nothing switches on the status — the web client
// renders the message body (useApplicationChatStreaming.hooks.ts:163-171) — and
// a permission failure reported as "bad request" is the kind of thing that
// sends the next reader looking for a malformed payload.
//
// PORTED SINCE: pylon's attachment cleanup (`delete_attachment`). This method
// reports the refs (DeleteMessageResult.Attachments, collected below) and
// Handler.DeleteMessage removes the bytes when the request carries the flag —
// including the deliberate fix to pylon's ordering, which deletes the files
// BEFORE its own guards run.
//
// STILL NOT PORTED: the per-group socket event, which the returned UUIDs
// replace for the one client that exists.
func (r *ConversationsRepo) DeleteMessage(ctx context.Context, projectID, groupUID, userID string) (conversations.DeleteMessageResult, error) {
	s := schema(projectID)
	var result conversations.DeleteMessageResult
	if _, err := uuid.Parse(groupUID); err != nil {
		return result, apierr.NotFound("message not found")
	}

	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return result, fmt.Errorf("conversations: delete message: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	// One statement answers all three rules and finds the pair, so nothing can
	// change between the check and the delete: the transaction plus a single
	// read keeps "is this the last group?" true at the moment it is acted on.
	//
	// The paired columns are LEFT JOINed — a group with no reply_to_id, or one
	// whose target has already gone, is an ordinary single delete, not an
	// error.
	inspect := fmt.Sprintf(`
		SELECT mg.id,
			conv.author_id::text,
			author.entity_name,
			COALESCE(author.entity_meta->>'id', ''),
			COALESCE(mg.meta #>> '{context,included}', 'true'),
			NOT EXISTS (
				SELECT 1 FROM %s.chat_message_group later
				WHERE later.conversation_id = mg.conversation_id
				  AND (later.created_at, later.id) > (mg.created_at, mg.id)
			),
			paired.id,
			COALESCE(paired.uuid::text, ''),
			COALESCE(paired.meta #>> '{context,included}', 'true')
		FROM %s.chat_message_group mg
		JOIN %s.chat_conversations conv ON conv.id = mg.conversation_id
		JOIN %s.chat_participants author ON author.id = mg.author_participant_id
		LEFT JOIN %s.chat_message_group paired ON paired.id = mg.reply_to_id
		WHERE mg.uuid = $1::uuid`, s, s, s, s, s)

	var groupID int64
	var conversationAuthorID, authorEntityName, authorEntityID, contextIncluded string
	var isLast bool
	var pairedID *int64
	var pairedUUID, pairedContextIncluded string
	if err := transaction.QueryRow(ctx, inspect, groupUID).Scan(
		&groupID, &conversationAuthorID, &authorEntityName, &authorEntityID, &contextIncluded, &isLast,
		&pairedID, &pairedUUID, &pairedContextIncluded,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, apierr.NotFound("message not found")
		}
		return result, fmt.Errorf("conversations: inspect message: %w", err)
	}

	// An empty userID is an unauthenticated caller. It must not match an empty
	// author id either, so it is refused before the comparison rather than by
	// it.
	if userID == "" {
		return result, apierr.Forbidden("message can be deleted only by the message or conversation author")
	}
	authoredByCaller := authorEntityName == "user" && authorEntityID == userID
	if userID != conversationAuthorID && !authoredByCaller {
		return result, apierr.Forbidden("message can be deleted only by the message or conversation author")
	}
	if contextIncluded == "false" {
		return result, apierr.BadRequest("a summarized message cannot be deleted")
	}
	if pairedID != nil && pairedContextIncluded == "false" {
		return result, apierr.BadRequest("a summarized message cannot be deleted")
	}
	if !isLast {
		return result, apierr.BadRequest("only the last message in the conversation can be deleted")
	}

	doomed := []int64{groupID}
	deleted := []string{groupUID}
	if pairedID != nil {
		doomed = append(doomed, *pairedID)
		deleted = append(deleted, pairedUUID)
	}

	// Read the attachments BEFORE anything is deleted. `chat_messages_attachment`
	// cascades from `chat_message_items` (tenant migration 0127), so the item
	// delete below takes these rows with it — after that there is nothing left
	// naming the stored files, and the handler could not clean them up even if
	// asked. The bytes themselves are the handler's to remove, and only once
	// this transaction has committed; see Handler.DeleteMessage for why that
	// ordering deliberately differs from pylon's.
	//
	// Selected by `item_type` as well as by the join, because the discriminator
	// and the payload table are two statements of the same fact and this is the
	// place a disagreement between them would show up as silent data loss.
	// Shared with ConversationsRepo.Delete, which needs the same refs for the
	// whole conversation — see collectGroupAttachments.
	result.Attachments, err = collectGroupAttachments(ctx, transaction, s, doomed)
	if err != nil {
		return result, err
	}

	// chat_message_items does not cascade from chat_message_group, so the items
	// go first. chat_messages_text/_context/_attachment cascade from the items,
	// and chat_message_trace_step cascades from the group.
	items := fmt.Sprintf(`DELETE FROM %s.chat_message_items WHERE message_group_id = ANY($1)`, s)
	if _, err := transaction.Exec(ctx, items, doomed); err != nil {
		return result, fmt.Errorf("conversations: delete message items: %w", err)
	}

	// reply_to_id points at another group. Detaching references from groups that
	// are NOT themselves doomed is what lets these rows go without failing the
	// FK; the reply between the two doomed groups needs no detaching, because
	// both disappear in the same statement.
	detach := fmt.Sprintf(`UPDATE %s.chat_message_group SET reply_to_id = NULL
		WHERE reply_to_id = ANY($1) AND NOT (id = ANY($1))`, s)
	if _, err := transaction.Exec(ctx, detach, doomed); err != nil {
		return result, fmt.Errorf("conversations: detach message replies: %w", err)
	}

	ct, err := transaction.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.chat_message_group WHERE id = ANY($1)`, s), doomed)
	if err != nil {
		return result, fmt.Errorf("conversations: delete message: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return result, apierr.NotFound("message not found")
	}
	if err := transaction.Commit(ctx); err != nil {
		return result, fmt.Errorf("conversations: delete message commit: %w", err)
	}
	result.Deleted = deleted
	return result, nil
}

// ListMessages serves the transcript window the caller asked for.
//
// DEFECT #603: the window used to be computed from `page`/`page_size`, a pair
// no caller sends — see conversations.MessagesQuery for the client and pylon
// evidence — so `offset` never advanced and `sort_order` never applied. The
// parameters now arrive resolved, as limit/offset/sort, which is the shape
// pylon applied them in: `.order_by(sorting(sorting_by), sorting(id))
// .limit(limit).offset(offset)` (messages.py:98-100).
func (r *ConversationsRepo) ListMessages(ctx context.Context, projectID, conversationID string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
	s := schema(projectID)

	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return conversations.MessagesListResponse{}, err
	}

	// Every failure below propagates. Returning an empty, SUCCESSFUL response
	// instead — what this method used to do — is what turned the #599 type
	// error into apparent data loss: a broken query and an empty conversation
	// became indistinguishable, so nobody saw a failure to investigate.
	// The free-text filter, and why it is an EXISTS rather than pylon's JOIN.
	//
	// Pylon wrote `query.join(TextMessageItem, ...).filter(content.ilike(...))`
	// (messages.py:86-91). A group with two matching text items therefore
	// appeared TWICE in its result and was counted twice by the `total` on the
	// next line — the join multiplies the group by its matching items. EXISTS
	// asks the same question ("does this group contain matching text?") and
	// answers it once per group, so a group is one row and `total` is a count
	// of groups, which is what the envelope's `total` claims to be everywhere
	// else.
	//
	// The term is escaped. `%` and `_` are ILIKE metacharacters, so an
	// unescaped search for `50%` or `a_b` silently matches far more than the
	// user typed; pylon interpolated the raw term and had exactly that
	// behaviour. Nothing sends `query` today — the web client never sets it —
	// so there is no client relying on the wildcard reading, and a literal
	// search is what a search box means. `\` is escaped first, or it would
	// escape the escapes added after it.
	filter, filterArgs := "", []any{}
	if query.Query != "" {
		pattern := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query.Query)
		filter = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM %s.chat_message_items mi
			JOIN %s.chat_messages_text mt ON mt.id = mi.id
			WHERE mi.message_group_id = mg.id
			  AND mi.item_type = 'text_message'
			  AND mt.content ILIKE $%%d ESCAPE '\')`, s, s)
		filterArgs = append(filterArgs, "%"+pattern+"%")
	}

	// `total` counts the FILTERED set, as pylon's did — it is computed after the
	// filter is applied (messages.py:93). A total that ignored the filter would
	// make total_pages describe a different result set than items.
	var total int
	countFilter := ""
	countArgs := []any{id}
	if filter != "" {
		countFilter = fmt.Sprintf(filter, 2)
		countArgs = append(countArgs, filterArgs...)
	}
	countQ := fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_message_group mg WHERE mg.conversation_id = $1%s`, s, countFilter)
	if err := r.pool.QueryRow(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return conversations.MessagesListResponse{}, fmt.Errorf("conversations: count messages: %w", err)
	}

	// The handler resolves these, but this method is reachable from anything
	// holding the repository, and a limit of 0 would answer an empty transcript
	// for a conversation that has one — the exact failure shape #599 was.
	limit := query.Limit
	if limit < 1 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	offset := query.Offset
	if offset < 0 {
		offset = 0
	}

	// sort_by names a COLUMN, so it cannot be bound as a parameter — it is
	// concatenated into the statement below. Only a value this map produces is
	// ever interpolated, so an attacker-supplied sort_by reaches Postgres as
	// `created_at` and nothing else. Pylon resolved it with
	// `getattr(ConversationMessageGroup, sort_by)` (messages.py:75), which
	// accepted any attribute of the model and raised AttributeError -> 500 on
	// anything else; in Python that was merely rude, in this form it would be
	// an injection. The three entries are the columns of chat_message_group
	// (migrations/tenant/0123_agent_chat_message_tables.sql:93-105) that a
	// transcript can meaningfully be ordered by.
	sortColumn := "created_at"
	switch query.SortBy {
	case "created_at", "updated_at", "id":
		sortColumn = query.SortBy
	}

	direction := "DESC"
	if query.SortOrder == "asc" {
		direction = "ASC"
	}

	// The `id` tiebreaker is pylon's, and it is load-bearing rather than
	// decorative: created_at defaults to now(), which in Postgres is
	// transaction-scoped, so every group written by one transaction — the
	// user-input/assistant-reply pair among them — shares a timestamp to the
	// microsecond. Without it the two halves of a turn can come back in either
	// order, and worse, a row can appear on two consecutive offset pages while
	// another appears on neither. Ordering BY id when id is already the sort
	// key would just repeat the clause.
	orderBy := fmt.Sprintf("mg.%s %s, mg.id %s", sortColumn, direction, direction)
	if sortColumn == "id" {
		orderBy = fmt.Sprintf("mg.id %s", direction)
	}

	// $1..$3 are taken by the conversation id, the limit and the offset, so the
	// filter's placeholder is $4 here and $2 in the count above. Same clause,
	// different position — hence the %d the builder left in it.
	pageFilter := ""
	if filter != "" {
		pageFilter = fmt.Sprintf(filter, 4)
	}

	q := fmt.Sprintf(`
		SELECT mg.id, mg.conversation_id, COALESCE(mg.uuid::text, ''),
			p.entity_name, mg.meta, mg.created_at,
			mg.author_participant_id, mg.sent_to_id, mg.reply_to_id,
			COALESCE((
				SELECT string_agg(mt.content, E'\n' ORDER BY mi.order_index)
				FROM %s.chat_message_items mi
				JOIN %s.chat_messages_text mt ON mt.id = mi.id
				WHERE mi.message_group_id = mg.id AND mi.item_type = 'text_message'
			), '')
		FROM %s.chat_message_group mg
		JOIN %s.chat_participants p ON p.id = mg.author_participant_id
		WHERE mg.conversation_id = $1%s
		ORDER BY %s
		LIMIT $2 OFFSET $3`, s, s, s, s, pageFilter, orderBy)

	rows, err := r.pool.Query(ctx, q, append([]any{id, limit, offset}, filterArgs...)...)
	if err != nil {
		return conversations.MessagesListResponse{}, fmt.Errorf("conversations: list messages: %w", err)
	}
	defer rows.Close()

	items := []conversations.Message{}
	// Index-aligned with `items`: the numeric group id each row was built
	// from, which the attachment projection below keys on. Kept beside the
	// slice rather than re-parsed out of `Message.ID` (a string on the wire)
	// so the join reads the id the database returned, not a round trip
	// through its decimal spelling.
	groupIDs := []int{}
	for rows.Next() {
		var m conversations.Message
		var meta []byte
		var entityName string
		var groupID int
		// A scan failure used to `continue`, so an unreadable row silently
		// dropped a message out of the transcript.
		if err := rows.Scan(&groupID, &m.ConversationID, &m.UUID, &entityName, &meta, &m.CreatedAt,
			&m.AuthorParticipantID, &m.SentToID, &m.ReplyToID, &m.Content); err != nil {
			return conversations.MessagesListResponse{}, fmt.Errorf("conversations: scan message: %w", err)
		}
		m.ID = strconv.Itoa(groupID)
		if meta != nil {
			_ = json.Unmarshal(meta, &m.Metadata) // best-effort: DB column is trusted JSON
		}
		// Map entity_name to role
		if entityName == "user" {
			m.Role = "user"
		} else {
			m.Role = "assistant"
		}
		m.ContentType = "text"
		items = append(items, m)
		groupIDs = append(groupIDs, groupID)
	}
	if err := rows.Err(); err != nil {
		return conversations.MessagesListResponse{}, fmt.Errorf("conversations: list messages: %w", err)
	}

	// The files each question was sent with (#606 read path, part 2).
	//
	// This projection is what the CHAT PAGE reads: useChatPageData.ts hands
	// these rows to ChatBox as `message_groups`, and `UserMessage`'s
	// `findAttachmentItems` filters them for `attachment_message` items. Until
	// this join existed the route answered every row with no items at all, so a
	// reloaded conversation showed the question and silently dropped the file
	// that rode it — while the details route, reading the SAME rows through
	// ListMessageGroups, returned it. Two projections of one transcript
	// disagreeing is the defect; the second one is now this.
	//
	// TEXT ITEMS ARE DELIBERATELY NOT INCLUDED. This route already collapses
	// each group's text into `content` (the string_agg above), which every
	// client reads as the message body; re-emitting the same text as items
	// would give two sources for one sentence and let them drift. Attachments
	// have no such representation here — they exist in this response only as
	// items — so they are what this carries.
	if len(groupIDs) > 0 {
		byGroup, err := r.attachmentItemsByGroup(ctx, s, groupIDs)
		if err != nil {
			return conversations.MessagesListResponse{}, err
		}
		for i := range items {
			if attachments := byGroup[groupIDs[i]]; len(attachments) > 0 {
				items[i].MessageItems = attachments
			}
		}
	}

	totalPages := total / limit
	if total%limit > 0 {
		totalPages++
	}

	// The envelope keeps its {items,total,page,page_size,total_pages} shape:
	// apps/elitea-web/e2e/streaming/chat.streaming.spec.ts:212 reads
	// `body.items`, so switching to pylon's {rows,total} would trade this
	// defect for a red e2e. A limit/offset caller never named a page, so both
	// fields are DERIVED from the window actually served — page_size is the
	// limit, and page is the 1-based index of the page containing the first row
	// returned. For a page/page_size caller that round-trips exactly. For an
	// offset that is not a multiple of the limit it is the nearest true
	// statement available, and it is reported rather than omitted because a
	// zero there would read as "page 0 of N", which is false for every request.
	page := offset/limit + 1

	return conversations.MessagesListResponse{
		Items:      items,
		Total:      total,
		Page:       page,
		PageSize:   limit,
		TotalPages: totalPages,
	}, nil
}

// attachmentItemsByGroup projects the `attachment_message` items of the given
// message groups, in the SAME shape ListMessageGroups embeds them in — item id,
// discriminator, order index and the `attachmentItemDetails` payload — so a
// client that already renders the details route's items needs no second reader
// for this one. Two shapes for one item is how a renderer ends up branching on
// which endpoint it was handed.
//
// INNER JOIN, not LEFT. An item whose discriminator says `attachment_message`
// but which has no chat_messages_attachment row is not an attachment with empty
// fields, it is a broken row; joining it in with nil name/bucket would hang an
// unaddressable `filepath: "//"` off a message and point the client's download
// at artifact storage for a file that does not exist. It is dropped instead,
// which is the same "the row is required as well as the item_type" rule
// ListMessageGroups states for its own LEFT-joined equivalent.
//
// A query failure PROPAGATES. `chat_messages_attachment` is a tenant table
// (migrations/tenant/0127) and a schema where it is missing answers 42P01 here;
// reporting that as "this conversation's messages carry no files" is the #599
// failure shape — an empty successful answer that a caller cannot tell from the
// truth — and it is exactly how the attachment gap stayed invisible on the read
// side for as long as it did.
func (r *ConversationsRepo) attachmentItemsByGroup(ctx context.Context, s string, groupIDs []int) (map[int][]map[string]any, error) {
	q := fmt.Sprintf(`
		SELECT mi.message_group_id, mi.id, mi.item_type, mi.order_index,
			ma.name, ma.bucket, ma.attachment_type, ma.content
		FROM %s.chat_message_items mi
		JOIN %s.chat_messages_attachment ma ON ma.id = mi.id
		WHERE mi.message_group_id = ANY($1) AND mi.item_type = 'attachment_message'
		ORDER BY mi.message_group_id, mi.order_index`, s, s)

	rows, err := r.pool.Query(ctx, q, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("conversations: list message attachments: %w", err)
	}
	defer rows.Close()

	byGroup := map[int][]map[string]any{}
	for rows.Next() {
		var groupID, itemID, orderIndex int
		var itemType, name, bucket, attachmentType string
		var content []byte
		if err := rows.Scan(&groupID, &itemID, &itemType, &orderIndex, &name, &bucket, &attachmentType, &content); err != nil {
			return nil, fmt.Errorf("conversations: scan message attachment: %w", err)
		}
		byGroup[groupID] = append(byGroup[groupID], map[string]any{
			"id":           itemID,
			"item_type":    itemType,
			"order_index":  orderIndex,
			"item_details": attachmentItemDetails(itemID, itemType, name, bucket, attachmentType, content),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversations: list message attachments: %w", err)
	}
	return byGroup, nil
}

func (r *ConversationsRepo) ListMessageGroups(ctx context.Context, projectID, conversationID string, limit int, sortOrder string) ([]map[string]any, error) {
	s := schema(projectID)
	id, err := r.resolveConversationID(ctx, projectID, conversationID)
	if err != nil {
		return nil, err
	}

	order := "DESC"
	if sortOrder == "asc" {
		order = "ASC"
	}

	q := fmt.Sprintf(`
		SELECT mg.id, COALESCE(mg.uuid::text, ''), mg.author_participant_id,
			mg.sent_to_id, mg.reply_to_id, mg.meta, mg.is_streaming,
			mg.created_at, mg.updated_at, mg.task_id
		FROM %s.chat_message_group mg
		WHERE mg.conversation_id = $1
		ORDER BY mg.created_at %s
		LIMIT $2`, s, order)

	rows, err := r.pool.Query(ctx, q, id, limit)
	if err != nil {
		return []map[string]any{}, nil
	}
	defer rows.Close()

	var groups []map[string]any
	var groupIDs []int
	for rows.Next() {
		var id int
		var uuid string
		var authorParticipantID int
		var sentToID, replyToID *int
		var meta []byte
		var isStreaming bool
		var createdAt time.Time
		var updatedAt *time.Time
		var taskID *string

		if err := rows.Scan(&id, &uuid, &authorParticipantID, &sentToID, &replyToID, &meta, &isStreaming, &createdAt, &updatedAt, &taskID); err != nil {
			continue
		}

		var metaObj map[string]any
		if meta != nil {
			_ = json.Unmarshal(meta, &metaObj) // best-effort: DB column is trusted JSON
		}
		if metaObj == nil {
			metaObj = map[string]any{}
		}

		group := map[string]any{
			"id":                    id,
			"uuid":                  uuid,
			"author_participant_id": authorParticipantID,
			"meta":                  metaObj,
			"is_streaming":          isStreaming,
			"created_at":            createdAt.Format("2006-01-02 15:04:05.999999"),
			"message_items":         []map[string]any{},
		}
		if sentToID != nil {
			group["sent_to_id"] = *sentToID
		}
		if replyToID != nil {
			group["reply_to_id"] = *replyToID
		}
		if updatedAt != nil {
			group["updated_at"] = updatedAt.Format("2006-01-02 15:04:05.999999")
		}
		if taskID != nil {
			group["task_id"] = *taskID
		}

		groups = append(groups, group)
		groupIDs = append(groupIDs, id)
	}

	if len(groupIDs) == 0 {
		return []map[string]any{}, nil
	}

	// Fetch message_items with their payload for all groups.
	//
	// #606: the attachment join is what makes an uploaded file appear inline in
	// the transcript at all. Before it, an attachment row existed (0127) but
	// this query only ever joined chat_messages_text, so an `attachment_message`
	// item came back with no `item_details` whatsoever — the web client's
	// `extractMessageItemText` rendered it as the literal `[undefined]`
	// (apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxHandlers.helpers.ts:265)
	// and `MessageAttachmentList` had no filepath to download.
	//
	// The text column keeps its COALESCE and the attachment columns do not:
	// `mt.content` is NOT NULL in its own table, so an empty string is
	// unambiguously "no text item here" and scanning it into a plain string is
	// safe. The attachment columns are NOT NULL in THEIR table too, but that
	// says nothing about this LEFT JOIN — every one of them is NULL for a text
	// item — and here the difference between "absent" and "present but empty"
	// is load-bearing: COALESCE'ing `ma.name` to '' would make a text item
	// indistinguishable from an attachment whose name really is empty, and this
	// function must not hang empty attachment keys off a text item. So they are
	// scanned as pointers and nil means "no attachment row", full stop.
	// The canvas join is the same class of gap #606 closed for attachments:
	// a `canvas_message` item came back with NO `item_details` at all, so the
	// text a user moved into a canvas was invisible to every reader of a
	// conversation — the transcript route drops it (it aggregates
	// `text_message` items alone, deliberately) and this route carried
	// nothing in its place. The canvas was written and then unreadable.
	itemQ := fmt.Sprintf(`
		SELECT mi.id, COALESCE(mi.uuid::text, ''), mi.message_group_id, mi.item_type, mi.order_index, mi.meta,
			COALESCE(mt.content, ''),
			ma.name, ma.bucket, ma.attachment_type, ma.content,
			cv.name, cv.canvas_type, ver.id, ver.canvas_content, ver.code_language, ver.created_at
		FROM %s.chat_message_items mi
		LEFT JOIN %s.chat_messages_text mt ON mt.id = mi.id
		LEFT JOIN %s.chat_messages_attachment ma ON ma.id = mi.id
		LEFT JOIN %s.chat_messages_canvas cv ON cv.id = mi.id
		LEFT JOIN LATERAL (
			SELECT id, canvas_content, code_language, created_at
			FROM %s.chat_canvas_versions v
			WHERE v.canvas_item_id = mi.id
			ORDER BY v.created_at DESC, v.id DESC
			LIMIT 1
		) ver ON true
		WHERE mi.message_group_id = ANY($1)
		ORDER BY mi.message_group_id, mi.order_index`, s, s, s, s, s)

	// PROPAGATED, not swallowed. `if err == nil { ... }` returned every group
	// with an empty `message_items` and a 200, which a caller cannot tell from
	// groups that genuinely have no items — the #599 defect class, in the
	// function that now also joins chat_messages_attachment. A database where
	// tenant 0127 has not run answers 42P01 here, and reporting that as "this
	// conversation has no messages" is how the original defect stayed invisible
	// for as long as it did.
	itemRows, err := r.pool.Query(ctx, itemQ, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("conversations: list message items: %w", err)
	}
	{
		defer itemRows.Close()
		// Index groups by id for fast lookup
		groupIndex := map[int]int{}
		for i, g := range groups {
			gid, _ := g["id"].(int)
			groupIndex[gid] = i
		}

		for itemRows.Next() {
			var itemID, groupID int
			var itemUUID string
			var itemType string
			var orderIndex int
			var itemMeta []byte
			var textContent string
			var attachmentName, attachmentBucket, attachmentType *string
			var attachmentContent []byte
			var canvasName, canvasType, canvasContent, canvasLanguage *string
			var canvasVersionID *int
			var canvasCreatedAt *time.Time

			// A scan failure used to `continue`, silently dropping one message
			// from the transcript.
			if err := itemRows.Scan(&itemID, &itemUUID, &groupID, &itemType, &orderIndex, &itemMeta, &textContent,
				&attachmentName, &attachmentBucket, &attachmentType, &attachmentContent,
				&canvasName, &canvasType, &canvasVersionID, &canvasContent, &canvasLanguage, &canvasCreatedAt); err != nil {
				return nil, fmt.Errorf("conversations: scan message item: %w", err)
			}

			item := map[string]any{
				"id":          itemID,
				"item_type":   itemType,
				"order_index": orderIndex,
			}

			if itemType == "text_message" {
				item["item_details"] = map[string]any{"content": textContent}
			}

			// The discriminator is `attachment_message`, not `attachment`
			// (elitea_core/models/message_items/attachment.py:15-17
			// `polymorphic_identity`); 0127 records the same. The row is
			// required as well as the item_type, so an item mislabelled in the
			// database yields no item_details rather than a map of nils.
			if itemType == "attachment_message" && attachmentName != nil && attachmentBucket != nil && attachmentType != nil {
				item["item_details"] = attachmentItemDetails(itemID, itemType, *attachmentName, *attachmentBucket, *attachmentType, attachmentContent)
			}

			// The item's own uuid rides INSIDE `item_details`, never beside
			// `item_type`: `TestListMessagesCarriesTheGroupsAttachmentItem`
			// pins that this projection and the transcript route's serve one
			// shape, and a key only one of them carries is exactly the drift
			// that test exists to catch.
			//
			// The canvas row is required as well as the item_type, for the
			// reason the attachment branch states: an item mislabelled in the
			// database yields no details rather than a map of nils. A canvas
			// with no version yet still gets its details, carrying an empty
			// document — the client reads
			// `item_details.latest_version.canvas_content` and an absent
			// `latest_version` there is a crash, not an empty editor.
			if itemType == "canvas_message" && canvasName != nil && canvasType != nil {
				item["item_details"] = canvasItemDetails(canvasItemDetailsInput{
					itemID:    itemID,
					itemUUID:  itemUUID,
					name:      *canvasName,
					canvasT:   *canvasType,
					versionID: canvasVersionID,
					content:   canvasContent,
					language:  canvasLanguage,
					createdAt: canvasCreatedAt,
				})
			}

			if idx, ok := groupIndex[groupID]; ok {
				items := groups[idx]["message_items"].([]map[string]any)
				groups[idx]["message_items"] = append(items, item)
			}
		}
		// A mid-iteration failure ends the loop without ever reporting itself,
		// which is the same "short transcript, HTTP 200" outcome by another
		// route.
		if err := itemRows.Err(); err != nil {
			return nil, fmt.Errorf("conversations: list message items: %w", err)
		}
	}

	// Also set "content" on each group from its text items (for backward compat).
	//
	// #606: this stays keyed on `text_message` deliberately. An attachment now
	// carries an `item_details` too, so widening the filter — or dropping it
	// and joining every item's details — would splice an attachment's payload
	// into the message's rendered text. It cannot happen by accident either:
	// an attachment's `item_details["content"]` is a decoded JSON value, never
	// a string, so the assertion below would reject it. The item_type filter is
	// the intent; the assertion is the second line of defence.
	for i, g := range groups {
		items := g["message_items"].([]map[string]any)
		var content string
		for _, item := range items {
			if item["item_type"] == "text_message" {
				if details, ok := item["item_details"].(map[string]any); ok {
					if c, ok := details["content"].(string); ok {
						if content != "" {
							content += "\n"
						}
						content += c
					}
				}
			}
		}
		groups[i]["content"] = content
	}

	return groups, nil
}

// attachmentItemDetails shapes one `attachment_message` item's payload for the
// transcript (#606).
//
// KEYS. Pylon's own API returns exactly {filepath, attachment_type, content,
// id, item_type} — `filepath` being the computed property "/" + bucket + "/" +
// name (elitea_core/models/pd/attachment.py:32-43,
// models/message_items/attachment.py:29-32) — and never exposes `bucket` or
// `name` separately. This function emits those five AND `name` and `bucket`,
// a deliberate divergence, because the web client reads both directly:
//
//   - `name`   — useChatBoxHandlers.helpers.ts:265 renders an attachment in the
//     transcript as `[${details?.name}]`, PlaybackChatBox.tsx:283-287 keys the
//     playback toolbar on it, and entities/attachment's `getAttachmentName`
//     prefers it over the filepath basename. Omitting it renders `[undefined]`.
//   - `bucket` — chat-input/ui/imageAttachment.helpers.ts:78 chooses the
//     download path with `details?.bucket !== '__undefined__'`; without the key
//     that sentinel check can never fire and a sentinel-bucket attachment is
//     routed to artifact storage, which has nothing to serve.
//
// `id` is the message ITEM's id (shared with the attachment row by the 1:1
// primary key), which is what PlaybackChatBox.tsx:286 keys on.
//
// CONTENT IS DECODED, AND NEVER NULL. The column is `json` holding a LIST of
// content chunks in pylon, which the client walks looking for an `image_url`
// entry (entities/attachment/model/selectors.ts:36-44). Handing it back as a
// string would make that walk see one opaque scalar and every inline image
// would stop rendering, so it is unmarshalled to `any` and re-encoded as JSON.
//
// An absent or unparseable content emits `[]`, not null and not an omission.
// That is not cosmetic: the client's `findContentByType` tests
// `content === undefined` and then `Array.isArray(content)`, and falls through
// to `content.type` — so a JSON `null` on the wire is neither undefined nor an
// array and throws a TypeError reading `type` of null, taking the whole
// message list down. `[]` and omission are both safe; `[]` is chosen so the
// key's type is invariant across every attachment, which is one less shape for
// a client to branch on.
func attachmentItemDetails(itemID int, itemType, name, bucket, attachmentType string, content []byte) map[string]any {
	var decoded any = []any{}
	if len(content) > 0 {
		var parsed any
		if err := json.Unmarshal(content, &parsed); err == nil && parsed != nil {
			decoded = parsed
		}
	}
	return map[string]any{
		"id":              itemID,
		"item_type":       itemType,
		"name":            name,
		"bucket":          bucket,
		"filepath":        "/" + bucket + "/" + name,
		"attachment_type": attachmentType,
		"content":         decoded,
	}
}

func intFromAny(v any) int {
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case int64:
		return int(val)
	case json.Number:
		n, _ := val.Int64()
		return int(n)
	}
	return 0
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
