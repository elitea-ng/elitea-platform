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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"

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

// newMembershipTestPool answers a private database carrying the LEGACY auth
// tables `ProjectMemberUserIDs` reads.
//
// NOT the migrated template the rest of this file uses. That template replays
// the shared/tenant migration CORPUS, which never creates
// `public.auth_core__user` or `public.auth_core__project_user_role` — they are
// pylon-owned in a migrated deployment, and on continuous integration's
// Postgres they simply do not exist. Seeding them there failed with
// `relation "public.auth_core__user" does not exist`, and the first version of
// this test answered that with `t.Skipf`, which the skip ledger (#423) refuses
// for exactly the right reason: a skip is an assertion that stopped running,
// and this one would have stopped running on the only machine that matters.
//
// `db.RunMigrations` over an EMPTY database is how every other test that needs
// a user row gets one — the skills author join
// (`newSkillsTestPool`, skills_postgres_integration_test.go) and the
// applications `getVersions` author cases both do it, and both pass on CI. It
// replays `internal/infra/db/migrations/001_initial.sql`, which declares the
// two auth tables (:956, :1028) and `centry.notifications` alongside them.
func newMembershipTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	return pool
}

func TestProjectMemberUserIDsExcludesSystemAccounts(t *testing.T) {
	pool := newMembershipTestPool(t)
	repo := NewChatMentionNotificationRepo(pool)
	ctx := context.Background()

	const projectID = int64(910_978)
	// The membership row's `role_id` is a FOREIGN KEY into
	// `auth_core__project_role`, and 001_initial seeds no project roles — so
	// the role has to exist before any member can hold it.
	var roleID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
VALUES ($1, 'member')
ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
RETURNING id`, projectID).Scan(&roleID); err != nil {
		t.Fatalf("seed project role: %v", err)
	}

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
			t.Fatalf("seed user %s: %v", seed.email, err)
		}
	}
	for _, userID := range []int64{humanA, humanB, system} {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`, projectID, userID, roleID); err != nil {
			t.Fatalf("seed membership for user %d: %v", userID, err)
		}
	}

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

// #984: THE MEMBERSHIP ANSWER IS THE GATE FOR A NAMED LIST TOO.
//
// `user_ids` used to reach the writer unfiltered — `ProjectMemberUserIDs` was
// consulted only for `@everyone` — so any member of any project could POST an
// arbitrary list and write a `centry.notifications` row, carrying this
// conversation, this project and this sender, onto a stranger's bell in
// another tenant. `mentionAudience` now intersects with this answer, which
// makes THIS query the thing that decides, and this test is what says it
// answers the project asked about rather than the deployment.
//
// A real user who is a member of ANOTHER project is the shape that matters: a
// row that exists, that an attacker can name, and that must not come back.
func TestProjectMemberUserIDsExcludesAMemberOfAnotherProject(t *testing.T) {
	pool := newMembershipTestPool(t)
	repo := NewChatMentionNotificationRepo(pool)
	ctx := context.Background()

	const homeProject = int64(910_984)
	const otherProject = int64(910_985)
	roleOf := func(projectID int64) int64 {
		var roleID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
VALUES ($1, 'member')
ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
RETURNING id`, projectID).Scan(&roleID); err != nil {
			t.Fatalf("seed project role for %d: %v", projectID, err)
		}
		return roleID
	}
	userOf := func(email string) int64 {
		var userID int64
		if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name)
VALUES ($1, $1)
ON CONFLICT (email) DO UPDATE SET name = EXCLUDED.name
RETURNING id`, email).Scan(&userID); err != nil {
			t.Fatalf("seed user %s: %v", email, err)
		}
		return userID
	}

	insider := userOf("mention-insider@example.test")
	outsider := userOf("mention-outsider@example.test")
	for _, seed := range []struct {
		projectID int64
		userID    int64
	}{
		{homeProject, insider},
		{otherProject, outsider},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING`, seed.projectID, seed.userID, roleOf(seed.projectID)); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}

	members, err := repo.ProjectMemberUserIDs(ctx, homeProject)
	if err != nil {
		t.Fatalf("resolve membership: %v", err)
	}
	found := map[int64]bool{}
	for _, id := range members {
		found[id] = true
	}
	if !found[insider] {
		t.Fatalf("the project's own member is missing from %v", members)
	}
	if found[outsider] {
		t.Fatalf("a member of another project came back as a member of this one: %v", members)
	}
}
