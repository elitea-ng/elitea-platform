package repos

// ADR-0050 decision 9a (issue 1603): only the reader's own answers are
// `assistant` in the history projection.
//
// Before, every non-user author was an unnamed `assistant`, so in a
// conversation with several agents the agent being asked took a peer's words
// for its own. A peer agent's message, and a message by a person other than
// the one asking, is now user-role and carries its author in
// `additional_kwargs.elitea_author`; the worker frames it as a labelled,
// untrusted message.

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

func TestPostgresChatHistoryLabelsPeersAndKeepsOnlyTheReadersAnswersAsAssistant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	if _, err := tx.Exec(t.Context(), `
INSERT INTO chat_participants (id, uuid, entity_name, entity_meta, meta) VALUES
    (26, 'a0000000-0000-4000-8000-000000001603', 'application', '{"id":32,"project_id":1}'::jsonb, '{"name":"CI Doctor"}'::json),
    (27, '90000000-0000-4000-8000-000000001603', 'user', '{"id":12}'::jsonb, '{"user_name":"Sam"}'::json);
INSERT INTO chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES
    (1, 26, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (1, 27, '{}'::jsonb, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}
	// The asker's question, a peer's answer to it, another person's message
	// and the reader's answer to that one, in that order.
	question := insertPostgresHistoryGroup(t, tx, 1603001, 20, 0, "why is CI red?", 4)
	insertPostgresHistoryGroup(t, tx, 1603002, 26, question, "Step 3 failed.", 3)
	other := insertPostgresHistoryGroup(t, tx, 1603003, 27, 0, "can you fix it?", 2)
	insertPostgresHistoryGroup(t, tx, 1603004, 21, other, "On it.", 1)

	history := resolvePostgresApplicationChatHistory(
		t, queries, mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
		"50000000-0000-4000-8000-000000001603",
	)
	if len(history) != 4 {
		t.Fatalf("history=%+v", history)
	}
	want := []struct {
		role, text, kind, name string
	}{
		{"user", "why is CI red?", "", ""},
		{"user", "Step 3 failed.", "agent", "CI Doctor"},
		{"user", "can you fix it?", "person", "Sam"},
		{"assistant", "On it.", "", ""},
	}
	for index, expected := range want {
		group := history[index]
		if group.Role != expected.role || len(group.Content) != 1 ||
			group.Content[0]["text"] != expected.text {
			t.Fatalf("history[%d]=%+v", index, group)
		}
		if expected.kind == "" {
			if len(group.AdditionalKwargs) != 0 {
				t.Fatalf("history[%d] additional_kwargs=%+v", index, group.AdditionalKwargs)
			}
			continue
		}
		author, ok := group.AdditionalKwargs["elitea_author"].(map[string]any)
		if !ok || len(group.AdditionalKwargs) != 1 || len(author) != 2 ||
			author["kind"] != expected.kind || author["name"] != expected.name {
			t.Fatalf("history[%d] additional_kwargs=%+v", index, group.AdditionalKwargs)
		}
	}
}

// insertPostgresHistoryGroup writes one completed message group with one text
// item, `minutesAgo` minutes in the past, and returns its id. replyTo 0 means
// none.
func insertPostgresHistoryGroup(
	t *testing.T,
	tx pgx.Tx,
	id, author, replyTo int,
	text string,
	minutesAgo int,
) int {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
WITH message_group AS (
    INSERT INTO chat_message_group (
        id, uuid, author_participant_id, conversation_id, reply_to_id, meta,
        is_streaming, created_at
    ) VALUES (
        $1, gen_random_uuid(), $2, 1, NULLIF($3, 0), '{}'::jsonb, FALSE,
        (statement_timestamp() AT TIME ZONE 'UTC') - make_interval(mins => $5)
    )
    RETURNING id, created_at
), message_item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id, created_at
    )
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, message_group.id,
           message_group.created_at
    FROM message_group
    RETURNING id
)
INSERT INTO chat_messages_text (id, content)
SELECT message_item.id, $4 FROM message_item`, id, author, replyTo, text, minutesAgo); err != nil {
		t.Fatal(err)
	}
	return id
}
