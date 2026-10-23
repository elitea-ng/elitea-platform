package repos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/jackc/pgx/v5"
)

type recordingCurrentIndexActivityProjector struct {
	terminals []currentIndexActivityTerminal
}

func (*recordingCurrentIndexActivityProjector) projectNodeEvent(
	context.Context,
	sqlExecutor,
	int64,
	outputapp.NodeEventFrame,
) error {
	return nil
}

func (p *recordingCurrentIndexActivityProjector) projectTerminal(
	_ context.Context,
	_ sqlExecutor,
	_ int64,
	terminal currentIndexActivityTerminal,
) error {
	p.terminals = append(p.terminals, terminal)
	return nil
}

func TestCurrentIndexActivityMapsCurrentWorkerEventsToCurrentTraceContract(t *testing.T) {
	occurredAt := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	thinkingFixture := currentWorkerThinkingStepFixture(t)
	tests := []struct {
		name         string
		event        json.RawMessage
		kind         string
		runID        string
		text         string
		finishReason string
		stepType     string
		modelName    string
		isError      bool
	}{
		{
			name:  "tool start",
			event: json.RawMessage(`{"type":"agent_tool_start","stream_id":"event-controlled","message_id":"event-controlled","content":null,"response_metadata":{"tool_name":"index_data","tool_run_id":"run-1","timestamp_start":"2026-07-27T09:59:58Z","metadata":{"initiator":"user","tool_name":"index_data","display_name":"configurations","ignored":"must-not-persist"}},"references":[],"sio_event":"chat_predict","created_at":"2026-07-27T09:59:58Z"}`),
			kind:  "tool_call",
			runID: "run-1",
		},
		{
			name:  "standalone worker thinking step",
			event: thinkingFixture,
			kind:  "thinking_step",
			runID: "00000000-0000-0000-0000-000000000001",
			text:  "10 files processed",
		},
		{
			name:  "tool error",
			event: json.RawMessage(`{"type":"agent_tool_error","stream_id":"event-controlled","message_id":"event-controlled","content":null,"response_metadata":{"tool_name":"index_data","tool_run_id":"run-1","finish_reason":"error","timestamp_start":"2026-07-27T09:59:58Z","timestamp_finish":"2026-07-27T10:00:00Z"},"references":[],"sio_event":"chat_predict","created_at":"2026-07-27T10:00:00Z"}`),
			kind:  "tool_call", runID: "run-1", finishReason: "error", isError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, ok, err := currentIndexActivityNodeFromFrame(outputapp.NodeEventFrame{
				EventID:     "command-1:7",
				OccurredAt:  occurredAt,
				BrowserData: test.event,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Fatal("current worker fixture was not mapped")
			}
			if node.kind != test.kind || node.runID != test.runID ||
				node.text != test.text || node.finishReason != test.finishReason ||
				node.stepType != test.stepType || node.modelName != test.modelName ||
				node.isError != test.isError {
				t.Fatalf("unexpected current Activity node: %+v", node)
			}
			if test.kind == "thinking_step" {
				currentDatetime := time.Date(2026, 7, 27, 10, 0, 0, int(time.Millisecond), time.UTC)
				if !node.startedAt.Equal(currentDatetime) ||
					node.finishedAt == nil ||
					!node.finishedAt.Equal(currentDatetime) ||
					node.stepType != "" ||
					node.modelName != "" {
					t.Fatalf("standalone thinking fallback drifted: %+v", node)
				}
			}
			if strings.Contains(string(node.attrs), "ignored") ||
				strings.Contains(string(node.attrs), "event-controlled") {
				t.Fatalf("untrusted event identity/metadata reached attrs: %s", node.attrs)
			}
			if test.kind == "tool_call" && strings.Contains(string(test.event), "agent_tool_start") &&
				!strings.Contains(string(node.attrs), `"display_name":"configurations"`) {
				t.Fatalf("current tool display metadata was not projected: %s", node.attrs)
			}
		})
	}
}

func TestCurrentIndexActivityRejectsUnboundedOrUnsupportedNodeFields(t *testing.T) {
	occurredAt := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	for _, event := range []string{
		`{"type":"agent_index_data_status","response_metadata":{"run_id":"run-1"}}`,
		`{"type":"agent_thinking_step","response_metadata":{"run_id":"run-1","message":"` +
			strings.Repeat("x", currentActivityMaxTextBytes+1) + `"}}`,
		`{"type":"agent_tool_start","response_metadata":{"tool_run_id":"` +
			strings.Repeat("x", currentActivityMaxNameBytes+1) + `"}}`,
	} {
		if _, ok, err := currentIndexActivityNodeFromFrame(outputapp.NodeEventFrame{
			OccurredAt: occurredAt, BrowserData: json.RawMessage(event),
		}); ok || (strings.Contains(event, "agent_index_data_status") && err != nil) ||
			(!strings.Contains(event, "agent_index_data_status") && err == nil) {
			t.Fatalf("unsupported/malformed classification: ok=%t err=%v", ok, err)
		}
	}
}

func TestCurrentIndexActivityMissingCurrentSchemaIsSafeNoop(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{{values: []any{false}}}}
	projector := &postgresCurrentIndexActivityProjector{}
	if err := projector.projectNodeEvent(t.Context(), executor, 7, currentActivityTestFrame()); err != nil {
		t.Fatal(err)
	}
	if len(executor.rowCalls) != 1 ||
		executor.rowCalls[0].args[0] != `"p_7".chat_conversations` {
		t.Fatalf("schema preflight did not use trusted identifier: %+v", executor.rowCalls)
	}
	for _, table := range []string{
		`"p_7".chat_conversations`,
		`"p_7".chat_participants`,
		`"p_7".chat_participant_mapping`,
		`"p_7".chat_message_group`,
		`"p_7".chat_message_trace_step`,
		`"p_7".chat_message_items`,
		`"p_7".chat_messages_text`,
	} {
		found := false
		for _, argument := range executor.rowCalls[0].args {
			if argument == table {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("schema preflight omitted %s: %+v", table, executor.rowCalls[0])
		}
	}
}

func TestCurrentIndexActivityUsesOneStatementPerEventAfterPositiveSchemaCapability(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{true}},
		{values: []any{int64(31)}},
		{values: []any{int64(31)}},
	}}
	projector := &postgresCurrentIndexActivityProjector{}
	frame := currentActivityTestFrame()
	if err := projector.projectNodeEvent(t.Context(), executor, 7, frame); err != nil {
		t.Fatal(err)
	}
	if err := projector.projectNodeEvent(t.Context(), executor, 7, frame); err != nil {
		t.Fatal(err)
	}
	if len(executor.rowCalls) != 3 {
		t.Fatalf("two events used %d queries, want one preflight plus one per event", len(executor.rowCalls))
	}
	for _, call := range executor.rowCalls[1:] {
		for _, fragment := range []string{
			"elitea_runtime.index_ingest_jobs",
			"chat_conversations",
			"chat_message_group",
			"chat_message_trace_step",
		} {
			if !strings.Contains(call.sql, fragment) {
				t.Fatalf("atomic Activity statement omitted %s", fragment)
			}
		}
	}
}

func TestCurrentIndexActivityUsesOneStatementPerTerminalAfterPositiveSchemaCapability(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{true}},
		{values: []any{int64(41)}},
		{values: []any{int64(41)}},
	}}
	projector := &postgresCurrentIndexActivityProjector{}
	terminal := currentIndexActivityTerminal{
		ExecutionID: "execution-1",
		Generation:  1,
		OccurredAt:  time.Date(2026, 7, 27, 10, 0, 1, 0, time.UTC),
		Message:     "Successfully indexed 10 files.",
	}
	if err := projector.projectTerminal(t.Context(), executor, 7, terminal); err != nil {
		t.Fatal(err)
	}
	if err := projector.projectTerminal(t.Context(), executor, 7, terminal); err != nil {
		t.Fatal(err)
	}
	if len(executor.rowCalls) != 3 || len(executor.execCalls) != 0 {
		t.Fatalf(
			"two terminals used QueryRow=%d Exec=%d, want one preflight plus one statement/terminal",
			len(executor.rowCalls), len(executor.execCalls),
		)
	}
	for _, fragment := range []string{
		"elitea_runtime.index_ingest_jobs",
		"chat_message_group",
		"chat_message_items",
		"chat_messages_text",
	} {
		if !strings.Contains(executor.rowCalls[1].sql, fragment) {
			t.Fatalf("atomic terminal statement omitted %s", fragment)
		}
	}
}

func TestCurrentIndexActivityMissingOrCrossProjectTargetIsSafeNoop(t *testing.T) {
	executor := &scriptedExecutor{rowResults: []scriptedRow{
		{values: []any{true}},
		{err: pgx.ErrNoRows},
	}}
	projector := &postgresCurrentIndexActivityProjector{}
	if err := projector.projectNodeEvent(t.Context(), executor, 8, currentActivityTestFrame()); err != nil {
		t.Fatal(err)
	}
	if len(executor.rowCalls) != 2 ||
		strings.Contains(executor.rowCalls[1].sql, `"p_7"`) ||
		!strings.Contains(executor.rowCalls[1].sql, `"p_8"`) {
		t.Fatalf("cross-project lookup escaped trusted tenant: %+v", executor.rowCalls)
	}
}

func currentActivityTestFrame() outputapp.NodeEventFrame {
	return outputapp.NodeEventFrame{
		OccurredAt: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
		Fence: runtimedomain.Fence{
			ExecutionID: "execution-1",
			Generation:  1,
		},
		BrowserData: json.RawMessage(
			`{"type":"agent_thinking_step","response_metadata":{"run_id":"run-1","message":"10 files processed","datetime":"2026-07-27T09:59:59Z"}}`,
		),
	}
}

func currentWorkerThinkingStepFixture(t *testing.T) json.RawMessage {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate current index Activity test source")
	}
	// Recorded from the former Python worker's index thinking step, the shape
	// stored index activity rows carry; kept as data after that worker went.
	fixturePath := filepath.Join(filepath.Dir(sourceFile), "testdata", "current_index_thinking_step.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read standalone worker thinking fixture: %v", err)
	}
	if !json.Valid(fixture) {
		t.Fatal("standalone worker thinking fixture is not valid JSON")
	}
	var event struct {
		Type             string         `json:"type"`
		ResponseMetadata map[string]any `json:"response_metadata"`
	}
	if err := json.Unmarshal(fixture, &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "agent_thinking_step" {
		t.Fatalf("standalone worker fixture type=%q", event.Type)
	}
	for _, enriched := range []string{
		"timestamp_start",
		"timestamp_finish",
		"type",
		"model_name",
	} {
		if _, found := event.ResponseMetadata[enriched]; found {
			t.Fatalf("standalone worker fixture invented %q", enriched)
		}
	}
	return fixture
}
