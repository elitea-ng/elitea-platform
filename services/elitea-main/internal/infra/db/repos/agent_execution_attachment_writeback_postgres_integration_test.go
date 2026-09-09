package repos

// #607: the worker's extracted document text is PERSISTED, so a LATER turn sees
// the file's contents instead of just its filename.
//
// Pylon extracts once, at message-persist time, and stores the text as a second
// `{"type":"text"}` chunk of the item's `content` (rpc/chat_all.py:344-377);
// every later turn is then pure DB (utils/chat_history.py:67-73). The Go port
// extracts in the WORKER, at invoke time, and until now threw the result away:
// the turn carrying the file worked, and a follow-up question about it saw the
// filename. #606 part 3 taught the chat-history projection to read an
// attachment's stored chunks; nothing ever wrote the text into them.
//
// These run against real Postgres because every property under test is a
// property of the SQL: whether the write lands on the right row, whether the
// scope refuses a row that is not on this turn's question, and -- the
// acceptance criterion for the whole issue -- whether what was written comes
// back out of `chat_history` on the NEXT turn.

import (
	"encoding/json"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

const (
	writeBackConversationUUID = "10000000-0000-4000-8000-000000000031"
	writeBackAdhocUUID        = "10000000-0000-4000-8000-000000000032"
	writeBackItemID           = "50000000-0000-4000-8000-0000000000a1"
)

// writeBackScaffold is what admission stored: ONE header chunk naming the file,
// carrying the `elitea_attachment` marker with this item's own id
// (application/agentexecution/attachments.go, attachmentContentScaffold, whose
// exact production shape is pinned by that package's own test). The marker is
// the load-bearing part -- a later turn's worker reads it to decide whether the
// file still needs fetching -- so it is spelled out here rather than elided.
const writeBackScaffold = `{"type":"text",` +
	`"text":"Bucket: chat-attachments\nFilename: conv/report.pdf\nfilepath: /chat-attachments/conv/report.pdf",` +
	`"elitea_attachment":{"needs_content_extraction":true,"bucket":"chat-attachments",` +
	`"name":"conv/report.pdf","filepath":"/chat-attachments/conv/report.pdf",` +
	`"item_id":"` + writeBackItemID + `"}}`

// writeBackEnriched is what the worker reports back: the SAME header chunk,
// byte for byte, plus the extracted text. Not a delta -- the complete value the
// column is to hold, which is what makes a redelivered terminal frame rewrite
// the same bytes instead of appending the file's text twice.
const writeBackEnriched = `[` + writeBackScaffold + `,{"type":"text","text":"EXTRACTED TEXT"}]`

// TestPostgresAttachmentWriteBackLetsTheNextTurnSeeAPriorTurnsDocument is the
// acceptance criterion for #607: it asserts the loop CLOSES, from the row
// admission wrote through the write-back to what `chat_history` hands the model
// on a later turn -- and it asserts the before-state too, because "the text is
// in the history" only means something if it was demonstrably absent first.
func TestPostgresAttachmentWriteBackLetsTheNextTurnSeeAPriorTurnsDocument(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := writeBackApplicationTurn(
		t, queries, "20000000-0000-4000-8000-0000000000a1",
		"30000000-0000-4000-8000-0000000000a1", "40000000-0000-4000-8000-0000000000a1",
		"execution-writeback-1",
	)
	completePostgresCurrentApplicationTurn(
		t, tx, mustCurrentPGUUID(t, turn.ResponseMessageID), "it is a report",
	)

	// BEFORE. The scaffold is all the model would get: the filename, and no
	// idea what the file says.
	before := writeBackUserContent(t, queries)
	if len(before) != 2 || before[1]["text"] == nil ||
		!strings.Contains(before[1]["text"].(string), "Filename: conv/report.pdf") {
		t.Fatalf("pre-write-back history=%+v", before)
	}
	for _, chunk := range before {
		if text, _ := chunk["text"].(string); strings.Contains(text, "EXTRACTED TEXT") {
			t.Fatalf("the extracted text was already in the history: %+v", before)
		}
	}

	rows := writeBackAttachmentContent(
		t, queries, turn, writeBackItemID, writeBackEnriched,
	)
	if rows != 1 {
		t.Fatalf("write-back affected %d rows", rows)
	}

	// The column holds EXACTLY the bytes that crossed the seam. `content` is
	// `json`, not `jsonb`, precisely so this is true (migrations/tenant/0127):
	// jsonb would reorder the marker's keys and renormalise its whitespace.
	if stored := writeBackStoredContent(t, tx, writeBackItemID); stored != writeBackEnriched {
		t.Fatalf("stored content = %s", stored)
	}

	// AFTER. The next turn's prompt carries the question, the header chunk and
	// the extracted text, flattened in item order.
	after := writeBackUserContent(t, queries)
	if len(after) != 3 || after[0]["text"] != "what does this say?" ||
		after[2]["type"] != "text" || after[2]["text"] != "EXTRACTED TEXT" {
		t.Fatalf("post-write-back history=%+v", after)
	}
	// The marker survived the write-back and the json -> jsonb round trip the
	// projection performs, so a later worker still recognises this attachment
	// as one it must not fetch again.
	marker, ok := after[1]["elitea_attachment"].(map[string]any)
	if !ok || marker["item_id"] != writeBackItemID ||
		marker["name"] != "conv/report.pdf" || marker["needs_content_extraction"] != true {
		t.Fatalf("marker=%+v", after[1])
	}
}

// TestPostgresAttachmentWriteBackRefusesAnItemFromAnotherConversation is the
// reason the scope is in SQL and not in Go: `item_id` arrives from a worker
// process, and without the join a worker holding one valid claim could rewrite
// any attachment row in the project -- another user's conversation included --
// by naming its id.
//
// The CONTROL is the point of the test. "0 rows" is what a broken fixture
// produces too (a missing reply_to link, an item that was never written, a
// project that is not bound), so the identical write, with the identical item
// id and content, is replayed against its OWN turn and must land. One id, two
// scopes, two different answers.
func TestPostgresAttachmentWriteBackRefusesAnItemFromAnotherConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	owner := writeBackApplicationTurn(
		t, queries, "20000000-0000-4000-8000-0000000000a1",
		"30000000-0000-4000-8000-0000000000a1", "40000000-0000-4000-8000-0000000000a1",
		"execution-writeback-owner",
	)
	// A turn in a DIFFERENT conversation, whose worker names the other
	// conversation's item id.
	intruder := agentexecutionapp.CurrentAdhocTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 23,
		ConversationUUID:  writeBackAdhocUUID,
		QuestionID:        "20000000-0000-4000-8000-0000000000b1",
		QuestionItemID:    "30000000-0000-4000-8000-0000000000b1",
		ResponseMessageID: "40000000-0000-4000-8000-0000000000b1",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "unrelated question",
	}
	if err := insertCurrentAdhocTurn(
		t.Context(), queries, "execution-writeback-intruder", intruder,
	); err != nil {
		t.Fatal(err)
	}

	stolen := writeBackAttachmentContentFor(
		t, queries, intruder.ConversationUUID, intruder.ResponseMessageID,
		intruder.QuestionID, "execution-writeback-intruder",
		writeBackItemID, `[{"type":"text","text":"STOLEN"}]`,
	)
	if stolen != 0 {
		t.Fatalf("a foreign conversation wrote %d rows", stolen)
	}
	if stored := writeBackStoredContent(t, tx, writeBackItemID); strings.Contains(stored, "STOLEN") {
		t.Fatalf("stored content = %s", stored)
	}

	// CONTROL: same id, same statement, its own turn.
	if rows := writeBackAttachmentContent(
		t, queries, owner, writeBackItemID, writeBackEnriched,
	); rows != 1 {
		t.Fatalf("the owning turn's write-back affected %d rows", rows)
	}
}

// A second turn in the SAME conversation is refused too. The scope is the
// QUESTION this terminal belongs to, not the conversation: an attachment's text
// belongs to the turn that carried it, and a later turn has no business
// rewriting it.
func TestPostgresAttachmentWriteBackRefusesAnItemFromAnEarlierTurnOfTheSameConversation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := writeBackApplicationTurn(
		t, queries, "20000000-0000-4000-8000-0000000000a1",
		"30000000-0000-4000-8000-0000000000a1", "40000000-0000-4000-8000-0000000000a1",
		"execution-writeback-first",
	)
	completePostgresCurrentApplicationTurn(
		t, tx, mustCurrentPGUUID(t, first.ResponseMessageID), "it is a report",
	)
	second := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  writeBackConversationUUID,
		QuestionID:        "20000000-0000-4000-8000-0000000000c1",
		QuestionItemID:    "30000000-0000-4000-8000-0000000000c1",
		ResponseMessageID: "40000000-0000-4000-8000-0000000000c1",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "and now?",
	}
	if err := insertCurrentApplicationTurn(
		t.Context(), queries, "execution-writeback-second", second, 1,
	); err != nil {
		t.Fatal(err)
	}

	rows := writeBackAttachmentContentFor(
		t, queries, second.ConversationUUID, second.ResponseMessageID,
		second.QuestionID, "execution-writeback-second",
		writeBackItemID, `[{"type":"text","text":"LATER TURN"}]`,
	)
	if rows != 0 {
		t.Fatalf("a later turn wrote %d rows", rows)
	}
	// CONTROL, again: the same statement against the turn that owns the item.
	if rows := writeBackAttachmentContent(
		t, queries, first, writeBackItemID, writeBackEnriched,
	); rows != 1 {
		t.Fatalf("the owning turn's write-back affected %d rows", rows)
	}
}

// An id that is not an attachment at all -- here the question's own text item,
// which IS on the right group -- matches nothing, because
// `chat_messages_attachment` shares chat_message_items' primary key (0127) and
// the join to it finds no row. This is a regression guard on that join, not a
// discriminating test of a predicate: the query carries no item_type check,
// precisely because removing one left this case green (see the query's own
// note).
func TestPostgresAttachmentWriteBackRefusesANonAttachmentItem(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	turn := writeBackApplicationTurn(
		t, queries, "20000000-0000-4000-8000-0000000000a1",
		"30000000-0000-4000-8000-0000000000a1", "40000000-0000-4000-8000-0000000000a1",
		"execution-writeback-text-item",
	)
	for name, itemID := range map[string]string{
		"the question's own text item": turn.QuestionItemID,
		"an id no row carries":         "50000000-0000-4000-8000-0000000000ff",
	} {
		t.Run(name, func(t *testing.T) {
			if rows := writeBackAttachmentContent(
				t, queries, turn, itemID, `[{"type":"text","text":"NOPE"}]`,
			); rows != 0 {
				t.Fatalf("wrote %d rows", rows)
			}
		})
	}
	if rows := writeBackAttachmentContent(
		t, queries, turn, writeBackItemID, writeBackEnriched,
	); rows != 1 {
		t.Fatalf("the attachment item's own write-back affected %d rows", rows)
	}
}

// writeBackApplicationTurn admits one application turn on conversation 1
// carrying a single document attachment, exactly as the admission transaction
// does, and returns the turn so its identifiers can address the write-back.
func writeBackApplicationTurn(
	t *testing.T,
	queries *sqlcgen.Queries,
	questionID, questionItemID, responseMessageID, executionID string,
) writeBackTurn {
	t.Helper()
	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  writeBackConversationUUID,
		QuestionID:        questionID,
		QuestionItemID:    questionItemID,
		ResponseMessageID: responseMessageID,
		QuestionMeta:      json.RawMessage(`{}`),
		UserInput:         "what does this say?",
		Attachments: []agentexecutionapp.CurrentTurnAttachment{{
			ItemID:         writeBackItemID,
			Name:           "conv/report.pdf",
			Bucket:         "chat-attachments",
			AttachmentType: agentexecutionapp.AttachmentKindDocument,
			Content:        json.RawMessage(`[` + writeBackScaffold + `]`),
		}},
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, executionID, turn, 1); err != nil {
		t.Fatal(err)
	}
	return writeBackTurn{CurrentApplicationTurn: turn, ExecutionID: executionID}
}

// writeBackTurn is the admitted turn plus the execution id it was admitted
// with: LockCurrentAgentResponseForTerminal matches on the execution id, and
// reading it back off the group would let a fixture bug pass as a lock.
type writeBackTurn struct {
	agentexecutionapp.CurrentApplicationTurn
	ExecutionID string
}

func writeBackAttachmentContent(
	t *testing.T,
	queries *sqlcgen.Queries,
	turn writeBackTurn,
	itemID, content string,
) int64 {
	t.Helper()
	return writeBackAttachmentContentFor(
		t, queries, turn.ConversationUUID, turn.ResponseMessageID, turn.QuestionID,
		turn.ExecutionID, itemID, content,
	)
}

// writeBackAttachmentContentFor goes through the SAME two statements the
// terminal path uses: lock the response group, then write scoped to it.
func writeBackAttachmentContentFor(
	t *testing.T,
	queries *sqlcgen.Queries,
	conversationUUID, responseMessageID, questionID, executionID, itemID, content string,
) int64 {
	t.Helper()
	messageGroupID, err := queries.LockCurrentAgentResponseForTerminal(
		t.Context(),
		sqlcgen.LockCurrentAgentResponseForTerminalParams{
			MessageID:      responseMessageID,
			ConversationID: conversationUUID,
			ExecutionID:    executionID,
			// InsertCurrentApplicationTurn stamps the question id as the
			// generation (agent_execution_jobs.go).
			ExecutionGeneration: questionID,
		},
	)
	if err != nil {
		t.Fatalf("lock response group: %v", err)
	}
	rows, err := queries.UpdateCurrentAgentAttachmentContent(
		t.Context(),
		sqlcgen.UpdateCurrentAgentAttachmentContentParams{
			Content:                []byte(content),
			ItemID:                 mustCurrentPGUUID(t, itemID),
			ResponseMessageGroupID: int64(messageGroupID),
		},
	)
	if err != nil {
		t.Fatalf("write back attachment content: %v", err)
	}
	return rows
}

func writeBackStoredContent(t *testing.T, tx pgx.Tx, itemID string) string {
	t.Helper()
	var stored string
	if err := tx.QueryRow(t.Context(), `
SELECT attachment.content::text
FROM p_1.chat_messages_attachment AS attachment
JOIN p_1.chat_message_items AS item ON item.id = attachment.id
WHERE item.uuid = $1::uuid`, itemID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

// writeBackUserContent resolves a NEXT turn and returns the prior turn's user
// message content -- the chunks the model would actually be handed.
func writeBackUserContent(t *testing.T, queries *sqlcgen.Queries) []map[string]any {
	t.Helper()
	history := resolvePostgresApplicationChatHistory(
		t, queries, mustCurrentPGUUID(t, writeBackConversationUUID),
		"50000000-0000-4000-8000-0000000000e1",
	)
	if len(history) == 0 || history[0].Role != "user" {
		t.Fatalf("history=%+v", history)
	}
	return history[0].Content
}
