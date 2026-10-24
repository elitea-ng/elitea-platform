package repos

// Cloud child turns of a desktop local turn (issue 1239).
//
// DiscardChildConversation removed a refused child's conversation with a
// bare DELETE, which chat_participant_mapping's non-cascading key refused
// (23503) on every call: each refused start left the conversation behind.
// The discard now removes the mappings first, in one transaction, and still
// never removes a conversation that holds a turn.
//
// ReadChildTurn is the desktop's poll: it must find the child by the
// indexed expressions of tenant 0156 and join its response group uuid to
// uuid.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
)

const childTestParent = "0123456789abcdef0123456789abcdef"

func prepareChildForTest(t *testing.T, repo *LocalChildTurnsRepo, key string) localturn.ChildConversation {
	t.Helper()
	conversation, err := repo.PrepareChildConversation(context.Background(), localturn.ChildConversationRequest{
		ProjectID: 1, ActorUserID: 7, ParentExecutionID: childTestParent, IdempotencyKey: key,
		InputSHA256: "digest", ApplicationID: 21, VersionID: 22, Name: "Jira reader",
	})
	if err != nil {
		t.Fatalf("prepare child conversation: %v", err)
	}
	if !conversation.Created || conversation.ParticipantID == 0 {
		t.Fatalf("prepared = %+v", conversation)
	}
	return conversation
}

func childConversationRows(t *testing.T, repo *LocalChildTurnsRepo, conversationUUID string) (conversations, mappings int) {
	t.Helper()
	if err := repo.pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM p_1.chat_conversations WHERE uuid = $1::uuid),
       (SELECT count(*) FROM p_1.chat_participant_mapping AS mapping
          JOIN p_1.chat_conversations AS conversation ON conversation.id = mapping.conversation_id
         WHERE conversation.uuid = $1::uuid)`, conversationUUID).Scan(&conversations, &mappings); err != nil {
		t.Fatalf("count child rows: %v", err)
	}
	return conversations, mappings
}

// A refused start's conversation goes, its participant mappings with it.
func TestDiscardChildConversationRemovesTheConversationAndItsMappings(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewLocalChildTurnsRepo(pool)
	conversation := prepareChildForTest(t, repo, "k1")
	if conversations, mappings := childConversationRows(t, repo, conversation.ConversationUUID); conversations != 1 || mappings != 2 {
		t.Fatalf("before discard: %d conversations, %d mappings", conversations, mappings)
	}

	repo.DiscardChildConversation(context.Background(), 1, conversation.ConversationUUID)

	if conversations, _ := childConversationRows(t, repo, conversation.ConversationUUID); conversations != 0 {
		t.Fatal("the refused child's conversation was left behind")
	}
	var orphans int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM p_1.chat_participant_mapping AS mapping
 WHERE NOT EXISTS (SELECT 1 FROM p_1.chat_conversations AS c WHERE c.id = mapping.conversation_id)`).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d mappings name no conversation", orphans)
	}
	// The key is free again: a later attempt creates a new conversation.
	if again := prepareChildForTest(t, repo, "k1"); again.ConversationUUID == conversation.ConversationUUID {
		t.Fatal("the discarded conversation was answered again")
	}
}

// A conversation that holds a turn is never discarded, and neither is a
// conversation of another source.
func TestDiscardChildConversationKeepsATurnAndOtherSources(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewLocalChildTurnsRepo(pool)
	ctx := context.Background()
	conversation := prepareChildForTest(t, repo, "k1")
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
SELECT gen_random_uuid(), $2, id, now() AT TIME ZONE 'UTC' FROM p_1.chat_conversations WHERE uuid = $1::uuid`,
		conversation.ConversationUUID, conversation.ParticipantID); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	repo.DiscardChildConversation(ctx, 1, conversation.ConversationUUID)
	if conversations, mappings := childConversationRows(t, repo, conversation.ConversationUUID); conversations != 1 || mappings != 2 {
		t.Fatalf("a conversation with a turn lost rows: %d conversations, %d mappings", conversations, mappings)
	}

	var chatUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'mine', 7, 'elitea', now() AT TIME ZONE 'UTC') RETURNING uuid::text`).Scan(&chatUUID); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	repo.DiscardChildConversation(ctx, 1, chatUUID)
	if conversations, _ := childConversationRows(t, repo, chatUUID); conversations != 1 {
		t.Fatal("a person's own conversation was discarded")
	}
}

// The poll finds the child by its execution ids and reads its response group
// (joined uuid to uuid), and the lookups have the partial indexes of 0156.
func TestReadChildTurnFindsTheStampedResponseGroup(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewLocalChildTurnsRepo(pool)
	ctx := context.Background()
	conversation := prepareChildForTest(t, repo, "k1")
	var responseUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at, task_id, is_streaming)
SELECT gen_random_uuid(), $2, id, now() AT TIME ZONE 'UTC', 'child-exec-1', TRUE
  FROM p_1.chat_conversations WHERE uuid = $1::uuid
RETURNING uuid::text`, conversation.ConversationUUID, conversation.ParticipantID).Scan(&responseUUID); err != nil {
		t.Fatalf("seed response group: %v", err)
	}
	if err := repo.RecordChildStart(ctx, 1, conversation.ConversationUUID, "child-exec-1", responseUUID); err != nil {
		t.Fatalf("record start: %v", err)
	}

	state, err := repo.ReadChildTurn(ctx, 1, 7, childTestParent, "child-exec-1")
	if err != nil {
		t.Fatalf("read child: %v", err)
	}
	if state.Removed || !state.Streaming || state.ResponseMessageID != responseUUID ||
		state.ConversationUUID != conversation.ConversationUUID || state.ExecutionID != "child-exec-1" {
		t.Fatalf("state = %+v", state)
	}
	if _, err := repo.ReadChildTurn(ctx, 1, 8, childTestParent, "child-exec-1"); err != localturn.ErrChildTurnNotFound {
		t.Fatalf("another caller's read: %v", err)
	}

	// A stamp that is not a uuid is refused, never stored.
	if err := repo.RecordChildStart(ctx, 1, conversation.ConversationUUID, "child-exec-2", "not-a-uuid"); err == nil {
		t.Fatal("a non-uuid response id was stamped")
	}

	var indexes int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_indexes
 WHERE schemaname = 'p_1' AND tablename = 'chat_conversations'
   AND indexname IN ('chat_conversations_local_child_execution_idx', 'chat_conversations_local_child_key_idx')
   AND indexdef LIKE '%WHERE ((source)::text = ''local_child''::text)%'`).Scan(&indexes); err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if indexes != 2 {
		t.Fatalf("%d of the 2 partial local_child indexes exist", indexes)
	}
}
