package repos

// The transcript endpoint carried no author identity: GET
// /messages/prompt_lib/{project}/{conversation} answered flat rows with no
// `author_participant_id`, so a reloaded conversation could not attribute a
// single user message — and, before the web client dropped its reader-name
// fallback, it captioned every OTHER participant's question with whoever
// happened to be reading. The author IS persisted (`chat_message_group.
// author_participant_id`, and the list query already joins the participant
// row for its role mapping); the projection just discarded it.
//
// Two halves make attribution real, and each has its own test here:
//   - ListMessages now serves `author_participant_id` (and sent_to/reply_to)
//     per row, which is the lookup key `normaliseUserMessage` resolves
//     against the conversation's participants payload;
//   - ListParticipants resolves a DISPLAY NAME for user participants from
//     auth_core__user, because the REST attach path stores only
//     `entity_meta.id` and the caption would otherwise read "User <n>".
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

func TestListMessagesCarriesTheAuthorParticipantID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	numericID, _ := seedTranscript(t, repo)
	ctx := context.Background()

	// The corpus schema declares author_participant_id NOT NULL
	// (agent_chat_baseline.sql), so every listed row must attribute — the
	// pointer field exists for the 001_initial tenant shape, where the
	// column is nullable and an absent author must read as "states none".
	resp, err := repo.ListMessages(ctx, "1", numericID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("listed %d rows, want 2", len(resp.Items))
	}
	for _, m := range resp.Items {
		if m.AuthorParticipantID == nil {
			t.Fatalf("row %q states no author although the group names one", m.Content)
		}
		var entityName string
		if err := pool.QueryRow(ctx,
			`SELECT entity_name FROM p_1.chat_participants WHERE id = $1`,
			*m.AuthorParticipantID).Scan(&entityName); err != nil {
			t.Errorf("author_participant_id %d resolves no participant row: %v", *m.AuthorParticipantID, err)
		}
		wantEntity := "application"
		if m.Role == "user" {
			wantEntity = "user"
		}
		if entityName != wantEntity {
			t.Errorf("row %q (role %s) is authored by a %q participant", m.Content, m.Role, entityName)
		}
	}
}

func TestListParticipantsResolvesUserDisplayNames(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()

	var conversationID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'author identity', 7, 'agent', now() AT TIME ZONE 'UTC')
RETURNING id::text`).Scan(&conversationID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// auth_core__user is bootstrap-owned and absent from the shared corpus,
	// exactly the state the probe guard exists for — created here in its
	// 001_initial shape, the way the legacy-table fixtures do elsewhere.
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS auth_core__user (
    id SERIAL PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT,
    last_login TIMESTAMP,
    suspended BOOLEAN NOT NULL DEFAULT false
)`); err != nil {
		t.Fatalf("create the bootstrap auth table: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO auth_core__user (id, email, name)
VALUES (4207, 'alice@example.test', 'Alice Author')
ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name`); err != nil {
		t.Fatalf("seed auth user: %v", err)
	}

	seedParticipant := func(entityMeta, meta string) int {
		t.Helper()
		var id int
		if err := pool.QueryRow(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
    VALUES (gen_random_uuid(), 'user', $1::jsonb, $2::jsonb)
    RETURNING id
), mapping AS (
    INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
    SELECT $3::int, participant.id, now() AT TIME ZONE 'UTC' FROM participant
)
SELECT id FROM participant`, entityMeta, meta, conversationID).Scan(&id); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		return id
	}

	// The three shapes in the wild: the REST attach (id only, needs the
	// lookup), a socket-era row that already carries its name (must be kept),
	// and an id the auth table no longer holds (must stay bare — the reader
	// renders its own "no longer available" state for it).
	resolved := seedParticipant(`{"id": 4207}`, `{}`)
	kept := seedParticipant(`{"id": 4207}`, `{"user_name": "Stored Name"}`)
	departed := seedParticipant(`{"id": 999999999}`, `{}`)

	items, err := repo.ListParticipants(ctx, "1", conversationID)
	if err != nil {
		t.Fatalf("list participants: %v", err)
	}
	names := map[int]any{}
	for _, p := range items {
		names[p.ID] = p.Meta["user_name"]
	}
	if names[resolved] != "Alice Author" {
		t.Errorf("REST-attached participant resolved user_name %v, want the auth table's 'Alice Author'", names[resolved])
	}
	if names[kept] != "Stored Name" {
		t.Errorf("a stored user_name was overwritten: got %v, want 'Stored Name'", names[kept])
	}
	if value, exists := names[departed]; exists && value != nil {
		t.Errorf("a departed user grew a user_name: %v", value)
	}
}
