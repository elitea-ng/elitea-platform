package repos

// A legal hold keeps a transcript against every user-driven destructive path,
// not only the whole-conversation delete (ADR-0042 decision 4): clearing the
// chat, deleting a message, stripping the attachments, carving a canvas out of
// a message, and regenerating or editing a turn. Requires
// ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

func requireHoldConflict(t *testing.T, label string, err error) {
	t.Helper()
	var apiErr *apierr.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("%s on a held conversation = %v, want 409", label, err)
	}
}

func TestLegalHoldRefusesEveryDestructivePath(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	holds := NewRetentionPolicyRepo(pool)
	ctx := context.Background()

	numericID, conversationUUID, groups := seedConversationWithParticipant(t, repo, "question", "answer")
	seedAttachmentItem(t, repo, groups[1], "chat-attachments", "conv/held.pdf")
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations
		SET meta = '{"attachments": [{"name": "held.pdf"}]}'::jsonb WHERE id = $1::int`, numericID); err != nil {
		t.Fatal(err)
	}
	stored, err := holds.CreateHold(ctx, retention.Hold{
		Scope: retention.HoldUser, UserID: ptrInt64(7), Reason: "matter", CreatedBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	held, err := repo.ConversationHeld(ctx, "1", conversationUUID)
	if err != nil || !held {
		t.Fatalf("ConversationHeld = %v, %v; want true", held, err)
	}

	requireHoldConflict(t, "clear", repo.DeleteMessages(ctx, "1", conversationUUID))
	_, err = repo.DeleteMessage(ctx, "1", groups[1], conversationAuthorID)
	requireHoldConflict(t, "delete message", err)
	requireHoldConflict(t, "delete attachments", repo.DeleteAttachments(ctx, "1", conversationUUID))

	var groupID, itemID int
	if err := pool.QueryRow(ctx, `SELECT g.id, i.id FROM p_1.chat_message_group g
		JOIN p_1.chat_message_items i ON i.message_group_id = g.id AND i.item_type = 'text_message'
		WHERE g.uuid = $1::uuid`, groups[0]).Scan(&groupID, &itemID); err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateCanvas(ctx, "1", map[string]any{
		"message_group_id": groupID, "message_item_id": itemID,
		"canvas_content_starts_at": 0, "canvas_content_ends_at": 4,
	})
	requireHoldConflict(t, "carve canvas", err)

	// Regenerate and edit: the admission's reset refuses before it deletes
	// the answer or rewrites the question. The hold check runs first, so the
	// remaining identities only need to be well-formed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = resetCurrentAgentResponse(ctx, tx, nil, uuid.NewString(), agentexecutionapp.CurrentRegenerateTurn{
		ProjectID: 1, ActorUserID: 7, ConversationUUID: conversationUUID, TargetParticipantID: 1,
		ApplicationID: 1, ApplicationVersionID: 1,
		QuestionID: groups[0], ResponseMessageID: groups[1],
	})
	_ = tx.Rollback(ctx)
	if !errors.Is(err, agentexecutionapp.ErrConversationLegalHold) {
		t.Fatalf("regenerate on a held conversation = %v, want ErrConversationLegalHold", err)
	}

	conversations, groupsLeft, items, texts, _ := countConversationRows(t, repo)
	if conversations != 1 || groupsLeft != 2 || items != 3 || texts != 2 {
		t.Fatalf("held transcript changed: %d conversations, %d groups, %d items, %d texts",
			conversations, groupsLeft, items, texts)
	}
	var attachmentsKept bool
	if err := pool.QueryRow(ctx, `SELECT meta ? 'attachments' FROM p_1.chat_conversations WHERE id = $1::int`,
		numericID).Scan(&attachmentsKept); err != nil || !attachmentsKept {
		t.Fatalf("held conversation lost its attachment meta: %v, %v", attachmentsKept, err)
	}

	// Released: the same paths work again.
	if _, err := holds.ReleaseHold(ctx, stored.ID, 1, "closed"); err != nil {
		t.Fatal(err)
	}
	if held, err := repo.ConversationHeld(ctx, "1", conversationUUID); err != nil || held {
		t.Fatalf("after release ConversationHeld = %v, %v", held, err)
	}
	if err := repo.DeleteAttachments(ctx, "1", conversationUUID); err != nil {
		t.Fatalf("delete attachments after release: %v", err)
	}
	if err := repo.DeleteMessages(ctx, "1", conversationUUID); err != nil {
		t.Fatalf("clear after release: %v", err)
	}
}
