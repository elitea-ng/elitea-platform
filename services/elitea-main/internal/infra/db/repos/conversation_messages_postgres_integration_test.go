package repos

// DEFECT #599: ListMessages returned an empty transcript for every conversation
// the web client opened.
//
// `chat_message_group.conversation_id` is an `integer` FK to
// `chat_conversations.id`, but the route the transcript is fetched over —
// GET /messages/prompt_lib/{projectID}/{conversationID} — carries the
// conversation UUID (apps/elitea-web/src/entities/conversation/api/messageApi.ts).
// The repository passed that string straight into the comparison, so Postgres
// raised `invalid input syntax for type integer`, and the method answered
// `{"items":[],"total":0}` with a nil error. A failure was reported as an empty
// conversation.
//
// WHAT THE RED RUN SHOWS. Against the unchanged previous code
// TestListMessagesFindsTheTranscriptByConversationUUID fails with total 0 and 0
// items where 2 are seeded, and TestDeleteMessagesAcceptsTheConversationUUID
// fails with the raw SQLSTATE 22P02. The `...ByNumericID` test passes both
// before and after — which is the point: a test written against the numeric id
// exercises the form the defect never reached.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

// seedTranscript writes a conversation with two message groups — one authored
// by the user participant, one by the agent — and returns both identifier
// forms the routes use for it.
func seedTranscript(t *testing.T, repo *ConversationsRepo) (numericID, conversationUUID string) {
	t.Helper()
	ctx := context.Background()

	if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'transcript', 7, 'agent')
RETURNING id::text, uuid::text`).Scan(&numericID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	for _, entityName := range []string{"user", "application"} {
		if _, err := repo.pool.Exec(ctx, `
WITH participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), $1, '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
), grp AS (
    INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id)
    SELECT gen_random_uuid(), participant.id, $2::int FROM participant
    RETURNING id
), item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, grp.id FROM grp
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT item.id, $3 FROM item`,
			entityName, numericID, "said by "+entityName); err != nil {
			t.Fatalf("seed %s message group: %v", entityName, err)
		}
	}

	return numericID, conversationUUID
}

// The regression the issue names: the transcript must be reachable by the
// identifier the web client actually sends.
func TestListMessagesFindsTheTranscriptByConversationUUID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	_, conversationUUID := seedTranscript(t, repo)

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages by UUID: %v", err)
	}
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("listing by conversation UUID returned total %d and %d items, want 2 and 2",
			resp.Total, len(resp.Items))
	}
	roles := map[string]int{}
	for _, m := range resp.Items {
		roles[m.Role]++
		if m.Content == "" {
			t.Errorf("message %s came back with empty content", m.UUID)
		}
	}
	if roles["user"] != 1 || roles["assistant"] != 1 {
		t.Fatalf("roles %v, want one user and one assistant", roles)
	}
}

// The numeric form has to keep working: the same repository method is reached
// from routes that pass the id.
func TestListMessagesFindsTheTranscriptByNumericID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	numericID, _ := seedTranscript(t, repo)

	resp, err := repo.ListMessages(context.Background(), "1", numericID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages by numeric id: %v", err)
	}
	if resp.Total != 2 || len(resp.Items) != 2 {
		t.Fatalf("listing by numeric id returned total %d and %d items, want 2 and 2",
			resp.Total, len(resp.Items))
	}
}

// An identifier no conversation carries is a 404, not a successful empty
// transcript — the distinction the old code could not express.
func TestListMessagesRejectsAnUnknownConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	seedTranscript(t, repo)

	for _, identifier := range []string{
		"2147483000",
		"e0ac9d1e-06e4-4d3f-9e39-1f3a1c7f6d55",
		"not-an-identifier",
	} {
		if _, err := repo.ListMessages(context.Background(), "1", identifier, wholeTranscript()); err == nil {
			t.Errorf("listing messages for %q succeeded, want an error", identifier)
		}
	}
}

// DeleteMessages sits on the same route and took the same parameter, so it
// carried the same defect — it just failed loudly instead of silently. It also
// named `chat_messages`, a table this repository's migrations never create, so
// the fix has to be checked by reading the transcript back rather than by the
// absence of an error.
func TestDeleteMessagesClearsTheTranscriptByConversationUUID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTranscript(t, repo)

	if err := repo.DeleteMessages(ctx, "1", conversationUUID); err != nil {
		t.Fatalf("delete messages by conversation UUID: %v", err)
	}

	resp, err := repo.ListMessages(ctx, "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages after delete: %v", err)
	}
	if resp.Total != 0 || len(resp.Items) != 0 {
		t.Fatalf("after clearing, the transcript still reports total %d and %d items",
			resp.Total, len(resp.Items))
	}

	// The conversation itself survives; only its messages go.
	var conversations int
	if err := repo.pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.chat_conversations WHERE uuid = $1::uuid`, conversationUUID).
		Scan(&conversations); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if conversations != 1 {
		t.Fatalf("clearing messages left %d conversation rows, want 1", conversations)
	}
}

// The sibling read paths take the same parameter and compared it the same way.
func TestConversationReadsAcceptTheConversationUUID(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID := seedTranscript(t, repo)

	groups, err := repo.ListMessageGroups(ctx, "1", conversationUUID, 50, "asc")
	if err != nil {
		t.Fatalf("list message groups by UUID: %v", err)
	}
	if len(groups) != 2 {
		t.Errorf("list message groups by UUID returned %d groups, want 2", len(groups))
	}

	// GetContextState replaced GetContextAnalytics, and the count moved with
	// it: `MessageGroupsTotal` is the COUNT(*) this assertion always meant,
	// while `message_groups_in_context` now means what its name says — how
	// many groups the runtime kept in the assembled context — and is reported
	// as unavailable until that runtime records it.
	state, err := repo.GetContextState(ctx, "1", conversationUUID)
	if err != nil {
		t.Fatalf("context state by UUID: %v", err)
	}
	if state.MessageGroupsTotal != 2 {
		t.Errorf("context state counted %d message groups, want 2", state.MessageGroupsTotal)
	}
	// A conversation nobody has configured stores neither document.
	if state.Strategy != nil {
		t.Errorf("unconfigured conversation carries a stored strategy: %s", state.Strategy)
	}
	if state.Analytics != nil {
		t.Errorf("conversation with no runtime record carries analytics: %s", state.Analytics)
	}
}

// #975: a REWRITTEN group must serve the time it was rewritten, and one that
// was never rewritten must state no update time at all.
//
// The column is already stamped by every finalize path
// (`FinalizeCurrentAgentFullMessage`, `ResetCurrentAgentResponse`, and the two
// pause finalizes — internal/db/queries/agent_chat.sql); what was missing was
// the read. Until this, a regenerated answer came back carrying only
// `created_at` — the time the text it REPLACED had arrived — so the transcript
// showed fresh words under a stale timestamp and no other field existed to
// read instead.
func TestListMessagesServesTheUpdateTimeOfARewrittenGroup(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	numericID, _ := seedTranscript(t, repo)

	before, err := repo.ListMessages(ctx, "1", numericID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, message := range before.Items {
		if message.UpdatedAt != nil {
			t.Fatalf("a group that was never rewritten must state no update time, got %v", *message.UpdatedAt)
		}
	}

	// Exactly what a regeneration's finalize does to the answer row.
	if _, err := repo.pool.Exec(ctx, `
UPDATE p_1.chat_message_group
SET updated_at = clock_timestamp()
WHERE conversation_id = $1::int
  AND author_participant_id IN (
      SELECT id FROM p_1.chat_participants WHERE entity_name <> 'user'
  )`, numericID); err != nil {
		t.Fatalf("stamp the rewritten group: %v", err)
	}

	after, err := repo.ListMessages(ctx, "1", numericID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages after the rewrite: %v", err)
	}
	stamped := 0
	for _, message := range after.Items {
		if message.Role == "user" {
			if message.UpdatedAt != nil {
				t.Fatalf("the question was not rewritten and must keep stating no update time")
			}
			continue
		}
		if message.UpdatedAt == nil {
			t.Fatalf("the rewritten answer must serve its update time")
		}
		// The point of the field: it is LATER than the row's creation, so a
		// renderer preferring it shows when the text actually arrived.
		if !message.UpdatedAt.After(message.CreatedAt) {
			t.Fatalf("update time %v must be after the creation time %v", *message.UpdatedAt, message.CreatedAt)
		}
		stamped++
	}
	if stamped != 1 {
		t.Fatalf("exactly one group was rewritten, %d came back stamped", stamped)
	}
}
