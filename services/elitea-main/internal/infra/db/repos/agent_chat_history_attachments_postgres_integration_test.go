package repos

// #606 part 3, gap 2: a PRIOR turn's attachment chunks reach the model.
//
// Part 2 made an upload an `attachment_message` item on the question group.
// The chat-history projection still joined `text_message` alone, so the row
// existed, the transcript rendered it, and the prompt did not mention it: a
// follow-up question about a file attached one turn earlier had nothing to
// answer from. Pylon extends the item's stored `content` LIST into the group's
// content array instead (utils/chat_history.py:67-73).
//
// These run against real Postgres because every property under test is a
// property of the SQL: what the join set is, where the chunks land relative to
// the text, and what the projection does with the payloads pylon's own data
// contains -- NULL and a `'{}'::json` default that is an object, not a list.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type postgresHistoryGroup struct {
	Role             string           `json:"role"`
	Content          []map[string]any `json:"content"`
	AdditionalKwargs map[string]any   `json:"additional_kwargs"`
}

func TestPostgresChatHistoryExtendsAPriorTurnsAttachmentChunksInItemOrder(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000031"
	response := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"what does this say?", "execution-history-attach-1",
	)
	// order_index 1..n are the attachments of that same question (pylon
	// enumerates them from 1 — rpc/chat_all.py:303). Their payloads are the
	// four states real data is in: a scaffold the worker has already extended
	// with a second chunk, a NULL, the `'{}'::json` pylon default, and an
	// image chunk — which is now WITHHELD from history (#984, and
	// `TestPostgresChatHistoryDoesNotCarryAnEmbeddedImageIntoALaterTurn`).
	attachPostgresChunksToGroup(t, tx, questionID, 1, `[
        {"type":"text","text":"Bucket: chat-attachments","elitea_attachment":{"needs_content_extraction":true,"name":"conv/report.pdf"}},
        {"type":"text","text":"EXTRACTED TEXT"}
    ]`)
	attachPostgresChunksToGroup(t, tx, questionID, 2, "")
	attachPostgresChunksToGroup(t, tx, questionID, 3, `{}`)
	attachPostgresChunksToGroup(t, tx, questionID, 4, `[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]`)
	// An empty text item was excluded before #606 and must stay excluded: the
	// old `FILTER (WHERE message_text.content <> '')` had to move into the
	// text branch's own WHERE to make room for the attachment branch.
	appendPostgresTextItemToGroup(t, tx, questionID, 5, "")
	completePostgresCurrentApplicationTurn(t, tx, response, "it is a report")

	history := resolvePostgresApplicationChatHistory(
		t, queries, conversationID, "50000000-0000-4000-8000-000000000031",
	)
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("history=%+v", history)
	}
	content := history[0].Content
	if len(content) != 3 {
		t.Fatalf("user content=%+v", content)
	}
	// The question's own text stays first (order_index 0), then the
	// attachment's chunks in the order they are stored, flattened -- not
	// nested, not one entry per file. The fourth item's `image_url` chunk is
	// not among them: it belongs to the turn it was attached on.
	if content[0]["type"] != "text" || content[0]["text"] != "what does this say?" ||
		content[1]["text"] != "Bucket: chat-attachments" ||
		content[2]["text"] != "EXTRACTED TEXT" {
		t.Fatalf("user content=%+v", content)
	}
	// The extraction marker survives the json -> jsonb round trip the
	// projection performs, so a worker reading history sees the same
	// convention it sees on input_attachments.
	marker, ok := content[1]["elitea_attachment"].(map[string]any)
	if !ok || marker["needs_content_extraction"] != true ||
		marker["name"] != "conv/report.pdf" {
		t.Fatalf("marker=%+v", content[1])
	}
	if len(history[1].Content) != 1 || history[1].Content[0]["text"] != "it is a report" {
		t.Fatalf("assistant content=%+v", history[1].Content)
	}
}

// #984: A SECOND TURN DOES NOT CARRY THE FIRST TURN'S DATA URL.
//
// #979 appends an `image_url` chunk carrying the picture's base64 bytes to the
// attachment's stored `content`. That chunk belongs to the turn it was
// attached on: admission splices it into `input_attachments` from memory
// (`currentTurnInputAttachments`) and never reads it back from here.
//
// Riding this projection put the data URL into EVERY later turn of the
// conversation. An image is a third again of its raw size once base64'd, so a
// handful of them alone exceed the 256 KiB ceiling BOTH workers fetch the
// input bundle under — and the conversation becomes unrecoverable, because
// history only grows. That is the #607 shape the "four newest attachments"
// bound was added to prevent, reopened by a chunk type it did not know about.
//
// What the model still gets is the file's HEADER chunk: the picture is named,
// and a file-reading tool is offered. That is the pre-#979 behaviour, and it
// is the right thing to degrade to.
func TestPostgresChatHistoryDoesNotCarryAnEmbeddedImageIntoALaterTurn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000031"
	response := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"what is in this picture?", "execution-history-attach-image",
	)
	// Exactly what `attachmentContentScaffold` writes for an EMBEDDED image:
	// the header chunk, then the data URL. No extraction marker on this one —
	// an embedded image has been read.
	const dataURL = "data:image/png;base64,QVVUT1RFU1RNRUQtSU1BR0UtQllURVM="
	attachPostgresChunksToGroup(t, tx, questionID, 1, `[
        {"type":"text","text":"Bucket: chat-attachments / Filename: conv/shot.png"},
        {"type":"image_url","image_url":{"url":"`+dataURL+`"}}
    ]`)
	completePostgresCurrentApplicationTurn(t, tx, response, "a cat")

	history := resolvePostgresApplicationChatHistory(
		t, queries, conversationID, "50000000-0000-4000-8000-000000000031",
	)
	if len(history) != 2 || history[0].Role != "user" {
		t.Fatalf("history=%+v", history)
	}
	content := history[0].Content
	// The question and the file's header survive; the bytes do not.
	if len(content) != 2 ||
		content[0]["text"] != "what is in this picture?" ||
		content[1]["type"] != "text" {
		t.Fatalf("user content=%+v", content)
	}
	if text, _ := content[1]["text"].(string); !strings.Contains(text, "conv/shot.png") {
		t.Fatalf("the file must still be NAMED to the model: %+v", content[1])
	}
	// The assertion that decides the defect: the base64 is nowhere in the
	// projected history at all, under any key or nesting.
	projected, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projected), "base64") || strings.Contains(string(projected), dataURL) {
		t.Fatalf("a prior turn's data URL reached the next turn's history: %s", projected)
	}
}

func TestPostgresChatHistoryKeepsAGroupThatIsAttachmentsOnly(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000031"
	response := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"", "execution-history-attach-2",
	)
	attachPostgresChunksToGroup(t, tx, questionID, 1, `[{"type":"text","text":"only the file"}]`)
	// The turn was sent with no words, only a file — the group's single text
	// item is empty. Before #606 that made the group's content NULL and the
	// outer `jsonb_array_length(...) > 0` dropped the whole group; the file
	// then vanished from the prompt AND took the turn's position in the
	// conversation with it.
	completePostgresCurrentApplicationTurn(t, tx, response, "seen")

	history := resolvePostgresApplicationChatHistory(
		t, queries, conversationID, "50000000-0000-4000-8000-000000000031",
	)
	if len(history) != 2 || history[0].Role != "user" ||
		len(history[0].Content) != 1 ||
		history[0].Content[0]["text"] != "only the file" {
		t.Fatalf("history=%+v", history)
	}
}

func TestPostgresChatHistoryDropsAGroupWhoseOnlyAttachmentHasNoUsableContent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	questionID := "20000000-0000-4000-8000-000000000031"
	response := insertPostgresCurrentApplicationTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"", "execution-history-attach-3",
	)
	// A NULL payload and pylon's `'{}'::json` default are not arrays.
	// `jsonb_array_elements` on either raises 22023 and would fail the whole
	// resolve — so the projection must skip them, and skipping them must leave
	// nothing behind rather than an empty-content group the worker would send
	// to a model as a contentless message.
	attachPostgresChunksToGroup(t, tx, questionID, 1, "")
	attachPostgresChunksToGroup(t, tx, questionID, 2, `{}`)
	attachPostgresChunksToGroup(t, tx, questionID, 3, `[]`)
	completePostgresCurrentApplicationTurn(t, tx, response, "nothing to read")

	history := resolvePostgresApplicationChatHistory(
		t, queries, conversationID, "50000000-0000-4000-8000-000000000031",
	)
	if len(history) != 1 || history[0].Role != "assistant" ||
		history[0].Content[0]["text"] != "nothing to read" {
		t.Fatalf("history=%+v", history)
	}
}

func TestPostgresAdhocChatHistoryExtendsAPriorTurnsAttachmentChunks(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	conversationID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	questionID := "20000000-0000-4000-8000-000000000032"
	response := insertPostgresCurrentAdhocTurn(
		t, queries, conversationID, questionID,
		"30000000-0000-4000-8000-000000000032",
		"40000000-0000-4000-8000-000000000032",
		"what does this say?", "execution-history-attach-4",
	)
	attachPostgresChunksToGroup(t, tx, questionID, 1, `[{"type":"text","text":"Bucket: chat-attachments"},{"type":"text","text":"EXTRACTED TEXT"}]`)
	attachPostgresChunksToGroup(t, tx, questionID, 2, "")
	completePostgresCurrentApplicationTurn(t, tx, response, "it is a report")

	target, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 0, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000032"),
			ConversationUuid: conversationID,
		},
	)
	if err != nil {
		t.Fatalf("resolve ad-hoc turn: %v", err)
	}
	var history []postgresHistoryGroup
	if err := json.Unmarshal([]byte(target.ChatHistoryJson), &history); err != nil {
		t.Fatalf("decode ad-hoc chat history: %v", err)
	}
	if len(history) != 2 || history[0].Role != "user" || len(history[0].Content) != 3 ||
		history[0].Content[0]["text"] != "what does this say?" ||
		history[0].Content[1]["text"] != "Bucket: chat-attachments" ||
		history[0].Content[2]["text"] != "EXTRACTED TEXT" {
		t.Fatalf("ad-hoc history=%s", target.ChatHistoryJson)
	}
}

func resolvePostgresApplicationChatHistory(
	t *testing.T,
	queries *sqlcgen.Queries,
	conversationID pgtype.UUID,
	nextQuestionID string,
) []postgresHistoryGroup {
	t.Helper()
	target, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, nextQuestionID),
			ConversationUuid: conversationID,
		},
	)
	if err != nil {
		t.Fatalf("resolve application turn: %v", err)
	}
	var history []postgresHistoryGroup
	if err := json.Unmarshal([]byte(target.ChatHistoryJson), &history); err != nil {
		t.Fatalf("decode chat history %s: %v", target.ChatHistoryJson, err)
	}
	return history
}

// attachPostgresChunksToGroup writes one `attachment_message` item onto an
// existing message group. content is written verbatim as `json` — an empty
// string means the NULL the column allows — because the point of several of
// these cases is a payload the application layer would never produce but the
// deployed table already holds.
func attachPostgresChunksToGroup(
	t *testing.T,
	tx pgx.Tx,
	groupUUID string,
	orderIndex int32,
	content string,
) {
	t.Helper()
	var payload *string
	if content != "" {
		payload = &content
	}
	if _, err := tx.Exec(t.Context(), `
WITH target_group AS (
    SELECT id FROM chat_message_group WHERE uuid = $1::uuid
), item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(), 'attachment_message', $2, '{}'::jsonb, target_group.id
    FROM target_group
    RETURNING id
)
INSERT INTO chat_messages_attachment (id, name, bucket, attachment_type, content)
SELECT item.id, 'conv/file-' || $2::text, 'chat-attachments', 'document', $3::json
FROM item`, groupUUID, orderIndex, payload); err != nil {
		t.Fatal(err)
	}
}

func appendPostgresTextItemToGroup(
	t *testing.T,
	tx pgx.Tx,
	groupUUID string,
	orderIndex int32,
	content string,
) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `
WITH target_group AS (
    SELECT id FROM chat_message_group WHERE uuid = $1::uuid
), item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(), 'text_message', $2, '{}'::jsonb, target_group.id
    FROM target_group
    RETURNING id
)
INSERT INTO chat_messages_text (id, content)
SELECT item.id, $3 FROM item`, groupUUID, orderIndex, content); err != nil {
		t.Fatal(err)
	}
}
