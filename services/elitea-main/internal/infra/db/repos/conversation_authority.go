package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
	"github.com/jackc/pgx/v5"
)

// AuthorizeChatResource resolves each resource through its owning conversation.
// Missing and invisible resources have the same response.
func (r *ConversationsRepo) AuthorizeChatResource(ctx context.Context, projectID, kind, resourceID string) error {
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return err
	}
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	identity, ok := idPredicate(resourceID)
	if !ok {
		return apierr.NotFound("chat resource not found")
	}
	join := ""
	switch kind {
	case "conversation":
	case "message":
		join = fmt.Sprintf(` JOIN %s.chat_message_group resource ON resource.conversation_id=c.id`, s)
		identity = strings.ReplaceAll(identity, "c.", "resource.")
	case "canvas":
		join = fmt.Sprintf(` JOIN %[1]s.chat_message_group resource_group ON resource_group.conversation_id=c.id
		 JOIN %[1]s.chat_message_items resource ON resource.message_group_id=resource_group.id AND resource.item_type='canvas_message'
		 JOIN %[1]s.chat_messages_canvas resource_canvas ON resource_canvas.id=resource.id`, s)
		identity = strings.ReplaceAll(identity, "c.", "resource.")
	default:
		return apierr.NotFound("chat resource not found")
	}
	visible, args := access.Predicate(s, "c", 2, chatauthority.Detail)
	var id int64
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT c.id FROM %s.chat_conversations c%s WHERE %s AND %s`, s, join, identity, visible), append([]any{resourceID}, args...)...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound("chat resource not found")
	}
	if err != nil {
		return fmt.Errorf("chat authority lookup: %w", err)
	}
	return nil
}
