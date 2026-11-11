package repos

import (
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

// TestPostgresSubAgentLaneFoldsOntoTheAnswer (#1823, storage per issue
// 1923): each accepted `agent_subagent` frame updates that child's own row
// in chat_message_subagent_lanes, in its own transaction, so a reload's
// fold (subAgentLanesByGroup/decodeAnswerSubAgentLanes) reads the settled
// lane, and the answer's own meta (never touched by the lane write any
// more) survives untouched.
func TestPostgresSubAgentLaneFoldsOntoTheAnswer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	const (
		conversationID   = "10000000-0000-4000-8000-000000001823"
		responseID       = "20000000-0000-4000-8000-000000001823"
		clientGeneration = "30000000-0000-4000-8000-000000001823"
	)
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, clientGeneration)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, clientGeneration, admitted.ExecutionID)
	groupID := currentResponseGroupID(t, pool, responseID)
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group SET meta = COALESCE(meta, '{}'::jsonb) || '{"grounding":{"version":1}}'::jsonb WHERE id = $1`, groupID); err != nil {
		t.Fatal(err)
	}

	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{
		`{"kind":"started","agent_id":"a_1","parent_call_id":"call-spawn","ordinal":1,"display_name":"Owl %","avatar":"default:owl"}`,
		`{"kind":"tool_start","agent_id":"a_1","call_id":"k1","tool":"grep","args_summary":"100% of it"}`,
		`{"kind":"tool_end","agent_id":"a_1","call_id":"k1","ok":true,"summary":"2 matches"}`,
		`{"kind":"finished","agent_id":"a_1","status":"completed","steps":2,"input_tokens":10,"output_tokens":3,"result":"Two callers."}`,
		// A depth-2 grandchild of a_1 (#1872): its own row names a_1 as
		// parent_agent_id, so the fold nests it under a_1's lane.
		`{"kind":"started","agent_id":"a_2","parent_call_id":"call-nested","ordinal":2,"depth":2,"parent_agent_id":"a_1"}`,
		`{"kind":"finished","agent_id":"a_2","status":"completed","result":"Nested."}`,
		// An entry for a child that never started is ignored, not refused.
		`{"kind":"text","agent_id":"a_unknown","text":"x"}`,
	} {
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

	var grounding bool
	if err := pool.QueryRow(t.Context(), `
SELECT meta ? 'grounding' FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&grounding); err != nil {
		t.Fatal(err)
	}
	if !grounding {
		t.Fatal("the lane write touched the answer's other meta")
	}
	var subagentsKey bool
	if err := pool.QueryRow(t.Context(), `
SELECT meta ? 'subagents_v1' FROM p_1.chat_message_group WHERE id = $1`, groupID).Scan(&subagentsKey); err != nil {
		t.Fatal(err)
	}
	if subagentsKey {
		t.Fatal("the lane write still wrote the whole-array meta key")
	}
	// Each child is its own row, not a growing array in one row.
	var rowCount int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM p_1.chat_message_subagent_lanes WHERE message_group_id = $1`, groupID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 2 {
		t.Fatalf("rows = %d, want one per started child", rowCount)
	}

	lanesByGroup, err := subAgentLanesByGroup(t.Context(), pool, "p_1", []int{int(groupID)})
	if err != nil {
		t.Fatal(err)
	}
	lanes := lanesByGroup[int(groupID)]
	if len(lanes) != 1 {
		t.Fatalf("top-level lanes = %+v, want the grandchild nested under its parent", lanes)
	}
	lane := lanes[0]
	if lane.DisplayName != "Owl %" || lane.Status != "completed" || lane.Result != "Two callers." || len(lane.Items) != 1 {
		t.Fatalf("lane = %+v", lane)
	}
	if item := lane.Items[0]; item.ArgsSummary != "100% of it" || item.OK == nil || !*item.OK || item.Summary != "2 matches" {
		t.Fatalf("item = %+v", item)
	}
	if len(lane.Children) != 1 || lane.Children[0].AgentID != "a_2" || lane.Children[0].Result != "Nested." {
		t.Fatalf("children = %+v", lane.Children)
	}

	// A frame bound to another answer is refused.
	stray := outputapp.NodeEventFrame{
		Fence: runtimedomain.Fence{ExecutionID: admitted.ExecutionID, Generation: 1},
		BrowserData: []byte(fmt.Sprintf(`{"type":"agent_subagent","stream_id":%q,"message_id":"20000000-0000-4000-8000-00000000ffff",
"execution_generation":%q,"sio_event":"chat_predict","content":null,"response_metadata":{"subagent_v1":{"kind":"resumed","agent_id":"a_1"}}}`,
			conversationID, clientGeneration)),
	}
	if err := store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
		return projectSubAgentLane(t.Context(), tx, 1, stray)
	}); err == nil {
		t.Fatal("a lane frame of another answer was accepted")
	}
}
