package repos

// The conversation's own settings document — `chat_conversations.meta` — from
// the repository's side.
//
// The composer's Modules panel is a CONVERSATION setting, not a screen state:
// switching a module on PUTs the whole `meta` document with one more entry in
// `internal_tools`, and the panel reads its state back from the details
// response (apps/elitea-web/src/widgets/chat-box/ui/hooks/
// useChatBoxInternalTools.ts:56-62, src/pages/chat/useChatPageData.ts:142).
// Pylon accepted the key on PUT and answered it on GET
// (legacy/plugins/elitea_core/api/v2/conversation.py:123).
//
// This repository wrote `'{}'::jsonb` on create and never read or wrote the
// column again, so the round trip had no server side at all: every switch
// answered 200 and stored nothing, and a reload offered every module off.
//
// The three subtests are the three ways that can be wrong again — a write that
// does not land, a read that does not project, and a rename that silently
// blanks a document it was never asked to touch.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The tenant the shared template migrates (postgresIntegrationTenant).
const conversationMetaProject = "1"

func TestConversationMetaRoundTrips(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)

	t.Run("create stores the document the first send carries", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		created, err := repo.Create(ctx, conversationMetaProject, conversations.Conversation{
			Name: "meta-create",
			Meta: map[string]any{"steps_limit": float64(7)},
		})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}
		if created.Meta["steps_limit"] != float64(7) {
			t.Fatalf("create answered meta %v, want steps_limit 7", created.Meta)
		}

		fetched, err := repo.Get(ctx, conversationMetaProject, created.ID)
		if err != nil {
			t.Fatalf("get conversation: %v", err)
		}
		if fetched.Meta["steps_limit"] != float64(7) {
			t.Errorf("get answered meta %v, want steps_limit 7", fetched.Meta)
		}
	})

	t.Run("update writes internal_tools and get reads it back", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		conv, err := repo.Create(ctx, conversationMetaProject, conversations.Conversation{Name: "meta-switch"})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}
		fresh, err := repo.Get(ctx, conversationMetaProject, conv.ID)
		if err != nil {
			t.Fatalf("get conversation: %v", err)
		}
		if len(fresh.Meta) != 0 {
			t.Fatalf("a new conversation carries meta %v, want an empty document", fresh.Meta)
		}

		// Switching one module on: the client PUTs the previous document plus
		// its one change, which is why the column is written whole.
		on := map[string]any{"internal_tools": []any{"planner"}}
		updated, err := repo.Update(ctx, conversationMetaProject, conv.ID, conversations.Conversation{Meta: on})
		if err != nil {
			t.Fatalf("update meta: %v", err)
		}
		if tools, _ := updated.Meta["internal_tools"].([]any); len(tools) != 1 || tools[0] != "planner" {
			t.Fatalf("update answered meta %v, want internal_tools [planner]", updated.Meta)
		}
		stored, err := repo.Get(ctx, conversationMetaProject, conv.ID)
		if err != nil {
			t.Fatalf("re-get conversation: %v", err)
		}
		if tools, _ := stored.Meta["internal_tools"].([]any); len(tools) != 1 || tools[0] != "planner" {
			t.Fatalf("get answered meta %v, want internal_tools [planner]", stored.Meta)
		}

		// Switching it off again must REMOVE the entry, not leave the previous
		// document standing — the direction a write-only-on-append would pass.
		off := map[string]any{"internal_tools": []any{}}
		if _, err := repo.Update(ctx, conversationMetaProject, conv.ID, conversations.Conversation{Meta: off}); err != nil {
			t.Fatalf("update meta off: %v", err)
		}
		cleared, err := repo.Get(ctx, conversationMetaProject, conv.ID)
		if err != nil {
			t.Fatalf("re-get conversation: %v", err)
		}
		if tools, _ := cleared.Meta["internal_tools"].([]any); len(tools) != 0 {
			t.Errorf("get answered internal_tools %v after the switch went off, want empty", tools)
		}
	})

	t.Run("a rename leaves the stored document alone", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
		defer cancel()

		conv, err := repo.Create(ctx, conversationMetaProject, conversations.Conversation{
			Name: "meta-rename",
			Meta: map[string]any{"internal_tools": []any{"planner"}},
		})
		if err != nil {
			t.Fatalf("create conversation: %v", err)
		}
		if _, err := repo.Update(ctx, conversationMetaProject, conv.ID, conversations.Conversation{Name: "renamed"}); err != nil {
			t.Fatalf("rename conversation: %v", err)
		}
		after, err := repo.Get(ctx, conversationMetaProject, conv.ID)
		if err != nil {
			t.Fatalf("get conversation: %v", err)
		}
		if after.Name != "renamed" {
			t.Errorf("the rename did not land: name = %q", after.Name)
		}
		if tools, _ := after.Meta["internal_tools"].([]any); len(tools) != 1 {
			t.Errorf("the rename blanked the settings: meta = %v", after.Meta)
		}
	})
}
