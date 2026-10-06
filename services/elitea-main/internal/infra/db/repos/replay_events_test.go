package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	executionsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
)

func TestReplayEventsRepositoryVerifiesDurableDigestAndCopiesData(t *testing.T) {
	data := []byte(`{"valid":true}`)
	digest := runtimedomain.SHA256(data)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(0), int64(7), true, true}}},
		rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{
			int64(7), "configuration.validation.completed", data, digest[:], true,
		}}}},
	}
	repository := newReplayEventsRepository(executor)
	events, err := repository.Replay(context.Background(), "42", "execution-1", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Cursor != 7 || string(events[0].Data) != string(data) {
		t.Fatalf("unexpected replay: %+v", events)
	}
	data[0] = 'x'
	if string(events[0].Data) != `{"valid":true}` {
		t.Fatal("replay data aliases the database buffer")
	}
}

func TestReplayEventsRepositoryHidesAgentTerminalUntilProjectionCommits(t *testing.T) {
	suggestion := []byte(`{"type":"next_input_suggestion_ready","content":"Continue?"}`)
	suggestionDigest := runtimedomain.SHA256(suggestion)
	terminal := []byte(`{"type":"full_message","content":"Done"}`)
	terminalDigest := runtimedomain.SHA256(terminal)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(0), int64(7), true, true}}},
		rowsResult: &scriptedRows{rows: []scriptedRow{
			{values: []any{int64(6), replayEventNodeEvent, suggestion, suggestionDigest[:], true}},
			{values: []any{int64(7), replayEventNodeEvent, terminal, terminalDigest[:], false}},
		}},
	}
	events, err := newReplayEventsRepository(executor).Replay(
		context.Background(),
		"42",
		"execution-1",
		0,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Cursor != 6 {
		t.Fatalf("nonterminal replay = %+v", events)
	}
	query := executor.queryCalls[0].sql
	for _, required := range []string{
		"'full_message'",
		"'agent_hitl_interrupt'",
		"'mcp_authorization_required'",
		"terminal.payload_type = 'AGENT_EXECUTION_RESULT'",
		"terminal.projected_at IS NOT NULL",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("terminal visibility barrier is missing %q: %s", required, query)
		}
	}
}

func TestReplayEventsRepositoryReleasesAgentTerminalAfterProjectionCommits(t *testing.T) {
	terminal := []byte(`{"type":"full_message","content":"Done"}`)
	digest := runtimedomain.SHA256(terminal)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(0), int64(7), true, true}}},
		rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{
			int64(7), replayEventNodeEvent, terminal, digest[:], true,
		}}}},
	}
	events, err := newReplayEventsRepository(executor).Replay(
		context.Background(),
		"42",
		"execution-1",
		6,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Cursor != 7 || string(events[0].Data) != string(terminal) {
		t.Fatalf("terminal replay = %+v", events)
	}
}

func TestReplayEventsRepositoryDoesNotAdvancePastUnprojectedTerminal(t *testing.T) {
	progress := []byte(`{"type":"next_input_suggestion_ready","content":"Continue?"}`)
	progressDigest := runtimedomain.SHA256(progress)
	terminal := []byte(`{"type":"full_message","content":"Done"}`)
	terminalDigest := runtimedomain.SHA256(terminal)
	later := []byte(`{"type":"execution_metadata","content":"Settled"}`)
	laterDigest := runtimedomain.SHA256(later)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{
			{values: []any{int64(0), int64(8), true, true}},
			{values: []any{int64(0), int64(8), true, true}},
			{values: []any{int64(0), int64(8), true, true}},
		},
		rowsResults: []*scriptedRows{
			{rows: []scriptedRow{
				{values: []any{int64(6), replayEventNodeEvent, progress, progressDigest[:], true}},
				{values: []any{int64(7), replayEventNodeEvent, terminal, terminalDigest[:], false}},
				{values: []any{int64(8), replayEventNodeEvent, later, laterDigest[:], true}},
			}},
			{rows: []scriptedRow{
				{values: []any{int64(7), replayEventNodeEvent, terminal, terminalDigest[:], true}},
				{values: []any{int64(8), replayEventNodeEvent, later, laterDigest[:], true}},
			}},
		},
	}
	repository := newReplayEventsRepository(executor)
	first, err := repository.Replay(context.Background(), "42", "execution-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Cursor != 6 {
		t.Fatalf("replay skipped the projection barrier: %+v", first)
	}
	second, err := repository.Replay(context.Background(), "42", "execution-1", first[0].Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].Cursor != 7 || second[1].Cursor != 8 {
		t.Fatalf("reconnect lost the now-visible terminal: %+v", second)
	}
	third, err := repository.Replay(context.Background(), "42", "execution-1", second[1].Cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 || len(executor.queryCalls) != 2 {
		t.Fatalf("caught-up replay duplicated events: %+v", third)
	}
	if got := executor.queryCalls[1].args[2]; got != int64(6) {
		t.Fatalf("reconnect did not keep the last visible cursor: %v", got)
	}
}

func TestReplayEventsRepositoryRejectsTamperedDurableData(t *testing.T) {
	data := []byte(`{"valid":true}`)
	wrong := runtimedomain.SHA256([]byte("other"))
	repository := newReplayEventsRepository(&scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(0), int64(7), true, true}}},
		rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{
			int64(7), "configuration.validation.completed", data, wrong[:], true,
		}}}},
	})
	_, err := repository.Replay(context.Background(), "42", "execution-1", 0, 10)
	if !errors.Is(err, executionsapi.ErrInvalidEventStream) {
		t.Fatalf("expected invalid event stream, got %v", err)
	}
}

func TestReplayEventsRepositoryEmitsResetThenRetainedTerminal(t *testing.T) {
	terminal := []byte(`{"status":"ok"}`)
	digest := runtimedomain.SHA256(terminal)
	executor := &scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(11), int64(15), true, false}}},
		rowsResult: &scriptedRows{rows: []scriptedRow{{values: []any{
			int64(15), "index.ingest.completed", terminal, digest[:], true,
		}}}},
	}
	events, err := newReplayEventsRepository(executor).Replay(context.Background(), "42", "execution-1", 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].Cursor != 11 ||
		events[0].Type != replayEventReset ||
		events[1].Cursor != 15 ||
		events[1].Type != "index.ingest.completed" {
		t.Fatalf("unexpected reset replay: %+v", events)
	}
	if got := executor.queryCalls[0].args[2]; got != int64(11) {
		t.Fatalf("durable replay did not resume at reset cursor: %v", got)
	}
}

func TestReplayEventsRepositoryRejectsUnknownOrCrossExecutionCursor(t *testing.T) {
	repository := newReplayEventsRepository(&scriptedExecutor{
		rowResults: []scriptedRow{{values: []any{int64(4), int64(12), true, false}}},
	})
	_, err := repository.Replay(context.Background(), "42", "execution-1", 9, 10)
	if !errors.Is(err, executionsapi.ErrInvalidEventStream) {
		t.Fatalf("expected invalid event stream, got %v", err)
	}
}

func TestReplayEventsRepositoryBoundsStaleReconnectStorm(t *testing.T) {
	const reconnects = 128
	terminal := []byte(`{"status":"ok"}`)
	digest := runtimedomain.SHA256(terminal)
	executor := &scriptedExecutor{
		rowResults:  make([]scriptedRow, 0, reconnects),
		rowsResults: make([]*scriptedRows, 0, reconnects),
	}
	for range reconnects {
		executor.rowResults = append(executor.rowResults, scriptedRow{
			values: []any{int64(11), int64(15), true, false},
		})
		executor.rowsResults = append(executor.rowsResults, &scriptedRows{rows: []scriptedRow{{values: []any{
			int64(15), "index.ingest.completed", terminal, digest[:], true,
		}}}})
	}
	repository := newReplayEventsRepository(executor)
	for reconnect := range reconnects {
		events, err := repository.Replay(context.Background(), "42", "execution-1", 3, 10)
		if err != nil {
			t.Fatalf("reconnect %d: %v", reconnect, err)
		}
		if len(events) != 2 || events[0].Type != replayEventReset || events[1].Type != "index.ingest.completed" {
			t.Fatalf("reconnect %d returned an unbounded or incomplete replay: %+v", reconnect, events)
		}
	}
	if len(executor.rowCalls) != reconnects || len(executor.queryCalls) != reconnects {
		t.Fatalf(
			"stale reconnects performed unexpected queries: bounds=%d replay=%d",
			len(executor.rowCalls),
			len(executor.queryCalls),
		)
	}
}

func TestReplayEventsRepositorySkipsRowsQueryAtHighCursor(t *testing.T) {
	tests := []struct {
		name          string
		afterCursor   uint64
		bounds        scriptedRow
		wantEventType string
		wantCursor    uint64
	}{
		{
			name:        "caught up",
			afterCursor: 15,
			bounds:      scriptedRow{values: []any{int64(11), int64(15), true, true}},
		},
		{
			name:          "retention reset reaches high cursor",
			afterCursor:   3,
			bounds:        scriptedRow{values: []any{int64(11), int64(11), true, false}},
			wantEventType: replayEventReset,
			wantCursor:    11,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &scriptedExecutor{rowResults: []scriptedRow{test.bounds}}
			events, err := newReplayEventsRepository(executor).Replay(
				context.Background(),
				"42",
				"execution-1",
				test.afterCursor,
				10,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(executor.rowCalls) != 1 || len(executor.queryCalls) != 0 {
				t.Fatalf(
					"caught-up replay performed unexpected queries: bounds=%d replay=%d",
					len(executor.rowCalls),
					len(executor.queryCalls),
				)
			}
			if test.wantEventType == "" {
				if events == nil || len(events) != 0 {
					t.Fatalf("caught-up replay = %+v, want non-nil empty result", events)
				}
				return
			}
			if len(events) != 1 || events[0].Type != test.wantEventType || events[0].Cursor != test.wantCursor {
				t.Fatalf("reset replay = %+v, want one %s event at %d", events, test.wantEventType, test.wantCursor)
			}
		})
	}
}
