package repos

// The delegation assertion stores (ADR-0044 decision 4, #1501): the run
// facts the mint reads, the one-time record of inbound assertions
// (shared 0210) and the security audit append of both.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// RemoteAgentRuns reads a claimed agent run for the mint.
type RemoteAgentRuns struct{ pool *pgxpool.Pool }

// NewRemoteAgentRuns builds the reader over the runtime pool.
func NewRemoteAgentRuns(pool *pgxpool.Pool) (*RemoteAgentRuns, error) {
	if pool == nil {
		return nil, errors.New("remote agent runs: a pool is required")
	}
	return &RemoteAgentRuns{pool: pool}, nil
}

// remoteAgentRunSQL reads the run, the request entry the worker reads (its
// frozen toolkit list) and the security context of a governed run.
const remoteAgentRunSQL = `SELECT j.capability_id, j.actor_id, j.resource_project_id,
       COALESCE(j.toolkit_types, '{}'::text[]), COALESCE(a.application_id, 0), e.content_bytes, s.context_bytes
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = a.input_bundle_id AND e.entry_id = a.request_entry_id
LEFT JOIN elitea_runtime.execution_security_contexts AS s
  ON s.execution_id = j.execution_id
WHERE j.execution_id = $1 AND j.generation = $2`

// ReadRun implements remoteagent.RunReader.
func (r *RemoteAgentRuns) ReadRun(ctx context.Context, executionID string, generation uint64) (remoteagent.Run, error) {
	if executionID == "" || generation == 0 || generation > 1<<62 {
		return remoteagent.Run{}, errors.New("remote agent runs: invalid execution")
	}
	var (
		run      = remoteagent.Run{ExecutionID: executionID, Generation: generation}
		project  int64
		content  []byte
		security []byte
	)
	err := r.pool.QueryRow(ctx, remoteAgentRunSQL, executionID, int64(generation)).Scan(
		&run.CapabilityID, &run.ActorUserID, &project, &run.ToolkitTypes, &run.ApplicationID, &content, &security)
	if errors.Is(err, pgx.ErrNoRows) {
		return remoteagent.Run{}, errors.New("remote agent runs: not an agent run")
	}
	if err != nil {
		return remoteagent.Run{}, fmt.Errorf("remote agent runs: read: %w", err)
	}
	run.TenantProjectID = project
	input := &runtimev1.AgentExecutionInputV1{}
	if err := proto.Unmarshal(content, input); err != nil {
		return remoteagent.Run{}, errors.New("remote agent runs: the stored input cannot be read")
	}
	if run.Tools, err = agentexecutionapp.FrozenTools(run.CapabilityID, input); err != nil {
		return remoteagent.Run{}, errors.New("remote agent runs: the frozen toolkit list cannot be read")
	}
	if security != nil {
		sc := &runtimev1.ExecutionSecurityContextV1{}
		if err := proto.Unmarshal(security, sc); err != nil {
			return remoteagent.Run{}, errors.New("remote agent runs: the security context cannot be read")
		}
		run.Governed = true
		run.AgentBindingID = sc.GetAgentBindingId()
		run.MandateID = sc.GetMandateId()
		run.SubjectPrincipalRef = sc.GetSubjectPrincipalRef()
		if tenant := sc.GetTenantProjectId(); tenant > 0 && tenant != project {
			return remoteagent.Run{}, errors.New("remote agent runs: the security context names another project")
		}
	}
	if _, err := strconv.ParseInt(run.ActorUserID, 10, 64); err != nil {
		run.ActorUserID = ""
	}
	return run, nil
}

// RemoteAgentReplays is the one-time record of inbound assertions.
type RemoteAgentReplays struct {
	pool   *pgxpool.Pool
	claims atomic.Uint64
}

// NewRemoteAgentReplays builds the store over the main pool.
func NewRemoteAgentReplays(pool *pgxpool.Pool) (*RemoteAgentReplays, error) {
	if pool == nil {
		return nil, errors.New("remote agent replays: a pool is required")
	}
	return &RemoteAgentReplays{pool: pool}, nil
}

// remoteAgentReplayCleanupEvery: every this many claims, one batch of
// expired rows is deleted.
const (
	remoteAgentReplayCleanupEvery = 64
	remoteAgentReplayCleanupBatch = 1000
)

// Claim implements remoteagent.ReplayStore: true when (issuer, jti) was not
// recorded before, or was recorded for the same presenter.
func (r *RemoteAgentReplays) Claim(ctx context.Context, issuer, jti, presenter string, expiresAt time.Time) (bool, error) {
	if r.claims.Add(1)%remoteAgentReplayCleanupEvery == 0 {
		if _, err := r.pool.Exec(ctx, `DELETE FROM elitea_identity.remote_agent_assertion_replays
WHERE ctid IN (SELECT ctid FROM elitea_identity.remote_agent_assertion_replays
               WHERE expires_at < now() LIMIT $1)`, remoteAgentReplayCleanupBatch); err != nil {
			return false, fmt.Errorf("remote agent replays: clean up: %w", err)
		}
	}
	// A row comes back for a new pair and for the presenter that holds it;
	// none for another presenter.
	var held int
	err := r.pool.QueryRow(ctx, `INSERT INTO elitea_identity.remote_agent_assertion_replays AS r (issuer, jti, presenter, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (issuer, jti) DO UPDATE SET seen_at = clock_timestamp()
WHERE r.presenter = EXCLUDED.presenter
RETURNING 1`, issuer, jti, presenter, expiresAt.UTC()).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remote agent replays: record: %w", err)
	}
	return true, nil
}

// RemoteAgentAudit appends the security events of delegation assertions.
type RemoteAgentAudit struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewRemoteAgentAudit builds the appender over the main pool.
func NewRemoteAgentAudit(pool *pgxpool.Pool, audit securityaudit.Appender) (*RemoteAgentAudit, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("remote agent audit: a pool and an appender are required")
	}
	return &RemoteAgentAudit{pool: pool, audit: audit}, nil
}

// RecordRemoteAgent appends one event in its own transaction.
func (r *RemoteAgentAudit) RecordRemoteAgent(ctx context.Context, event securityaudit.SecurityEvent) error {
	if event.Type != securityaudit.TypeRemoteAgent {
		return fmt.Errorf("remote agent audit: event type %q", event.Type)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("remote agent audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := r.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("remote agent audit: append: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("remote agent audit: commit: %w", err)
	}
	return nil
}
