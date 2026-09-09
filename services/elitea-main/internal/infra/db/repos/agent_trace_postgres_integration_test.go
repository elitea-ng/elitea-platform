package repos

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestPostgresCurrentAgentTraceUsesExistingTenantAccumulatorAndStableRows(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	const (
		conversationID   = "10000000-0000-4000-8000-000000000011"
		responseID       = "20000000-0000-4000-8000-000000000011"
		clientGeneration = "30000000-0000-4000-8000-000000000011"
	)
	admitted := admitPostgresAgentExecution(
		t,
		pool,
		conversationID,
		responseID,
		clientGeneration,
	)
	seedCurrentAgentResponseGroup(
		t,
		pool,
		conversationID,
		responseID,
		clientGeneration,
		admitted.ExecutionID,
	)

	projector := &postgresCurrentAgentTraceProjector{}
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	start := currentAgentPartialFrame(
		admitted.ExecutionID,
		conversationID,
		responseID,
		clientGeneration,
		fmt.Sprintf(`{
  "tool_calls":{"tool-run":{"tool_name":"child-agent","tool_run_id":"tool-run","run_id":"tool-run","tool_inputs":{"task":"inspect"},"metadata":{"parent_agent_name":"orchestrator","parent_agent_call_id":"outer-call","parent_agent_path":[{"name":"orchestrator","call_id":"outer-call"}]},"timestamp_start":%q}},
  "thinking_steps":[]
}`, base.Format(time.RFC3339Nano)),
	)
	projectCurrentAgentFrame(t, store, projector, start)

	var firstID int64
	if err := pool.QueryRow(t.Context(), `
SELECT trace.id
FROM p_1.chat_message_trace_step AS trace
JOIN p_1.chat_message_group AS message_group
  ON message_group.id = trace.message_group_id
WHERE message_group.uuid::text = $1
  AND trace.kind = 'tool_call'
  AND trace.run_id = 'tool-run'`, responseID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}

	completedAt := base.Add(time.Second)
	completed := currentAgentPartialFrame(
		admitted.ExecutionID,
		conversationID,
		responseID,
		clientGeneration,
		fmt.Sprintf(`{
  "tool_calls":{"tool-run":{"tool_name":"child-agent","tool_run_id":"tool-run","run_id":"tool-run","tool_inputs":{"task":"inspect"},"tool_output":"done","finish_reason":"stop","metadata":{"parent_agent_name":"orchestrator","parent_agent_call_id":"outer-call","parent_agent_path":[{"name":"orchestrator","call_id":"outer-call"}]},"timestamp_start":%q,"timestamp_finish":%q}},
  "thinking_steps":[{"tool_run_id":"thinking-run","parent_agent_name":"child-agent","parent_agent_call_id":"outer-call","parent_agent_path":[{"name":"orchestrator","call_id":"outer-call"},{"name":"child-agent","call_id":"child-call","sibling_ordinal":1}],"type":"AIMessageChunk","text":"reasoning","thinking":"private reasoning","timestamp_start":%q,"timestamp_finish":%q,"message":{"response_metadata":{"model_name":"model-1","tool_name":"Thinking step"}}}],
  "invoked_skills":[{"skill_id":51,"name":"Repository review","icon_meta":{"name":"search"},"instructions":"worker only"}]
}`,
			base.Format(time.RFC3339Nano),
			completedAt.Format(time.RFC3339Nano),
			base.Add(500*time.Millisecond).Format(time.RFC3339Nano),
			completedAt.Format(time.RFC3339Nano),
		),
	)
	projectCurrentAgentFrame(t, store, projector, completed)

	var toolID, rowCount int64
	var output, parentName, parentCallID, thinking, modelName string
	var attrs string
	if err := pool.QueryRow(t.Context(), `
SELECT
    max(trace.id) FILTER (WHERE trace.kind = 'tool_call'),
    count(*),
    max(trace.tool_output) FILTER (WHERE trace.kind = 'tool_call'),
    max(trace.parent_agent_name) FILTER (WHERE trace.kind = 'thinking_step'),
    max(trace.parent_agent_call_id) FILTER (WHERE trace.kind = 'thinking_step'),
    max(trace.thinking) FILTER (WHERE trace.kind = 'thinking_step'),
    max(trace.model_name) FILTER (WHERE trace.kind = 'thinking_step'),
    max(trace.attrs::text) FILTER (WHERE trace.kind = 'thinking_step')
FROM p_1.chat_message_trace_step AS trace
JOIN p_1.chat_message_group AS message_group
  ON message_group.id = trace.message_group_id
WHERE message_group.uuid::text = $1`, responseID).Scan(
		&toolID,
		&rowCount,
		&output,
		&parentName,
		&parentCallID,
		&thinking,
		&modelName,
		&attrs,
	); err != nil {
		t.Fatal(err)
	}
	if toolID != firstID || rowCount != 2 || output != "done" ||
		parentName != "child-agent" || parentCallID != "outer-call" ||
		thinking != "private reasoning" || modelName != "model-1" ||
		!containsAll(string(attrs), "parent_agent_path", "child-call") {
		t.Fatalf("current agent trace projection changed: first=%d tool=%d rows=%d output=%q parent=%q call=%q thinking=%q model=%q attrs=%s",
			firstID, toolID, rowCount, output, parentName, parentCallID,
			thinking, modelName, attrs)
	}
	var invokedSkills string
	if err := pool.QueryRow(t.Context(), `
SELECT message_group.meta -> 'invoked_skills'
FROM p_1.chat_message_group AS message_group
WHERE message_group.uuid::text = $1`, responseID).Scan(&invokedSkills); err != nil {
		t.Fatal(err)
	}
	if invokedSkills != `[{"name": "Repository review", "skill_id": 51, "icon_meta": {"name": "search"}}]` {
		t.Fatalf("persisted invoked skills = %s", invokedSkills)
	}

	// The execution's admitted project fence prevents the same valid frame from
	// being projected into another tenant schema.
	err = store.WithinTx(
		t.Context(),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			return projector.projectAgentTraceDelta(t.Context(), tx, 2, completed)
		},
	)
	if err == nil {
		t.Fatal("current agent invoked skills crossed the project fence")
	}

	// An immutable runtime binding cannot be redirected by forged browser JSON.
	forged := currentAgentPartialFrame(
		admitted.ExecutionID,
		conversationID,
		"20000000-0000-4000-8000-000000000099",
		clientGeneration,
		`{"tool_calls":{},"thinking_steps":[]}`,
	)
	err = store.WithinTx(
		t.Context(),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			return projector.projectAgentTraceDelta(t.Context(), tx, 1, forged)
		},
	)
	if err == nil {
		t.Fatal("forged current agent message identity was accepted")
	}
}

func admitPostgresAgentExecution(
	t *testing.T,
	pool *pgxpool.Pool,
	conversationID,
	responseID,
	clientGeneration string,
) executionapp.AdmissionOutcome {
	t.Helper()
	policy := AgentExecutionDispatchPolicy{
		StreamName:        "elitea:runtime:agent:commands",
		CapabilityVersion: "1",
		ResourceClass:     "agent",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "agent-limits-v1",
		MaxOutstanding:    2,
	}
	repository, err := NewAgentExecutionJobsRepository(pool, policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"bundle-agent", "content-agent", "execution-agent", "command-agent", "outbox-agent"}
	idIndex := 0
	newID := func() (string, error) {
		value := ids[idIndex]
		idIndex++
		return value, nil
	}
	factory, err := agentexecutionapp.NewInputBundleFactory(
		agentexecutionapp.InputProfile{
			Classification:        "tenant-confidential",
			RequiredGrantAudience: "elitea.runtime.input.read.v1",
		},
		newID,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentexecutionapp.NewAdmissionService(
		repository,
		factory,
		func() time.Time { return time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC) },
		newID,
	)
	if err != nil {
		t.Fatal(err)
	}
	steps := int32(12)
	input := &runtimev1.AgentExecutionInputV1{
		SchemaRevision:           "elitea.runtime.agent-execution-input.v1",
		Llm:                      []byte(`{"kwargs":{"model":"model-1"}}`),
		ChatHistory:              []byte(`[]`),
		UserInput:                []byte(`"hello"`),
		Tools:                    []byte(`[]`),
		Application:              []byte(`{"id":7,"version_id":9}`),
		InternalTools:            []byte(`[]`),
		StepsLimit:               &steps,
		McpTokens:                []byte(`{}`),
		IgnoredMcpServers:        []byte(`[]`),
		UserDeclinedMcpServers:   []byte(`[]`),
		HitlDecisions:            []byte(`[]`),
		ExecutionGeneration:      proto.String(clientGeneration),
		Meta:                     []byte(`{}`),
		Persona:                  "generic",
		ContextSettings:          []byte(`{}`),
		InvokedSkills:            []byte(`[]`),
		AppliedSkills:            []byte(`[]`),
		AttachedSkills:           []byte(`[]`),
		InputAttachments:         []byte(`[]`),
		ParallelReconcile:        []byte(`null`),
		ParallelTerminalErrors:   []byte(`[]`),
		ExceptionHandlingEnabled: proto.Bool(false),
		DebugMode:                proto.Bool(true),
	}
	outcome, err := service.Submit(t.Context(), agentexecutionapp.SubmitRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "tenant-agent", ResourceProjectID: "1",
			ProjectionProjectID: "1", ActorID: "7",
		},
		IdempotencyKey:  responseID,
		CapabilityID:    executiondomain.AgentApplicationCapability,
		ClientStreamID:  conversationID,
		ClientMessageID: responseID,
		SIOEvent:        "chat_predict",
		Input:           input,
	})
	if err != nil || !outcome.Created {
		t.Fatalf("admit agent execution: outcome=%+v err=%v", outcome, err)
	}
	return outcome
}

func seedCurrentAgentResponseGroup(
	t *testing.T,
	pool *pgxpool.Pool,
	conversationID,
	responseID,
	clientGeneration,
	executionID string,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
WITH conversation AS (
    INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
    VALUES ($1, 'trace', 7, 'agent')
    RETURNING id
),
participant AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta)
    VALUES ('40000000-0000-4000-8000-000000000011', 'application',
            '{"id":7,"project_id":1}'::jsonb)
    RETURNING id
)
INSERT INTO p_1.chat_message_group (
    uuid, author_participant_id, conversation_id, meta, is_streaming, task_id
)
SELECT $2, participant.id, conversation.id,
       jsonb_build_object('execution_generation', $3::text), TRUE, $4
FROM conversation, participant`,
		conversationID,
		responseID,
		clientGeneration,
		executionID,
	); err != nil {
		t.Fatal(err)
	}
}

func currentAgentPartialFrame(
	executionID,
	conversationID,
	responseID,
	clientGeneration,
	responseMetadata string,
) outputapp.NodeEventFrame {
	return outputapp.NodeEventFrame{
		Fence: runtimedomain.Fence{ExecutionID: executionID, Generation: 1},
		BrowserData: []byte(fmt.Sprintf(`{
  "type":"partial_message",
  "stream_id":%q,
  "message_id":%q,
  "execution_generation":%q,
  "sio_event":"chat_predict",
  "response_metadata":%s
}`, conversationID, responseID, clientGeneration, responseMetadata)),
	}
}

// TestPostgresCurrentAgentTraceRecordsToolCallsForAnalytics is the AGENT half
// of issue 618's producer, driven through the real projector rather than
// through the record repository.
//
// This is the half that matters most: everything an agent reaches for inside a
// chat turn used to be recorded only in p_<id>.chat_message_trace_step, which
// is per-tenant, carries no toolkit id, and covers chat turns only — so the
// Tools tab could not be built from it without silently excluding the other
// producer.
//
// It asserts the STREAMING case, because that is where a naive insert breaks:
// the same tool call is projected twice, first without a finish time and then
// with one, and it must be ONE record whose duration settles.
func TestPostgresCurrentAgentTraceRecordsToolCallsForAnalytics(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	const (
		conversationID   = "10000000-0000-4000-8000-000000000618"
		responseID       = "20000000-0000-4000-8000-000000000618"
		clientGeneration = "30000000-0000-4000-8000-000000000618"
	)
	admitted := admitPostgresAgentExecution(t, pool, conversationID, responseID, clientGeneration)
	seedCurrentAgentResponseGroup(t, pool, conversationID, responseID, clientGeneration, admitted.ExecutionID)

	projector := &postgresCurrentAgentTraceProjector{}
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute)
	body := func(finish string) string {
		return fmt.Sprintf(`{
  "tool_calls":{"tool-run":{"tool_name":"list_issues","tool_run_id":"tool-run","run_id":"tool-run","tool_inputs":{"repo":"elitea"},"metadata":{"toolkit_name":"GitHub","toolkit_type":"github"},"timestamp_start":%q%s}},
  "thinking_steps":[]
}`, base.Format(time.RFC3339Nano), finish)
	}
	projectCurrentAgentFrame(t, store, projector, currentAgentPartialFrame(
		admitted.ExecutionID, conversationID, responseID, clientGeneration, body("")))
	projectCurrentAgentFrame(t, store, projector, currentAgentPartialFrame(
		admitted.ExecutionID, conversationID, responseID, clientGeneration,
		body(`,"timestamp_finish":"`+base.Add(750*time.Millisecond).Format(time.RFC3339Nano)+`"`)))

	var (
		rows        int64
		projectID   int64
		source      string
		toolName    string
		toolkitName string
		toolkitType string
		toolkitID   *int64
		durationMS  float64
		executionID string
	)
	if err := pool.QueryRow(t.Context(), `
SELECT count(*),
       max(project_id),
       max(source),
       max(tool_name),
       max(toolkit_name),
       max(toolkit_type),
       max(toolkit_id),
       max(EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000),
       max(execution_id)
FROM elitea_runtime.tool_call_records
WHERE source = 'agent_turn' AND tool_name = 'list_issues'`).Scan(
		&rows, &projectID, &source, &toolName, &toolkitName, &toolkitType,
		&toolkitID, &durationMS, &executionID,
	); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("a re-projected tool call must be ONE record, got %d", rows)
	}
	if projectID != 1 || source != "agent_turn" || toolName != "list_issues" {
		t.Fatalf("record identity: project=%d source=%q tool=%q", projectID, source, toolName)
	}
	if toolkitName != "GitHub" || toolkitType != "github" {
		t.Fatalf("the toolkit the producer KNEW was not recorded: name=%q type=%q", toolkitName, toolkitType)
	}
	if toolkitID != nil {
		// The worker's tool metadata carries a toolkit name and type and no id.
		// Storing a resolved id here would be a guess presented as a fact.
		t.Fatalf("toolkit_id must stay NULL for an agent turn, got %d", *toolkitID)
	}
	if durationMS < 700 || durationMS > 800 {
		t.Fatalf("the second projection must settle the duration, got %.1fms", durationMS)
	}
	// #875: an agent-turn tool call must carry the execution id, so a cost read
	// can correlate it back to the LLM spend of the execution it ran inside —
	// the same value gateway.llm_request_logs.execution_id is written under.
	if executionID != admitted.ExecutionID {
		t.Fatalf("execution_id: got %q, want %q", executionID, admitted.ExecutionID)
	}
}

func projectCurrentAgentFrame(
	t *testing.T,
	store *postgresSharedStore,
	projector *postgresCurrentAgentTraceProjector,
	frame outputapp.NodeEventFrame,
) {
	t.Helper()
	if err := store.WithinTx(
		context.Background(),
		pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite},
		func(tx sqlExecutor) error {
			return projector.projectAgentTraceDelta(t.Context(), tx, 1, frame)
		},
	); err != nil {
		t.Fatal(err)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
