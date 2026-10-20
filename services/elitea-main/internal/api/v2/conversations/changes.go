package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// conversationSelectSQL is the projection of one conversation-list row,
// shared by the legacy page and the delta so a delta row is byte-identical to
// the legacy row. withSync appends c.sync_at (the cursor position, never a
// wire field). The caller appends WHERE / ORDER BY / LIMIT.
//
// `is_pinned` (client contract 1.4) is whether the project holds a
// conversation pin for the row (centry.social_pins, one shared pin per
// project and entity, the pin the web rail sets). Only the flag is read: the
// pin's user_id (its last pinner) never reaches the row. A pin or unpin
// stamps the conversation's sync_at (repos.CurrentSocialPinsRepository), so
// the delta re-delivers the row with the new flag. projectID is the path's
// project id, already validated as a tenant id by the caller; it is written
// as an integer literal so the projection binds no argument of its own.
//
// `is_archived`, `archived_by` and `archived_at` (client contract 1.10) read
// the project's archive of the row (tenant chat_conversation_archives, one
// row per archived conversation). The archive is shared by the project, as
// the pin is, so every reader gets the same values. An archive or unarchive
// stamps sync_at as a pin does (repos.ConversationsRepo's
// ArchiveConversation), so the delta re-delivers the row to every member who
// can see it. The archiver's display name is added after the scan
// (annotateArchivers).
func conversationSelectSQL(s, projectID string, withSync bool) string {
	sync := ""
	if withSync {
		sync = ", c.sync_at"
	}
	project, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil {
		project = -1 // no project has this id, so no row reads as pinned
	}
	return fmt.Sprintf(`
		SELECT c.id, c.name, c.created_at, COALESCE(c.updated_at, c.created_at), c.meta,
			(SELECT COUNT(*) FROM %[1]s.chat_message_group mg WHERE mg.conversation_id = c.id),
			EXISTS (SELECT 1 FROM centry.social_pins sp
			        WHERE sp.entity = 'conversation' AND sp.project_id = %[3]d AND sp.entity_id = c.id),
			arch.archived_by, arch.archived_at%[2]s
		FROM %[1]s.chat_conversations c
		LEFT JOIN %[1]s.chat_conversation_archives arch ON arch.conversation_id = c.id`, s, sync, project)
}

// archivedPredicate is true when the project archived the conversation
// aliased `c`. s is a quoted tenant schema, so the fragment is never caller
// text.
func archivedPredicate(s string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM %s.chat_conversation_archives ca WHERE ca.conversation_id = c.id)`, s)
}

// The values of the list's `archived` parameter (client contract 1.10).
const (
	archivedExclude = "exclude"
	archivedOnly    = "only"
	archivedInclude = "include"
)

// archivedMode reads `archived`. Absent and every unknown value are
// `exclude`, the default, as `hidden` treats its unknown values.
func archivedMode(values url.Values) string {
	switch v := values.Get("archived"); v {
	case archivedOnly, archivedInclude:
		return v
	default:
		return archivedExclude
	}
}

// archivedFilter is the SQL clause (alias c) of one `archived` mode.
func archivedFilter(s, mode string) string {
	switch mode {
	case archivedOnly:
		return archivedPredicate(s)
	case archivedInclude:
		return "TRUE"
	default:
		return "NOT " + archivedPredicate(s)
	}
}

// scanConversationRow reads one conversationSelectSQL row into the wire map.
func scanConversationRow(rows pgx.Rows, withSync bool) (map[string]any, changesync.Position, error) {
	var id int
	var name string
	var createdAt, updatedAt time.Time
	var metaBytes []byte
	var msgCount int
	var pinned bool
	var archivedBy *int64
	var archivedAt *time.Time
	var syncAt time.Time
	dest := []any{&id, &name, &createdAt, &updatedAt, &metaBytes, &msgCount, &pinned, &archivedBy, &archivedAt}
	if withSync {
		dest = append(dest, &syncAt)
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, changesync.Position{}, err
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
		"is_pinned":            pinned,
		"is_archived":          archivedBy != nil,
	}
	if archivedBy != nil && archivedAt != nil {
		// archived_at is a UTC wall clock without a zone; the wire is RFC 3339.
		row["archived_by"] = map[string]any{"id": *archivedBy, "name": ""}
		row["archived_at"] = archivedAt.UTC().Format(time.RFC3339Nano)
	}
	return row, changesync.Position{At: syncAt, ID: int64(id)}, nil
}

// annotateArchivers fills `archived_by.name` with the archiver's display
// name. A name is an enrichment: auth_core__user is bootstrap-owned and is
// absent from a corpus-only database, so it is probed, and any failure
// leaves the empty name the schema allows (the participants read in
// repos.ConversationsRepo does the same).
func annotateArchivers(ctx context.Context, pool *pgxpool.Pool, rows []map[string]any) {
	ids := []int64{}
	for _, row := range rows {
		if by, ok := row["archived_by"].(map[string]any); ok {
			ids = append(ids, by["id"].(int64))
		}
	}
	if len(ids) == 0 {
		return
	}
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('auth_core__user')::text`).Scan(&table); err != nil || table == nil {
		return
	}
	result, err := pool.Query(ctx, `SELECT id::bigint, COALESCE(name, '') FROM auth_core__user WHERE id::bigint = ANY($1)`, ids)
	if err != nil {
		return
	}
	defer result.Close()
	names := map[int64]string{}
	for result.Next() {
		var id int64
		var name string
		if result.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	if result.Err() != nil {
		return
	}
	for _, row := range rows {
		if by, ok := row["archived_by"].(map[string]any); ok {
			by["name"] = names[by["id"].(int64)]
		}
	}
}

// conversationChangesQuery is what List resolved before it branched into
// delta mode: the tenant schema, the caller's visibility predicate (alias c,
// actor id bound at $1) and the list filters (alias c, bound after it).
//
// archived is the `archived` clause, kept apart from filters on purpose. It
// narrows `total` and the rows of a full sync's first page. It never narrows
// the rows of a later delta, and it never takes part in the tombstones: an
// archive or unarchive re-delivers the row with its new `is_archived` and the
// client keeps or drops it. A tombstone means a deletion or a loss of access,
// and an archive is neither.
type conversationChangesQuery struct {
	schema       string
	projectID    string
	visibility   string
	filters      string
	archived     string
	archivedMode string
	args         []any
	admin        bool
	raw          string
}

// ConversationChanges is the delta shape of the conversation list
// (`changes_since`, ADR-0025 WP6). `rows` and `total` keep their legacy
// meaning — the rows are the legacy row shape, `total` is the size of the
// caller's whole filtered list now — so a client parses one row type for
// both modes. Tombstones name conversations deleted (`deleted`) or gone from
// this list for this caller (`access_lost`) since the cursor.
type ConversationChanges struct {
	Total      int                    `json:"total"`
	Rows       []map[string]any       `json:"rows"`
	Tombstones []changesync.Tombstone `json:"tombstones"`
	NextCursor string                 `json:"next_cursor"`
	HasMore    bool                   `json:"has_more"`
}

// conversationFilterParams are the query parameters that select a list; a
// cursor is bound to their values (changesync.FilterDigest).
//
// `archived` (client contract 1.10) is not in this list: see
// conversationListScope.
var conversationFilterParams = []string{"source", "entity_name", "entity_meta_id", "mine", "hidden"}

// conversationListScope is the cursor's list identity: the project, the
// filter digest, and the `archived` mode. The mode is appended only when it
// is not the default, so `archived` absent and `archived=exclude` are the
// same list, and a cursor issued before contract 1.10 (which had no
// `archived`) is still valid for the default list.
func conversationListScope(projectID string, values url.Values, mode string) string {
	base := projectID + "/" + changesync.FilterDigest(values, conversationFilterParams...)
	if mode != archivedExclude {
		base += "/archived=" + mode
	}
	return base
}

// conversationScope binds a conversation cursor to the caller's role as well
// as the project and filter: see listChanges.
func conversationScope(base string, admin bool) string {
	if admin {
		return base + "/admin"
	}
	return base + "/member"
}

func (h *Handler) listChanges(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, q conversationChangesQuery) {
	limit, err := changesync.Limit(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, changesync.ErrorBody{Error: "invalid_limit", Message: "limit must be a positive integer"})
		return
	}
	base := conversationListScope(q.projectID, r.URL.Query(), q.archivedMode)
	cursor, err := changesync.Decode(q.raw, changesync.StreamConversations, conversationScope(base, q.admin))
	if errors.Is(err, changesync.ErrInvalidCursor) {
		// A cursor issued under the caller's OTHER project role. Admin
		// widens what this list shows (every private conversation with a
		// user participant), and a role change writes no tombstone: nothing
		// in the chat tables changed. Advancing the cursor would leave a
		// demoted admin holding conversations it can no longer see, and a
		// promoted one missing conversations it now can. Answer 410 so the
		// client discards the list and resyncs.
		if _, other := changesync.Decode(q.raw, changesync.StreamConversations, conversationScope(base, !q.admin)); other == nil {
			err = changesync.ErrCursorExpired
		}
	}
	if err != nil {
		writeSyncError(w, err)
		return
	}
	page, err := loadConversationChanges(r.Context(), pool, q, cursor, limit)
	if err != nil {
		writeSyncError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// writeSyncError answers a cursor refusal with its 400/410 body and anything
// else through apierr.
func writeSyncError(w http.ResponseWriter, err error) {
	if status, body, ok := changesync.HTTPError(err); ok {
		writeJSON(w, status, body)
		return
	}
	apierr.Write(w, err)
}

func loadConversationChanges(ctx context.Context, pool *pgxpool.Pool, q conversationChangesQuery, cursor changesync.Cursor, limit int) (ConversationChanges, error) {
	var dbNow time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: sync clock: %w", err)
	}
	if err := cursor.CheckExpiry(dbNow); err != nil {
		return ConversationChanges{}, err
	}
	bound := changesync.SettleBound(dbNow)
	s := q.schema
	where := q.visibility + " AND " + q.filters

	page := ConversationChanges{Rows: []map[string]any{}, Tombstones: []changesync.Tombstone{}}
	if err := pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s.chat_conversations c WHERE %s AND %s`, s, where, q.archived), q.args...,
	).Scan(&page.Total); err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: count: %w", err)
	}

	// Rows: everything visible under the list filter that changed after the
	// cursor, oldest change first. The `archived` clause narrows only the
	// first page of a full sync: the client holds nothing yet, so a row of
	// the other archive state is only noise. Every later delta carries the
	// rows of both states, because an archive or unarchive is a change the
	// client must see (the row with its new `is_archived`) to drop the row
	// from, or add it to, its list. See conversationChangesQuery.
	rowWhere := where
	if cursor.Full {
		rowWhere += " AND " + q.archived
	}
	rowsStart := cursor.RowsStart()
	n := len(q.args)
	rowArgs := append(append([]any{}, q.args...), rowsStart.At, rowsStart.ID, limit+1)
	rows, err := pool.Query(ctx, conversationSelectSQL(s, q.projectID, true)+fmt.Sprintf(`
		WHERE %s AND (c.sync_at, c.id) > ($%d::timestamptz, $%d::bigint)
		ORDER BY c.sync_at, c.id
		LIMIT $%d`, rowWhere, n+1, n+2, n+3), rowArgs...)
	if err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: list changes: %w", err)
	}
	positions := []changesync.Position{}
	for rows.Next() {
		row, position, err := scanConversationRow(rows, true)
		if err != nil {
			rows.Close()
			return ConversationChanges{}, fmt.Errorf("conversations: scan change: %w", err)
		}
		page.Rows = append(page.Rows, row)
		positions = append(positions, position)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: list changes: %w", err)
	}
	rowsTruncated := len(page.Rows) > limit
	if rowsTruncated {
		page.Rows, positions = page.Rows[:limit], positions[:limit]
	}
	annotateWorkspaceExecutors(ctx, pool, s, q.projectID, page.Rows)
	annotateArchivers(ctx, pool, page.Rows)

	// Tombstones. Which ones this caller is told about:
	//   conversation        a deletion — of a public conversation to everyone;
	//                       of a private one to its author, its former user
	//                       participants (their access_participant rows were
	//                       written when the mappings were deleted first),
	//                       project admins, and — when it was public once (an
	//                       access_private row exists) — everyone: a member
	//                       who cached it while public and was offline while
	//                       it was made private and deleted has no other
	//                       record left, because the access_private row can
	//                       no longer join a live conversation. The id and
	//                       uuid belonged to a conversation everyone could see;
	//   access_private      a conversation turned private the caller can no
	//                       longer see;
	//   access_participant  the caller was removed and can no longer see it;
	//   access_orphaned     a private conversation lost its last user
	//                       participant; admins listed it only through one,
	//                       so an admin who can no longer see it is told;
	//   access_filter       the caller can still see it, but it no longer
	//                       matches this list's filter.
	// A conversation that is visible and matching again is simply a row (the
	// trigger that wrote the marker also bumped its sync_at).
	tombsStart := cursor.TombstonesStart(bound)
	tombArgs := append(append([]any{}, q.args...), tombsStart.At, tombsStart.ID, limit+1, q.admin)
	tombRows, err := pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.kind, t.entity_id, COALESCE(t.entity_uuid, c.uuid)::text, t.deleted_at
		FROM %[1]s.chat_sync_tombstones t
		LEFT JOIN %[1]s.chat_conversations c ON c.id = t.entity_id AND t.kind <> 'conversation'
		WHERE t.kind <> 'message_group'
		  AND (t.deleted_at, t.id) > ($%[2]d::timestamptz, $%[3]d::bigint)
		  AND (
		      (t.kind = 'conversation' AND (
		           t.is_private IS DISTINCT FROM true
		           OR t.author_id::text = $1::text
		           OR $%[5]d::boolean
		           OR EXISTS (SELECT 1 FROM %[1]s.chat_sync_tombstones a
		                      WHERE a.entity_id = t.entity_id
		                        AND ((a.kind = 'access_participant' AND a.user_id = $1::text)
		                             OR a.kind = 'access_private'))))
		   OR (t.kind = 'access_private' AND c.id IS NOT NULL AND NOT COALESCE(%[6]s, false))
		   OR (t.kind = 'access_orphaned' AND $%[5]d::boolean
		       AND c.id IS NOT NULL AND NOT COALESCE(%[6]s, false))
		   OR (t.kind = 'access_participant' AND t.user_id = $1::text
		       AND c.id IS NOT NULL AND NOT COALESCE(%[6]s, false))
		   OR (t.kind = 'access_filter' AND c.id IS NOT NULL
		       AND COALESCE(%[6]s, false) AND NOT COALESCE(%[7]s, false))
		  )
		ORDER BY t.deleted_at, t.id
		LIMIT $%[4]d`, s, n+1, n+2, n+3, n+4, q.visibility, "("+q.filters+")"), tombArgs...)
	if err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: list tombstones: %w", err)
	}
	tombPositions := []changesync.Position{}
	for tombRows.Next() {
		var id int64
		var kind string
		var entityID int64
		var uuid *string
		var deletedAt time.Time
		if err := tombRows.Scan(&id, &kind, &entityID, &uuid, &deletedAt); err != nil {
			tombRows.Close()
			return ConversationChanges{}, fmt.Errorf("conversations: scan tombstone: %w", err)
		}
		reason := changesync.ReasonAccessLost
		if kind == "conversation" {
			reason = changesync.ReasonDeleted
		}
		page.Tombstones = append(page.Tombstones, changesync.Tombstone{
			ID: entityID, UUID: uuid, Reason: reason, DeletedAt: changesync.FormatTime(deletedAt),
		})
		tombPositions = append(tombPositions, changesync.Position{At: deletedAt, ID: id})
	}
	tombRows.Close()
	if err := tombRows.Err(); err != nil {
		return ConversationChanges{}, fmt.Errorf("conversations: list tombstones: %w", err)
	}
	tombsTruncated := len(page.Tombstones) > limit
	if tombsTruncated {
		page.Tombstones, tombPositions = page.Tombstones[:limit], tombPositions[:limit]
	}

	nextRows, moreRows := changesync.Advance(rowsStart, lastPosition(positions), rowsTruncated, bound)
	nextTombs, moreTombs := changesync.Advance(tombsStart, lastPosition(tombPositions), tombsTruncated, bound)
	page.NextCursor = changesync.Encode(changesync.Cursor{
		Stream: cursor.Stream, Scope: cursor.Scope, Rows: nextRows, Tombs: nextTombs,
	})
	page.HasMore = moreRows || moreTombs
	return page, nil
}

func lastPosition(positions []changesync.Position) *changesync.Position {
	if len(positions) == 0 {
		return nil
	}
	last := positions[len(positions)-1]
	return &last
}

// MessageChanges is the delta shape of a conversation's transcript
// (`changes_since`, ADR-0025 WP6). `items` and `total` keep their legacy
// meaning (the legacy item shape; the transcript's size now); the page fields
// of the legacy envelope do not apply to a delta and are absent.
type MessageChanges struct {
	Items      []Message              `json:"items"`
	Total      int                    `json:"total"`
	Tombstones []changesync.Tombstone `json:"tombstones"`
	NextCursor string                 `json:"next_cursor"`
	HasMore    bool                   `json:"has_more"`
}

// MessageChangesLister is the optional repository half of the message delta.
// Optional so test doubles of Repository keep compiling; the production
// repository implements it, and a composition without it answers 501.
type MessageChangesLister interface {
	ListMessageChanges(ctx context.Context, projectID, conversationID, changesSince string, limit int) (MessageChanges, error)
}

// ErrSyncUnsupported is answered when the repository has no delta half.
var ErrSyncUnsupported = errors.New("changes_since is not supported by this composition")

func (h *Handler) listMessageChanges(w http.ResponseWriter, r *http.Request, projectID, conversationID, raw string) {
	lister, ok := h.repo.(MessageChangesLister)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, changesync.ErrorBody{Error: "not_implemented", Message: ErrSyncUnsupported.Error()})
		return
	}
	limit, err := changesync.Limit(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, changesync.ErrorBody{Error: "invalid_limit", Message: "limit must be a positive integer"})
		return
	}
	page, err := lister.ListMessageChanges(r.Context(), projectID, conversationID, raw, limit)
	if err != nil {
		writeSyncError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// MessageScope is the cursor scope of one conversation's transcript: the
// project and the NUMERIC conversation id (the path may carry the uuid).
func MessageScope(projectID string, conversationID int64) string {
	return projectID + "/" + strconv.FormatInt(conversationID, 10)
}
