package repos

// DEFECT: AddParticipant always INSERTed a new chat_participants row.
//
// The statement supplied its own `gen_random_uuid()`, and p_N.chat_participants
// has no unique key on (entity_name, entity_meta) — only `id` PRIMARY KEY and
// `uuid` UNIQUE (migrations/tenant/0123_agent_chat_message_tables.sql:69-74).
// The INSERT therefore could not conflict, so the `err != nil` fallback that
// looked for an existing participant was unreachable dead code, and the
// mapping insert's `ON CONFLICT ON CONSTRAINT _participant_conversation_uc`
// guard never fired either, because participant_id was new every time.
//
// Adding the same agent twice — a double click, or a retry after a failed
// first attempt — left two chat_participants rows and two
// chat_participant_mapping rows in one conversation. The participant picker
// showed the agent twice; internal/db/queries/agent_chat.sql aggregates the
// mapping rows into the adhoc tools payload, so a duplicated toolkit was sent
// to the model twice; and ResolveCurrentAdhocTurn is a sqlc `:one` query, so a
// duplicated user participant silently kept whichever row came first.
//
// legacy/plugins/elitea_core/utils/participant_utils.py `get_or_create_one`
// looks the participant up first, keyed on a per-type subset of entity_meta.
// The Go port kept the ON CONFLICT clause but dropped the find-first.
//
// WHAT THE RED RUN SHOWS, in two stages. The previous code carried TWO
// defects, and the first one hides the second.
//
//  1. Against the unchanged previous code all four tests stop earlier than the
//     duplicate-row assertion. Every one fails with `add participant mapping:
//     ERROR: constraint "_participant_conversation_uc" ... does not exist
//     (SQLSTATE 42704)`. The tenant migration declares that unique key
//     anonymously (migrations/tenant/0123_agent_chat_message_tables.sql:87),
//     so the name exists only in the dev bootstrap.
//  2. Correct the ON CONFLICT clause alone, and all four still fail, now on
//     the defect this file is named for: 2 participant rows and 2 mapping
//     rows for one agent added twice, and a nil display name.
//
// Read a red run against stage 1 as evidence of the constraint-name defect
// only. Stage 2 is the evidence for the find-or-create fix.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

func countParticipantRows(t *testing.T, repo *ConversationsRepo, conversationID string) (participants, mappings int) {
	t.Helper()
	ctx := context.Background()
	if err := repo.pool.QueryRow(ctx, `
SELECT count(DISTINCT p.id), count(m.id)
FROM p_1.chat_participant_mapping m
JOIN p_1.chat_participants p ON p.id = m.participant_id
WHERE m.conversation_id = $1`, conversationID).Scan(&participants, &mappings); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	return participants, mappings
}

func seedConversation(t *testing.T, repo *ConversationsRepo) string {
	t.Helper()
	var conversationID string
	if err := repo.pool.QueryRow(context.Background(), `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'participants', 7, 'agent', now() AT TIME ZONE 'UTC')
RETURNING id::text`).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return conversationID
}

func TestAddParticipantIsIdempotentForTheSameEntity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)

	body := func() map[string]any {
		return map[string]any{
			"entity_name": "application",
			"entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Foo"},
		}
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := repo.AddParticipant(context.Background(), "1", conversationID, body()); err != nil {
			t.Fatalf("add participant, attempt %d: %v", attempt, err)
		}
	}

	participants, mappings := countParticipantRows(t, repo, conversationID)
	if participants != 1 || mappings != 1 {
		t.Fatalf("adding the same agent twice produced %d participant rows and %d mapping rows, want 1 and 1",
			participants, mappings)
	}
}

// A rename must not create a second participant. The web client puts a `name`
// inside entity_meta, so whole-document equality — the shape the dead fallback
// used — would treat a renamed agent as a new one.
func TestAddParticipantIgnoresARenameWhenItMatchesTheEntity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)

	ctx := context.Background()
	if err := repo.AddParticipant(ctx, "1", conversationID, map[string]any{
		"entity_name": "application",
		"entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Foo"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddParticipant(ctx, "1", conversationID, map[string]any{
		"entity_name": "application",
		"entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Foo renamed"},
	}); err != nil {
		t.Fatal(err)
	}

	participants, mappings := countParticipantRows(t, repo, conversationID)
	if participants != 1 || mappings != 1 {
		t.Fatalf("a rename produced %d participant rows and %d mapping rows, want 1 and 1",
			participants, mappings)
	}
}

// Two different agents stay two participants. Without this the idempotence
// test above would also pass against a repository that reused one row for
// everything.
func TestAddParticipantKeepsDistinctEntitiesApart(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)

	ctx := context.Background()
	for _, entity := range []map[string]any{
		{"id": 42, "project_id": 1, "name": "Foo"},
		{"id": 43, "project_id": 1, "name": "Bar"},
		{"id": 42, "project_id": 2, "name": "Foo elsewhere"},
	} {
		if err := repo.AddParticipant(ctx, "1", conversationID, map[string]any{
			"entity_name": "application",
			"entity_meta": entity,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One model participant keys on model_name, not on an id it does not have.
	if err := repo.AddParticipant(ctx, "1", conversationID, map[string]any{
		"entity_name": "llm",
		"entity_meta": map[string]any{"integration_uid": "u", "model_name": "gpt-4o"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.AddParticipant(ctx, "1", conversationID, map[string]any{
		"entity_name": "llm",
		"entity_meta": map[string]any{"integration_uid": "u", "model_name": "gpt-4o"},
	}); err != nil {
		t.Fatal(err)
	}

	participants, mappings := countParticipantRows(t, repo, conversationID)
	if participants != 4 || mappings != 4 {
		t.Fatalf("three agents plus one model produced %d participant rows and %d mapping rows, want 4 and 4",
			participants, mappings)
	}
}

// agent_chat.sql reads `meta->>'name'` when it builds the adhoc tools payload.
// The INSERT used to hardcode `'{}'::json`, so every participant this
// repository created contributed a NULL name there.
func TestAddParticipantRecordsTheDisplayName(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationID := seedConversation(t, repo)

	if err := repo.AddParticipant(context.Background(), "1", conversationID, map[string]any{
		"entity_name": "application",
		"entity_meta": map[string]any{"id": 42, "project_id": 1, "name": "Foo"},
	}); err != nil {
		t.Fatal(err)
	}

	var name *string
	if err := repo.pool.QueryRow(context.Background(), `
SELECT p.meta->>'name'
FROM p_1.chat_participant_mapping m
JOIN p_1.chat_participants p ON p.id = m.participant_id
WHERE m.conversation_id = $1`, conversationID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name == nil || *name != "Foo" {
		t.Fatalf("participant meta name = %v, want \"Foo\"", name)
	}
}
