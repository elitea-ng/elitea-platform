package repos

import (
	"encoding/json"
	"testing"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/jackc/pgx/v5"
)

func TestPostgresCurrentAgentContextSurvivesRefreshAndSettlesWithResponse(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)
	const conversationID = "10000000-0000-4000-8000-000000000041"
	const responseID = "20000000-0000-4000-8000-000000000041"
	const clientGeneration = "30000000-0000-4000-8000-000000000041"
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, clientGeneration)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, clientGeneration, admitted.ExecutionID)
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	projector := postgresCurrentAgentContextProjector{}
	project := func(frame outputapp.NodeEventFrame) error {
		return store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
			return projector.projectAgentContext(t.Context(), tx, 1, frame)
		})
	}
	frame := currentAgentPartialFrame(admitted.ExecutionID, conversationID, responseID, clientGeneration, `{}`)
	var event map[string]any
	if err := json.Unmarshal(frame.BrowserData, &event); err != nil {
		t.Fatal(err)
	}
	measurement := map[string]any{
		"version": 1, "phase": "compacting", "budget_mode": "balanced", "total_tokens": 272000,
		"usable_input_tokens": 205280, "reserved_output_tokens": 64000, "safety_margin_tokens": 2720,
		"estimated_input_tokens": 190000, "compaction_trigger_tokens": 184752, "compaction_target_tokens": 143696,
	}
	metadata := map[string]any{"model_scope": "agent", "context_status": measurement}
	event["type"] = "agent_context_status"
	event["response_metadata"] = metadata
	encode := func() outputapp.NodeEventFrame {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		frame.BrowserData = raw
		return frame
	}
	if err := project(encode()); err != nil {
		t.Fatal(err)
	}
	read := func() contextsettings.Status {
		// A fresh reader uses only product read-model rows, never worker state.
		state, err := NewConversationsRepo(pool).GetContextState(t.Context(), "1", conversationID)
		if err != nil {
			t.Fatal(err)
		}
		return contextsettings.WithRuntimeContext(contextsettings.BuildStatus(contextsettings.DefaultStrategy(), state.Analytics, state.MessageGroupsTotal), state.RuntimeContext)
	}
	status := read()
	if status.RuntimeContext == nil || !status.RuntimeContext.Active || status.CurrentTokens != 190000 {
		t.Fatalf("status=%+v", status)
	}
	// Child, pipeline-node and cross-admission data cannot change the root meter.
	measurement["estimated_input_tokens"] = 120000
	metadata["parent_agent_call_id"] = "child-call"
	if err := project(encode()); err != nil {
		t.Fatal(err)
	}
	delete(metadata, "parent_agent_call_id")
	metadata["model_scope"] = "pipeline_node"
	if err := project(encode()); err != nil {
		t.Fatal(err)
	}
	metadata["model_scope"] = "agent"
	event["message_id"] = "another-response"
	if err := project(encode()); err == nil {
		t.Fatal("accepted another response")
	}
	event["message_id"] = responseID
	if read().CurrentTokens != 190000 {
		t.Fatal("another scope replaced the root measurement")
	}
	// A terminal failure leaves the truthful pre-compaction estimate, but no
	// longer claims that compaction is running after refresh.
	err = store.WithinTx(t.Context(), pgx.TxOptions{}, func(tx sqlExecutor) error {
		return persistCurrentAgentRuntimeTerminal(t.Context(), tx, 1, executiondomain.AgentApplicationCapability,
			outputRecord{ExecutionID: admitted.ExecutionID, Generation: 1}, "INTERNAL", "The runtime operation failed.")
	})
	if err != nil {
		t.Fatal(err)
	}
	status = read()
	if status.RuntimeContext == nil || status.RuntimeContext.Active || status.RuntimeContext.Measurement.Phase != "compacting" {
		t.Fatalf("terminal status=%+v", status)
	}
	// Regeneration changes the immutable client generation: the old meter must
	// disappear until the new run has measured its own prepared request.
	_, err = pool.Exec(t.Context(), `UPDATE p_1.chat_message_group SET meta=jsonb_set(meta, '{execution_generation}', '"replacement"') WHERE uuid::text=$1`, responseID)
	if err != nil {
		t.Fatal(err)
	}
	if read().RuntimeContext != nil {
		t.Fatal("regeneration inherited stale occupancy")
	}
	// The newest unmeasured response must not fall back to the previous turn.
	_, err = pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group SET meta=jsonb_set(meta, '{execution_generation}', to_jsonb($2::text)) WHERE uuid::text=$1`, responseID, clientGeneration)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `
INSERT INTO p_1.chat_message_group (uuid,author_participant_id,conversation_id,meta,is_streaming,task_id,created_at)
SELECT gen_random_uuid(),author_participant_id,conversation_id,'{}'::jsonb,FALSE,'new-unmeasured-run',CURRENT_TIMESTAMP+interval '1 minute'
FROM p_1.chat_message_group WHERE uuid::text=$1`, responseID)
	if err != nil {
		t.Fatal(err)
	}
	if read().RuntimeContext != nil {
		t.Fatal("new response borrowed previous occupancy")
	}
	// When that older response is the active one, it owns the displayed meter.
	_, err = pool.Exec(t.Context(), `UPDATE p_1.chat_message_group SET is_streaming=TRUE WHERE uuid::text=$1`, responseID)
	if err != nil {
		t.Fatal(err)
	}
	if current := read().RuntimeContext; current == nil || current.ResponseMessageID != responseID || !current.Active {
		t.Fatal("active response lost its own measurement")
	}
}
