package repos

// Incremental sync (ADR-0025 WP6) against the ledgered corpus: the triggers
// tenant/0144 and shared/0144 install, and the delta reads built on them.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/changesync"
)

// backdate moves a row's sync stamp into the past. The stamp trigger rewrites
// sync_at on every UPDATE, so the write runs with triggers off
// (session_replication_role=replica) on one dedicated connection.
func backdate(t *testing.T, pool *pgxpool.Pool, table string, id int64, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET session_replication_role`) }()
	if _, err := conn.Exec(ctx, `UPDATE `+table+` SET sync_at = clock_timestamp() - $2::interval WHERE id = $1`,
		id, ago.String()); err != nil {
		t.Fatalf("backdate %s %d: %v", table, id, err)
	}
}

func syncAt(t *testing.T, pool *pgxpool.Pool, table string, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(context.Background(), `SELECT sync_at FROM `+table+` WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatalf("read %s.sync_at: %v", table, err)
	}
	return at
}

func tombstoneCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT kind, count(*) FROM p_1.chat_sync_tombstones GROUP BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatal(err)
		}
		counts[kind] = count
	}
	return counts
}

func groupIDByUUID(t *testing.T, pool *pgxpool.Pool, uuid string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM p_1.chat_message_group WHERE uuid = $1::uuid`, uuid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestChatSyncTriggersStampAndBumpWithThrottle(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	numericID, _, groups := seedConversationWithParticipant(t, repo, "first")
	conversationID, _ := strconv.ParseInt(numericID, 10, 64)
	groupID := groupIDByUUID(t, pool, groups[0])

	// Insert stamps; an existing row backdated by a minute is bumped by a new
	// message in it.
	backdate(t, pool, "p_1.chat_conversations", conversationID, time.Minute)
	before := syncAt(t, pool, "p_1.chat_conversations", conversationID)
	var participantID int64
	if err := pool.QueryRow(ctx, `SELECT author_participant_id FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&participantID); err != nil {
		t.Fatal(err)
	}
	insertGroup := func() int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id)
			VALUES (gen_random_uuid(), $1, $2) RETURNING id`, participantID, conversationID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insertGroup()
	bumped := syncAt(t, pool, "p_1.chat_conversations", conversationID)
	if !bumped.After(before.Add(50 * time.Second)) {
		t.Fatalf("a message insert did not bump its conversation: before=%v after=%v", before, bumped)
	}

	// Throttle: a second insert within a second leaves the stamp alone.
	insertGroup()
	if again := syncAt(t, pool, "p_1.chat_conversations", conversationID); !again.Equal(bumped) {
		t.Fatalf("the throttle did not hold: %v then %v", bumped, again)
	}

	// An item's text update bumps its group.
	backdate(t, pool, "p_1.chat_message_group", groupID, time.Minute)
	groupBefore := syncAt(t, pool, "p_1.chat_message_group", groupID)
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_messages_text SET content = 'edited'
		WHERE id IN (SELECT id FROM p_1.chat_message_items WHERE message_group_id = $1)`, groupID); err != nil {
		t.Fatal(err)
	}
	if groupAfter := syncAt(t, pool, "p_1.chat_message_group", groupID); !groupAfter.After(groupBefore.Add(50 * time.Second)) {
		t.Fatalf("a text update did not bump its group: %v then %v", groupBefore, groupAfter)
	}

	// An explicit sync_at from a writer is overwritten by the server clock.
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET sync_at = '2001-01-01' WHERE id = $1`, conversationID); err != nil {
		t.Fatal(err)
	}
	if stamped := syncAt(t, pool, "p_1.chat_conversations", conversationID); stamped.Year() == 2001 {
		t.Fatal("a writer-supplied sync_at survived the stamp trigger")
	}

	// The wire meaning of updated_at is untouched: a message no writer
	// rewrote still has none.
	var updatedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt != nil {
		t.Fatalf("the sync triggers stamped updated_at = %v", updatedAt)
	}
}

func TestChatSyncConversationDeleteWritesOneTombstone(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	numericID, conversationUUID, _ := seedConversationWithParticipant(t, repo, "a", "b", "c")

	if _, err := repo.Delete(context.Background(), "1", conversationUUID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	counts := tombstoneCounts(t, pool)
	if counts["conversation"] != 1 || counts["message_group"] != 0 {
		t.Fatalf("tombstones after a conversation delete = %v, want one conversation and no message tombstones", counts)
	}
	// The removed user participant is recorded: it is who a private
	// conversation's deletion is shown to.
	if counts["access_participant"] != 1 {
		t.Fatalf("access_participant markers = %d, want 1 (user 7)", counts["access_participant"])
	}
	var entityID int64
	var uuid string
	var isPrivate bool
	if err := pool.QueryRow(context.Background(), `SELECT entity_id, entity_uuid::text, is_private
		FROM p_1.chat_sync_tombstones WHERE kind = 'conversation'`).Scan(&entityID, &uuid, &isPrivate); err != nil {
		t.Fatal(err)
	}
	if strconv.FormatInt(entityID, 10) != numericID || uuid != conversationUUID || !isPrivate {
		t.Fatalf("conversation tombstone = (%d, %s, private=%v), want (%s, %s, true)", entityID, uuid, isPrivate, numericID, conversationUUID)
	}
}

func TestChatSyncMessageDeltaMatchesTheLegacyRowsAndReportsDeletes(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID, groups := seedConversationWithParticipant(t, repo, "one", "two", "three")
	for i, uuid := range groups {
		backdate(t, pool, "p_1.chat_message_group", groupIDByUUID(t, pool, uuid), time.Duration(10-i)*time.Minute)
	}

	// Paging a full sync, oldest change first, one row per page.
	cursor := ""
	var delta []conversations.Message
	for page := 0; page < 10; page++ {
		got, err := repo.ListMessageChanges(ctx, "1", conversationUUID, cursor, 1)
		if err != nil {
			t.Fatalf("ListMessageChanges page %d: %v", page, err)
		}
		if got.Total != 3 {
			t.Fatalf("total = %d, want 3", got.Total)
		}
		delta = append(delta, got.Items...)
		cursor = got.NextCursor
		if !got.HasMore {
			break
		}
	}
	if len(delta) != 3 || delta[0].UUID != groups[0] || delta[2].UUID != groups[2] {
		t.Fatalf("paged delta returned %d rows in order %v", len(delta), delta)
	}

	// Byte-identical to the legacy row of the same group.
	legacy, err := repo.ListMessages(ctx, "1", conversationUUID, conversations.MessagesQuery{Limit: 10, SortBy: "id", SortOrder: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range legacy.Items {
		want, _ := json.Marshal(legacy.Items[i])
		got, _ := json.Marshal(delta[i])
		if string(want) != string(got) {
			t.Fatalf("delta row %d differs from the legacy row:\nlegacy %s\ndelta  %s", i, want, got)
		}
	}

	// The cursor has settled past all three; deleting the last one (the only
	// message DeleteMessage accepts) surfaces a tombstone on the next call.
	deleted, err := repo.DeleteMessage(ctx, "1", groups[2], conversationAuthorID)
	if err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	next, err := repo.ListMessageChanges(ctx, "1", conversationUUID, cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tomb := range next.Tombstones {
		if tomb.UUID != nil {
			found[*tomb.UUID] = tomb.Reason == changesync.ReasonDeleted
		}
	}
	for _, uuid := range deleted.Deleted {
		if !found[uuid] {
			t.Fatalf("deleted group %s has no tombstone in %+v", uuid, next.Tombstones)
		}
	}
	for _, item := range next.Items {
		if item.UUID == groups[2] {
			t.Fatal("a deleted group came back as a row")
		}
	}
}

func TestChatSyncMessageDeltaSurvivesAnOutOfOrderCommit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	numericID, conversationUUID, groups := seedConversationWithParticipant(t, repo, "seed")
	backdate(t, pool, "p_1.chat_message_group", groupIDByUUID(t, pool, groups[0]), time.Hour)
	var participantID int64
	if err := pool.QueryRow(ctx, `SELECT author_participant_id FROM p_1.chat_message_group WHERE uuid = $1::uuid`, groups[0]).Scan(&participantID); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id)
		VALUES (gen_random_uuid(), $1, $2::int) RETURNING uuid::text`

	// A stamps first and commits last.
	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	var uuidA, uuidB string
	if err := txA.QueryRow(ctx, insert, participantID, numericID).Scan(&uuidA); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := pool.QueryRow(ctx, insert, participantID, numericID).Scan(&uuidB); err != nil {
		t.Fatal(err)
	}

	first, err := repo.ListMessageChanges(ctx, "1", conversationUUID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range first.Items {
		seen[item.UUID] = true
	}
	if !seen[uuidB] || seen[uuidA] {
		t.Fatalf("first sync saw A=%v B=%v, want B only", seen[uuidA], seen[uuidB])
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	second, err := repo.ListMessageChanges(ctx, "1", conversationUUID, first.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range second.Items {
		if item.UUID == uuidA {
			return
		}
	}
	t.Fatalf("the late-committing row %s was skipped by the cursor %s", uuidA, first.NextCursor)
}

func TestChatSyncMessageCursorRefusals(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	numericID, conversationUUID, _ := seedConversationWithParticipant(t, repo, "x")
	otherID, _, _ := seedConversationWithParticipant(t, repo, "y")
	id, _ := strconv.ParseInt(numericID, 10, 64)
	other, _ := strconv.ParseInt(otherID, 10, 64)

	page, err := repo.ListMessageChanges(ctx, "1", conversationUUID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The numeric id and the uuid name the same transcript, so its cursor
	// works under either spelling of the path.
	if _, err := repo.ListMessageChanges(ctx, "1", numericID, page.NextCursor, 10); err != nil {
		t.Fatalf("cursor under the numeric id: %v", err)
	}
	if _, err := repo.ListMessageChanges(ctx, "1", strconv.FormatInt(other, 10), page.NextCursor, 10); !errors.Is(err, changesync.ErrInvalidCursor) {
		t.Fatalf("another conversation's cursor: err = %v, want ErrInvalidCursor", err)
	}

	var now time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	expired := changesync.Encode(changesync.Cursor{
		Stream: changesync.StreamMessages, Scope: conversations.MessageScope("1", id),
		Rows:  changesync.Position{At: now.Add(-time.Hour)},
		Tombs: changesync.Position{At: now.Add(-changesync.TombstoneRetention - time.Hour)},
	})
	if _, err := repo.ListMessageChanges(ctx, "1", conversationUUID, expired, 10); !errors.Is(err, changesync.ErrCursorExpired) {
		t.Fatalf("back-dated cursor: err = %v, want ErrCursorExpired", err)
	}
}

// TestChatSyncMessageDeltaCarriesCanvasAndAttachmentEdits: the message delta
// projects canvas items (name, type, newest version) and attachments, and the
// canvas editor's save writes only chat_messages_canvas and a new
// chat_canvas_versions row. Those writes must bump the owning group, or a
// client that already synced the transcript never sees the edit.
func TestChatSyncMessageDeltaCarriesCanvasAndAttachmentEdits(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	ctx := context.Background()
	_, conversationUUID, groups := seedConversationWithParticipant(t, repo, "before CANVAS BODY after")
	groupID := groupIDByUUID(t, pool, groups[0])
	var itemID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM p_1.chat_message_items WHERE message_group_id = $1`, groupID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	canvas, err := repo.CreateCanvas(ctx, "1", map[string]any{
		"message_group_id": int(groupID), "message_item_id": int(itemID),
		"name": "snippet", "canvas_type": "code", "code_language": "python",
		"canvas_content_starts_at": 7, "canvas_content_ends_at": 18,
	})
	if err != nil {
		t.Fatalf("CreateCanvas: %v", err)
	}
	canvasUUID, _ := canvas["uuid"].(string)

	settled := func() string {
		t.Helper()
		backdate(t, pool, "p_1.chat_message_group", groupID, 10*time.Minute)
		page, err := repo.ListMessageChanges(ctx, "1", conversationUUID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		return page.NextCursor
	}
	delivers := func(label, cursor string) {
		t.Helper()
		page, err := repo.ListMessageChanges(ctx, "1", conversationUUID, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.UUID == groups[0] {
				return
			}
		}
		t.Fatalf("%s: the edited group is not in the next delta (%d items)", label, len(page.Items))
	}

	cursor := settled()
	if err := repo.UpdateCanvas(ctx, "1", canvasUUID, map[string]any{"canvas_content": "rewritten"}); err != nil {
		t.Fatalf("UpdateCanvas content: %v", err)
	}
	delivers("canvas content edit", cursor)

	cursor = settled()
	if err := repo.UpdateCanvas(ctx, "1", canvasUUID, map[string]any{"name": "renamed"}); err != nil {
		t.Fatalf("UpdateCanvas name: %v", err)
	}
	delivers("canvas rename", cursor)

	cursor = settled()
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_messages_attachment (id, name, bucket, attachment_type)
		SELECT id, 'a.txt', 'b', 'file' FROM p_1.chat_message_items WHERE message_group_id = $1 LIMIT 1`, groupID); err != nil {
		t.Fatal(err)
	}
	delivers("attachment write", cursor)
}
