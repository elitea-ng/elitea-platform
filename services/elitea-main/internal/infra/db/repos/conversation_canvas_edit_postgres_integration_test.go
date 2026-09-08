package repos

// The canvas EDIT path — the one a user's own words travel down.
//
// `CreateCanvas` (proven next door) is only the first half: it carves a canvas
// out of an answer and writes version one. Every keystroke after that reaches
// the server through `PUT /elitea_core/canvas/prompt_lib/{p}/{canvasID}`, and
// that route used to run
//
//	UPDATE p_N.chat_conversations SET name = $1 WHERE id = $2
//
// with the CANVAS id in the conversation's id column. Two failures in one
// statement: the edited text was written nowhere at all — `chat_canvas_versions`
// had exactly one writer, the create — and the id, when it happened to match,
// renamed an unrelated conversation to the canvas's title. Nothing reported
// either: the route answered `{"ok": true}` and the editor kept showing the
// text it had just sent.
//
// These tests state the contract that replaced it, against a real database,
// because all of it is SQL: a save appends a VERSION, the reads serve the
// newest one, and the conversation is not touched.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL); `newFreshInstallPool`
// skips without one.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCanvas puts one canvas on a fresh tenant and answers its uuid, the
// message group it belongs to, and the conversation that holds it.
func seedCanvas(t *testing.T, pool *pgxpool.Pool) (canvasUUID string, groupID, conversationID int) {
	t.Helper()
	const seeded = "before CANVAS BODY after"
	const startsAt, endsAt = 7, 18

	groupID, itemID, _ := seedFreshInstallTextItem(t, pool, seeded)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	canvas, err := NewConversationsRepo(pool).CreateCanvas(ctx, "1", map[string]any{
		"message_group_id":         groupID,
		"message_item_id":          itemID,
		"name":                     "extracted snippet",
		"canvas_type":              "code",
		"code_language":            "python",
		"canvas_content_starts_at": startsAt,
		"canvas_content_ends_at":   endsAt,
	})
	if err != nil {
		t.Fatalf("CreateCanvas: %v", err)
	}
	canvasUUID, _ = canvas["uuid"].(string)
	if canvasUUID == "" {
		t.Fatalf("CreateCanvas answered no uuid: %v", canvas)
	}
	if err := pool.QueryRow(ctx,
		`SELECT conversation_id FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&conversationID); err != nil {
		t.Fatalf("read the group's conversation: %v", err)
	}
	return canvasUUID, groupID, conversationID
}

func conversationName(t *testing.T, pool *pgxpool.Pool, conversationID int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var name string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM p_1.chat_conversations WHERE id = $1`, conversationID).Scan(&name); err != nil {
		t.Fatalf("read the conversation name: %v", err)
	}
	return name
}

// The save itself, addressed the way the web client addresses it: by the
// canvas's UUID, which is the only identifier `entities/canvas` keeps.
func TestUpdateCanvasStoresTheEditedText(t *testing.T) {
	pool := newFreshInstallPool(t)
	canvasUUID, groupID, conversationID := seedCanvas(t, pool)
	before := conversationName(t, pool, conversationID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo := NewConversationsRepo(pool)

	const edited = "CANVAS BODY, rewritten by hand"
	if err := repo.UpdateCanvas(ctx, "1", canvasUUID, map[string]any{"canvas_content": edited}); err != nil {
		t.Fatalf("UpdateCanvas: %v", err)
	}

	// 1. The read the editor makes.
	saved, err := repo.GetCanvas(ctx, "1", canvasUUID)
	if err != nil {
		t.Fatalf("GetCanvas after the save: %v", err)
	}
	if got := saved["canvas_content"]; got != edited {
		t.Errorf("canvas_content=%v, want the edited text", got)
	}
	latest, _ := saved["latest_version"].(map[string]any)
	if got := latest["canvas_content"]; got != edited {
		t.Errorf("latest_version.canvas_content=%v, want the edited text", got)
	}

	// 2. The read the TRANSCRIPT makes, which is the one that decides whether
	//    the user ever sees their edit again. It is a different projection —
	//    `ListMessageGroups` — and a save that only the canvas route can see
	//    is a save nobody reads.
	groups, err := repo.ListMessageGroups(ctx, "1", conversationIDString(t, pool, groupID), 50, "asc")
	if err != nil {
		t.Fatalf("ListMessageGroups after the save: %v", err)
	}
	var sawCanvas bool
	for _, group := range groups {
		items, _ := group["message_items"].([]map[string]any)
		for _, item := range items {
			if item["item_type"] != "canvas_message" {
				continue
			}
			sawCanvas = true
			details, _ := item["item_details"].(map[string]any)
			if details == nil {
				t.Fatal("the canvas item came back with no item_details at all")
			}
			version, _ := details["latest_version"].(map[string]any)
			if got := version["canvas_content"]; got != edited {
				t.Errorf("transcript canvas content=%v, want the edited text", got)
			}
			if got := details["code_language"]; got != "python" {
				t.Errorf("transcript canvas language=%v, want python (unchanged by a content-only save)", got)
			}
		}
	}
	if !sawCanvas {
		t.Error("no canvas_message item in the transcript after the edit")
	}

	// 3. HISTORY, not overwrite: the previous text is still there.
	var versions int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM p_1.chat_canvas_versions v
JOIN p_1.chat_message_items mi ON mi.id = v.canvas_item_id
WHERE mi.uuid::text = $1`, canvasUUID).Scan(&versions); err != nil {
		t.Fatalf("count the canvas versions: %v", err)
	}
	if versions != 2 {
		t.Errorf("canvas versions=%d, want 2 (the created one and the saved one)", versions)
	}

	// 4. AND THE CONVERSATION IS UNTOUCHED. This is the destructive half of
	//    the old statement, and it renamed a row the user never asked about.
	if after := conversationName(t, pool, conversationID); after != before {
		t.Errorf("conversation renamed to %q by a canvas save (was %q)", after, before)
	}
}

// A language-only save — what the editor's language picker sends — must carry
// the text forward. `canvas_content` is NOT NULL, so a version written with ""
// would read back as a canvas the user had emptied.
func TestUpdateCanvasLanguageOnlyKeepsTheText(t *testing.T) {
	pool := newFreshInstallPool(t)
	canvasUUID, _, _ := seedCanvas(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo := NewConversationsRepo(pool)

	if err := repo.UpdateCanvas(ctx, "1", canvasUUID, map[string]any{"code_language": "javascript"}); err != nil {
		t.Fatalf("UpdateCanvas: %v", err)
	}
	saved, err := repo.GetCanvas(ctx, "1", canvasUUID)
	if err != nil {
		t.Fatalf("GetCanvas: %v", err)
	}
	if got := saved["canvas_content"]; got != "CANVAS BODY" {
		t.Errorf("canvas_content=%v, want the text the create carved out", got)
	}
	if got := saved["code_language"]; got != "javascript" {
		t.Errorf("code_language=%v, want javascript", got)
	}
}

// A canvas nobody has is a 404, not a rename of whatever row shares the id.
func TestUpdateCanvasRefusesAnUnknownCanvas(t *testing.T) {
	pool := newFreshInstallPool(t)
	_, _, conversationID := seedCanvas(t, pool)
	before := conversationName(t, pool, conversationID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := NewConversationsRepo(pool).UpdateCanvas(ctx, "1", "00000000-0000-0000-0000-000000000000",
		map[string]any{"name": "renamed", "canvas_content": "x"})
	if err == nil {
		t.Fatal("UpdateCanvas accepted a canvas that does not exist")
	}
	if after := conversationName(t, pool, conversationID); after != before {
		t.Errorf("a refused save still renamed the conversation to %q", after)
	}
}

func conversationIDString(t *testing.T, pool *pgxpool.Pool, groupID int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT conversation_id::text FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&id); err != nil {
		t.Fatalf("read the group's conversation id: %v", err)
	}
	return id
}
