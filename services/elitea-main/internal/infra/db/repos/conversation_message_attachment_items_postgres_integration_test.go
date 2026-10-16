package repos

// #606 (option 1): an uploaded chat attachment never appeared inline in the
// transcript, because ListMessageGroups only ever joined chat_messages_text.
//
// The item row itself was returned — an `attachment_message` entry with an
// `id`, an `item_type` and an `order_index` — but with NO `item_details` at
// all, so the web client rendered it as the literal `[undefined]`
// (apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxHandlers.helpers.ts:265
// does `'[' + details?.name + ']'`) and had no `filepath` to download from.
// migrations/tenant/0127 gave the payload table an owner; this is the read
// path that finally surfaces it.
//
// WHY THESE TESTS AND NOT UNIT TESTS. Every defect here lives in the SQL and
// in the scan, not in a pure function: a missing LEFT JOIN, a COALESCE that
// erases the absent/empty distinction, a `json` column handed back as a string.
// A fake pool would have to restate the very join under test. These run the
// real query against the real ledgered schema.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"testing"
)

// seedAttachmentTranscript writes one conversation holding a single message
// group, and returns the conversation UUID plus the group's numeric id.
//
// Items are seeded by the caller afterwards rather than here, because the
// point of most of these tests is WHICH items a group holds and in what order.
func seedAttachmentTranscript(t *testing.T, repo *ConversationsRepo) (conversationUUID string, groupID int) {
	t.Helper()
	ctx := context.Background()

	if err := repo.pool.QueryRow(ctx, `
WITH conversation AS (
    INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
    VALUES (gen_random_uuid(), 'attachment transcript', 7, 'agent', now() AT TIME ZONE 'UTC')
    RETURNING id, uuid
), participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 42, "project_id": 1}'::jsonb)
    RETURNING id
)
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
SELECT gen_random_uuid(), participant.id, conversation.id, now() AT TIME ZONE 'UTC' FROM conversation, participant
RETURNING id, (SELECT uuid::text FROM conversation)`).Scan(&groupID, &conversationUUID); err != nil {
		t.Fatalf("seed conversation and message group: %v", err)
	}
	return conversationUUID, groupID
}

func seedAttachmentTextItem(t *testing.T, repo *ConversationsRepo, groupID, orderIndex int, content string) {
	t.Helper()
	if _, err := repo.pool.Exec(context.Background(), `
WITH item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id, created_at)
    VALUES (gen_random_uuid(), 'text_message', $1, $2, now() AT TIME ZONE 'UTC')
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content) SELECT item.id, $3 FROM item`,
		orderIndex, groupID, content); err != nil {
		t.Fatalf("seed text item: %v", err)
	}
}

// seedAttachmentPayloadItem writes an `attachment_message` item and its
// chat_messages_attachment payload. `content` is passed as the raw JSON text
// the `json` column stores; the empty string writes SQL NULL, which is the
// "attachment with no content chunks" case.
func seedAttachmentPayloadItem(t *testing.T, repo *ConversationsRepo, groupID, orderIndex int, name, bucket, attachmentType, content string) int {
	t.Helper()
	var itemID int
	var contentArg any
	if content != "" {
		contentArg = content
	}
	if err := repo.pool.QueryRow(context.Background(), `
WITH item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, message_group_id, created_at)
    VALUES (gen_random_uuid(), 'attachment_message', $1, $2, now() AT TIME ZONE 'UTC')
    RETURNING id
)
INSERT INTO p_1.chat_messages_attachment (id, name, bucket, attachment_type, content)
SELECT item.id, $3, $4, $5, $6::json FROM item
RETURNING id`, orderIndex, groupID, name, bucket, attachmentType, contentArg).Scan(&itemID); err != nil {
		t.Fatalf("seed attachment item: %v", err)
	}
	return itemID
}

// singleAttachmentGroup asserts the repository returned exactly one group and hands it
// back, so each test's own assertions stay about the items.
func singleAttachmentGroup(t *testing.T, repo *ConversationsRepo, conversationUUID string) map[string]any {
	t.Helper()
	groups, err := repo.ListMessageGroups(context.Background(), "1", conversationUUID, 50, "asc")
	if err != nil {
		t.Fatalf("list message groups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("returned %d groups, want 1", len(groups))
	}
	return groups[0]
}

func attachmentGroupItems(t *testing.T, group map[string]any) []map[string]any {
	t.Helper()
	items, ok := group["message_items"].([]map[string]any)
	if !ok {
		t.Fatalf("message_items is %T, want []map[string]any", group["message_items"])
	}
	return items
}

func attachmentItemDetailsOf(t *testing.T, item map[string]any) map[string]any {
	t.Helper()
	details, ok := item["item_details"].(map[string]any)
	if !ok {
		t.Fatalf("item_details is %T, want map[string]any (item %v)", item["item_details"], item)
	}
	return details
}

// The whole point of #606 option 1: a group holding both a question and the
// file sent with it returns both items, in order_index order, and the
// attachment's item_details carries every key the web client reads.
func TestListMessageGroupsReturnsAttachmentItemAlongsideText(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "look at this")
	attachmentID := seedAttachmentPayloadItem(t, repo, groupID, 1, "diagram.png", "chat-uploads", "image",
		`[{"type": "image_url", "image_url": {"url": "filepath:/chat-uploads/diagram.png"}}]`)

	items := attachmentGroupItems(t, singleAttachmentGroup(t, repo, conversationUUID))
	if len(items) != 2 {
		t.Fatalf("group returned %d items, want the text item and the attachment", len(items))
	}
	if items[0]["item_type"] != "text_message" || items[1]["item_type"] != "attachment_message" {
		t.Fatalf("items came back as %v then %v, want text_message then attachment_message (order_index order)",
			items[0]["item_type"], items[1]["item_type"])
	}

	details := attachmentItemDetailsOf(t, items[1])

	// `name` and `bucket` are a DELIBERATE divergence from pylon's own API
	// shape, which exposes neither (elitea_core/models/pd/attachment.py:32-43).
	// The client reads both directly — `name` for the transcript label and the
	// playback toolbar, `bucket` for imageAttachment.helpers.ts:78's
	// `!== '__undefined__'` download-path decision — so both are emitted. See
	// attachmentItemDetails' own doc comment.
	for key, want := range map[string]any{
		"id":              attachmentID,
		"item_type":       "attachment_message",
		"name":            "diagram.png",
		"bucket":          "chat-uploads",
		"filepath":        "/chat-uploads/diagram.png",
		"attachment_type": "image",
	} {
		if got := details[key]; got != want {
			t.Errorf("item_details[%q] = %#v, want %#v", key, got, want)
		}
	}
}

// The group-level `content` is the message's rendered TEXT. An attachment now
// carries an item_details of its own, so the newline-join must keep counting
// only text items — otherwise a filename or a base64 chunk silently becomes
// part of what the user reads as the message.
func TestListMessageGroupsContentCountsOnlyTextItems(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "first line")
	seedAttachmentPayloadItem(t, repo, groupID, 1, "notes.txt", "chat-uploads", "document", `[{"type": "text", "text": "SHOULD NOT APPEAR"}]`)
	seedAttachmentTextItem(t, repo, groupID, 2, "second line")

	group := singleAttachmentGroup(t, repo, conversationUUID)
	if got, want := group["content"], "first line\nsecond line"; got != want {
		t.Errorf("group content = %#v, want only the two text items joined: %#v", got, want)
	}
}

// An attachment sent with no accompanying question. The item must still come
// back — this is the case that used to disappear entirely from the reader's
// point of view, since a group with no text renders nothing at all — and the
// group's content is legitimately empty because there is no text item.
func TestListMessageGroupsReturnsAttachmentOnlyGroup(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentPayloadItem(t, repo, groupID, 0, "solo.pdf", "chat-uploads", "document", "")

	group := singleAttachmentGroup(t, repo, conversationUUID)
	items := attachmentGroupItems(t, group)
	if len(items) != 1 {
		t.Fatalf("group returned %d items, want the single attachment", len(items))
	}
	details := attachmentItemDetailsOf(t, items[0])
	if got, want := details["filepath"], "/chat-uploads/solo.pdf"; got != want {
		t.Errorf("item_details[\"filepath\"] = %#v, want %#v", got, want)
	}
	if got := group["content"]; got != "" {
		t.Errorf("group content = %#v, want empty: the group holds no text item", got)
	}

	// An absent `content` column emits `[]`, never null and never omitted. The
	// client's findContentByType (entities/attachment/model/selectors.ts:36-44)
	// tests `content === undefined`, then `Array.isArray(content)`, then falls
	// through to `content.type` — so a JSON null is neither undefined nor an
	// array and throws reading `type` of null, taking the message list down.
	chunks, ok := details["content"].([]any)
	if !ok || len(chunks) != 0 {
		t.Errorf("item_details[\"content\"] = %#v (%T), want an empty []any for a NULL content column",
			details["content"], details["content"])
	}
}

// The `content` column is `json` holding a LIST of content chunks, and the
// client walks that list looking for an `image_url` entry. Handing it back as
// a string would make the walk see one opaque scalar and every inline image
// would stop rendering, so this asserts on the ENCODED form the handler will
// actually put on the wire, not just on the in-memory type.
func TestListMessageGroupsAttachmentContentRoundTripsAsJSONArray(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentPayloadItem(t, repo, groupID, 0, "photo.png", "chat-uploads", "image",
		`[{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}]`)

	details := attachmentItemDetailsOf(t, attachmentGroupItems(t, singleAttachmentGroup(t, repo, conversationUUID))[0])

	chunks, ok := details["content"].([]any)
	if !ok {
		t.Fatalf("item_details[\"content\"] is %T, want []any — a JSON string here breaks every inline image", details["content"])
	}
	if len(chunks) != 1 {
		t.Fatalf("content decoded to %d chunks, want 1", len(chunks))
	}
	chunk, ok := chunks[0].(map[string]any)
	if !ok {
		t.Fatalf("content chunk is %T, want map[string]any", chunks[0])
	}
	if chunk["type"] != "image_url" {
		t.Errorf("content chunk type = %#v, want image_url", chunk["type"])
	}

	encoded, err := json.Marshal(details["content"])
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if len(encoded) == 0 || encoded[0] != '[' {
		t.Errorf("content marshals to %s, want a JSON array", encoded)
	}
}

// The absent case, which the COALESCE pattern next door makes easy to get
// wrong: a text item has no attachment row, and must not gain empty
// `name`/`bucket`/`filepath`/`attachment_type` keys. A filepath of "//" would
// route the client's download straight at artifact storage
// (imageAttachment.helpers.ts:78) for a message that has no file at all.
func TestListMessageGroupsTextItemGainsNoAttachmentKeys(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "no file here")

	items := attachmentGroupItems(t, singleAttachmentGroup(t, repo, conversationUUID))
	if len(items) != 1 {
		t.Fatalf("group returned %d items, want the single text item", len(items))
	}
	details := attachmentItemDetailsOf(t, items[0])
	if got, want := details["content"], "no file here"; got != want {
		t.Errorf("item_details[\"content\"] = %#v, want %#v", got, want)
	}
	for _, key := range []string{"name", "bucket", "filepath", "attachment_type"} {
		if got, present := details[key]; present {
			t.Errorf("text item's item_details carries attachment key %q = %#v; it must be absent", key, got)
		}
	}
}

// PART 2 OF THE SAME GAP, on the OTHER projection.
//
// Everything above reads the conversation-DETAILS route (ListMessageGroups).
// The chat page does not: `useChatPageData.ts` runs the flat transcript route
// (ListMessages) and hands ITS rows to ChatBox as `message_groups`. Those rows
// carried `{id, uid, role, content, metadata}` and no items at all, so a
// reloaded conversation rendered the question and silently dropped the file
// that rode it — `UserMessage`'s `findAttachmentItems` had nothing to filter
// and `MessageAttachmentList` rendered null. Two projections of one transcript
// disagreeing about whether a message has files is the defect; these are the
// tests that keep them agreeing.

// The row carries the file, and carries it in the shape the details route
// already serves — asserted as an ENCODED comparison between the two
// projections rather than key by key, because "the client needs no second
// reader for this route" is a statement about the whole item, and a key added
// to one projection and not the other is exactly the drift that would break it.
func TestListMessagesCarriesTheGroupsAttachmentItem(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "look at this")
	seedAttachmentPayloadItem(t, repo, groupID, 1, conversationUUID+"/report.pdf", "chat-attachments", "document",
		`[{"type": "text", "text": "Bucket: chat-attachments", "elitea_attachment": {"needs_content_extraction": true}}]`)

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("listed %d rows, want the single group", len(resp.Items))
	}
	row := resp.Items[0]
	if len(row.MessageItems) != 1 {
		t.Fatalf("row carries %d message_items, want the one attachment: %#v", len(row.MessageItems), row.MessageItems)
	}
	// The question's own text stays in `content` and is NOT re-emitted as an
	// item: two sources for one sentence is how a renderer ends up showing it
	// twice, or showing the stale one.
	if row.Content != "look at this" {
		t.Errorf("row content = %q, want the text item alone", row.Content)
	}

	groups, err := repo.ListMessageGroups(context.Background(), "1", conversationUUID, 50, "asc")
	if err != nil {
		t.Fatalf("list message groups: %v", err)
	}
	var fromDetails map[string]any
	for _, item := range attachmentGroupItems(t, groups[0]) {
		if item["item_type"] == "attachment_message" {
			fromDetails = item
		}
	}
	if fromDetails == nil {
		t.Fatal("the details route returned no attachment item to compare against")
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

	// The keys the renderer actually reads, spelled out once: `name` is the
	// object key (conversation prefix included) that addresses the stored
	// object, and `filepath` is what the download hands to artifact storage.
	details, ok := row.MessageItems[0]["item_details"].(map[string]any)
	if !ok {
		t.Fatalf("item_details is %T, want map[string]any", row.MessageItems[0]["item_details"])
	}
	if got, want := details["name"], conversationUUID+"/report.pdf"; got != want {
		t.Errorf("item_details[\"name\"] = %#v, want %#v", got, want)
	}
	if got, want := details["filepath"], "/chat-attachments/"+conversationUUID+"/report.pdf"; got != want {
		t.Errorf("item_details[\"filepath\"] = %#v, want %#v", got, want)
	}
	if _, ok := details["content"].([]any); !ok {
		t.Errorf("item_details[\"content\"] is %T, want a decoded []any — a string breaks the client's image walk", details["content"])
	}
}

// A message with no files must not claim an items list. Asserted on the
// ENCODED row rather than on the Go field: `entities/message`'s normaliser
// sets `messageItems` only when the KEY is present (its own unit test pins
// that), so an always-emitted `"message_items": []` would be a wire-level
// change even though the Go value looks equally empty.
func TestListMessagesOmitsMessageItemsWhenTheGroupHasNoAttachment(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, groupID := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, groupID, 0, "no file here")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("listed %d rows, want the single group", len(resp.Items))
	}
	encoded, err := json.Marshal(resp.Items[0])
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	if got, present := wire["message_items"]; present {
		t.Errorf("row carries message_items = %#v for a group with no attachment; the key must be absent", got)
	}
}

// Only the group's OWN attachments. The projection fetches every listed
// group's items in one query keyed by group id, and a join that lost that key
// — or a map assembled by position — would hang one message's file off
// another, which reads as a file the user never sent with that question.
func TestListMessagesKeepsEachGroupsAttachmentOnItsOwnRow(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	conversationUUID, firstGroup := seedAttachmentTranscript(t, repo)
	seedAttachmentTextItem(t, repo, firstGroup, 0, "first question")
	seedAttachmentPayloadItem(t, repo, firstGroup, 1, conversationUUID+"/first.txt", "chat-attachments", "document", "")

	// A second group in the SAME conversation, carrying no file at all.
	var secondGroup int
	if err := repo.pool.QueryRow(context.Background(), `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, created_at)
SELECT gen_random_uuid(), mg.author_participant_id, mg.conversation_id, now() AT TIME ZONE 'UTC'
FROM p_1.chat_message_group mg WHERE mg.id = $1
RETURNING id`, firstGroup).Scan(&secondGroup); err != nil {
		t.Fatalf("seed second group: %v", err)
	}
	seedAttachmentTextItem(t, repo, secondGroup, 0, "second question")

	resp, err := repo.ListMessages(context.Background(), "1", conversationUUID, wholeTranscript())
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("listed %d rows, want 2", len(resp.Items))
	}
	for _, row := range resp.Items {
		switch row.Content {
		case "first question":
			if len(row.MessageItems) != 1 {
				t.Errorf("the group that carries a file returned %d items", len(row.MessageItems))
			}
		case "second question":
			if len(row.MessageItems) != 0 {
				t.Errorf("a group with no file returned %d items: %#v", len(row.MessageItems), row.MessageItems)
			}
		default:
			t.Errorf("unexpected row %q", row.Content)
		}
	}
}
