package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
)

func TestPostgresCurrentIndexActivityPreservesOrderedStepsAndTenantIsolation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentActivitySchemas(t, pool)

	dispatchPolicy := IndexIngestDispatchPolicy{
		StreamName:        "elitea:runtime:index:commands",
		CapabilityVersion: "1",
		ResourceClass:     "indexing",
		IsolationClass:    "project",
		Priority:          1,
		DeadlineTTL:       time.Hour,
		LimitsRevision:    "index-limits-v1",
		MaxOutstanding:    2,
	}
	jobs, err := NewIndexIngestJobsRepository(pool, dispatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest("activity-project-1", "activity-project-1")
	request.ClientStreamID = "10000000-0000-4000-8000-000000000001"
	request.ClientMessageID = "20000000-0000-4000-8000-000000000001"
	admitted, err := newPostgresIndexAdmissionService(t, jobs, "activity-project-1").Submit(t.Context(), request)
	if err != nil || !admitted.Created {
		t.Fatalf("admit index execution: outcome=%+v err=%v", admitted, err)
	}
	seedCurrentActivityConversation(t, pool, 1, request.ClientStreamID, 7, int(request.ToolkitID))

	results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
		LimitsRevision:    dispatchPolicy.LimitsRevision,
		ArtifactMediaType: "application/json",
		MaxArtifactBytes:  1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := results.ExpectedIndexIngest(t.Context(), admitted.ExecutionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	fence := claimPostgresIndexExecution(t, pool, expected)
	claims, err := NewClaimsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	claimService, err := executionapp.NewClaimService(claims, time.Now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	nodeRepository, err := NewNodeEventsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	nodeService, err := outputapp.NewNodeEventService(claimService, nodeRepository)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	const currentRunID = "00000000-0000-0000-0000-000000000001"
	events := make([]outputapp.NodeEventFrame, 0, 15)
	events = append(events, postgresCurrentActivityNodeFrame(
		t, expected, fence, 1, base, "agent_tool_start",
		map[string]any{
			"tool_name": "index_data", "tool_run_id": currentRunID,
			"timestamp_start": base.Format(time.RFC3339Nano),
			"metadata": map[string]string{
				"initiator": "user", "tool_name": "index_data", "display_name": "configurations",
			},
		},
	))
	events = append(events, postgresCurrentActivityNodeFrameWithBrowserData(
		t, expected, fence, 2, base.Add(time.Millisecond), currentWorkerThinkingStepFixture(t),
	))
	for step := 2; step <= 13; step++ {
		at := base.Add(time.Duration(step) * time.Millisecond)
		events = append(events, postgresCurrentActivityNodeFrame(
			t, expected, fence, uint64(step+1), at, "agent_thinking_step",
			map[string]any{
				"name": "thinking_step", "run_id": currentRunID, "tool_run_id": currentRunID,
				"tool_name": "loader", "message": fmt.Sprintf("%d files processed", step*10),
				"datetime": at.Format(time.RFC3339Nano), "toolkit": "EliteaGitHubAPIWrapper",
				"metadata": map[string]string{
					"initiator": "user", "tool_name": "index_data", "display_name": "configurations",
				},
			},
		))
	}
	events = append(events, postgresCurrentActivityNodeFrame(
		t, expected, fence, 15, base.Add(15*time.Millisecond), "agent_tool_end",
		map[string]any{
			"tool_name": "index_data", "tool_run_id": currentRunID, "finish_reason": "stop",
			"timestamp_start":  base.Format(time.RFC3339Nano),
			"timestamp_finish": base.Add(15 * time.Millisecond).Format(time.RFC3339Nano),
		},
	))
	for index, event := range events {
		if _, err := nodeService.IngestNodeEvent(t.Context(), event); err != nil {
			t.Fatalf("project node sequence %d: %v", event.Sequence, err)
		}
		if index == 0 {
			var groupCount, traceCount int
			if err := pool.QueryRow(t.Context(), `
SELECT (SELECT count(*) FROM p_1.chat_message_group),
       (SELECT count(*) FROM p_1.chat_message_trace_step)`).Scan(&groupCount, &traceCount); err != nil {
				t.Fatal(err)
			}
			var attrs []byte
			if err := pool.QueryRow(t.Context(), `
SELECT trace.attrs
FROM p_1.chat_message_trace_step AS trace
JOIN p_1.chat_message_group AS message_group
  ON message_group.id = trace.message_group_id
WHERE message_group.uuid = $1
  AND trace.kind = 'tool_call'`, request.ClientMessageID).Scan(&attrs); err != nil {
				t.Fatalf("tool-start group=%d trace=%d: %v", groupCount, traceCount, err)
			}
			if !json.Valid(attrs) || !strings.Contains(string(attrs), `"display_name": "configurations"`) {
				t.Fatalf("tool-start attrs were not stored: %s", attrs)
			}
		}
	}
	// The repository accepts the exact replay without invoking the Activity
	// projector again, so a worker retry cannot duplicate a trace row.
	replayed, err := nodeService.IngestNodeEvent(t.Context(), events[7])
	if err != nil || replayed.Inserted {
		t.Fatalf("idempotent node replay: outcome=%+v err=%v", replayed, err)
	}

	terminal := postgresInlineIndexOutputFrame(t, expected, fence, outputapp.IndexIngestSummary{
		Status: outputapp.IndexIngestStatusOK, Message: "Successfully indexed 130 files.",
	})
	resequenceCurrentActivityTerminal(t, &terminal, 16)
	if _, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(t.Context(), terminal); err != nil {
		t.Fatal(err)
	}

	// Current persistence is an accumulator keyed by emitter run_id. All 13
	// ordered SSE deltas remain in replay, while reload shows the latest state
	// for the one SDK run exactly as elitea_core trace_step_writer does.
	assertCurrentActivityProjection(
		t, pool, "p_1", request.ClientMessageID, 1, "130 files processed",
		base.Add(13*time.Millisecond), "Successfully indexed 130 files.",
	)
	assertCurrentActivityUIReloadFixture(t, pool, "p_1", request.ClientMessageID)
	var otherTenantGroups int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_2.chat_message_group`).Scan(&otherTenantGroups); err != nil {
		t.Fatal(err)
	}
	if otherTenantGroups != 0 {
		t.Fatalf("project-1 execution wrote %d groups into project 2", otherTenantGroups)
	}

	requestTwo := postgresIndexSubmitRequest("activity-project-2", "activity-project-2")
	requestTwo.Identity.TenantID = "tenant-postgres-2"
	requestTwo.Identity.ResourceProjectID = "2"
	requestTwo.Identity.ProjectionProjectID = "2"
	requestTwo.ClientStreamID = "10000000-0000-4000-8000-000000000002"
	requestTwo.ClientMessageID = "20000000-0000-4000-8000-000000000002"
	admittedTwo, err := newPostgresIndexAdmissionService(t, jobs, "activity-project-2").Submit(t.Context(), requestTwo)
	if err != nil || !admittedTwo.Created {
		t.Fatalf("admit project-2 index execution: outcome=%+v err=%v", admittedTwo, err)
	}
	seedCurrentActivityConversation(t, pool, 2, requestTwo.ClientStreamID, 7, int(requestTwo.ToolkitID))
	expectedTwo, err := results.ExpectedIndexIngest(t.Context(), admittedTwo.ExecutionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	fenceTwo := claimPostgresIndexExecution(t, pool, expectedTwo)
	malformed := postgresCurrentActivityNodeFrame(
		t, expectedTwo, fenceTwo, 1, base, "agent_thinking_step",
		map[string]any{"message": "must roll back"},
	)
	if _, err := nodeService.IngestNodeEvent(t.Context(), malformed); err == nil {
		t.Fatal("recognized malformed current event committed")
	}
	var malformedReplay int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*)
FROM elitea_runtime.execution_replay_events
WHERE event_id = $1`, malformed.EventID).Scan(&malformedReplay); err != nil {
		t.Fatal(err)
	}
	if malformedReplay != 0 {
		t.Fatalf("malformed event left %d replay rows after transaction rollback", malformedReplay)
	}
	validProjectTwo := postgresCurrentActivityNodeFrame(
		t, expectedTwo, fenceTwo, 1, base, "agent_tool_start",
		map[string]any{
			"tool_name": "index_data", "tool_run_id": "project-2-run",
			"timestamp_start": base.Format(time.RFC3339Nano),
		},
	)
	if _, err := nodeService.IngestNodeEvent(t.Context(), validProjectTwo); err != nil {
		t.Fatalf("corrected project-2 event after rollback: %v", err)
	}
	terminalTwo := postgresInlineIndexOutputFrame(t, expectedTwo, fenceTwo, outputapp.IndexIngestSummary{
		Status: outputapp.IndexIngestStatusOK, Message: "Project 2 indexing completed.",
	})
	resequenceCurrentActivityTerminal(t, &terminalTwo, 2)
	if _, err := newPostgresIndexOutputService(t, pool, results).IngestIndex(t.Context(), terminalTwo); err != nil {
		t.Fatal(err)
	}
	var p1Groups, p2Groups int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_1.chat_message_group`).Scan(&p1Groups); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM p_2.chat_message_group`).Scan(&p2Groups); err != nil {
		t.Fatal(err)
	}
	if p1Groups != 1 || p2Groups != 1 {
		t.Fatalf("two-tenant current Activity isolation: p1=%d p2=%d", p1Groups, p2Groups)
	}
	assertCurrentActivityPostgresQueryBudget(t, pool, expected, fence, base)
}

func assertCurrentActivityUIReloadFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
	messageUUID string,
) {
	t.Helper()
	qualified := pgx.Identifier{schema}.Sanitize()
	rows, err := pool.Query(t.Context(), fmt.Sprintf(`
SELECT jsonb_build_object(
    'id', trace.id,
    'message_group_id', trace.message_group_id,
    'kind', trace.kind,
    'tool_name', trace.tool_name,
    'parent_agent_name', trace.parent_agent_name,
    'parent_agent_call_id', trace.parent_agent_call_id,
    'started_at', trace.started_at,
    'finished_at', trace.finished_at,
    'is_error', trace.is_error,
    'step_type', trace.step_type,
    'model_name', trace.model_name,
    'finish_reason', trace.finish_reason,
    'attrs', trace.attrs
)
FROM %s.chat_message_trace_step AS trace
JOIN %s.chat_message_group AS message_group
  ON message_group.id = trace.message_group_id
WHERE message_group.uuid = $1
ORDER BY trace.started_at NULLS LAST, trace.id`, qualified, qualified), messageUUID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	fixtures := make([]map[string]any, 0, 2)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var fixture map[string]any
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 {
		t.Fatalf("current UI trace list rows=%d", len(fixtures))
	}
	for _, fixture := range fixtures {
		for _, heavy := range []string{"text", "thinking", "tool_inputs", "tool_output"} {
			if _, found := fixture[heavy]; found {
				t.Fatalf("current light trace DTO included heavy field %q: %#v", heavy, fixture)
			}
		}
	}
	tool := fixtures[0]
	if tool["kind"] != "tool_call" || tool["step_type"] != nil {
		t.Fatalf("current UI tool fixture changed: %#v", tool)
	}
	toolAttrs, ok := tool["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("current UI tool attrs changed: %#v", tool["attrs"])
	}
	toolMetadata, ok := toolAttrs["metadata"].(map[string]any)
	if !ok || toolMetadata["display_name"] != "configurations" {
		t.Fatalf("tool-end lost current tool-start display metadata: %#v", toolAttrs)
	}
	thinking := fixtures[1]
	if thinking["kind"] != "thinking_step" ||
		thinking["step_type"] != nil ||
		thinking["model_name"] != nil ||
		thinking["started_at"] != thinking["finished_at"] {
		t.Fatalf("current UI thinking fixture changed: %#v", thinking)
	}
	attrs, ok := thinking["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("current UI thinking attrs changed: %#v", thinking["attrs"])
	}
	responseMetadata, ok := attrs["response_metadata"].(map[string]any)
	if !ok || responseMetadata["tool_name"] != "loader" {
		t.Fatalf("current UI thinking label changed: %#v", attrs)
	}
}

func assertCurrentActivityPostgresQueryBudget(
	t *testing.T,
	pool *pgxpool.Pool,
	expected outputapp.ExpectedIndexIngest,
	fence runtimedomain.Fence,
	base time.Time,
) {
	t.Helper()
	for _, test := range []struct {
		name       string
		eventCount int
		maxElapsed time.Duration
	}{
		{name: "golden-15", eventCount: 15, maxElapsed: 5 * time.Second},
		{name: "admission-bound-2048", eventCount: 2048, maxElapsed: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := make([]outputapp.NodeEventFrame, 0, test.eventCount)
			for index := 0; index < test.eventCount; index++ {
				at := base.Add(time.Duration(index) * time.Microsecond)
				frames = append(frames, postgresCurrentActivityNodeFrame(
					t, expected, fence, uint64(index+1), at, "agent_thinking_step_update",
					map[string]any{
						"name":   "thinking_step",
						"run_id": "query-budget-run", "tool_run_id": "query-budget-run",
						"tool_name": "loader", "message": fmt.Sprintf("%d files processed", index+1),
						"datetime": at.Format(time.RFC3339Nano),
						"toolkit":  "EliteaGitHubAPIWrapper",
						"metadata": map[string]string{
							"initiator": "user", "tool_name": "index_data",
							"display_name": "configurations",
						},
					},
				))
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			executor := &countingCurrentActivityExecutor{tx: tx}
			projector := &postgresCurrentIndexActivityProjector{}
			started := time.Now()
			for _, frame := range frames {
				if err := projector.projectNodeEvent(t.Context(), executor, 1, frame); err != nil {
					t.Fatalf("project budget event %d: %v", frame.Sequence, err)
				}
			}
			elapsed := time.Since(started)
			if executor.queryRows != test.eventCount+1 || executor.queries != 0 || executor.execs != 0 {
				t.Fatalf(
					"%d events used QueryRow=%d Query=%d Exec=%d; want one schema preflight plus one statement/event",
					test.eventCount, executor.queryRows, executor.queries, executor.execs,
				)
			}
			if elapsed > test.maxElapsed {
				t.Fatalf(
					"%d current Activity events took %s, budget %s",
					test.eventCount, elapsed, test.maxElapsed,
				)
			}
			t.Logf(
				"current Activity %d-event PostgreSQL budget: %d statements in %s",
				test.eventCount, executor.queryRows, elapsed,
			)
		})
	}
}

type countingCurrentActivityExecutor struct {
	tx        pgx.Tx
	queryRows int
	queries   int
	execs     int
}

func (e *countingCurrentActivityExecutor) QueryRow(
	ctx context.Context,
	query string,
	args ...any,
) sqlRow {
	e.queryRows++
	return e.tx.QueryRow(ctx, query, args...)
}

func (e *countingCurrentActivityExecutor) Query(
	ctx context.Context,
	query string,
	args ...any,
) (sqlRows, error) {
	e.queries++
	return e.tx.Query(ctx, query, args...)
}

func (e *countingCurrentActivityExecutor) Exec(
	ctx context.Context,
	query string,
	args ...any,
) (pgconn.CommandTag, error) {
	e.execs++
	return e.tx.Exec(ctx, query, args...)
}

func resequenceCurrentActivityTerminal(
	t *testing.T,
	frame *outputapp.IndexIngestFrame,
	sequence uint64,
) {
	t.Helper()
	frame.Sequence = sequence
	frame.EventID = frame.Fence.CommandID + ":" + fmt.Sprint(sequence)
	frame.Settlement.TerminalSequence = sequence
	frame.Settlement.TerminalEventID = frame.EventID
	wireOutcome := runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_SUCCEEDED
	if frame.Settlement.Outcome == executionapp.SettlementFailed {
		wireOutcome = runtimev1.ExecutionOutcomeV1_EXECUTION_OUTCOME_V1_FAILED
	}
	wireSettlement := &runtimev1.SettlementProposalV1{
		ProposalId:              frame.Settlement.ProposalID,
		RequestedOutcome:        wireOutcome,
		TerminalLogicalOutputId: frame.Settlement.TerminalLogicalOutputID,
		TerminalEventId:         frame.EventID,
		TerminalSequence:        sequence,
		TerminalPayloadDigest:   postgresDigestV1(frame.Settlement.TerminalPayloadDigest),
		PrepareIdempotencyKey:   frame.Settlement.IdempotencyKey,
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(wireSettlement)
	if err != nil {
		t.Fatal(err)
	}
	frame.EncodedSettlement = encoded
	frame.Settlement.ProposalDigest = runtimedomain.SHA256(encoded)
	if err := frame.Validate(); err != nil {
		t.Fatalf("resequence current Activity terminal: %v", err)
	}
}

func postgresCurrentActivityNodeFrame(
	t *testing.T,
	expected outputapp.ExpectedIndexIngest,
	fence runtimedomain.Fence,
	sequence uint64,
	occurredAt time.Time,
	eventType string,
	responseMetadata map[string]any,
) outputapp.NodeEventFrame {
	t.Helper()
	browserData, err := json.Marshal(map[string]any{
		"type": eventType, "stream_id": "untrusted-stream", "message_id": "untrusted-message",
		"content": nil, "response_metadata": responseMetadata, "references": []any{},
		"sio_event": "chat_predict", "created_at": occurredAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return postgresCurrentActivityNodeFrameWithBrowserData(
		t, expected, fence, sequence, occurredAt, browserData,
	)
}

func postgresCurrentActivityNodeFrameWithBrowserData(
	t *testing.T,
	expected outputapp.ExpectedIndexIngest,
	fence runtimedomain.Fence,
	sequence uint64,
	occurredAt time.Time,
	browserData json.RawMessage,
) outputapp.NodeEventFrame {
	t.Helper()
	wire := []byte(fmt.Sprintf("current-activity-node-%d", sequence))
	return outputapp.NodeEventFrame{
		StreamID: fence.ExecutionID + ":1", TenantID: expected.TenantID,
		ResourceProjectID: expected.ResourceProjectID, ProjectionProjectID: expected.ProjectionProjectID,
		WorkloadSessionID: fence.WorkloadSessionID, ProducerID: fence.ProducerID,
		EventID:         fence.CommandID + ":" + fmt.Sprint(sequence),
		LogicalOutputID: outputapp.NodeEventLogicalOutputID(fence.ExecutionID, sequence),
		Sequence:        sequence, OccurredAt: occurredAt, Fence: fence,
		PayloadDigest: runtimedomain.SHA256(wire), EncodedEvent: wire, BrowserData: browserData,
	}
}

func seedCurrentActivitySchemas(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO centry.project (id, create_success, suspended)
VALUES (2, TRUE, FALSE);
CREATE SCHEMA p_2;`); err != nil {
		t.Fatal(err)
	}
	// p_1 only: tenant/0123 (#287) now owns the chat tables, and
	// newMigratedPostgresIntegrationPool gives a database that already holds the
	// tenant history of project 1 before this seed runs — creating them again
	// there is a duplicate relation.
	// p_2 exists purely to prove tenant isolation and gets no tenant history, so
	// it still needs the tables declared by hand. The two shapes differ slightly
	// (p_2 keeps the deployed-legacy extras attachment_participant_id and
	// folder_id, which no query in this repository reads); that difference is not
	// what any assertion here turns on.
	for _, projectID := range []int64{2} {
		schema := pgx.Identifier{fmt.Sprintf("p_%d", projectID)}.Sanitize()
		if _, err := pool.Exec(t.Context(), fmt.Sprintf(`
CREATE TABLE %s.chat_conversations (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, name VARCHAR,
    is_private BOOLEAN NOT NULL DEFAULT TRUE, author_id INTEGER NOT NULL,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb, source VARCHAR NOT NULL,
    instructions VARCHAR, attachment_participant_id INTEGER, folder_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP
);
CREATE TABLE %s.chat_participants (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, entity_name VARCHAR NOT NULL,
    entity_meta JSONB NOT NULL DEFAULT '{}'::jsonb, meta JSON NOT NULL DEFAULT '{}'::json
);
CREATE TABLE %s.chat_participant_mapping (
    id SERIAL PRIMARY KEY, conversation_id INTEGER NOT NULL REFERENCES %s.chat_conversations(id),
    participant_id INTEGER NOT NULL REFERENCES %s.chat_participants(id),
    entity_settings JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP,
    UNIQUE (participant_id, conversation_id)
);
CREATE TABLE %s.chat_message_group (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE,
    author_participant_id INTEGER NOT NULL REFERENCES %s.chat_participants(id),
    conversation_id INTEGER NOT NULL REFERENCES %s.chat_conversations(id),
    sent_to_id INTEGER, reply_to_id INTEGER REFERENCES %s.chat_message_group(id) ON DELETE SET NULL,
    meta JSONB NOT NULL DEFAULT '{}'::jsonb, is_streaming BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP, task_id VARCHAR
);
CREATE TABLE %s.chat_message_items (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, item_type VARCHAR NOT NULL,
    order_index INTEGER NOT NULL, meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP,
    message_group_id INTEGER NOT NULL REFERENCES %s.chat_message_group(id) ON DELETE CASCADE
);
CREATE TABLE %s.chat_messages_text (
    id INTEGER PRIMARY KEY REFERENCES %s.chat_message_items(id) ON DELETE CASCADE,
    content TEXT NOT NULL
);
CREATE TABLE %s.chat_message_trace_step (
    id BIGSERIAL PRIMARY KEY,
    message_group_id INTEGER NOT NULL REFERENCES %s.chat_message_group(id) ON DELETE CASCADE,
    kind TEXT NOT NULL, run_id TEXT, parent_agent_name TEXT, parent_agent_call_id TEXT,
    started_at TIMESTAMPTZ, finished_at TIMESTAMPTZ, is_error BOOLEAN NOT NULL DEFAULT FALSE,
    has_visible_content BOOLEAN NOT NULL DEFAULT TRUE, tool_name TEXT, tool_inputs JSONB,
    tool_output TEXT, finish_reason TEXT, step_type TEXT, text TEXT, thinking TEXT,
    model_name TEXT, attrs JSONB NOT NULL DEFAULT '{}'::jsonb
);`,
			schema,
			schema,
			schema, schema, schema,
			schema, schema, schema, schema,
			schema, schema,
			schema, schema,
			schema, schema,
		)); err != nil {
			t.Fatal(err)
		}
	}
}

func seedCurrentActivityConversation(
	t *testing.T,
	pool *pgxpool.Pool,
	projectID int64,
	conversationUUID string,
	actorID int64,
	toolkitID int,
) {
	t.Helper()
	schema := pgx.Identifier{fmt.Sprintf("p_%d", projectID)}.Sanitize()
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(`
WITH conversation AS (
    INSERT INTO %s.chat_conversations (uuid, name, author_id, source, created_at)
    VALUES ($1, 'activity', $2, 'toolkit', now() AT TIME ZONE 'UTC')
    RETURNING id
),
participant AS (
    INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta)
    VALUES ('30000000-0000-4000-8000-000000000001', 'toolkit',
            jsonb_build_object('id', $3::integer, 'project_id', $4::integer))
    RETURNING id
)
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, created_at)
SELECT conversation.id, participant.id, now() AT TIME ZONE 'UTC'
FROM conversation, participant`, schema, schema, schema),
		conversationUUID, actorID, toolkitID, projectID,
	); err != nil {
		t.Fatal(err)
	}
}

func assertCurrentActivityProjection(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
	messageUUID string,
	wantThinking int,
	wantThinkingText string,
	wantThinkingAt time.Time,
	wantText string,
) {
	t.Helper()
	qualified := pgx.Identifier{schema}.Sanitize()
	var groups, toolCalls, thinking int
	var streaming, toolVisible, indexActivity, orphanLinkage bool
	var text, thinkingText, taskID string
	var thinkingStartedAt, thinkingFinishedAt time.Time
	if err := pool.QueryRow(t.Context(), fmt.Sprintf(`
SELECT count(*)::integer,
       bool_or(message_group.is_streaming),
       count(*) FILTER (WHERE trace.kind = 'tool_call')::integer,
       count(*) FILTER (WHERE trace.kind = 'thinking_step')::integer,
       bool_and(trace.has_visible_content) FILTER (WHERE trace.kind = 'tool_call'),
       max(trace.text) FILTER (WHERE trace.kind = 'thinking_step'),
       max(trace.started_at) FILTER (WHERE trace.kind = 'thinking_step'),
       max(trace.finished_at) FILTER (WHERE trace.kind = 'thinking_step'),
       max(message_text.content),
       bool_and(message_group.meta->>'activity_kind' = 'indexing'),
       max(message_group.task_id),
       bool_and(message_group.sent_to_id IS NULL AND message_group.reply_to_id IS NULL)
FROM %s.chat_message_group AS message_group
LEFT JOIN %s.chat_message_trace_step AS trace
  ON trace.message_group_id = message_group.id
LEFT JOIN %s.chat_message_items AS item
  ON item.message_group_id = message_group.id
 AND item.item_type = 'text_message'
LEFT JOIN %s.chat_messages_text AS message_text
  ON message_text.id = item.id
WHERE message_group.uuid = $1`,
		qualified, qualified, qualified, qualified),
		messageUUID,
	).Scan(
		&groups,
		&streaming,
		&toolCalls,
		&thinking,
		&toolVisible,
		&thinkingText,
		&thinkingStartedAt,
		&thinkingFinishedAt,
		&text,
		&indexActivity,
		&taskID,
		&orphanLinkage,
	); err != nil {
		t.Fatal(err)
	}
	// The joins repeat the group/tool row for the text item, but exactly one
	// text item exists, so these counts remain the actual trace cardinalities.
	if groups != wantThinking+1 || streaming || toolCalls != 1 || !toolVisible ||
		thinking != wantThinking || thinkingText != wantThinkingText ||
		!thinkingStartedAt.Equal(wantThinkingAt) ||
		!thinkingFinishedAt.Equal(wantThinkingAt) || text != wantText ||
		!indexActivity || taskID == "" || !orphanLinkage {
		t.Fatalf("current Activity projection: rows=%d streaming=%t tools=%d tool_visible=%t thinking=%d thinking_text=%q thinking_started_at=%s thinking_finished_at=%s text=%q index_activity=%t task_id=%q orphan_linkage=%t",
			groups, streaming, toolCalls, toolVisible, thinking, thinkingText,
			thinkingStartedAt, thinkingFinishedAt, text,
			indexActivity, taskID, orphanLinkage)
	}
}
