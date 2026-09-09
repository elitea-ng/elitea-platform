package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ToolCallRecord is one tool call, from either of the two places a tool
// actually runs. See migrations/shared/0119_tool_call_records.sql for why the
// record exists at all and why neither producer could stand in for the other.
//
// A producer fills what it KNOWS. ToolkitID is 0 for an agent turn, which
// carries a toolkit name and type in its trace metadata and no id. Nothing
// here is resolved by guessing: an unknown field is stored as NULL rather than
// looked up through a join whose answer a later rename would change.
//
// ExecutionID is set by BOTH producers as of #875 (it was left empty for an
// agent turn before that): agent_trace.go's recordAgentToolCalls now passes
// the same frame.Fence.ExecutionID that lockCurrentAgentMessageGroup already
// matches against message_group.task_id — the value gateway.llm_request_logs
// carries under the same name (shared 0100). That correlation is what lets a
// cost read (internal/api/v2/analytics/estimate.go) attribute an execution's
// LLM spend to the tools it called, the way GetAgentAnalytics already
// attributes it to an agent. A row projected before #875 shipped keeps
// ExecutionID NULL; it still counts in the Tools tab's call totals, it is just
// invisible to the cost-by-tool read.
type ToolCallRecord struct {
	ProjectID int64
	// Source is "explicit_run" or "agent_turn". It is stored rather than
	// inferred because the acceptance test of issue 618 — a tool call made
	// OUTSIDE a chat turn appears in the breakdown — has to be able to tell the
	// two halves apart.
	Source string
	// SourceRef is the producing row's own natural key, so a replay UPDATES its
	// record instead of adding a second one.
	SourceRef   string
	ToolkitID   int64
	ToolkitName string
	ToolkitType string
	ToolName    string
	StartedAt   time.Time
	// FinishedAt is the zero time while the call is still running.
	FinishedAt  time.Time
	IsError     bool
	ActorUserID int64
	ExecutionID string
}

const (
	ToolCallSourceExplicitRun = "explicit_run"
	ToolCallSourceAgentTurn   = "agent_turn"
)

// ErrInvalidToolCallRecord is returned rather than silently skipping a row: a
// producer that stops recording is the failure this table exists to prevent, so
// it must be loud at its own call site.
var ErrInvalidToolCallRecord = errors.New("invalid tool call record")

func (r ToolCallRecord) validate() error {
	switch {
	case r.ProjectID <= 0,
		r.Source != ToolCallSourceExplicitRun && r.Source != ToolCallSourceAgentTurn,
		strings.TrimSpace(r.SourceRef) == "",
		strings.TrimSpace(r.ToolName) == "",
		r.StartedAt.IsZero(),
		!r.FinishedAt.IsZero() && r.FinishedAt.Before(r.StartedAt):
		return ErrInvalidToolCallRecord
	}
	return nil
}

// ToolCallRecordsRepository writes elitea_runtime.tool_call_records.
//
// Every write takes an explicit executor. Both producers already hold a
// transaction of their own — the agent turn is inside the trace projection's
// tenant transaction, the explicit run settles inside the caller's request —
// and a record written on a separate connection could survive a rollback of the
// thing it claims to describe.
type ToolCallRecordsRepository struct {
	store sqlExecutor
}

func NewToolCallRecordsRepository(pool *pgxpool.Pool) (*ToolCallRecordsRepository, error) {
	store, err := newPostgresSharedStore(pool)
	if err != nil {
		return nil, err
	}
	return &ToolCallRecordsRepository{store: store}, nil
}

// Record writes one tool call, or updates the one already written for the same
// producing row.
//
// The UPSERT is what makes a streaming producer safe. The agent turn
// re-projects the same tool call on every partial message as its output and
// finish time fill in, and an idempotent re-admission of an explicit run reuses
// its execution id. An INSERT would count one call many times; this counts it
// once and keeps the latest terminal state.
func (r *ToolCallRecordsRepository) Record(ctx context.Context, record ToolCallRecord) error {
	if r == nil || r.store == nil {
		return ErrInvalidToolCallRecord
	}
	return RecordToolCall(ctx, r.store, record)
}

// RecordToolCall is the same write against a caller-supplied executor, so a
// producer that already owns a transaction commits the record with the row it
// describes.
func RecordToolCall(ctx context.Context, exec sqlExecutor, record ToolCallRecord) error {
	if exec == nil {
		return ErrInvalidToolCallRecord
	}
	if err := record.validate(); err != nil {
		return err
	}

	var finished any
	if !record.FinishedAt.IsZero() {
		finished = record.FinishedAt.UTC()
	}
	var toolkitID any
	if record.ToolkitID > 0 {
		toolkitID = record.ToolkitID
	}
	var actorUserID any
	if record.ActorUserID > 0 {
		actorUserID = record.ActorUserID
	}

	// finished_at is never moved back to NULL: a later delta that has lost the
	// finish time must not un-settle a call that already reported one.
	if _, err := exec.Exec(ctx, `
INSERT INTO elitea_runtime.tool_call_records (
    project_id, source, source_ref, toolkit_id, toolkit_name, toolkit_type,
    tool_name, started_at, finished_at, is_error, actor_user_id, execution_id
) VALUES (
    $1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''),
    $7, $8, $9, $10, $11, NULLIF($12, '')
)
ON CONFLICT (project_id, source, source_ref) DO UPDATE SET
    toolkit_id    = COALESCE(EXCLUDED.toolkit_id, elitea_runtime.tool_call_records.toolkit_id),
    toolkit_name  = COALESCE(EXCLUDED.toolkit_name, elitea_runtime.tool_call_records.toolkit_name),
    toolkit_type  = COALESCE(EXCLUDED.toolkit_type, elitea_runtime.tool_call_records.toolkit_type),
    tool_name     = EXCLUDED.tool_name,
    started_at    = LEAST(EXCLUDED.started_at, elitea_runtime.tool_call_records.started_at),
    finished_at   = COALESCE(EXCLUDED.finished_at, elitea_runtime.tool_call_records.finished_at),
    is_error      = EXCLUDED.is_error,
    actor_user_id = COALESCE(EXCLUDED.actor_user_id, elitea_runtime.tool_call_records.actor_user_id),
    execution_id  = COALESCE(EXCLUDED.execution_id, elitea_runtime.tool_call_records.execution_id)`,
		record.ProjectID, record.Source, record.SourceRef, toolkitID,
		record.ToolkitName, record.ToolkitType, record.ToolName,
		record.StartedAt.UTC(), finished, record.IsError, actorUserID,
		record.ExecutionID,
	); err != nil {
		return fmt.Errorf("record tool call: %w", err)
	}
	return nil
}
