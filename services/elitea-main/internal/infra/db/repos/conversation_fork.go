package repos

// The conversation fork (client contract 1.11, forkConversation).
//
// ONE TRANSACTION, SET-BASED. The source is locked FOR KEY SHARE (a delete
// of the source waits; a rename, a new message and a sync stamp do not), the
// fork point is checked, and then every table is copied with one
// INSERT … SELECT. Only id arrays come back to Go: the old-to-new id maps of
// the message groups and the message items, which the next statements bind
// as unnest() pairs. A group or an item gets its new id from its column
// default; its new UUID is drawn in a MATERIALIZED CTE and is how the
// RETURNING rows are matched back to their source rows. The groups'
// old-to-new UUID map is the result's MessageUUIDs: the desktop rewrites the
// message ids its copied turn history names with it.
//
// WHAT IS COPIED. The participants that are not users, and the caller's own
// user participant, with their per-conversation entity_settings (agent
// versions, model settings). Another member's user participant is NOT
// mapped: a user participant of a private conversation can read it, and the
// fork is the caller's private conversation. Their messages are copied as
// history, still authored by their participant row, so the transcript keeps
// them as user messages (the detail's `role`). Then the message groups up to
// and including the fork point, in transcript order (created_at, id), with
// their created_at, and every group that belongs to a copied group although
// it was written later: a reply (`reply_to_id`, an answer, a regenerated or
// swarm answer) or a swarm child (`meta.parent_message_id`), followed to any
// depth. Then their items; the item payloads (text, context, attachment,
// canvas and its versions); and the trace steps. `reply_to_id` and
// `meta.parent_message_id` are remapped onto the copies. The conversation's
// instructions, source, attachment participant and meta are copied; the
// folder only when it is the caller's own folder.
//
// WHAT IS NOT. The project pin and archive (other tables, keyed by the
// source id), likes and dislikes (keyed by the source groups' UUIDs), the
// per-user selection, a group's execution binding (`task_id`), and a
// pending pause: the pause keys are removed from every copied group's meta,
// and a fork point that is still streaming or paused is refused
// (ErrForkPointUnsettled), as is a later reply of the copy that is.
// chat_canvas_version_authors is not copied: no read uses it.
//
// ATTACHMENTS. An attachment item names its object `{conversationUUID}/{file}`
// and the download route derives the key from the conversation it
// authorized. The copied rows are renamed to the fork's UUID (name, content
// and the item meta), and CopyAttachments copies the objects BEFORE the
// transaction begins: the fork is planned once without locks (the source,
// the fork point, the groups and their attachments), the bytes are copied,
// and the transaction then plans again under its locks. An attachment the
// second plan names that the first did not (a reply written in between) is
// copied then, inside the transaction; that is the only store I/O that can
// run there. The two conversations then own separate objects, so deleting
// either one leaves the other readable. A name without the source prefix
// (an older writer) is copied as it is and shared; the delete paths keep an
// object that another conversation's row still names
// (collectConversationAttachments, collectGroupAttachments).
//
// REWIND AND REPLAY (issue 1234, conversation_fork_rewind.go). The marker
// also records the mode, the client's fork_id, a rewind's branch summary
// and, once the groups are copied, the copy's UUID of the fork point
// (cut_message_uuid). A fork_id seen before answers its fork, read again
// under an advisory lock on the key inside the transaction. A Local work
// source the cloud executor owns is refused, under the handoff's own lock.
// A plan of more than maxForkGroups groups is refused before anything is
// copied.
//
// SYNC. The copy runs with `elitea.sync_cascade = conversation`, as Delete
// does: the per-row parent bumps are skipped. The new rows are stamped by
// their own BEFORE triggers, and the new conversation is stamped last, so
// its list row carries the newest stamp of the copy.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

var _ conversations.ConversationForker = (*ConversationsRepo)(nil)

// conversationForkTimeout bounds one fork, the attachment copies included.
const conversationForkTimeout = 2 * time.Minute

// forkPauseKeys are the meta keys of a pending pause (agent_cancel.sql
// removes the same keys when a turn is stopped).
var forkPauseKeys = []string{"hitl_interrupt", "hitl_interrupts", "authorization_requests", "node_recovery_required_v1"}

// forkLeafTables are the optional payload tables the fork copies. Some
// fixtures and older tenants lack a few of them.
var forkLeafTables = []string{"chat_messages_text", "chat_messages_context", "chat_messages_attachment",
	"chat_messages_canvas", "chat_canvas_versions", "chat_message_trace_step"}

// forkSource is the source conversation.
type forkSource struct {
	id                      int64
	uuid                    string
	name                    string
	source                  string
	meta                    []byte
	instructions            *string
	folderID                *int64
	attachmentParticipantID *int64
}

// forkPoint is the last message group the fork cuts at.
type forkPoint struct {
	id        int64
	uuid      string
	createdAt time.Time
}

// forkPlan is what one fork copies: the source, the fork point (nil when
// the source has no messages) and the groups, in transcript order.
type forkPlan struct {
	src    forkSource
	point  *forkPoint
	groups []int64
}

// forkQuerier is the read half of a pool or a transaction.
type forkQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ForkConversation copies the source conversation into a new conversation
// of the caller. See the file comment.
func (r *ConversationsRepo) ForkConversation(ctx context.Context, projectID, conversationID string, request conversations.ForkRequest) (conversations.ForkResult, error) {
	if r.pool == nil {
		return conversations.ForkResult{}, fmt.Errorf("conversation fork storage is unavailable")
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	identity, ok := idPredicate(conversationID)
	if !ok {
		return conversations.ForkResult{}, apierr.NotFound("chat resource not found")
	}
	newUUID := request.NewUUID
	if newUUID == "" {
		newUUID = uuid.NewString()
	}
	ctx, cancel := context.WithTimeout(ctx, conversationForkTimeout)
	defer cancel()
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	tables, err := forkTables(ctx, r.pool, s)
	if err != nil {
		return conversations.ForkResult{}, err
	}

	// The first plan, without locks: it names the attachment objects, which
	// are copied before the transaction holds anything.
	plan, err := readForkPlan(ctx, r.pool, s, identity, access, conversationID, request.MessageID, false)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	// A fork_id is looked up once, under its lock in the transaction: a
	// replay copies the objects this plan names, which the handler deletes.
	copiedObjects := map[conversations.AttachmentCopy]bool{}
	copyObjects := func(copies []conversations.AttachmentCopy) error {
		var pending []conversations.AttachmentCopy
		for _, item := range copies {
			if !copiedObjects[item] {
				pending = append(pending, item)
			}
		}
		if len(pending) == 0 || request.CopyAttachments == nil {
			return nil
		}
		if err := request.CopyAttachments(ctx, pending); err != nil {
			return err
		}
		for _, item := range pending {
			copiedObjects[item] = true
		}
		return nil
	}
	if tables["chat_messages_attachment"] != nil && len(plan.groups) > 0 && request.CopyAttachments != nil {
		copies, err := forkAttachmentCopies(ctx, r.pool, s, plan.groups, plan.src.uuid, newUUID)
		if err != nil {
			return conversations.ForkResult{}, err
		}
		if err := copyObjects(copies); err != nil {
			return conversations.ForkResult{}, err
		}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return conversations.ForkResult{}, fmt.Errorf("conversations: fork: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if request.ForkID != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`,
			"elitea.conversation_fork:"+projectID+":"+request.ForkID); err != nil {
			return conversations.ForkResult{}, fmt.Errorf("conversations: fork: lock fork_id: %w", err)
		}
	}
	// The plan again, under the locks the copy relies on.
	plan, err = readForkPlan(ctx, tx, s, identity, access, conversationID, request.MessageID, true)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	src, point, groups := plan.src, plan.point, plan.groups
	if request.ForkID != "" {
		// A retry (after a lost answer, or one that raced this request):
		// its fork stands, and the objects this one copied are the
		// handler's to delete.
		if replay, found, err := replayFork(ctx, tx, s, access.ActorID, request, plan); found || err != nil {
			return replay, err
		}
	}
	if err := refuseForkWhileHandedOff(ctx, tx, projectID, src); err != nil {
		return conversations.ForkResult{}, err
	}
	if tables["chat_messages_attachment"] != nil && len(groups) > 0 && request.CopyAttachments != nil {
		copies, err := forkAttachmentCopies(ctx, tx, s, groups, src.uuid, newUUID)
		if err != nil {
			return conversations.ForkResult{}, err
		}
		// Only what changed since the first plan (see the file comment).
		if err := copyObjects(copies); err != nil {
			return conversations.ForkResult{}, err
		}
	}

	if _, err := tx.Exec(ctx, `SELECT set_config('elitea.sync_cascade', 'conversation', true)`); err != nil {
		return conversations.ForkResult{}, fmt.Errorf("conversations: fork: sync cascade: %w", err)
	}

	// The marker names a fork point only when the request named one.
	marked := point
	if request.MessageID == "" {
		marked = nil
	}
	options, err := forkMarkerExtras(ctx, tx, s, tables, request, plan)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	meta, err := forkMeta(src, marked, options)
	if err != nil {
		return conversations.ForkResult{}, err
	}
	name := request.Name
	if name == "" {
		name = defaultForkName(src.name)
	}
	var newID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`INSERT INTO %[1]s.chat_conversations
		(uuid, name, author_id, is_private, meta, source, instructions, folder_id, attachment_participant_id, created_at)
		VALUES ($1::uuid, $2, $3, TRUE, $4::jsonb, $5, $6,
		        (SELECT f.id FROM %[1]s.chat_conversation_folders f WHERE f.id = $7 AND f.owner_id = $3),
		        $8, now() AT TIME ZONE 'UTC')
		RETURNING id`, s),
		newUUID, name, access.ActorID, meta, src.source, src.instructions, src.folderID, src.attachmentParticipantID).Scan(&newID); err != nil {
		return conversations.ForkResult{}, fmt.Errorf("conversations: fork: create: %w", err)
	}

	if err := forkParticipants(ctx, tx, s, src.id, newID, access.ActorID); err != nil {
		return conversations.ForkResult{}, err
	}
	messageUUIDs := map[string]string{}
	if len(groups) > 0 {
		if messageUUIDs, err = forkMessages(ctx, tx, s, tables, groups, newID, src.uuid, newUUID); err != nil {
			return conversations.ForkResult{}, err
		}
	}

	// The newest stamp of the copy goes on the conversation (chat_sync_stamp),
	// with the copy's UUID of the fork point on the marker.
	cut := ""
	if marked != nil {
		cut = messageUUIDs[marked.uuid]
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_conversations SET updated_at = updated_at,
		meta = CASE WHEN $2::text = '' THEN meta
		            ELSE jsonb_set(meta, '{%s,cut_message_uuid}', to_jsonb($2::text)) END
		WHERE id = $1`, s, conversations.ForkedFromMetaKey), newID, cut); err != nil {
		return conversations.ForkResult{}, fmt.Errorf("conversations: fork: stamp: %w", err)
	}
	// The fork starts read for the forker (client contract 1.12): the copied
	// answers are not new to the person who copied them. Probed like the
	// payload tables, for fixtures that build part of the tenant chain.
	if err := markForkRead(ctx, tx, s, newID, access.ActorID); err != nil {
		return conversations.ForkResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return conversations.ForkResult{}, fmt.Errorf("conversations: fork commit: %w", err)
	}
	return conversations.ForkResult{ID: newID, UUID: newUUID, MessageUUIDs: messageUUIDs}, nil
}

// readForkPlan reads the source under the detail rule, the fork point and
// the groups. With lock, the source and the fork point are locked FOR KEY
// SHARE against a delete (q is then the transaction).
func readForkPlan(ctx context.Context, q forkQuerier, s, identity string, access chatauthority.Scope, conversationID, messageID string, lock bool) (forkPlan, error) {
	locking := ""
	if lock {
		locking = " FOR KEY SHARE OF c"
	}
	visible, args := access.Predicate(s, "c", 2, chatauthority.Detail)
	var plan forkPlan
	src := &plan.src
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT c.id, c.uuid::text, c.name, COALESCE(c.source, 'elitea'), c.meta,
		c.instructions, c.folder_id, c.attachment_participant_id
		FROM %s.chat_conversations c WHERE %s AND %s%s`, s, identity, visible, locking),
		append([]any{conversationID}, args...)...).
		Scan(&src.id, &src.uuid, &src.name, &src.source, &src.meta, &src.instructions, &src.folderID, &src.attachmentParticipantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return forkPlan{}, apierr.NotFound("chat resource not found")
	}
	if err != nil {
		return forkPlan{}, fmt.Errorf("conversations: fork: read source: %w", err)
	}
	if src.source == conversations.EditorTestSource {
		return forkPlan{}, conversations.ErrForkNotSupported
	}
	if plan.point, err = resolveForkPoint(ctx, q, s, src.id, messageID, lock); err != nil {
		return forkPlan{}, err
	}
	if plan.point != nil {
		if plan.groups, err = forkGroups(ctx, q, s, src.id, plan.point); err != nil {
			return forkPlan{}, err
		}
		if len(plan.groups) > maxForkGroups {
			return forkPlan{}, conversations.ErrForkTooLarge
		}
	}
	return plan, nil
}

// markForkRead writes the forker's read marker of the new conversation, up to
// its newest copied message group.
func markForkRead(ctx context.Context, tx pgx.Tx, s string, newID, actorID int64) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, s+".chat_conversation_reads").Scan(&exists); err != nil {
		return fmt.Errorf("conversations: fork: probe reads: %w", err)
	}
	if !exists {
		return nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.chat_conversation_reads
		(conversation_id, user_id, read_at, last_read_message_id)
		VALUES ($1, $2, clock_timestamp() AT TIME ZONE 'UTC',
		        (SELECT g.id FROM %[1]s.chat_message_group g WHERE g.conversation_id = $1
		          ORDER BY g.created_at DESC, g.id DESC LIMIT 1))
		ON CONFLICT (conversation_id, user_id) DO NOTHING`, s), newID, actorID); err != nil {
		return fmt.Errorf("conversations: fork: mark read: %w", err)
	}
	return nil
}

// resolveForkPoint reads the fork point: the named group of the source, or
// the newest group when messageID is empty (nil when the source has no
// messages). A group that is streaming or holds a pending pause is
// ErrForkPointUnsettled. With lock, it is locked FOR KEY SHARE against a
// delete.
func resolveForkPoint(ctx context.Context, q forkQuerier, s string, sourceID int64, messageID string, lock bool) (*forkPoint, error) {
	where := "TRUE ORDER BY g.created_at DESC, g.id DESC LIMIT 1"
	args := []any{sourceID}
	if messageID != "" {
		if isNumericRowID(messageID) {
			where = "g.id = $2::bigint"
		} else if _, err := uuid.Parse(messageID); err == nil {
			where = "g.uuid = $2::uuid"
		} else {
			return nil, apierr.NotFound("message not found")
		}
		args = append(args, messageID)
	}
	locking := ""
	if lock {
		locking = " FOR KEY SHARE OF g"
	}
	var point forkPoint
	var unsettled bool
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT g.id, g.uuid::text, g.created_at,
		g.is_streaming OR g.meta ?| $%d::text[]
		FROM %s.chat_message_group g WHERE g.conversation_id = $1 AND %s%s`, len(args)+1, s, where, locking),
		append(args, forkPauseKeys)...).Scan(&point.id, &point.uuid, &point.createdAt, &unsettled)
	if errors.Is(err, pgx.ErrNoRows) {
		if messageID == "" {
			return nil, nil
		}
		return nil, apierr.NotFound("message not found")
	}
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: read fork point: %w", err)
	}
	if unsettled {
		return nil, conversations.ErrForkPointUnsettled
	}
	return &point, nil
}

// forkGroups reads the groups the fork copies, in transcript order: those up
// to and including the fork point, and then, to any depth, every group that
// replies to a copied group or is a swarm child of one, whenever it was
// written. Such a later group that is still streaming or paused makes the
// fork point ErrForkPointUnsettled: its answer is not finished.
func forkGroups(ctx context.Context, q forkQuerier, s string, sourceID int64, point *forkPoint) ([]int64, error) {
	var groups []int64
	var unsettled bool
	if err := q.QueryRow(ctx, fmt.Sprintf(`WITH RECURSIVE copied AS (
		  SELECT g.id, g.uuid FROM %[1]s.chat_message_group g
		  WHERE g.conversation_id = $1 AND (g.created_at, g.id) <= ($2::timestamp, $3::integer)
		  UNION
		  SELECT later.id, later.uuid FROM %[1]s.chat_message_group later
		  JOIN copied ON later.reply_to_id = copied.id OR later.meta->>'parent_message_id' = copied.uuid::text
		  WHERE later.conversation_id = $1
		)
		SELECT COALESCE(array_agg(g.id ORDER BY g.created_at, g.id), '{}'),
		       COALESCE(bool_or((g.created_at, g.id) > ($2::timestamp, $3::integer) AND (g.is_streaming OR g.meta ?| $4::text[])), FALSE)
		FROM copied JOIN %[1]s.chat_message_group g ON g.id = copied.id`, s),
		sourceID, point.createdAt, point.id, forkPauseKeys).Scan(&groups, &unsettled); err != nil {
		return nil, fmt.Errorf("conversations: fork: read groups: %w", err)
	}
	if unsettled {
		return nil, conversations.ErrForkPointUnsettled
	}
	return groups, nil
}

// forkTables reads the columns of each optional payload table this tenant
// has, in one catalog read; a table it lacks is absent. s is the quoted
// schema, which to_regnamespace takes as it is.
func forkTables(ctx context.Context, q forkQuerier, s string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `SELECT c.relname::text, array_agg(a.attname::text ORDER BY a.attnum)
		FROM pg_class c
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		WHERE c.relnamespace = to_regnamespace($1) AND c.relname = ANY($2::text[]) AND c.relkind IN ('r', 'p')
		GROUP BY c.relname`, s, forkLeafTables)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: read tables: %w", err)
	}
	defer rows.Close()
	present := map[string][]string{}
	for rows.Next() {
		var table string
		var columns []string
		if err := rows.Scan(&table, &columns); err != nil {
			return nil, fmt.Errorf("conversations: fork: scan tables: %w", err)
		}
		present[table] = columns
	}
	return present, rows.Err()
}

// forkAttachmentCopies lists the objects the fork copies: the attachment
// rows of the copied groups whose name carries the source prefix.
func forkAttachmentCopies(ctx context.Context, q forkQuerier, s string, groups []int64, sourceUUID, newUUID string) ([]conversations.AttachmentCopy, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT DISTINCT a.bucket, a.name
		FROM %[1]s.chat_message_items i JOIN %[1]s.chat_messages_attachment a ON a.id = i.id
		WHERE i.message_group_id = ANY($1) AND i.item_type = 'attachment_message'
		  AND starts_with(a.name, $2 || '/')
		ORDER BY a.bucket, a.name`, s), groups, sourceUUID)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: read attachments: %w", err)
	}
	defer rows.Close()
	var copies []conversations.AttachmentCopy
	for rows.Next() {
		var bucket, name string
		if err := rows.Scan(&bucket, &name); err != nil {
			return nil, fmt.Errorf("conversations: fork: scan attachment: %w", err)
		}
		copies = append(copies, conversations.AttachmentCopy{Bucket: bucket, From: name, To: newUUID + name[len(sourceUUID):]})
	}
	return copies, rows.Err()
}

// forkMeta is the source meta with the server's fork marker, without the
// conversation-scoped upload listing (the copied items carry the
// attachments) and without an earlier marker.
func forkMeta(src forkSource, point *forkPoint, options map[string]any) ([]byte, error) {
	meta := map[string]any{}
	if len(src.meta) > 0 {
		if err := json.Unmarshal(src.meta, &meta); err != nil || meta == nil {
			meta = map[string]any{}
		}
	}
	delete(meta, "attachments")
	marker := map[string]any{"conversation_id": src.id, "conversation_uuid": src.uuid}
	if point != nil {
		marker["message_id"] = point.id
		marker["message_uuid"] = point.uuid
	}
	for key, value := range options {
		marker[key] = value
	}
	meta[conversations.ForkedFromMetaKey] = marker
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: encode meta: %w", err)
	}
	return encoded, nil
}

// defaultForkName is "<source> (fork)", within the name limit. A blank
// source name reads as the default conversation name.
func defaultForkName(source string) string {
	limit := conversations.MaxConversationNameLength - utf8.RuneCountInString(conversations.ForkNameSuffix)
	source = strings.TrimSpace(source)
	if source == "" {
		source = conversations.DefaultConversationName
	}
	if utf8.RuneCountInString(source) > limit {
		source = string([]rune(source)[:limit])
	}
	return source + conversations.ForkNameSuffix
}

// forkParticipants maps the source's participants onto the fork: every
// participant that is not a user, and the caller. The caller and the
// default dummy participant are then made sure of, as Create does. Another
// member is not mapped (a mapping is access, chatauthority.Predicate); the
// groups they wrote keep their participant row as the author.
func forkParticipants(ctx context.Context, tx pgx.Tx, s string, sourceID, newID, actorID int64) error {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.chat_participant_mapping
		(conversation_id, participant_id, entity_settings, created_at)
		SELECT $1, m.participant_id, m.entity_settings, now() AT TIME ZONE 'UTC'
		FROM %[1]s.chat_participant_mapping m JOIN %[1]s.chat_participants p ON p.id = m.participant_id
		WHERE m.conversation_id = $2 AND (p.entity_name <> 'user' OR p.entity_meta->>'id' = $3)
		ORDER BY m.id
		ON CONFLICT (participant_id, conversation_id) DO NOTHING`, s), newID, sourceID, strconv.FormatInt(actorID, 10)); err != nil {
		return fmt.Errorf("conversations: fork: participants: %w", err)
	}
	for _, body := range []map[string]any{
		{"entity_name": "user", "entity_meta": map[string]any{"id": actorID}},
		{"entity_name": "dummy", "entity_meta": map[string]any{}},
	} {
		if _, err := addConversationParticipant(ctx, tx, s, newID, body); err != nil {
			return err
		}
	}
	return nil
}

// forkMessages copies the groups, their items, the item payloads and the
// trace steps. It returns the groups' source-to-copy UUID map.
func forkMessages(ctx context.Context, tx pgx.Tx, s string, tables map[string][]string, groups []int64, newID int64, sourceUUID, newUUID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`WITH src AS MATERIALIZED (
		  SELECT o.id AS old_id, gen_random_uuid() AS new_uuid, o.ord
		  FROM unnest($2::integer[]) WITH ORDINALITY AS o(id, ord)
		), ins AS (
		  INSERT INTO %[1]s.chat_message_group
		    (uuid, author_participant_id, conversation_id, sent_to_id, meta, is_streaming, created_at, updated_at, task_id)
		  SELECT src.new_uuid, g.author_participant_id, $1, g.sent_to_id, g.meta - $3::text[], FALSE,
		         g.created_at, g.updated_at, NULL
		  FROM src JOIN %[1]s.chat_message_group g ON g.id = src.old_id
		  ORDER BY src.ord
		  RETURNING id, uuid
		)
		SELECT src.old_id, ins.id, g.uuid::text, ins.uuid::text
		FROM src JOIN ins ON ins.uuid = src.new_uuid JOIN %[1]s.chat_message_group g ON g.id = src.old_id
		ORDER BY src.ord`, s),
		newID, groups, forkPauseKeys)
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: message groups: %w", err)
	}
	var oldGroups, newGroups []int64
	var oldUUIDs, newUUIDs []string
	messageUUIDs := map[string]string{}
	for rows.Next() {
		var oldID, newGroupID int64
		var oldUUID, newGroupUUID string
		if err := rows.Scan(&oldID, &newGroupID, &oldUUID, &newGroupUUID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("conversations: fork: message groups: %w", err)
		}
		oldGroups, newGroups = append(oldGroups, oldID), append(newGroups, newGroupID)
		oldUUIDs, newUUIDs = append(oldUUIDs, oldUUID), append(newUUIDs, newGroupUUID)
		messageUUIDs[oldUUID] = newGroupUUID
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversations: fork: message groups: %w", err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s.chat_message_group n SET reply_to_id = target.new_id
		FROM unnest($1::integer[], $2::integer[]) AS m(old_id, new_id)
		JOIN %[1]s.chat_message_group o ON o.id = m.old_id
		JOIN unnest($1::integer[], $2::integer[]) AS target(old_id, new_id) ON target.old_id = o.reply_to_id
		WHERE n.id = m.new_id`, s), oldGroups, newGroups); err != nil {
		return nil, fmt.Errorf("conversations: fork: reply links: %w", err)
	}
	// A swarm child names its parent answer by UUID (meta.parent_message_id).
	if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %[1]s.chat_message_group n
		SET meta = jsonb_set(n.meta, '{parent_message_id}', to_jsonb(parent.new_uuid))
		FROM unnest($2::text[], $3::text[]) AS parent(old_uuid, new_uuid)
		WHERE n.conversation_id = $1 AND n.meta->>'parent_message_id' = parent.old_uuid`, s),
		newID, oldUUIDs, newUUIDs); err != nil {
		return nil, fmt.Errorf("conversations: fork: swarm parents: %w", err)
	}

	oldItems, newItems, err := scanIDPairs(tx.Query(ctx, fmt.Sprintf(`WITH gm AS (
		  SELECT * FROM unnest($1::integer[], $2::integer[]) AS gm(old_id, new_id)
		), src AS MATERIALIZED (
		  SELECT i.id AS old_id, gen_random_uuid() AS new_uuid, gm.new_id AS new_group
		  FROM %[1]s.chat_message_items i JOIN gm ON gm.old_id = i.message_group_id
		), ins AS (
		  INSERT INTO %[1]s.chat_message_items (uuid, item_type, order_index, meta, created_at, updated_at, message_group_id)
		  SELECT src.new_uuid, i.item_type, i.order_index,
		         CASE WHEN i.item_type = 'attachment_message' THEN replace(i.meta::text, $3, $4)::jsonb ELSE i.meta END,
		         i.created_at, i.updated_at, src.new_group
		  FROM src JOIN %[1]s.chat_message_items i ON i.id = src.old_id
		  ORDER BY src.new_group, i.order_index, i.id
		  RETURNING id, uuid
		)
		SELECT src.old_id, ins.id FROM src JOIN ins ON ins.uuid = src.new_uuid`, s),
		oldGroups, newGroups, sourceUUID, newUUID))
	if err != nil {
		return nil, fmt.Errorf("conversations: fork: message items: %w", err)
	}

	type leaf struct {
		table, mapColumn, order string
		exclude                 []string
		overrides               map[string]string
		old, new                []int64
		extra                   []any
	}
	attachmentName := `CASE WHEN starts_with(src.name, $3 || '/') THEN $4 || substr(src.name, length($3) + 1) ELSE src.name END`
	leaves := []leaf{
		{table: "chat_messages_text", mapColumn: "id", old: oldItems, new: newItems},
		{table: "chat_messages_context", mapColumn: "id", old: oldItems, new: newItems},
		{table: "chat_messages_attachment", mapColumn: "id", old: oldItems, new: newItems,
			overrides: map[string]string{"name": attachmentName, "content": `replace(src.content::text, $3, $4)::json`},
			extra:     []any{sourceUUID, newUUID}},
		{table: "chat_messages_canvas", mapColumn: "id", old: oldItems, new: newItems},
		{table: "chat_canvas_versions", mapColumn: "canvas_item_id", order: "src.created_at, src.id", exclude: []string{"id"}, old: oldItems, new: newItems},
		{table: "chat_message_trace_step", mapColumn: "message_group_id", order: "src.id", exclude: []string{"id"}, old: oldGroups, new: newGroups},
	}
	for _, l := range leaves {
		columns, present := tables[l.table]
		if !present || len(l.old) == 0 {
			continue
		}
		if err := copyForkLeaf(ctx, tx, s, l.table, l.mapColumn, l.order, columns, l.exclude, l.overrides, l.old, l.new, l.extra...); err != nil {
			return nil, err
		}
	}
	return messageUUIDs, nil
}

// copyForkLeaf copies the rows of one child table whose mapColumn holds an
// old id, writing the new id there. Every other column (columns, read from
// the catalog by forkTables) is copied as it is, so a tenant whose table has
// more columns than this repository declares keeps them; exclude names
// columns left to their defaults, and overrides replaces a column's
// expression (alias src; $3 onward are extra).
func copyForkLeaf(ctx context.Context, tx pgx.Tx, s, table, mapColumn, order string, columns, exclude []string, overrides map[string]string, oldIDs, newIDs []int64, extra ...any) error {
	skip := map[string]bool{mapColumn: true}
	for _, column := range exclude {
		skip[column] = true
	}
	targets := []string{pgx.Identifier{mapColumn}.Sanitize()}
	values := []string{"m.new_id"}
	for _, column := range columns {
		if skip[column] {
			continue
		}
		targets = append(targets, pgx.Identifier{column}.Sanitize())
		if expression, ok := overrides[column]; ok {
			values = append(values, expression)
		} else {
			values = append(values, "src."+pgx.Identifier{column}.Sanitize())
		}
	}
	orderBy := ""
	if order != "" {
		orderBy = " ORDER BY " + order
	}
	query := fmt.Sprintf(`INSERT INTO %[1]s.%[2]s (%[3]s)
		SELECT %[4]s FROM unnest($1::integer[], $2::integer[]) AS m(old_id, new_id)
		JOIN %[1]s.%[2]s src ON src.%[5]s = m.old_id%[6]s`,
		s, pgx.Identifier{table}.Sanitize(), strings.Join(targets, ", "), strings.Join(values, ", "),
		pgx.Identifier{mapColumn}.Sanitize(), orderBy)
	if _, err := tx.Exec(ctx, query, append([]any{oldIDs, newIDs}, extra...)...); err != nil {
		return fmt.Errorf("conversations: fork: copy %s: %w", table, err)
	}
	return nil
}

// scanIDPairs reads (old id, new id) rows into two parallel slices.
func scanIDPairs(rows pgx.Rows, err error) ([]int64, []int64, error) {
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var oldIDs, newIDs []int64
	for rows.Next() {
		var oldID, newID int64
		if err := rows.Scan(&oldID, &newID); err != nil {
			return nil, nil, err
		}
		oldIDs = append(oldIDs, oldID)
		newIDs = append(newIDs, newID)
	}
	return oldIDs, newIDs, rows.Err()
}

// ForkSourceName reads the source conversation's name when the caller can
// see it under the detail rule.
func (r *ConversationsRepo) ForkSourceName(ctx context.Context, projectID string, sourceID int64) (string, bool, error) {
	if r.pool == nil || sourceID <= 0 {
		return "", false, nil
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return "", false, err
	}
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return "", false, err
	}
	visible, args := access.Predicate(s, "c", 2, chatauthority.Detail)
	var name string
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT c.name FROM %s.chat_conversations c WHERE c.id = $1 AND %s`, s, visible),
		append([]any{sourceID}, args...)...).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
