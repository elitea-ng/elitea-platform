package repos

// "Make public", which used to be a gesture and nothing else.
//
// `chat_conversations.is_private` is set to TRUE by the INSERT and was read by
// no route and written by none: `Update` never looked at the key the client
// sends, and `Get` never answered it. So the menu item confirmed, the client
// patched its own list, and a reload put the conversation back to private —
// with nothing anywhere reporting a failure, because nothing had failed. The
// request was simply discarded.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
)

func TestUpdatePublishesAConversationAndTheReadAnswersIt(t *testing.T) {
	pool := newFreshInstallPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo := NewConversationsRepo(pool)

	created, err := repo.Create(ctx, "1", conversations.Conversation{Name: "autotest_visibility", CreatedBy: "1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A conversation starts private, and the read must say so: the row menu
	// offers "Make public" exactly while this is true.
	before, err := repo.Get(ctx, "1", created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if before.IsPrivate == nil || !*before.IsPrivate {
		t.Fatalf("a new conversation reads as is_private=%v, want true", before.IsPrivate)
	}

	published := false
	updated, err := repo.Update(ctx, "1", created.ID, conversations.Conversation{IsPrivate: &published})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.IsPrivate == nil || *updated.IsPrivate {
		t.Errorf("the PUT answered is_private=%v, want false", updated.IsPrivate)
	}

	after, err := repo.Get(ctx, "1", created.ID)
	if err != nil {
		t.Fatalf("Get after the publish: %v", err)
	}
	if after.IsPrivate == nil || *after.IsPrivate {
		t.Errorf("the conversation reads as is_private=%v after being published", after.IsPrivate)
	}

	// A RENAME must not republish it. Every other PUT this client makes sends
	// no `is_private` at all, so a repository that read an absent field as
	// `false` — or as `true` — would flip the flag on every rename.
	if _, err := repo.Update(ctx, "1", created.ID, conversations.Conversation{Name: "autotest_visibility_renamed"}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	renamed, err := repo.Get(ctx, "1", created.ID)
	if err != nil {
		t.Fatalf("Get after the rename: %v", err)
	}
	if renamed.Name != "autotest_visibility_renamed" {
		t.Errorf("name=%q, want the new name", renamed.Name)
	}
	if renamed.IsPrivate == nil || *renamed.IsPrivate {
		t.Errorf("the rename put the conversation back to is_private=%v", renamed.IsPrivate)
	}
}
