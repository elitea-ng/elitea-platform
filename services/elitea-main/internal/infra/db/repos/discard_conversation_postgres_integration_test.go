package repos

// DiscardUnadmittedConversation is the one discard the refused-admission
// paths share (#1429): the desktop child turn, the MCP agent call and the
// pipeline trigger run. Against the migrated schema, where
// chat_participant_mapping's key to chat_conversations does NOT cascade
// (tenant 0123), a bare DELETE failed with 23503 and left the conversation
// behind; the helper removes the mappings first, in one transaction.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"testing"
)

// Each source's conversation, seeded with the two participant mappings the
// callers build before admission, is refused by a bare DELETE and removed by
// the helper; a discard under another source does not reach it.
func TestDiscardUnadmittedConversationClearsMappingsForEverySource(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()

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

			// The defect itself: the bare DELETE the callers used is refused.
			if _, err := pool.Exec(ctx, `DELETE FROM p_1.chat_conversations WHERE uuid = $1::uuid`,
				conversationUUID); err == nil {
				t.Fatal("the bare DELETE succeeded: the migrated key cascades, so this test proves nothing")
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

			// A second call finds nothing and is not an error.
			if removed, err := DiscardUnadmittedConversation(ctx, pool, "p_1", conversationUUID, source); err != nil || removed {
				t.Fatalf("second discard: removed=%v err=%v", removed, err)
			}
		})
	}
}
