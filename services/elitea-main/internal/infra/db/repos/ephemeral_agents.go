package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EphemeralAgents is elitea_runtime.ephemeral_agents (shared/0232, ADR-0050
// decisions 4 and 9d, #1614): one row per sub-agent a run started with
// spawn_agent. The worker's ReportEphemeralAgents writes it.
type EphemeralAgents struct {
	pool *pgxpool.Pool
}

// ErrEphemeralAgentsRefused is a report with no job row for the fence's
// generation: nothing is stored.
var ErrEphemeralAgentsRefused = errors.New("ephemeral agents report refused")

// NewEphemeralAgents writes through pool.
func NewEphemeralAgents(pool *pgxpool.Pool) (*EphemeralAgents, error) {
	if pool == nil {
		return nil, errors.New("ephemeral agents require a database pool")
	}
	return &EphemeralAgents{pool: pool}, nil
}

// EphemeralAgentRow is one spawn as the worker reported it, checked by the
// control server. Empty strings and a zero FinishedAt are stored as NULL.
type EphemeralAgentRow struct {
	AgentID       string
	ParentCallID  string
	Ordinal       int
	Label         string
	DisplayName   string
	Avatar        string
	ProfileDigest string
	BriefDigest   string
	// BriefRefs is a JSON object, or nil without a brief.
	BriefRefs    []byte
	Status       string
	Steps        int64
	InputTokens  int64
	OutputTokens int64
	ErrorCode    string
	StartedAt    time.Time
	FinishedAt   time.Time
	// Depth is 1 or 2 (#1872); 0 is stored as 1. ParentAgentID is the
	// sub-agent that started this one, empty (NULL) at depth 1.
	Depth         int
	ParentAgentID string
}

// Record upserts every row in one transaction. The project, the
// conversation and the parent agent come from execution_jobs (and
// agent_execution_jobs) at the given generation, never from the worker.
// An existing row is updated only by the same execution and a generation
// no older than the stored one; its identity columns (the parent call, the
// ordinal, the profile, the brief, the depth and the parent sub-agent) never
// change.
func (s *EphemeralAgents) Record(ctx context.Context, executionID, commandID string, generation uint64, rows []EphemeralAgentRow) error {
	if executionID == "" || commandID == "" || generation == 0 || generation > math.MaxInt64 {
		return ErrEphemeralAgentsRefused
	}
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ephemeral agents: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND generation = $2)`,
		executionID, int64(generation)).Scan(&exists); err != nil { //nolint:gosec // bounded above
		return fmt.Errorf("ephemeral agents: read the run: %w", err)
	}
	if !exists {
		return ErrEphemeralAgentsRefused
	}
	batch := &pgx.Batch{}
	for _, row := range rows {
		var finished *time.Time
		if !row.FinishedAt.IsZero() {
			at := row.FinishedAt.UTC()
			finished = &at
		}
		var refs any
		if row.BriefRefs != nil {
			refs = string(row.BriefRefs)
		}
		batch.Queue(`
WITH run AS (
    SELECT job.execution_id, job.resource_project_id,
           CASE WHEN agent.client_stream_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN agent.client_stream_id::uuid END AS conversation_uuid,
           agent.application_id
    FROM elitea_runtime.execution_jobs AS job
    LEFT JOIN elitea_runtime.agent_execution_jobs AS agent
      ON agent.execution_id = job.execution_id AND agent.generation = job.generation
    WHERE job.execution_id = $1::text AND job.generation = $3::bigint
)
INSERT INTO elitea_runtime.ephemeral_agents AS e (
    execution_id, agent_id, command_id, generation, project_id, conversation_uuid, parent_application_id,
    parent_call_id, ordinal, label, display_name, avatar, profile_digest, brief_digest, brief_refs,
    status, steps, input_tokens, output_tokens, error_code, started_at, finished_at, depth, parent_agent_id)
SELECT run.execution_id, $4::text, $2::text, $3::bigint, run.resource_project_id, run.conversation_uuid,
       run.application_id, $5::text, $6::integer, NULLIF($7::text, ''), NULLIF($8::text, ''),
       NULLIF($9::text, ''), $10::text, NULLIF($11::text, ''), $12::jsonb, $13::text, $14::integer,
       $15::bigint, $16::bigint, NULLIF($17::text, ''), $18::timestamptz, $19::timestamptz,
       $20::smallint, NULLIF($21::text, '')
FROM run
ON CONFLICT (execution_id, agent_id) DO UPDATE SET
    command_id    = EXCLUDED.command_id,
    generation    = EXCLUDED.generation,
    status        = EXCLUDED.status,
    steps         = GREATEST(e.steps, EXCLUDED.steps),
    input_tokens  = GREATEST(e.input_tokens, EXCLUDED.input_tokens),
    output_tokens = GREATEST(e.output_tokens, EXCLUDED.output_tokens),
    error_code    = EXCLUDED.error_code,
    finished_at   = EXCLUDED.finished_at,
    updated_at    = clock_timestamp()
WHERE EXCLUDED.generation >= e.generation
  AND e.parent_call_id = EXCLUDED.parent_call_id
  AND e.profile_digest = EXCLUDED.profile_digest
  AND e.brief_digest IS NOT DISTINCT FROM EXCLUDED.brief_digest
  AND e.depth = EXCLUDED.depth
  AND e.parent_agent_id IS NOT DISTINCT FROM EXCLUDED.parent_agent_id`,
			executionID, commandID, int64(generation), row.AgentID, row.ParentCallID, row.Ordinal, //nolint:gosec // bounded above
			row.Label, row.DisplayName, row.Avatar, row.ProfileDigest, row.BriefDigest, refs, row.Status,
			row.Steps, row.InputTokens, row.OutputTokens, row.ErrorCode, row.StartedAt.UTC(), finished,
			max(row.Depth, 1), row.ParentAgentID)
	}
	results := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("ephemeral agents: upsert: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("ephemeral agents: upsert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ephemeral agents: commit: %w", err)
	}
	return nil
}
