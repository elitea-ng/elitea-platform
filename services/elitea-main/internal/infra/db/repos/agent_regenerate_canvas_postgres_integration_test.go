package repos

// Regenerating an answer that holds a canvas (#1098).
//
// Before this change ResolveCurrentRegeneration and ResetCurrentAgentResponse
// admitted a response with `text_message` items only. A canvas on the answer
// made both match no row, and the route answered 422 "This agent turn
// requires the current execution path".
//
// A regeneration replaces the answer IN PLACE (there is no older answer
// version to switch back to). So the rule is: the text goes, the canvas
// stays. The canvas holds what the user edited, and its chat_canvas_versions
// rows hold every saved edit. These tests run the real path against a real
// database: CreateCanvas carves the answer, UpdateCanvas saves an edit, the
// start repository resolves the regeneration, the admission helper resets the
// answer, and the canvas routes still open the canvas with its history.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	regenCanvasConversation = "10000000-0000-4000-8000-000000000031"
	regenCanvasQuestion     = "20000000-0000-4000-8000-000000001098"
	regenCanvasQuestionItem = "30000000-0000-4000-8000-000000001098"
	regenCanvasResponse     = "40000000-0000-4000-8000-000000001098"
	regenCanvasGeneration   = "50000000-0000-4000-8000-000000001098"
)

func TestPostgresRegeneratingACanvasAnswerKeepsTheEditedCanvas(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// 1. An answered turn, committed so the repositories (which use the pool)
	//    see it.
	groupID, textItemID := commitAnsweredTurnForRegeneration(t, pool, "Old answer with code.")

	// 2. The user carves the WHOLE answer into a canvas and saves an edit.
	conversationsRepo := NewConversationsRepo(pool)
	canvas, err := conversationsRepo.CreateCanvas(ctx, "1", map[string]any{
		"message_group_id":         groupID,
		"message_item_id":          textItemID,
		"name":                     "My edited canvas",
		"canvas_type":              "code",
		"code_language":            "python",
		"canvas_content_starts_at": 0,
		"canvas_content_ends_at":   len("Old answer with code."),
	})
	if err != nil {
		t.Fatalf("CreateCanvas: %v", err)
	}
	canvasUUID, _ := canvas["uuid"].(string)
	const edited = "What the user wrote by hand."
	if err := conversationsRepo.UpdateCanvas(ctx, "1", canvasUUID, map[string]any{"canvas_content": edited}); err != nil {
		t.Fatalf("UpdateCanvas: %v", err)
	}

	// 3. The regeneration is resolved by the start repository (the 422 came
	//    from here) and admitted by the same reset the admission runs.
	startRepo, err := NewCurrentAgentStartRepository(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	target, err := startRepo.ResolveCurrentRegeneration(ctx, agentexecutionapp.CurrentRegenerationResolveRequest{
		ProjectID: 1, ActorUserID: 11, ResponseMessageID: regenCanvasResponse,
	})
	if err != nil {
		t.Fatalf("an answer holding a canvas cannot be regenerated: %v", err)
	}
	if target.QuestionID != regenCanvasQuestion || target.UserInput != "regenerate my canvas answer" {
		t.Fatalf("target=%+v", target)
	}
	resetAndAnswerRegeneration(t, pool, target, "The regenerated answer.")

	// 4. The canvas and its full history are still there and still open.
	opened, err := conversationsRepo.GetCanvas(ctx, "1", canvasUUID)
	if err != nil {
		t.Fatalf("the kept canvas no longer opens: %v", err)
	}
	if got := opened["canvas_content"]; got != edited {
		t.Fatalf("canvas_content=%v, want the user's edit", got)
	}
	var versions int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM p_1.chat_canvas_versions AS version
JOIN p_1.chat_message_items AS item ON item.id = version.canvas_item_id
WHERE item.uuid = $1::uuid`, canvasUUID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("canvas versions=%d, want the carved original and the edit", versions)
	}

	// 5. The answer holds the kept canvas FIRST and the new words after it;
	//    the old words are gone.
	rows, err := pool.Query(ctx, `
SELECT item.item_type, item.order_index, COALESCE(text_item.content, '')
FROM p_1.chat_message_items AS item
LEFT JOIN p_1.chat_messages_text AS text_item ON text_item.id = item.id
WHERE item.message_group_id = $1
ORDER BY item.order_index, item.id`, groupID)
	if err != nil {
		t.Fatal(err)
	}
	type answerItem struct {
		Type    string
		Order   int32
		Content string
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[answerItem])
	if err != nil {
		t.Fatal(err)
	}
	want := []answerItem{
		{Type: "canvas_message", Order: 0},
		{Type: "text_message", Order: 1, Content: "The regenerated answer."},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("answer items=%+v, want %+v", got, want)
	}

	// 6. The transcript read shows the kept canvas on the answer.
	groups, err := conversationsRepo.ListMessageGroups(ctx, "1", conversationIDString(t, pool, groupID), 50, "asc")
	if err != nil {
		t.Fatal(err)
	}
	var sawCanvas bool
	for _, group := range groups {
		items, _ := group["message_items"].([]map[string]any)
		for _, item := range items {
			if item["item_type"] == "canvas_message" {
				sawCanvas = true
			}
		}
	}
	if !sawCanvas {
		t.Fatal("the transcript no longer shows the kept canvas")
	}

	// 7. A second regeneration also works: the kept canvas does not block it.
	target, err = startRepo.ResolveCurrentRegeneration(ctx, agentexecutionapp.CurrentRegenerationResolveRequest{
		ProjectID: 1, ActorUserID: 11, ResponseMessageID: regenCanvasResponse,
	})
	if err != nil {
		t.Fatalf("a second regeneration is refused: %v", err)
	}
	resetAndAnswerRegeneration(t, pool, target, "The third answer.")
	if _, err := conversationsRepo.GetCanvas(ctx, "1", canvasUUID); err != nil {
		t.Fatalf("the canvas is lost after a second regeneration: %v", err)
	}
}

// commitAnsweredTurnForRegeneration writes one answered application turn and
// commits it. It returns the response group and its one text item.
func commitAnsweredTurnForRegeneration(t *testing.T, pool *pgxpool.Pool, answer string) (groupID, textItemID int) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	turn := agentexecutionapp.CurrentApplicationTurn{
		ProjectID: 1, ActorUserID: 11, TargetParticipantID: 21,
		ApplicationID: 31, ApplicationVersionID: 41,
		ConversationUUID: regenCanvasConversation, QuestionID: regenCanvasQuestion,
		QuestionItemID: regenCanvasQuestionItem, ResponseMessageID: regenCanvasResponse,
		QuestionMeta: json.RawMessage(`{}`), UserInput: "regenerate my canvas answer",
	}
	if err := insertCurrentApplicationTurn(t.Context(), sqlcgen.New(tx), "execution-canvas-regen", turn, 1); err != nil {
		t.Fatal(err)
	}
	completePostgresCurrentApplicationTurn(t, tx, mustCurrentPGUUID(t, regenCanvasResponse), answer)
	if err := tx.QueryRow(t.Context(), `
SELECT response.id, item.id
FROM chat_message_group AS response
JOIN chat_message_items AS item ON item.message_group_id = response.id
WHERE response.uuid = $1`, mustCurrentPGUUID(t, regenCanvasResponse)).Scan(&groupID, &textItemID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return groupID, textItemID
}

// resetAndAnswerRegeneration runs the admission's reset for `target`, writes
// the new answer the way the result projection does (InsertCurrentAgentText*),
// settles it, and commits.
func resetAndAnswerRegeneration(
	t *testing.T,
	pool *pgxpool.Pool,
	target agentexecutionapp.CurrentRegenerationTarget,
	answer string,
) {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	if err := resetCurrentAgentResponse(t.Context(), tx, queries, "execution-canvas-regenerated", agentexecutionapp.CurrentRegenerateTurn{
		ProjectID: 1, ActorUserID: 11, ConversationUUID: target.ConversationUUID,
		TargetParticipantID: target.TargetParticipantID, Kind: target.Kind,
		ApplicationID: 31, ApplicationVersionID: 41,
		QuestionID: target.QuestionID, ResponseMessageID: regenCanvasResponse,
		ExecutionGeneration: regenCanvasGeneration,
	}); err != nil {
		t.Fatalf("the reset refused an answer holding a canvas: %v", err)
	}
	var groupID int64
	if err := tx.QueryRow(t.Context(), `SELECT id FROM chat_message_group WHERE uuid = $1`,
		mustCurrentPGUUID(t, regenCanvasResponse)).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	itemID, err := queries.InsertCurrentAgentTextItem(t.Context(), groupID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.InsertCurrentAgentTextContent(t.Context(), sqlcgen.InsertCurrentAgentTextContentParams{
		ItemID: int64(itemID), Content: answer,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE chat_message_group SET is_streaming = FALSE WHERE id = $1`, groupID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
