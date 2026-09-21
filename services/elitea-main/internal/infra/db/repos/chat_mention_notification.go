package repos

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// The @mention notification producer (#977).
//
// One `centry.notifications` row per person named in a chat message, written
// with the same INSERT the PAT-expiry sweep uses
// (`pat_expiry_notification.go`) against the same table. The event type and
// the `meta` keys are the ones the web ALREADY resolves — see
// `application/agentexecution/mentions.go`'s header for why inventing either
// would produce a notification the client renders as a blank line.
//
// # ONE STATEMENT, NOT ONE PER ROW
//
// A message naming twenty people is one `unnest`-driven INSERT rather than
// twenty round trips inside a loop. It is also all-or-nothing by construction:
// a partial write would leave some of the audience told and the rest not, with
// nothing to say which, and the caller's retry would then double-notify the
// half that succeeded.
type ChatMentionNotificationRepo struct {
	pool *pgxpool.Pool
}

func NewChatMentionNotificationRepo(pool *pgxpool.Pool) *ChatMentionNotificationRepo {
	return &ChatMentionNotificationRepo{pool: pool}
}

// chatMentionMeta is the `meta` blob, in the SNAKE_CASE the web's own
// normaliser reads (`features/notifications/api/normalize.ts` maps
// `conversation_id` → `conversationId`, `message_id` → `messageId`). A
// camelCase blob here would round-trip through the database and arrive as
// `undefined` at the renderer, which is the "invisible data" shape this
// repository keeps paying for.
type chatMentionMeta struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id,omitempty"`
	ProjectID      int64  `json:"project_id"`
	SenderUserID   int64  `json:"sender_user_id"`
}

// WriteChatMentionNotifications inserts one row per recipient.
func (repo *ChatMentionNotificationRepo) WriteChatMentionNotifications(
	ctx context.Context,
	rows []agentexecutionapp.MentionNotification,
) error {
	if repo == nil || repo.pool == nil || len(rows) == 0 {
		return nil
	}

	projectIDs := make([]int64, 0, len(rows))
	userIDs := make([]int64, 0, len(rows))
	metas := make([]string, 0, len(rows))
	for _, row := range rows {
		encoded, err := json.Marshal(chatMentionMeta{
			ConversationID: row.ConversationUUID,
			MessageID:      row.MessageID,
			ProjectID:      row.ProjectID,
			SenderUserID:   row.SenderUserID,
		})
		if err != nil {
			return fmt.Errorf("encode mention notification meta: %w", err)
		}
		projectIDs = append(projectIDs, row.ProjectID)
		userIDs = append(userIDs, row.UserID)
		metas = append(metas, string(encoded))
	}

	if _, err := repo.pool.Exec(ctx, `
INSERT INTO centry.notifications (is_seen, project_id, user_id, meta, event_type)
SELECT FALSE, source.project_id, source.user_id, source.meta::jsonb, $4
FROM unnest($1::bigint[], $2::bigint[], $3::text[]) AS source(project_id, user_id, meta)`,
		projectIDs, userIDs, metas, agentexecutionapp.ChatMentionNotificationEventType,
	); err != nil {
		return fmt.Errorf("insert %d mention notifications: %w", len(rows), err)
	}
	return nil
}

// ProjectMemberUserIDs answers `@everyone` from the SERVER's view of the
// project.
//
// The same membership shape `analytics.go` reads, INCLUDING its exclusion of
// `%@centry.user` accounts: those are the platform's own system users, one per
// project, and an `@everyone` that notified them would write a row nobody can
// ever see or clear.
func (repo *ChatMentionNotificationRepo) ProjectMemberUserIDs(
	ctx context.Context,
	projectID int64,
) ([]int64, error) {
	if repo == nil || repo.pool == nil {
		return nil, nil
	}
	rows, err := repo.pool.Query(ctx, `
SELECT DISTINCT pur.user_id
FROM public.auth_core__project_user_role AS pur
JOIN public.auth_core__user AS account ON account.id = pur.user_id
WHERE pur.project_id = $1
  AND account.email NOT LIKE '%@centry.user'`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project %d membership: %w", projectID, err)
	}
	defer rows.Close()

	members := make([]int64, 0, 8)
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scan project %d member: %w", projectID, err)
		}
		members = append(members, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read project %d membership: %w", projectID, err)
	}
	return members, nil
}

var _ agentexecutionapp.MentionNotificationWriter = (*ChatMentionNotificationRepo)(nil)
