package repos

// DiscardUnadmittedConversation is the one discard the refused-admission
// paths share (#1429): the desktop child turn, the MCP agent call and the
// pipeline trigger run. Before tenant 0157 chat_participant_mapping's key to
// chat_conversations did not cascade (tenant 0123), and a bare DELETE failed
// with 23503 and left the conversation behind. The helper removes the
// mappings first, in one transaction, under the sync cascade.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

// Each source's conversation, seeded with the two participant mappings the
// callers build before admission, is removed by the helper with its mappings;
// a discard under another source does not reach it. The sync journal is the
// one ConversationsRepo.Delete writes: the conversation tombstone and the
// user's access_participant row, and no access_orphaned marker for the
// private conversation losing its last user participant.
func TestDiscardUnadmittedConversationClearsMappingsForEverySource(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

	// Tenant 0157: the migrated key cascades.
	var deleteAction string
	if err := pool.QueryRow(ctx, `
SELECT confdeltype::text FROM pg_constraint
 WHERE conrelid = 'p_1.chat_participant_mapping'::regclass AND contype = 'f'
   AND confrelid = 'p_1.chat_conversations'::regclass`).Scan(&deleteAction); err != nil {
		t.Fatalf("read the participant key: %v", err)
	}
	if deleteAction != "c" {
		t.Fatalf("chat_participant_mapping → chat_conversations ON DELETE = %q, want cascade (c)", deleteAction)
	}

	for _, source := range []string{"mcp", "pipeline_trigger", "local_child"} {
		t.Run(source, func(t *testing.T) {
			var conversationUUID string
			if err := pool.QueryRow(ctx, `
WITH conversation AS (
    INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
    VALUES (gen_random_uuid(), 'run', 7, $1, now() AT TIME ZONE 'UTC')
    RETURNING id, uuid
), participants AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES (gen_random_uuid(), 'user', '{"id": 7}'), (gen_random_uuid(), 'application', '{"id": 21}')
    RETURNING id
), mappings AS (
    INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
    SELECT conversation.id, participants.id, now() AT TIME ZONE 'UTC'
      FROM conversation CROSS JOIN participants
)
SELECT uuid::text FROM conversation`, source).Scan(&conversationUUID); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}

			// Another source's discard does not reach it.
			removed, err := DiscardUnadmittedConversation(ctx, pool, "p_1", conversationUUID, "elitea")
			if err != nil || removed {
				t.Fatalf("discard under the wrong source: removed=%v err=%v", removed, err)
			}

			removed, err = DiscardUnadmittedConversation(ctx, pool, "p_1", conversationUUID, source)
			if err != nil || !removed {
				t.Fatalf("discard: removed=%v err=%v", removed, err)
			}
			var conversations, orphans int
			if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM p_1.chat_conversations WHERE uuid = $1::uuid),
       (SELECT count(*) FROM p_1.chat_participant_mapping AS mapping
         WHERE NOT EXISTS (SELECT 1 FROM p_1.chat_conversations AS c WHERE c.id = mapping.conversation_id))`,
				conversationUUID).Scan(&conversations, &orphans); err != nil {
				t.Fatalf("count: %v", err)
			}
			if conversations != 0 || orphans != 0 {
				t.Fatalf("after discard: %d conversations, %d orphan mappings", conversations, orphans)
			}
			journal := map[string]int{}
			rows, err := pool.Query(ctx, `
SELECT kind, count(*) FROM p_1.chat_sync_tombstones
 WHERE entity_uuid = $1::uuid
    OR entity_id = (SELECT entity_id FROM p_1.chat_sync_tombstones
                     WHERE kind = 'conversation' AND entity_uuid = $1::uuid)
 GROUP BY kind`, conversationUUID)
			if err != nil {
				t.Fatalf("read sync journal: %v", err)
			}
			for rows.Next() {
				var kind string
				var count int
				if err := rows.Scan(&kind, &count); err != nil {
					t.Fatal(err)
				}
				journal[kind] = count
			}
			rows.Close()
			if len(journal) != 2 || journal["conversation"] != 1 || journal["access_participant"] != 1 {
				t.Fatalf("sync journal = %v, want one conversation tombstone and one access_participant row "+
					"(what ConversationsRepo.Delete writes)", journal)
			}

			// A second call finds nothing and is not an error.
			if removed, err := DiscardUnadmittedConversation(ctx, pool, "p_1", conversationUUID, source); err != nil || removed {
				t.Fatalf("second discard: removed=%v err=%v", removed, err)
			}
		})
	}
}
