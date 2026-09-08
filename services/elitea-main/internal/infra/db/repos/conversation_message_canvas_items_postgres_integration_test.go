package repos

// The chat page's own transcript read carries a `canvas_message` item
// (issue 853).
//
// This is the SECOND half of the read-path gap #606 closed for attachments,
// one item type later. `ListMessages` — the route `useChatPageData` reads and
// hands to `ChatBox` as `message_groups` — aggregates each group's
// `text_message` items into `content` and projected `attachment_message` items
// alone. A canvas carved out of an answer was therefore invisible in BOTH
// halves of that one response: its text is not in `content` (the aggregate
// takes text items only, and the create route deletes the text it carved) and
// its item was not in `message_items`. The details route served it and this one
// did not — two projections of one transcript disagreeing, which is the defect
// the attachment tests next door already name.
//
// It matters because a client cannot work around it: with nothing in the
// payload, no renderer can mount an opener, and the canvas the user made can
// only be reached by calling the API by hand.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"testing"
)

// seedTranscriptCanvasItem writes a `canvas_message` item, its chat_messages_canvas row
// and one version, and returns the item id and its uuid — the id a client
// addresses the canvas by.
func seedTranscriptCanvasItem(t *testing.T, repo *ConversationsRepo, groupID, orderIndex int, name, canvasType, content, language string) (itemID int, itemUUID string) {
	t.Helper()
	if err := repo.pool.QueryRow(context.Background(), `
WITH item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id)
    VALUES (gen_random_uuid(), 'canvas_message', $1, $2)
    RETURNING id, uuid::text
), canvas AS (
    INSERT INTO p_1.chat_messages_canvas (id, name, canvas_type)
    SELECT item.id, $3, $4 FROM item
    RETURNING id
)
INSERT INTO p_1.chat_canvas_versions (canvas_content, code_language, canvas_item_id)
SELECT $5, $6, canvas.id FROM canvas
RETURNING (SELECT id FROM item), (SELECT uuid FROM item)`,
		orderIndex, groupID, name, canvasType, content, language).Scan(&itemID, &itemUUID); err != nil {
		t.Fatalf("seed canvas item: %v", err)
	}
	return itemID, itemUUID
}

// The row carries the canvas, in the shape the details route already serves —
// compared as ENCODED JSON rather than key by key, for the reason the
// attachment case states: "the client needs no second reader for this route" is
// a claim about the whole item, and a key added to one projection and not the
// other is exactly the drift that breaks it.
func TestListMessagesCarriesTheGroupsCanvasItem(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "here is the code")
	_, canvasUUID := seedTranscriptCanvasItem(t, repo, groupID, 1, "Edit code", "code", "print('hello')", "python")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("listed %d rows, want the single group", len(resp.Items))
	}
	row := resp.Items[0]
	if len(row.MessageItems) != 1 {
		t.Fatalf("row carries %d message_items, want the one canvas: %#v", len(row.MessageItems), row.MessageItems)
	}
	if row.Content != "here is the code" {
		t.Errorf("row content = %q, want the text item alone", row.Content)
	}

	groups, err := repo.ListMessageGroups(context.Background(), "1", conversationUUID, 50, "asc")
	if err != nil {
		t.Fatalf("list message groups: %v", err)
	}
	var fromDetails map[string]any
	for _, item := range attachmentGroupItems(t, groups[0]) {
		if item["item_type"] == "canvas_message" {
			fromDetails = item
		}
	}
	if fromDetails == nil {
		t.Fatal("the details route returned no canvas item to compare against")
	}
	wantJSON, err := json.Marshal(fromDetails)
	if err != nil {
		t.Fatalf("marshal details item: %v", err)
	}
	gotJSON, err := json.Marshal(row.MessageItems[0])
	if err != nil {
		t.Fatalf("marshal transcript item: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("transcript item = %s\ndetails item   = %s\nthe two projections must serve one shape", gotJSON, wantJSON)
	}

	// The three keys the renderer actually reads, spelled out once: the uuid it
	// addresses the canvas by, and the document under `latest_version`. An
	// absent `latest_version` is a crash in the client, not an empty editor.
	details, ok := row.MessageItems[0]["item_details"].(map[string]any)
	if !ok {
		t.Fatalf("item_details is %T, want map[string]any", row.MessageItems[0]["item_details"])
	}
	if got := details["uuid"]; got != canvasUUID {
		t.Errorf("item_details[\"uuid\"] = %#v, want the item uuid %q", got, canvasUUID)
	}
	if got := details["name"]; got != "Edit code" {
		t.Errorf("item_details[\"name\"] = %#v, want %q", got, "Edit code")
	}
	latest, ok := details["latest_version"].(map[string]any)
	if !ok {
		t.Fatalf("item_details[\"latest_version\"] is %T, want map[string]any", details["latest_version"])
	}
	if got := latest["canvas_content"]; got != "print('hello')" {
		t.Errorf("latest_version[\"canvas_content\"] = %#v, want the stored document", got)
	}
	if got := latest["code_language"]; got != "python" {
		t.Errorf("latest_version[\"code_language\"] = %#v, want %q", got, "python")
	}
}

// Attachments and canvases come back in the order the message STORES them.
// Each projection is ordered on its own, so a naive concatenation puts every
// attachment before every canvas whatever the message looks like — and a client
// that renders the items in the order given would then show the answer
// rearranged.
func TestListMessagesOrdersMixedItemsByTheirStoredOrder(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	// The canvas is stored BEFORE the attachment, which is the order a
	// concatenation of the two projections cannot produce.
	seedTranscriptCanvasItem(t, repo, groupID, 0, "Edit code", "code", "print('first')", "python")
	seedAttachmentPayloadItem(t, repo, groupID, 1, conversationUUID+"/report.pdf", "chat-attachments", "document", "")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	items := resp.Items[0].MessageItems
	if len(items) != 2 {
		t.Fatalf("row carries %d message_items, want the canvas and the attachment: %#v", len(items), items)
	}
	if got := items[0]["item_type"]; got != "canvas_message" {
		t.Errorf("first item is %#v, want the canvas at order_index 0", got)
	}
	if got := items[1]["item_type"]; got != "attachment_message" {
		t.Errorf("second item is %#v, want the attachment at order_index 1", got)
	}
}
