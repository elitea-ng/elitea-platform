package repos

// A people-only message is stored as the sender's message with no response
// group, no addressee and no execution behind it, once per message id.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

func TestHumanMessageRepoStoresAPeopleOnlyMessageOnceAndWithNoTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewHumanMessageRepo(pool)
	ctx := context.Background()

	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'people', 42, 'agent', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`).Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatal(err)
	}
	var sender int
	if err := pool.QueryRow(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
), mapping AS (
    INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
    SELECT $1, participant.id, '{}'::jsonb, now() AT TIME ZONE 'UTC' FROM participant
)
SELECT id FROM participant`, conversationID).Scan(&sender); err != nil {
		t.Fatal(err)
	}

	message := agentexecutionapp.HumanMessage{
		ProjectID: 1, AuthorUserID: 42, ConversationUUID: conversationUUID,
		QuestionID:     "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		QuestionItemID: "31df012a-300d-4722-9be2-521d987c63a8",
		UserInput:      "@Aksana can you see this?",
		Meta:           []byte(`{"mentioned_user_ids":[11]}`),
	}
	created, err := repo.PostHumanMessage(ctx, message)
	if err != nil || !created {
		t.Fatalf("first post: created=%v err=%v", created, err)
	}
	var (
		author, sentTo, replies int
		kind, content, meta     string
		addressed               int
	)
	if err := pool.QueryRow(ctx, `
SELECT g.author_participant_id, COALESCE(g.sent_to_id, 0), g.author_kind, cardinality(g.addressed_to),
       g.meta::text, t.content,
       (SELECT count(*) FROM p_1.chat_message_group r WHERE r.reply_to_id = g.id)
FROM p_1.chat_message_group g
JOIN p_1.chat_message_items i ON i.message_group_id = g.id
JOIN p_1.chat_messages_text t ON t.id = i.id
WHERE g.uuid = $1::uuid`, message.QuestionID).Scan(&author, &sentTo, &kind, &addressed, &meta, &content, &replies); err != nil {
		t.Fatal(err)
	}
	if author != sender || sentTo != 0 || kind != "human" || addressed != 0 || replies != 0 || content != message.UserInput {
		t.Fatalf("author=%d (sender %d) sent_to=%d kind=%q addressed=%d replies=%d content=%q",
			author, sender, sentTo, kind, addressed, replies, content)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.agent_execution_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("a people-only message started %d executions", jobs)
	}

	// The same id again stores nothing and says so.
	created, err = repo.PostHumanMessage(ctx, message)
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	var groups int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1::uuid`, message.QuestionID).Scan(&groups); err != nil || groups != 1 {
		t.Fatalf("groups=%d err=%v", groups, err)
	}

	// Somebody who is not in the conversation cannot post to it.
	stranger := message
	stranger.AuthorUserID = 99
	stranger.QuestionID = "5d1e9c42-0d8b-4f55-8d63-8b0f0c1a2b3c"
	stranger.QuestionItemID = "6e2f0d53-1e9c-4a66-9e74-9c1a1d2b3c4d"
	if _, err := repo.PostHumanMessage(ctx, stranger); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("a non-participant: err=%v", err)
	}
}

// The message id is the group's uuid, unique across conversations and shared
// with a turn's question: the same id elsewhere, or an id that already started a
// turn, is an idempotency conflict (never a unique violation, never "posted").
func TestHumanMessageRepoIdempotencyConflicts(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewHumanMessageRepo(pool)
	ctx := context.Background()

	conversation := func(name string) string {
		t.Helper()
		var id int
		var uuid string
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), $1, 42, 'agent', now() AT TIME ZONE 'UTC')
RETURNING id, uuid::text`, name).Scan(&id, &uuid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at)
SELECT $1, participant.id, '{}'::jsonb, now() AT TIME ZONE 'UTC' FROM participant`, id); err != nil {
			t.Fatal(err)
		}
		return uuid
	}
	firstUUID := conversation("first")
	secondUUID := conversation("second")

	message := agentexecutionapp.HumanMessage{
		ProjectID: 1, AuthorUserID: 42, ConversationUUID: firstUUID,
		QuestionID:     "ee92ccbd-3312-4c72-b20b-fddf224e7c0e",
		QuestionItemID: "31df012a-300d-4722-9be2-521d987c63a8",
		UserInput:      "hello people",
		Meta:           []byte(`{}`),
	}
	if created, err := repo.PostHumanMessage(ctx, message); err != nil || !created {
		t.Fatalf("post: created=%v err=%v", created, err)
	}
	// The group is stamped as a human message to nobody in particular.
	var cause, visibility string
	var addressed int
	if err := pool.QueryRow(ctx, `SELECT cause, visibility, cardinality(addressed_to) FROM p_1.chat_message_group WHERE uuid = $1::uuid`,
		message.QuestionID).Scan(&cause, &visibility, &addressed); err != nil {
		t.Fatal(err)
	}
	if cause != "human_message" || visibility != "all" || addressed != 0 {
		t.Fatalf("cause=%q visibility=%q addressed=%d", cause, visibility, addressed)
	}

	// The same id in ANOTHER conversation.
	elsewhere := message
	elsewhere.ConversationUUID = secondUUID
	if _, err := repo.PostHumanMessage(ctx, elsewhere); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("another conversation: err=%v", err)
	}

	// An id that already started a turn: its question has a reply group.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, reply_to_id, is_streaming, created_at)
SELECT gen_random_uuid(), g.author_participant_id, g.conversation_id, g.id, FALSE, now() AT TIME ZONE 'UTC'
FROM p_1.chat_message_group g WHERE g.uuid = $1::uuid`, message.QuestionID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PostHumanMessage(ctx, message); !errors.Is(err, executionapp.ErrIdempotencyConflict) {
		t.Fatalf("a question that started a turn: err=%v", err)
	}
}
