package repos

// A canvas must not end the conversation.
//
// "Open as document" (#879) carves a WHOLE answer into one `canvas_message`
// item and deletes the `text_message` it came from (ConversationsRepo.
// CreateCanvas). Until this change all four turn statements in agent_chat.sql
// — both resolvers and both inserters — refused any conversation whose history
// held a `canvas_message`, so the very next send answered 422 "This agent turn
// requires the current execution path." for direct model chats and agents
// alike. The resolvers now project a canvas's NEWEST version into the history
// the model is shown (pylon's chat_history.py:57-66), and a reply carved
// entirely into a canvas still counts as the completed reply that keeps its
// question in the history.
//
// Restore `canvas_message` to any gate and these fail with pgx.ErrNoRows;
// drop the projection branch and the history assertions fail.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type canvasHistoryEntry struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func TestPostgresAnApplicationTurnAfterOpenAsDocumentIsAdmittedAndSeesTheDocument(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  "10000000-0000-4000-8000-000000000031",
		QuestionID:        "20000000-0000-4000-8000-000000000071",
		QuestionItemID:    "30000000-0000-4000-8000-000000000071",
		ResponseMessageID: "40000000-0000-4000-8000-000000000071",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "write me a memo",
	}
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-canvas-1", first, 1); err != nil {
		t.Fatal(err)
	}
	responseID := mustCurrentPGUUID(t, first.ResponseMessageID)
	completePostgresCurrentApplicationTurn(t, tx, responseID, "# Memo\n\nDraft body.")
	carvePostgresResponseIntoCanvas(t, tx, responseID, "document",
		"# Memo\n\nDraft body.", "# Memo\n\nEdited body.")

	next := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID:  first.ConversationUUID,
		QuestionID:        "20000000-0000-4000-8000-000000000072",
		QuestionItemID:    "30000000-0000-4000-8000-000000000072",
		ResponseMessageID: "40000000-0000-4000-8000-000000000072",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "make it shorter",
	}
	row, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			QuestionID:       mustCurrentPGUUID(t, next.QuestionID),
			ConversationUuid: mustCurrentPGUUID(t, next.ConversationUUID),
			ProjectID:        1,
		},
	)
	if err != nil {
		t.Fatalf("a conversation holding a document canvas refuses its next turn: %v", err)
	}
	// The document goes in as its prose (no ```document fence), and it is the
	// NEWEST version — the edit the user saved, not the carved original.
	assertCanvasHistory(t, row.ChatHistoryJson, []canvasHistoryEntry{
		historyEntry("user", "write me a memo"),
		historyEntry("assistant", "# Memo\n\nEdited body."),
	})
	if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-canvas-2", next, 1); err != nil {
		t.Fatalf("the insert gate still refuses a conversation holding a canvas: %v", err)
	}
}

func TestPostgresAnAdhocTurnAfterACodeCanvasIsAdmittedAndSeesTheFencedCode(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := agentexecutionapp.CurrentAdhocTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 23,
		ConversationUUID:  "10000000-0000-4000-8000-000000000032",
		QuestionID:        "20000000-0000-4000-8000-000000000073",
		QuestionItemID:    "30000000-0000-4000-8000-000000000073",
		ResponseMessageID: "40000000-0000-4000-8000-000000000073",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "print hello",
	}
	if err := insertCurrentAdhocTurn(t.Context(), queries, "execution-canvas-3", first); err != nil {
		t.Fatal(err)
	}
	responseID := mustCurrentPGUUID(t, first.ResponseMessageID)
	completePostgresCurrentApplicationTurn(t, tx, responseID, "print('hello')")
	carvePostgresResponseIntoCanvas(t, tx, responseID, "python", "print('hello')")

	next := agentexecutionapp.CurrentAdhocTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 23,
		ConversationUUID:  first.ConversationUUID,
		QuestionID:        "20000000-0000-4000-8000-000000000074",
		QuestionItemID:    "30000000-0000-4000-8000-000000000074",
		ResponseMessageID: "40000000-0000-4000-8000-000000000074",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "now in upper case",
	}
	row, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, next.QuestionID),
			ConversationUuid: mustCurrentPGUUID(t, next.ConversationUUID),
		},
	)
	if err != nil {
		t.Fatalf("a direct model chat holding a canvas refuses its next turn: %v", err)
	}
	// Pylon's rendering of a canvas with a language, byte for byte.
	assertCanvasHistory(t, row.ChatHistoryJson, []canvasHistoryEntry{
		historyEntry("user", "print hello"),
		historyEntry("assistant", "```python\n\nprint('hello')\n\n```"),
	})
	if err := insertCurrentAdhocTurn(t.Context(), queries, "execution-canvas-4", next); err != nil {
		t.Fatalf("the ad-hoc insert gate still refuses a conversation holding a canvas: %v", err)
	}
}

// carvePostgresResponseIntoCanvas rewrites a completed response exactly as a
// whole-range ConversationsRepo.CreateCanvas does — the text item deleted, one
// canvas item in its place — and writes each of `versions` in order, so the
// last one is the newest.
func carvePostgresResponseIntoCanvas(
	t *testing.T,
	tx pgx.Tx,
	responseID pgtype.UUID,
	language string,
	versions ...string,
) {
	t.Helper()
	var groupID int32
	if err := tx.QueryRow(t.Context(),
		`SELECT id FROM chat_message_group WHERE uuid = $1`, responseID,
	).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
DELETE FROM chat_messages_text
WHERE id IN (SELECT id FROM chat_message_items WHERE message_group_id = $1);
`, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(),
		`DELETE FROM chat_message_items WHERE message_group_id = $1`, groupID,
	); err != nil {
		t.Fatal(err)
	}
	var canvasID int32
	if err := tx.QueryRow(t.Context(), `
INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id)
VALUES (gen_random_uuid(), 'canvas_message', 0, '{}'::jsonb, $1)
RETURNING id`, groupID).Scan(&canvasID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(),
		`INSERT INTO chat_messages_canvas (id, name, canvas_type) VALUES ($1, 'Document', $2)`,
		canvasID, map[bool]string{true: "document", false: "code"}[language == "document"],
	); err != nil {
		t.Fatal(err)
	}
	for index, content := range versions {
		// Distinct, increasing timestamps: the newest is chosen by created_at
		// first, and two rows inside one transaction would otherwise tie on now().
		if _, err := tx.Exec(t.Context(), `
INSERT INTO chat_canvas_versions (canvas_content, code_language, canvas_item_id, created_at)
VALUES ($1, $2, $3, now() + make_interval(secs => $4))`,
			content, language, canvasID, index,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func historyEntry(role, text string) canvasHistoryEntry {
	entry := canvasHistoryEntry{Role: role}
	entry.Content = append(entry.Content, struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: text})
	return entry
}

func assertCanvasHistory(t *testing.T, raw string, want []canvasHistoryEntry) {
	t.Helper()
	var got []canvasHistoryEntry
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("chat history %q: %v", raw, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat history:\n got %s\nwant %+v", raw, want)
	}
}

// A canvas whose content holds a fenced block of its own must not close the
// fence it is wrapped in: the fence is one backtick longer than the longest
// backtick run in the content (and never shorter than three).
func TestPostgresACanvasHoldingFencesIsWrappedInALongerFence(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	first := agentexecutionapp.CurrentAdhocTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 23,
		ConversationUUID:  "10000000-0000-4000-8000-000000000032",
		QuestionID:        "20000000-0000-4000-8000-000000000075",
		QuestionItemID:    "30000000-0000-4000-8000-000000000075",
		ResponseMessageID: "40000000-0000-4000-8000-000000000075",
		QuestionMeta:      json.RawMessage(`{}`), UserInput: "write a readme",
	}
	if err := insertCurrentAdhocTurn(t.Context(), queries, "execution-canvas-fence-1", first); err != nil {
		t.Fatal(err)
	}
	responseID := mustCurrentPGUUID(t, first.ResponseMessageID)
	content := "# Readme\n\n```sh\nmake\n```\n\n````md\n```nested```\n````\n"
	completePostgresCurrentApplicationTurn(t, tx, responseID, content)
	carvePostgresResponseIntoCanvas(t, tx, responseID, "markdown", content)

	row, err := queries.ResolveCurrentAdhocTurn(
		t.Context(),
		sqlcgen.ResolveCurrentAdhocTurnParams{
			ActorUserID: 11, TargetParticipantID: 23, ProjectID: 1,
			QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000076"),
			ConversationUuid: mustCurrentPGUUID(t, first.ConversationUUID),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertCanvasHistory(t, row.ChatHistoryJson, []canvasHistoryEntry{
		historyEntry("user", "write a readme"),
		historyEntry("assistant", "`````markdown\n\n"+content+"\n\n`````"),
	})
}

// Canvas text in the history is BOUNDED and the bound is EXPLICIT. The newest
// canvases are carried in full while their combined size fits the 64 KiB
// budget; an older one past it is replaced by a marker naming it and its size
// — never silently dropped — so the input bundle stays under the worker's
// 256 KiB ceiling however many documents a conversation accumulates.
func TestPostgresCanvasHistoryKeepsTheNewestWithinBudgetAndMarksTheRest(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	tx := beginCurrentAgentAttachmentTx(t, pool)
	queries := sqlcgen.New(tx)

	older := strings.Repeat("o", 40<<10)
	newer := strings.Repeat("n", 40<<10)
	conversation := "10000000-0000-4000-8000-000000000031"
	for index, document := range []string{older, newer} {
		suffix := fmt.Sprintf("%012d", 77+index)
		turn := agentexecutionapp.CurrentApplicationTurn{
			ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
			ApplicationID: 31, ApplicationVersionID: 41,
			ConversationUUID:  conversation,
			QuestionID:        "20000000-0000-4000-8000-" + suffix,
			QuestionItemID:    "30000000-0000-4000-8000-" + suffix,
			ResponseMessageID: "40000000-0000-4000-8000-" + suffix,
			QuestionMeta:      json.RawMessage(`{}`), UserInput: fmt.Sprintf("document %d", index+1),
		}
		if err := insertCurrentApplicationTurn(t.Context(), queries, "execution-canvas-budget-"+suffix, turn, 1); err != nil {
			t.Fatal(err)
		}
		responseID := mustCurrentPGUUID(t, turn.ResponseMessageID)
		completePostgresCurrentApplicationTurn(t, tx, responseID, document)
		carvePostgresResponseIntoCanvas(t, tx, responseID, "document", document)
	}

	row, err := queries.ResolveCurrentApplicationTurn(
		t.Context(),
		sqlcgen.ResolveCurrentApplicationTurnParams{
			ActorUserID: 11, TargetParticipantID: 21,
			QuestionID:       mustCurrentPGUUID(t, "20000000-0000-4000-8000-000000000099"),
			ConversationUuid: mustCurrentPGUUID(t, conversation),
			ProjectID:        1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	marker := `[Canvas "Document" (40960 bytes) is not included in this history: the conversation's canvases exceed the 65536-byte budget for canvas text, and only the newest canvases that fit are shown. The user can still see and edit it in the conversation.]`
	assertCanvasHistory(t, row.ChatHistoryJson, []canvasHistoryEntry{
		historyEntry("user", "document 1"),
		historyEntry("assistant", marker),
		historyEntry("user", "document 2"),
		historyEntry("assistant", newer),
	})
	if len(row.ChatHistoryJson) > 96<<10 {
		t.Fatalf("history is %d bytes with two 40 KiB canvases; the budget did not hold", len(row.ChatHistoryJson))
	}

	// The migrated tenant carries 0147's newest-version index, which every one
	// of those per-canvas lookups is served by.
	var indexed bool
	if err := tx.QueryRow(t.Context(),
		`SELECT to_regclass('p_1.chat_canvas_versions_item_newest_idx') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("tenant migration 0147's newest-version index is missing")
	}
}
