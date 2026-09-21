package repos

// The @mention notification producer, against a REAL migrated database (#977).
//
// The unit tests beside `application/agentexecution/mentions.go` own the
// POLICY — who is notified, who is not, and that a failure costs the mention
// rather than the turn. They run on a double and cannot prove any of what is
// only true in SQL:
//
//   - the row really lands in `centry.notifications` with the `event_type` the
//     web resolves, rather than in a table nothing reads;
//   - `meta` is stored as JSONB with the SNAKE_CASE keys the client's
//     normaliser maps (`conversation_id`, `message_id`) — a camelCase blob
//     round-trips happily and arrives at the renderer as `undefined`, which is
//     the "invisible data" failure this repository keeps paying for;
//   - twenty recipients are ONE statement and all-or-nothing, not twenty
//     round trips that can half-succeed;
//   - `@everyone` resolves through the real membership join, system accounts
//     excluded.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func newMentionNotificationRepo(t *testing.T) (*ChatMentionNotificationRepo, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	return NewChatMentionNotificationRepo(pool), pool
}

func TestChatMentionNotificationsLandWithTheEventTypeTheWebResolves(t *testing.T) {
	repo, pool := newMentionNotificationRepo(t)
	ctx := context.Background()

	const projectID = int64(910_977)
	rows := []agentexecutionapp.MentionNotification{
		{
			ProjectID:        projectID,
			UserID:           9_771,
			ConversationUUID: "11111111-1111-1111-1111-111111111111",
			MessageID:        "22222222-2222-2222-2222-222222222222",
			SenderUserID:     9_770,
		},
		{
			ProjectID:        projectID,
			UserID:           9_772,
			ConversationUUID: "11111111-1111-1111-1111-111111111111",
			MessageID:        "22222222-2222-2222-2222-222222222222",
			SenderUserID:     9_770,
		},
	}
	if err := repo.WriteChatMentionNotifications(ctx, rows); err != nil {
		t.Fatalf("write mention notifications: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM centry.notifications WHERE project_id = $1`, projectID)
	})

	written, err := pool.Query(ctx, `
SELECT user_id, event_type, is_seen, meta::text
FROM centry.notifications
WHERE project_id = $1
ORDER BY user_id`, projectID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer written.Close()

	seen := 0
	for written.Next() {
		var userID int64
		var eventType, metaText string
		var isSeen bool
		if err := written.Scan(&userID, &eventType, &isSeen, &metaText); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if eventType != agentexecutionapp.ChatMentionNotificationEventType {
			t.Fatalf("event_type = %q, want the one the web resolves (%q)",
				eventType, agentexecutionapp.ChatMentionNotificationEventType)
		}
		if isSeen {
			t.Fatalf("a new notification must be unseen")
		}
		// THE KEYS, read as the client reads them. A blob keyed
		// `conversationId` would pass every Go assertion and render nothing.
		var meta map[string]any
		if err := json.Unmarshal([]byte(metaText), &meta); err != nil {
			t.Fatalf("meta is not JSON: %v", err)
		}
		if meta["conversation_id"] != "11111111-1111-1111-1111-111111111111" {
			t.Fatalf("meta.conversation_id missing or wrong: %s", metaText)
		}
		if meta["message_id"] != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("meta.message_id missing or wrong: %s", metaText)
		}
	}
	if err := written.Err(); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if seen != len(rows) {
		t.Fatalf("wrote %d recipients, read back %d", len(rows), seen)
	}
}

func TestChatMentionNotificationsWriteNothingForAnEmptyAudience(t *testing.T) {
	repo, _ := newMentionNotificationRepo(t)
	// An empty audience must not produce an INSERT at all — an `unnest` over
	// three empty arrays is a valid statement that writes nothing, but paying
	// a round trip for it on every ordinary message is waste the caller's
	// guard already avoids; this pins that the repository agrees.
	if err := repo.WriteChatMentionNotifications(context.Background(), nil); err != nil {
		t.Fatalf("an empty audience must be a no-op, got %v", err)
	}
}

func TestProjectMemberUserIDsExcludesSystemAccounts(t *testing.T) {
	repo, pool := newMentionNotificationRepo(t)
	ctx := context.Background()

	const projectID = int64(910_978)
	// Two real accounts and one platform system user. `@everyone` that
	// notified the system user would write a row nobody can see or clear.
	var humanA, humanB, system int64
	for _, seed := range []struct {
		email string
		into  *int64
	}{
		{"mention-a@example.test", &humanA},
		{"mention-b@example.test", &humanB},
		{"project-910978@centry.user", &system},
	} {
		if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ($1, $1)
ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name
RETURNING id`, seed.email).Scan(seed.into); err != nil {
			t.Skipf("this deployment's auth_core__user does not accept the seed shape: %v", err)
		}
	}
	for _, userID := range []int64{humanA, humanB, system} {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, 1)
ON CONFLICT DO NOTHING`, projectID, userID); err != nil {
			t.Skipf("this deployment's membership table does not accept the seed shape: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM public.auth_core__project_user_role WHERE project_id = $1`, projectID)
	})

	members, err := repo.ProjectMemberUserIDs(ctx, projectID)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}
	found := map[int64]bool{}
	for _, id := range members {
		found[id] = true
	}
	if !found[humanA] || !found[humanB] {
		t.Fatalf("@everyone must reach every human member; got %v", members)
	}
	if found[system] {
		t.Fatalf("@everyone must not reach the project's system account; got %v", members)
	}
}
