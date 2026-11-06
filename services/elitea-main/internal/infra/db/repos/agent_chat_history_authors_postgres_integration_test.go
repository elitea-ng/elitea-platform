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
	"encoding/json"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
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
	return insertPostgresHistoryGroupIn(t, tx, 1, id, author, replyTo, text, minutesAgo)
}

// insertPostgresHistoryGroupIn is insertPostgresHistoryGroup in conversation
// `conversation`.
func insertPostgresHistoryGroupIn(
	t *testing.T,
	tx pgx.Tx,
	conversation, id, author, replyTo int,
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
        $1, gen_random_uuid(), $2, $6, NULLIF($3, 0), '{}'::jsonb, FALSE,
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
SELECT message_item.id, $4 FROM message_item`, id, author, replyTo, text, minutesAgo, conversation); err != nil {
		t.Fatal(err)
	}
	return id
}

// The ad-hoc projection is a separate copy of the same SQL: a model chat
// with an agent participant labels that agent's answers too.
func TestPostgresAdhocChatHistoryLabelsAnAgentParticipantsAnswer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	// Conversation 2: the asker (20), the model chat (23, `dummy`) and
	// `leaf-agent` (30), a named agent participant.
	question := insertPostgresHistoryGroupIn(t, tx, 2, 1603011, 20, 0, "check the build", 3)
	insertPostgresHistoryGroupIn(t, tx, 2, 1603012, 30, question, "It is green.", 2)
	insertPostgresHistoryGroupIn(t, tx, 2, 1603013, 23, question, "Confirmed.", 1)

	target, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000001611"),
			ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032"),
		},
	)
	if err != nil {
		t.Fatalf("resolve ad-hoc turn: %v", err)
	}
	var history []postgresHistoryGroup
	if err := json.Unmarshal([]byte(target.ChatHistoryJson), &history); err != nil {
		t.Fatalf("decode %s: %v", target.ChatHistoryJson, err)
	}
	if len(history) != 3 || history[0].Role != "user" || len(history[0].AdditionalKwargs) != 0 ||
		history[1].Role != "user" || history[2].Role != "assistant" ||
		len(history[2].AdditionalKwargs) != 0 {
		t.Fatalf("history=%s", target.ChatHistoryJson)
	}
	author, _ := history[1].AdditionalKwargs["elitea_author"].(map[string]any)
	if author["kind"] != "agent" || author["name"] != "leaf-agent" {
		t.Fatalf("peer=%+v", history[1].AdditionalKwargs)
	}
}

// ADR-0050 decision 9c (#1603): every history entry is named by its message
// and marked when it is the reader's own, so the worker can append the rest
// to the reader's durable session once.
func TestPostgresChatHistoryRefsNameEveryEntryAndMarkTheReadersOwn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)
	if _, err := tx.Exec(t.Context(), `
INSERT INTO chat_participants (id, uuid, entity_name, entity_meta, meta) VALUES
    (26, 'a0000000-0000-4000-8000-000000001604', 'application', '{"id":32,"project_id":1}'::jsonb, '{"name":"CI Doctor"}'::json),
    (27, '90000000-0000-4000-8000-000000001604', 'user', '{"id":12}'::jsonb, '{"user_name":"Sam"}'::json);
INSERT INTO chat_participant_mapping (conversation_id, participant_id, entity_settings, created_at) VALUES
    (1, 26, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (1, 27, '{}'::jsonb, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatal(err)
	}
	// A question to the reader (21), its answer, and a question to the peer
	// (26) with the peer's answer.
	toReader := insertPostgresHistoryGroup(t, tx, 1603021, 20, 0, "why is CI red?", 4)
	insertPostgresHistoryGroup(t, tx, 1603022, 21, toReader, "Step 3 failed.", 3)
	toPeer := insertPostgresHistoryGroup(t, tx, 1603023, 20, 0, "can you fix it?", 2)
	insertPostgresHistoryGroup(t, tx, 1603024, 26, toPeer, "Fixed.", 1)
	// Another person's turn with the reader lives in that person's session,
	// not the asker's: it is not the reader's own here.
	fromSam := insertPostgresHistoryGroup(t, tx, 1603025, 27, 0, "and the docs?", 1)
	insertPostgresHistoryGroup(t, tx, 1603026, 21, fromSam, "Also fixed.", 0)
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group SET sent_to_id = CASE id WHEN 1603023 THEN 26 ELSE 21 END
WHERE id IN (1603021, 1603023, 1603025)`); err != nil {
		t.Fatal(err)
	}
	target, err := queries.ResolveCurrentApplicationTurn(t.Context(), sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21, ProjectID: 1,
		QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000001604"),
		ConversationUuid: mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031"),
	})
	if err != nil {
		t.Fatalf("resolve application turn: %v", err)
	}
	refs, err := agentexecutionapp.DecodeChatHistoryRefs(
		json.RawMessage(target.ChatHistoryJson), json.RawMessage(target.ChatHistoryRefsJson))
	if err != nil {
		t.Fatalf("decode refs %s: %v", target.ChatHistoryRefsJson, err)
	}
	var uuids []string
	if err := tx.QueryRow(t.Context(), `
SELECT array_agg(uuid::text ORDER BY id) FROM chat_message_group
WHERE id BETWEEN 1603021 AND 1603026`).Scan(&uuids); err != nil {
		t.Fatal(err)
	}
	own := []bool{true, true, false, false, false, false}
	if len(refs) != 6 {
		t.Fatalf("refs=%+v", refs)
	}
	for index, ref := range refs {
		if ref.MessageID != uuids[index] || ref.ReadersOwn != own[index] {
			t.Fatalf("refs[%d]=%+v, want %s own=%v", index, ref, uuids[index], own[index])
		}
	}
}
