package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
)

// SharedChatLinksRepo is the store behind "share a conversation by link".
//
// It spans the two tenancies the feature needs and keeps the split explicit:
// the LINKS live in one central table (elitea_chat.shared_chat_links,
// migrations/shared/0100 — see that file for why they cannot be tenant-scoped),
// while the TRANSCRIPT the link points at is read from the project's own
// `p_<id>` schema. The project id used for the second is always read out of the
// row found by the first, never taken from a request.
type SharedChatLinksRepo struct {
	pool *pgxpool.Pool
}

func NewSharedChatLinksRepo(pool *pgxpool.Pool) *SharedChatLinksRepo {
	return &SharedChatLinksRepo{pool: pool}
}

// projectRowID parses a project id for storage in the central table.
//
// The API layer's tenant helper already refuses anything that is not a plain
// decimal, so this is the second gate rather than the first; it exists because
// a value that reaches SQL as a bind parameter still has to be the right TYPE,
// and a non-numeric project id here is a row that could never match anything.
func projectRowID(projectID string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(projectID), 10, 64)
	if err != nil || id <= 0 {
		return 0, sharedchat.ErrNoLink
	}
	return id, nil
}

func (r *SharedChatLinksRepo) Create(ctx context.Context, in sharedchat.CreateInput) (sharedchat.Link, error) {
	projectID, err := projectRowID(in.ProjectID)
	if err != nil {
		return sharedchat.Link{}, err
	}

	groups := in.MessageGroupIDs
	if groups == nil {
		groups = []int64{}
	}

	const q = `
		INSERT INTO elitea_chat.shared_chat_links
			(token_hash, project_id, conversation_id, scope, message_group_ids,
			 password_hash, password_salt, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, scope, message_group_ids, created_by, created_at,
			expires_at, revoked_at, access_count, last_accessed_at,
			password_hash IS NOT NULL`

	var link sharedchat.Link
	var hasPassword bool
	err = r.pool.QueryRow(ctx, q,
		in.TokenHash, projectID, in.ConversationID, in.Scope, groups,
		in.PasswordHash, in.PasswordSalt, in.CreatedBy, in.ExpiresAt,
	).Scan(
		&link.ID, &link.Scope, &link.MessageGroupIDs, &link.CreatedBy, &link.CreatedAt,
		&link.ExpiresAt, &link.RevokedAt, &link.AccessCount, &link.LastAccessedAt,
		&hasPassword,
	)
	if err != nil {
		return sharedchat.Link{}, fmt.Errorf("shared chat links: create: %w", err)
	}
	link.HasPassword = hasPassword
	link.Active = link.RevokedAt == nil && link.ExpiresAt.After(time.Now())
	return link, nil
}

func (r *SharedChatLinksRepo) ListByConversation(ctx context.Context, projectIDStr string, conversationID int64) ([]sharedchat.Link, error) {
	projectID, err := projectRowID(projectIDStr)
	if err != nil {
		return []sharedchat.Link{}, nil
	}

	// No token_hash column in the projection. It is not a secret in the sense
	// the token is, but it is the exact value an anonymous lookup is keyed on,
	// and a listing has no use for it.
	const q = `
		SELECT id, scope, message_group_ids, created_by, created_at, expires_at,
			revoked_at, access_count, last_accessed_at, password_hash IS NOT NULL
		FROM elitea_chat.shared_chat_links
		WHERE project_id = $1 AND conversation_id = $2
		ORDER BY created_at DESC`

	rows, err := r.pool.Query(ctx, q, projectID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("shared chat links: list: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	links := []sharedchat.Link{}
	for rows.Next() {
		var link sharedchat.Link
		var hasPassword bool
		if err := rows.Scan(&link.ID, &link.Scope, &link.MessageGroupIDs, &link.CreatedBy,
			&link.CreatedAt, &link.ExpiresAt, &link.RevokedAt, &link.AccessCount,
			&link.LastAccessedAt, &hasPassword); err != nil {
			return nil, fmt.Errorf("shared chat links: scan: %w", err)
		}
		link.HasPassword = hasPassword
		link.Active = link.RevokedAt == nil && link.ExpiresAt.After(now)
		links = append(links, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shared chat links: list: %w", err)
	}
	return links, nil
}

func (r *SharedChatLinksRepo) Revoke(ctx context.Context, projectIDStr string, conversationID, linkID int64) error {
	projectID, err := projectRowID(projectIDStr)
	if err != nil {
		return sharedchat.ErrNoLink
	}

	// The project and conversation are part of the WHERE, not just of the
	// caller's authorisation check — see Handler.Revoke. `revoked_at IS NULL`
	// makes a second revoke a no-op that still reports "already gone" rather
	// than moving the timestamp.
	const q = `
		UPDATE elitea_chat.shared_chat_links
		SET revoked_at = now()
		WHERE id = $1 AND project_id = $2 AND conversation_id = $3
			AND revoked_at IS NULL`

	tag, err := r.pool.Exec(ctx, q, linkID, projectID, conversationID)
	if err != nil {
		return fmt.Errorf("shared chat links: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return sharedchat.ErrNoLink
	}
	return nil
}

// ResolveByTokenHash is the anonymous lookup.
//
// REVOKED AND EXPIRED ARE FILTERED IN SQL, not in Go. The predicate is part of
// the statement that finds the row, so there is no window in which a caller
// holds a Resolved for a link that is no longer valid, and no later edit can
// forget the check: a revoked or expired link produces pgx.ErrNoRows and comes
// back as ErrNoLink, indistinguishable from a token that never existed. That
// indistinguishability is the point — Handler.View's doc comment records why.
func (r *SharedChatLinksRepo) ResolveByTokenHash(ctx context.Context, tokenHash []byte) (sharedchat.Resolved, error) {
	const q = `
		SELECT id, project_id, conversation_id, scope, message_group_ids,
			password_hash, password_salt, expires_at
		FROM elitea_chat.shared_chat_links
		WHERE token_hash = $1
			AND revoked_at IS NULL
			AND expires_at > now()`

	var out sharedchat.Resolved
	var projectID int64
	err := r.pool.QueryRow(ctx, q, tokenHash).Scan(
		&out.ID, &projectID, &out.ConversationID, &out.Scope, &out.MessageGroupIDs,
		&out.PasswordHash, &out.PasswordSalt, &out.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sharedchat.Resolved{}, sharedchat.ErrNoLink
		}
		return sharedchat.Resolved{}, fmt.Errorf("shared chat links: resolve: %w", err)
	}
	out.ProjectID = strconv.FormatInt(projectID, 10)
	if out.Scope != "partial" {
		// An 'all' link exposes the whole conversation regardless of what the
		// column happens to hold. Normalising here means a reader can treat
		// "empty group list" as "everything" without also having to know the
		// scope.
		out.MessageGroupIDs = nil
	}
	return out, nil
}

func (r *SharedChatLinksRepo) RecordAccess(ctx context.Context, linkID int64) error {
	const q = `
		UPDATE elitea_chat.shared_chat_links
		SET access_count = access_count + 1, last_accessed_at = now()
		WHERE id = $1`
	if _, err := r.pool.Exec(ctx, q, linkID); err != nil {
		return fmt.Errorf("shared chat links: record access: %w", err)
	}
	return nil
}

// SharedTranscript projects one conversation into the anonymous view's shape.
//
// It is a SEPARATE query from ListMessageGroups rather than a filter over it,
// and deliberately so: that function returns the full internal shape — real
// ids, meta, task ids, attachment buckets — and a projection layered on top of
// it is one forgotten field away from leaking all of them. This selects only
// the columns the anonymous response is allowed to contain, so the SQL itself
// is the allow-list.
func (r *SharedChatLinksRepo) SharedTranscript(ctx context.Context, projectID string, conversationID int64, groupIDs []int64) (string, []sharedchat.SharedMessage, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return "", nil, fmt.Errorf("shared chat links: transcript: %w", err)
	}

	var name string
	nameQ := fmt.Sprintf(`SELECT name FROM %s.chat_conversations WHERE id = $1`, s)
	if err := r.pool.QueryRow(ctx, nameQ, conversationID).Scan(&name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The conversation the link pointed at is gone. The link is a
			// pointer, not a copy, so this is "nothing to show" and not an
			// error page: an empty transcript under the conversation's absent
			// name is the honest answer.
			return "", []sharedchat.SharedMessage{}, nil
		}
		return "", nil, fmt.Errorf("shared chat links: transcript name: %w", err)
	}

	// The author's DISPLAY NAME only, and only via the same probe-guarded join
	// ListParticipants uses: auth_core__user is bootstrap-owned, so a
	// corpus-only database has chat_participants and no auth table. EMAIL IS
	// NOT SELECTED — the reference page renders a name and nothing else, and an
	// email address in an anonymously readable payload is a disclosure the
	// feature never needs.
	authorNameCol := "''"
	authorJoin := ""
	var authTable *string
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass('auth_core__user')::text`).Scan(&authTable); err == nil && authTable != nil {
		authorNameCol = "COALESCE(au.name, '')"
		authorJoin = `
			LEFT JOIN auth_core__user au
				ON p.entity_name = 'user'
				AND p.entity_meta->>'id' ~ '^[0-9]+$'
				AND au.id = (p.entity_meta->>'id')::integer`
	}

	// `$2::bigint[]` empty means "no group filter". Expressed as a single
	// predicate rather than two query strings so that the scoped and unscoped
	// reads cannot drift in what else they select.
	groups := groupIDs
	if groups == nil {
		groups = []int64{}
	}

	groupQ := fmt.Sprintf(`
		SELECT mg.id,
			COALESCE(p.entity_name, ''),
			COALESCE(p.entity_meta->>'agent_type', ''),
			COALESCE(NULLIF(%s, ''), COALESCE(p.entity_meta->>'name', '')),
			mg.created_at,
			COALESCE((mg.meta->>'is_error')::boolean, false)
		FROM %s.chat_message_group mg
		LEFT JOIN %s.chat_participants p ON p.id = mg.author_participant_id%s
		WHERE mg.conversation_id = $1
			AND (cardinality($2::bigint[]) = 0 OR mg.id = ANY($2::bigint[]))
		ORDER BY mg.created_at ASC, mg.id ASC
		LIMIT $3`, authorNameCol, s, s, authorJoin)

	rows, err := r.pool.Query(ctx, groupQ, conversationID, groups, sharedchatMessageLimit)
	if err != nil {
		return "", nil, fmt.Errorf("shared chat links: transcript groups: %w", err)
	}
	defer rows.Close()

	messages := []sharedchat.SharedMessage{}
	var ids []int64
	index := map[int64]int{}
	for rows.Next() {
		var id int64
		var entityName, agentType, authorName string
		var createdAt time.Time
		var isError bool
		if err := rows.Scan(&id, &entityName, &agentType, &authorName, &createdAt, &isError); err != nil {
			return "", nil, fmt.Errorf("shared chat links: scan transcript group: %w", err)
		}
		authorType := "assistant"
		if entityName == "user" {
			authorType = "user"
		}
		index[id] = len(messages)
		ids = append(ids, id)
		messages = append(messages, sharedchat.SharedMessage{
			AuthorType:           authorType,
			AuthorName:           authorName,
			ParticipantType:      entityName,
			ParticipantAgentType: agentType,
			CreatedAt:            createdAt,
			IsError:              isError,
			Items:                []sharedchat.SharedMessageIt{},
		})
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("shared chat links: transcript groups: %w", err)
	}
	if len(ids) == 0 {
		return name, messages, nil
	}

	// Attachment NAME and TYPE only. `ma.bucket` and `ma.content` are not
	// selected: the bytes are not served anonymously, so a locator would be an
	// unusable string that still discloses the deployment's storage layout.
	itemQ := fmt.Sprintf(`
		SELECT mi.message_group_id, mi.item_type,
			COALESCE(mt.content, ''), COALESCE(ma.name, ''), COALESCE(ma.attachment_type, '')
		FROM %s.chat_message_items mi
		LEFT JOIN %s.chat_messages_text mt ON mt.id = mi.id
		LEFT JOIN %s.chat_messages_attachment ma ON ma.id = mi.id
		WHERE mi.message_group_id = ANY($1)
		ORDER BY mi.message_group_id, mi.order_index`, s, s, s)

	itemRows, err := r.pool.Query(ctx, itemQ, ids)
	if err != nil {
		return "", nil, fmt.Errorf("shared chat links: transcript items: %w", err)
	}
	defer itemRows.Close()

	for itemRows.Next() {
		var groupID int64
		var itemType, content, attachmentName, attachmentType string
		if err := itemRows.Scan(&groupID, &itemType, &content, &attachmentName, &attachmentType); err != nil {
			return "", nil, fmt.Errorf("shared chat links: scan transcript item: %w", err)
		}
		idx, ok := index[groupID]
		if !ok {
			continue
		}
		switch itemType {
		case "text_message", "canvas_message":
			messages[idx].Items = append(messages[idx].Items, sharedchat.SharedMessageIt{
				Type:    itemType,
				Content: content,
			})
		case "attachment_message":
			if attachmentName == "" {
				continue
			}
			// The stored name carries a `{conversationUUID}/` prefix from the
			// upload path. The prefix is an internal key, so only the file's
			// own name crosses the boundary.
			display := attachmentName
			if i := strings.LastIndex(display, "/"); i >= 0 {
				display = display[i+1:]
			}
			messages[idx].Items = append(messages[idx].Items, sharedchat.SharedMessageIt{
				Type:       itemType,
				Attachment: &sharedchat.SharedAttachment{Name: display, Type: attachmentType},
			})
		default:
			// Every other item type — context payloads, traces, anything added
			// later — is DROPPED rather than passed through. A default that
			// forwarded unknown types would make each new item type an
			// anonymous disclosure by omission.
			continue
		}
	}
	if err := itemRows.Err(); err != nil {
		return "", nil, fmt.Errorf("shared chat links: transcript items: %w", err)
	}

	return name, messages, nil
}

// sharedchatMessageLimit bounds the transcript one anonymous request can pull
// out of the database. The handler caps the RESPONSE too; this caps the WORK,
// which is the half a handler-side slice cannot do.
const sharedchatMessageLimit = 500
