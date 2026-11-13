package repos

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// TestPostgresSubAgentLaneWriteBounds: the per-answer lane cap, an ordinal
// collision between two agents, and a lane that was only in the legacy
// meta.subagents_v1 when the table shipped never fail or lose frames.
func TestPostgresSubAgentLaneWriteBounds(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)
	const (
		conversationID   = "10000000-0000-4000-8000-000000001924"
		responseID       = "20000000-0000-4000-8000-000000001924"
		clientGeneration = "30000000-0000-4000-8000-000000001924"
	)
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, clientGeneration)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, clientGeneration, admitted.ExecutionID)
	groupID := currentResponseGroupID(t, pool, responseID)
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	send := func(entry string) {
		t.Helper()
		frame := outputapp.NodeEventFrame{
			Fence: runtimedomain.Fence{ExecutionID: admitted.ExecutionID, Generation: 1},
			BrowserData: []byte(fmt.Sprintf(`{"type":"agent_subagent","stream_id":%q,"message_id":%q,
"execution_generation":%q,"sio_event":"chat_predict","content":null,"response_metadata":{"subagent_v1":%s}}`,
				conversationID, responseID, clientGeneration, entry)),
		}
		if err := store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
			return projectSubAgentLane(t.Context(), tx, 1, frame)
		}); err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_1.chat_message_subagent_lanes WHERE message_group_id = $1`, groupID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Ordinal collision: dropped, not an error.
	send(`{"kind":"started","agent_id":"a_1","parent_call_id":"c","ordinal":1}`)
	send(`{"kind":"started","agent_id":"a_dup","parent_call_id":"c","ordinal":1}`)
	if count() != 1 {
		t.Fatalf("rows = %d after an ordinal collision, want 1", count())
	}
	// The cap: the 25th lane is not stored.
	for i := 2; i <= maxSubAgentLanes; i++ {
		send(fmt.Sprintf(`{"kind":"started","agent_id":"a_%d","parent_call_id":"c","ordinal":%d}`, i, i))
	}
	if count() != maxSubAgentLanes {
		t.Fatalf("rows = %d, want %d", count(), maxSubAgentLanes)
	}
	send(`{"kind":"started","agent_id":"a_extra","parent_call_id":"c","ordinal":25}`)
	if count() != maxSubAgentLanes {
		t.Fatalf("rows = %d after the cap, want %d", count(), maxSubAgentLanes)
	}

	// A lane only in the legacy meta is seeded by its next frame.
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group SET meta = COALESCE(meta,'{}'::jsonb) ||
 '{"subagents_v1":[{"agent_id":"a_legacy","parent_call_id":"c","ordinal":30,"status":"running","items":[]}]}'::jsonb
WHERE id = $1`, groupID); err != nil {
		t.Fatal(err)
	}
	send(`{"kind":"finished","agent_id":"a_legacy","status":"completed","result":"Done."}`)
	var status string
	if err := pool.QueryRow(t.Context(), `
SELECT lane->>'status' FROM p_1.chat_message_subagent_lanes WHERE message_group_id = $1 AND agent_id = 'a_legacy'`, groupID).Scan(&status); err != nil {
		t.Fatalf("legacy lane not seeded: %v", err)
	}
	if status != "completed" {
		t.Fatalf("status = %q", status)
	}
}
