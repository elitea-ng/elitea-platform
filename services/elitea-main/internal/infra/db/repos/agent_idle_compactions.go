package repos

// Idle compaction per (conversation, agent) in the cloud (ADR-0050 decision
// 9b2, issue 1610; shared 0230). This file owns every statement on
// elitea_runtime.agent_idle_compactions:
//
//   - ArmIdleCompaction: the after-settle arm of an agent's answered turn;
//   - ClaimIdleCompactions / SkipIdleCompaction / DeferIdleCompaction: the
//     admitter's store (agentexecution.IdleCompactionAdmitter);
//   - admitCurrentIdleCompaction / cancelParticipantIdleCompactions: the two
//     steps inside the agent admission transaction;
//   - isIdleCompactionRun / projectIdleCompactionContext /
//     settleIdleCompactionRun: the quiet projections of an idle run;
//   - LatestAgentContexts: the per-agent context meter read model.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/contextsettings"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	interruptdomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/idlecompaction"
)

// AgentIdleCompactionsRepository is the PostgreSQL store of idle checks.
type AgentIdleCompactionsRepository struct {
	pool *pgxpool.Pool
	now  func() time.Time

	retireMu   sync.Mutex
	lastRetire time.Time
}

// NewAgentIdleCompactionsRepository builds the repository.
func NewAgentIdleCompactionsRepository(pool *pgxpool.Pool) (*AgentIdleCompactionsRepository, error) {
	if pool == nil {
		return nil, errors.New("idle compaction repository requires a pool")
	}
	return &AgentIdleCompactionsRepository{pool: pool, now: time.Now}, nil
}

var (
	_ agentexecutionapp.IdleCompactionStore    = (*AgentIdleCompactionsRepository)(nil)
	_ agentexecutionapp.IdleCompactionArmStore = (*AgentIdleCompactionsRepository)(nil)
)

// nonTerminalJobStates are the execution_jobs states of a run that can still
// run or settle.
const nonTerminalJobStates = `('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`

// idleRunExclusion: the binding (alias binding) is not an idle run.
const idleRunExclusion = `NOT EXISTS (SELECT 1 FROM elitea_runtime.agent_idle_compactions AS idle_run
                  WHERE idle_run.idle_execution_id = binding.execution_id)`

// participantLatestSQL is the participant's newest execution in the
// conversation, idle runs excluded, and whether a run is not terminal: the
// participant's own turn ($5 false, the arm), or the runs that can hold the
// participant's worker session ($5 true, the sweep and the admission).
//
// The worker keys an agent's durable session by the frozen request's thread
// id (with the actor and the project). With $6 true the thread is the
// conversation's (a single-agent conversation, an ad hoc chat, a session
// from before per-participant threads): two participants that are the same
// agent version, or two ad hoc chats, asked by the same person share a
// session writer, so ANY agent run of the conversation counts, and an idle
// claim would otherwise take the session from the live turn
// (WriterNotCurrent). With $6 false the participant has its own thread (issue
// 1869: IsParticipantSessionThread), so only the participant's own runs
// count, idle runs included, and a peer's turn neither delays nor skips the
// check. A peer's running turn on a shared session defers the check to the
// next sweep; it never cancels it, and it never stops the agent's own
// settled turn from arming one. $1 conversation uuid text, $2 participant,
// $3 projection project, $4 an execution to leave out (” for none), $5 the
// sweep/admission form, $6 the session is conversation-wide. Index
// agent_execution_jobs_current_correlation_idx leads with client_stream_id.
const participantLatestSQL = `
SELECT COALESCE((
    SELECT binding.execution_id
    FROM elitea_runtime.agent_execution_jobs AS binding
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = binding.execution_id AND job.generation = binding.generation
    WHERE binding.client_stream_id = $1 AND binding.participant_id = $2
      AND job.projection_project_id = $3 AND binding.execution_id <> $4
      AND ` + idleRunExclusion + `
    ORDER BY job.admitted_at DESC, binding.generation DESC, binding.execution_id DESC
    LIMIT 1), ''),
       EXISTS (
    SELECT 1
    FROM elitea_runtime.agent_execution_jobs AS binding
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = binding.execution_id AND job.generation = binding.generation
    WHERE binding.client_stream_id = $1
      AND (($5::boolean AND $6::boolean) OR binding.participant_id = $2)
      AND job.projection_project_id = $3 AND binding.execution_id <> $4
      AND job.state IN ` + nonTerminalJobStates + `
      AND ($5::boolean OR ` + idleRunExclusion + `))`

/* ── arm ─────────────────────────────────────────────────────────────────── */

// idleArmSource is what the arm reads of a settled execution.
type idleArmSource struct {
	projectID           int64
	participantID       int64
	conversationUUID    string
	responseMessageID   string
	executionGeneration string
	frozenInput         []byte
	admittedAt          time.Time
}

// ArmIdleCompaction arms the idle check of an agent's settled turn, in one
// transaction: it reads the execution's job, participant and answer, asks
// arm (idlecompaction.Arm) whether and when the check is due, and on yes
// ends the participant's earlier QUEUED check (CANCELLED,
// superseded_by_agent_turn) and writes a QUEUED one. It writes nothing for
// an execution that is not a root chat turn with a participant, is itself an
// idle run, is not the participant's latest turn, or runs on a handed-off
// thread, and nothing when arm says no. It reports whether a row was
// written. Idempotent: the key is idle-compaction/{execution}.
func (r *AgentIdleCompactionsRepository) ArmIdleCompaction(
	ctx context.Context, executionID string, generation uint64,
	arm func(idlecompaction.Turn) (time.Time, idlecompaction.SkipCode),
) (bool, error) {
	if r == nil || r.pool == nil || arm == nil || !interruptdomain.ValidExecutionID(executionID) ||
		generation == 0 || generation > math.MaxInt64 {
		return false, nil
	}
	armed := false
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		armed = false
		source, found, err := readIdleArmSource(ctx, tx, executionID, int64(generation))
		if err != nil || !found {
			return err
		}
		schema, err := tenantSchema(strconv.FormatInt(source.projectID, 10))
		if err != nil {
			return nil
		}
		// The conversation lock in share mode: the same participant's next
		// turn locks it for update in its admission, so either this arm
		// commits first and that turn cancels the row, or the turn commits
		// first and the latest-execution check below sees it.
		var conversationID int64
		err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT id FROM %s.chat_conversations WHERE uuid = $1::uuid FOR SHARE`, schema),
			source.conversationUUID).Scan(&conversationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("idle compaction: lock the conversation: %w", err)
		}
		var latest string
		var busy bool
		if err := tx.QueryRow(ctx, participantLatestSQL, source.conversationUUID, source.participantID,
			source.projectID, "", false, false).Scan(&latest, &busy); err != nil {
			return fmt.Errorf("idle compaction: read the agent's latest turn: %w", err)
		}
		if latest != executionID || busy {
			return nil
		}
		// A handed-off thread's session belongs to its executor's turn
		// cycle (ADR-0034); the desktop compacts a Local work thread itself.
		handedOff, _, err := handOffOwner(ctx, tx, source.projectID, source.conversationUUID)
		if err != nil {
			return err
		}
		if handedOff {
			return nil
		}
		facts, err := readResponseFacts(ctx, tx, source.projectID, source.responseMessageID, executionID)
		if err != nil {
			return err
		}
		tokens, started, err := readIdleArmMeasurement(ctx, tx, schema, conversationID, source, executionID)
		if err != nil {
			return err
		}
		var input runtimev1.AgentExecutionInputV1
		promptCache := proto.Unmarshal(source.frozenInput, &input) == nil &&
			agentexecutionapp.IdleCompactionPromptCache(&input)
		due, skip := arm(idlecompaction.Turn{
			Succeeded:      !facts.IsError && !facts.Paused,
			PromptCache:    promptCache,
			MessageTokens:  tokens,
			RequestStarted: started,
		})
		if skip != idlecompaction.SkipNone {
			return nil
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET state = 'CANCELLED', skip_code = $4, lease_until = NULL, updated_at = clock_timestamp()
WHERE project_id = $1 AND conversation_uuid = $2::uuid AND participant_id = $3 AND state = 'QUEUED'
  AND source_execution_id <> $5`, source.projectID, source.conversationUUID, source.participantID,
			string(idlecompaction.SkipSuperseded), executionID); err != nil {
			return fmt.Errorf("idle compaction: end the earlier check: %w", err)
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_idle_compactions (
    project_id, conversation_uuid, participant_id, source_execution_id, source_generation,
    request_started_at, cache_ttl_seconds, due_at, idempotency_key, state)
VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, 'QUEUED')
ON CONFLICT DO NOTHING`, source.projectID, source.conversationUUID, source.participantID, executionID,
			int64(generation), started, int64(idlecompaction.CacheTTL/time.Second), due,
			agentexecutionapp.IdleCompactionIdempotencyKey(executionID))
		if err != nil {
			return fmt.Errorf("idle compaction: queue the check: %w", err)
		}
		armed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return armed, nil
}

// readIdleArmSource reads a settled root chat run with a participant that is
// not itself an idle run.
func readIdleArmSource(ctx context.Context, tx pgx.Tx, executionID string, generation int64) (idleArmSource, bool, error) {
	var source idleArmSource
	var participant *int32
	var project int32
	err := tx.QueryRow(ctx, `
SELECT job.projection_project_id, binding.participant_id, binding.client_stream_id, binding.client_message_id,
       binding.client_execution_generation, entry.content_bytes, job.admitted_at
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
 AND binding.input_bundle_id = job.input_bundle_id
JOIN elitea_runtime.input_bundle_entries AS entry
  ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
 AND entry.semantic_role = '`+executiondomain.AgentExecutionRequestRole+`'
WHERE job.execution_id = $1 AND job.generation = $2 AND job.state = 'SUCCEEDED'
  AND job.capability_id IN ('agent.execute.application.v1', 'agent.execute.adhoc.v1')
  AND job.resource_project_id = job.projection_project_id
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.agent_idle_compactions AS idle_run
                  WHERE idle_run.idle_execution_id = job.execution_id)`, executionID, generation).Scan(
		&project, &participant, &source.conversationUUID, &source.responseMessageID,
		&source.executionGeneration, &source.frozenInput, &source.admittedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return idleArmSource{}, false, nil
	}
	if err != nil {
		return idleArmSource{}, false, fmt.Errorf("idle compaction: read the settled run: %w", err)
	}
	if participant == nil || *participant <= 0 || project <= 0 {
		return idleArmSource{}, false, nil
	}
	source.projectID, source.participantID = int64(project), int64(*participant)
	return source, true, nil
}

// readIdleArmMeasurement reads the answer's last request measurement: its
// provider input tokens when the provider reported them, else the runtime's
// estimate, and the start of that request. Both must be this execution's:
// the measurement names it, and the start is not older than its admission
// (an older one belongs to a run this execution continued). Zero values when
// the answer has none.
func readIdleArmMeasurement(ctx context.Context, tx pgx.Tx, schema string, conversationID int64, source idleArmSource, executionID string) (uint64, time.Time, error) {
	var raw []byte
	var started *time.Time
	err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT response.meta -> 'runtime_context' -> 'measurement',
       CASE WHEN jsonb_typeof(response.meta -> 'runtime_request_started_at') = 'string'
            THEN (response.meta ->> 'runtime_request_started_at')::timestamptz END
FROM %s.chat_message_group AS response
WHERE response.uuid = $1::uuid AND response.conversation_id = $2 AND response.task_id = $3
  AND response.meta ->> 'execution_generation' = $4
  AND response.meta -> 'runtime_context' ->> 'execution_id' = $3`, schema),
		source.responseMessageID, conversationID, executionID, source.executionGeneration).Scan(&raw, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("idle compaction: read the answer's measurement: %w", err)
	}
	var tokens uint64
	if measurement, decodeErr := contextsettings.DecodeMeasurement(raw); decodeErr == nil {
		tokens = measurement.EstimatedInputTokens
		if usage := measurement.ProviderUsage; usage != nil && usage.InputTokens != nil {
			tokens = uint64(*usage.InputTokens)
		}
	}
	if started == nil || started.Before(source.admittedAt) {
		return tokens, time.Time{}, nil
	}
	return tokens, started.UTC(), nil
}

// SettleIdleCompactionRun ends the row of an idle run when its execution
// settled: DONE on success, SKIPPED idle_run_failed or idle_run_cancelled
// otherwise. Only an ADMITTED row moves (the projection may have ended it
// already, or the agent's next turn cancelled it), so it is idempotent and a
// no-op for every other execution.
func (r *AgentIdleCompactionsRepository) SettleIdleCompactionRun(ctx context.Context, executionID string, outcome executionapp.SettlementOutcome) error {
	if r == nil || r.pool == nil || !interruptdomain.ValidExecutionID(executionID) {
		return nil
	}
	return settleIdleCompactionRun(ctx, pgxExecutor{queryer: r.pool}, executionID, idleRunEnd(outcome))
}

// idleRunEnd maps a settled outcome to the row's end.
func idleRunEnd(outcome executionapp.SettlementOutcome) string {
	switch outcome {
	case executionapp.SettlementSucceeded:
		return ""
	case executionapp.SettlementCancelled:
		return agentexecutionapp.IdleCompactionSkipRunCancelled
	default:
		return agentexecutionapp.IdleCompactionSkipRunFailed
	}
}

// settleIdleCompactionRun moves an ADMITTED row of the idle execution to DONE
// (skipCode "") or SKIPPED with skipCode.
func settleIdleCompactionRun(ctx context.Context, tx sqlExecutor, executionID, skipCode string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET state = CASE WHEN $2::text = '' THEN 'DONE' ELSE 'SKIPPED' END,
    skip_code = NULLIF($2::text, ''), lease_until = NULL, updated_at = clock_timestamp()
WHERE idle_execution_id = $1 AND state = 'ADMITTED'`, executionID, skipCode); err != nil {
		return fmt.Errorf("idle compaction: end the idle run's check: %w", err)
	}
	return nil
}

/* ── admitter store ──────────────────────────────────────────────────────── */

type claimedIdleCompaction struct {
	key              string
	projectID        int64
	conversationUUID string
	participantID    int64
	sourceExecution  string
	sourceGeneration int64
	requestStarted   time.Time
	cacheTTLSeconds  int64
	attempts         int
	now              time.Time
}

// ClaimIdleCompactions claims at most limit due QUEUED rows, oldest due
// first. A row another admitter holds is skipped (FOR UPDATE SKIP LOCKED),
// and a claimed row is skipped by everyone until its lease ends. A row whose
// attempts exceed maxAttempts ends SKIPPED (admission_exhausted); a row whose
// source can never be admitted ends SKIPPED (source_input_invalid). Each
// pass also retires ADMITTED rows whose idle job is terminal (a lost
// after-settle hook), at most once a minute on this replica.
func (r *AgentIdleCompactionsRepository) ClaimIdleCompactions(ctx context.Context, limit int, lease time.Duration, maxAttempts int) ([]agentexecutionapp.IdleCompactionTarget, error) {
	if r == nil || r.pool == nil || limit < 1 || limit > agentexecutionapp.IdleCompactionSweepBatch ||
		lease <= 0 || maxAttempts < 1 || maxAttempts > 1000 {
		return nil, errors.New("idle compaction claim is invalid")
	}
	if err := r.retireSettledIdleRuns(ctx, limit); err != nil {
		return nil, err
	}
	var claimed []claimedIdleCompaction
	rows, err := r.pool.Query(ctx, `
WITH candidates AS (
  SELECT id FROM elitea_runtime.agent_idle_compactions
  WHERE state = 'QUEUED' AND due_at <= clock_timestamp()
    AND (lease_until IS NULL OR lease_until <= clock_timestamp())
  ORDER BY due_at, id
  LIMIT $1
  FOR UPDATE SKIP LOCKED
)
UPDATE elitea_runtime.agent_idle_compactions AS row
SET admission_attempts = LEAST(row.admission_attempts + 1, 1000),
    lease_until = clock_timestamp() + make_interval(secs => $2::double precision),
    updated_at = clock_timestamp()
FROM candidates
WHERE row.id = candidates.id
RETURNING row.idempotency_key, row.project_id, row.conversation_uuid::text, row.participant_id,
          row.source_execution_id, row.source_generation, row.request_started_at, row.cache_ttl_seconds,
          row.admission_attempts, clock_timestamp()`, limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim idle compactions: %w", err)
	}
	for rows.Next() {
		var row claimedIdleCompaction
		var participant int32
		var ttl int32
		if err := rows.Scan(&row.key, &row.projectID, &row.conversationUUID, &participant, &row.sourceExecution,
			&row.sourceGeneration, &row.requestStarted, &ttl, &row.attempts, &row.now); err != nil {
			rows.Close()
			return nil, fmt.Errorf("claim idle compactions: %w", err)
		}
		row.participantID, row.cacheTTLSeconds = int64(participant), int64(ttl)
		claimed = append(claimed, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim idle compactions: %w", err)
	}
	targets := make([]agentexecutionapp.IdleCompactionTarget, 0, len(claimed))
	for _, row := range claimed {
		turn := agentexecutionapp.CurrentIdleCompactionTurn{SourceExecutionID: row.sourceExecution}
		refused := ""
		var target agentexecutionapp.IdleCompactionTarget
		if row.attempts > maxAttempts {
			refused = agentexecutionapp.IdleCompactionSkipExhausted
		} else if target, refused, err = r.loadIdleCompactionTarget(ctx, row); err != nil {
			return nil, err
		}
		if refused != "" {
			if err := r.SkipIdleCompaction(ctx, turn, refused); err != nil {
				return nil, err
			}
			continue
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// retireSettledIdleRuns ends ADMITTED rows whose idle job reached a
// terminal state without its projection or its settle hook ending the row.
// QUARANTINED is terminal here: nothing moves a job out of it (claims.go
// answers retry-later), so its row is ended like a FAILED one and the
// participant's check slot is freed.
//
// It is a backstop for a lost after-settle hook, so it runs at most once per
// retireInterval on each replica, not on every sweep pass.
func (r *AgentIdleCompactionsRepository) retireSettledIdleRuns(ctx context.Context, limit int) error {
	if !r.retireDue() {
		return nil
	}
	if _, err := r.pool.Exec(ctx, `
WITH settled AS (
  SELECT row.id, job.state
  FROM elitea_runtime.agent_idle_compactions AS row
  JOIN elitea_runtime.execution_jobs AS job ON job.execution_id = row.idle_execution_id
  WHERE row.state = 'ADMITTED' AND job.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED')
  ORDER BY row.id
  LIMIT $1
  FOR UPDATE OF row SKIP LOCKED
)
UPDATE elitea_runtime.agent_idle_compactions AS row
SET state = CASE WHEN settled.state = 'SUCCEEDED' THEN 'DONE' ELSE 'SKIPPED' END,
    skip_code = CASE settled.state WHEN 'SUCCEEDED' THEN NULL
                                   WHEN 'CANCELLED' THEN $2 ELSE $3 END,
    lease_until = NULL, updated_at = clock_timestamp()
FROM settled
WHERE row.id = settled.id`, limit, agentexecutionapp.IdleCompactionSkipRunCancelled,
		agentexecutionapp.IdleCompactionSkipRunFailed); err != nil {
		r.retireFailed()
		return fmt.Errorf("retire settled idle runs: %w", err)
	}
	return nil
}

// retireInterval is the lost-hook backstop's period on one replica.
const retireInterval = time.Minute

// retireDue reports whether this replica's backstop is due, and if so marks
// it run now.
func (r *AgentIdleCompactionsRepository) retireDue() bool {
	r.retireMu.Lock()
	defer r.retireMu.Unlock()
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	if !r.lastRetire.IsZero() && now.Sub(r.lastRetire) < retireInterval {
		return false
	}
	r.lastRetire = now
	return true
}

// retireFailed lets the next pass retry a backstop that failed.
func (r *AgentIdleCompactionsRepository) retireFailed() {
	r.retireMu.Lock()
	r.lastRetire = time.Time{}
	r.retireMu.Unlock()
}

// loadIdleCompactionTarget reads the source's job, binding and exact
// request entry and the participant's latest execution. It returns a skip
// code instead of a target when the row can never run.
func (r *AgentIdleCompactionsRepository) loadIdleCompactionTarget(ctx context.Context, row claimedIdleCompaction) (agentexecutionapp.IdleCompactionTarget, string, error) {
	var target agentexecutionapp.IdleCompactionTarget
	var triggerOrigin, threadID string
	var resourceProject, projectionProject int32
	var applicationID, releaseID, releaseVersionID *int64
	var releaseDigest []byte
	// The source's release (shared 0198) with its version row and digest:
	// the idle run binds the agent identity the source bound (issue 1776).
	err := r.pool.QueryRow(ctx, `
SELECT job.tenant_id, job.resource_project_id, job.projection_project_id, job.actor_id, job.capability_id,
       job.trigger_origin, binding.client_stream_id, binding.client_message_id, binding.client_execution_generation,
       binding.application_id, entry.content_bytes, entry.content_digest,
       binding.release_id, release.application_version_id, release.definition_digest
FROM elitea_runtime.execution_jobs AS job
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
 AND binding.input_bundle_id = job.input_bundle_id
JOIN elitea_runtime.input_bundle_entries AS entry
  ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
 AND entry.semantic_role = '`+executiondomain.AgentExecutionRequestRole+`'
LEFT JOIN elitea_lifecycle.application_releases AS release
  ON release.id = binding.release_id AND release.tenant_project_id = job.resource_project_id
 AND release.application_id = binding.application_id
WHERE job.execution_id = $1 AND job.generation = $2
  AND job.resource_project_id = $3 AND job.projection_project_id = $3
  AND binding.client_stream_id = $4 AND binding.participant_id = $5`,
		row.sourceExecution, row.sourceGeneration, row.projectID, row.conversationUUID, row.participantID).Scan(
		&target.TenantID, &resourceProject, &projectionProject, &target.ActorID, &target.CapabilityID,
		&triggerOrigin, &target.Turn.ConversationUUID, &target.Turn.ResponseMessageID,
		&target.Turn.ExecutionGeneration, &applicationID, &target.FrozenInput, &target.FrozenInputDigest,
		&releaseID, &releaseVersionID, &releaseDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	if err != nil {
		return target, "", fmt.Errorf("load idle compaction source: %w", err)
	}
	actorID, parseErr := strconv.ParseInt(target.ActorID, 10, 64)
	if parseErr != nil || actorID <= 0 || actorID > math.MaxInt32 {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	application := int64(0)
	if applicationID != nil {
		application = *applicationID
	}
	if (target.CapabilityID == executiondomain.AgentApplicationCapability) != (application > 0) ||
		(target.CapabilityID != executiondomain.AgentApplicationCapability && target.CapabilityID != executiondomain.AgentAdhocCapability) {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	versionID, ok := idleSourceVersion(target.FrozenInput, application)
	if !ok {
		// Every admission path freezes application.version_id (start.go
		// writes it; continuations and the governed freeze keep the frozen
		// map), so this is a source written some other way. Say so: the
		// row is skipped once and idle compaction stops for that agent
		// (#1919).
		slog.WarnContext(ctx, "idle compaction: the source's frozen application names no version of its agent; skipped",
			"source_execution_id", row.sourceExecution, "project_id", row.projectID, "application_id", application,
			"skip_code", agentexecutionapp.IdleCompactionSkipSourceInvalid)
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	if releaseID != nil {
		// A release whose row is gone, or whose digest is malformed, is a
		// source the idle run cannot bind the same identity for.
		if releaseVersionID == nil || *releaseVersionID <= 0 || len(releaseDigest) != sha256.Size || application == 0 {
			return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
		}
		target.Turn.Release = &agentexecutionapp.ReleaseRun{
			ID: *releaseID, ApplicationVersionID: *releaseVersionID, DefinitionDigest: [32]byte(releaseDigest),
		}
	}
	target.Turn.ApplicationVersionID = versionID
	// The thread the worker's session lives under: the one the answer
	// recorded (as a fan-out wake reads it), else the frozen request's. A
	// source with neither has no session to compact.
	schema, err := tenantSchema(strconv.FormatInt(row.projectID, 10))
	if err != nil {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(response.meta ->> 'thread_id', '')
FROM %[1]s.chat_message_group AS response
JOIN %[1]s.chat_conversations AS conversation ON conversation.id = response.conversation_id
WHERE response.uuid = $1::uuid AND conversation.uuid = $2::uuid`, schema),
		target.Turn.ResponseMessageID, row.conversationUUID).Scan(&threadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	if err != nil {
		return target, "", fmt.Errorf("load idle compaction answer: %w", err)
	}
	if threadID == "" {
		var input runtimev1.AgentExecutionInputV1
		if proto.Unmarshal(target.FrozenInput, &input) == nil {
			threadID = input.GetThreadId()
		}
	}
	if threadID == "" {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	if err := r.pool.QueryRow(ctx, participantLatestSQL, row.conversationUUID, row.participantID, row.projectID, "", true, !agentexecutionapp.IsParticipantSessionThread(threadID, row.conversationUUID)).
		Scan(&target.LatestExecution, &target.AgentBusy); err != nil {
		return target, "", fmt.Errorf("load idle compaction agent turns: %w", err)
	}
	target.ResourceProjectID = strconv.FormatInt(int64(resourceProject), 10)
	target.ProjectionProjectID = strconv.FormatInt(int64(projectionProject), 10)
	target.TriggerOrigin = executiondomain.TriggerOrigin(triggerOrigin)
	target.RequestStarted = row.requestStarted.UTC()
	target.CacheTTL = time.Duration(row.cacheTTLSeconds) * time.Second
	target.Now = row.now
	target.Turn.ProjectID, target.Turn.ActorUserID = row.projectID, actorID
	target.Turn.ParticipantID, target.Turn.ApplicationID = row.participantID, application
	target.Turn.SourceExecutionID, target.Turn.SourceGeneration = row.sourceExecution, row.sourceGeneration
	target.Turn.ThreadID = threadID
	if target.Turn.Validate() != nil {
		return target, agentexecutionapp.IdleCompactionSkipSourceInvalid, nil
	}
	return target, "", nil
}

// idleSourceVersion reads the agent version the source ran from its frozen
// request (application.id and application.version_id, as start.go freezes
// them). An ad hoc source (application 0) has none; an agent source whose
// frozen application does not name a positive version of the same agent is
// not a source an idle run can bind (false).
func idleSourceVersion(frozen []byte, application int64) (int64, bool) {
	if application == 0 {
		return 0, true
	}
	var input runtimev1.AgentExecutionInputV1
	if proto.Unmarshal(frozen, &input) != nil {
		return 0, false
	}
	var app struct {
		ID        int64 `json:"id"`
		VersionID int64 `json:"version_id"`
	}
	if json.Unmarshal(input.GetApplication(), &app) != nil || app.ID != application ||
		app.VersionID <= 0 || app.VersionID > math.MaxInt32 {
		return 0, false
	}
	return app.VersionID, true
}

// SkipIdleCompaction ends a QUEUED row SKIPPED with code. A row that is no
// longer QUEUED is left as it is.
func (r *AgentIdleCompactionsRepository) SkipIdleCompaction(ctx context.Context, turn agentexecutionapp.CurrentIdleCompactionTurn, code string) error {
	if r == nil || r.pool == nil || !interruptdomain.ValidExecutionID(turn.SourceExecutionID) || code == "" {
		return errors.New("idle compaction skip is invalid")
	}
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET state = 'SKIPPED', skip_code = $2, lease_until = NULL, updated_at = clock_timestamp()
WHERE idempotency_key = $1 AND state = 'QUEUED'`,
		agentexecutionapp.IdleCompactionIdempotencyKey(turn.SourceExecutionID), code); err != nil {
		return fmt.Errorf("skip idle compaction: %w", err)
	}
	return nil
}

// DeferIdleCompaction keeps a capacity-refused row QUEUED and gives back the
// attempt its claim took; the lease still spaces the retries.
func (r *AgentIdleCompactionsRepository) DeferIdleCompaction(ctx context.Context, turn agentexecutionapp.CurrentIdleCompactionTurn) error {
	if r == nil || r.pool == nil || !interruptdomain.ValidExecutionID(turn.SourceExecutionID) {
		return errors.New("idle compaction deferral is invalid")
	}
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET admission_attempts = GREATEST(admission_attempts - 1, 1)
WHERE idempotency_key = $1 AND state = 'QUEUED'`,
		agentexecutionapp.IdleCompactionIdempotencyKey(turn.SourceExecutionID)); err != nil {
		return fmt.Errorf("defer idle compaction: %w", err)
	}
	return nil
}

/* ── inside the agent admission transaction ──────────────────────────────── */

// admitCurrentIdleCompaction runs inside the agent admission transaction,
// after the conversation lock and the job insert, before the command outbox
// row. It locks the row, rechecks that it is still QUEUED, that the source is
// still the participant's latest turn and no turn of it runs, and that the
// source's actor may still write to the conversation; then it moves the row
// to ADMITTED with the job's id. It writes nothing on the transcript.
func admitCurrentIdleCompaction(ctx context.Context, tx sqlExecutor, executionID string, turn agentexecutionapp.CurrentIdleCompactionTurn) error {
	var state, source string
	err := tx.QueryRow(ctx, `
SELECT state, source_execution_id FROM elitea_runtime.agent_idle_compactions
WHERE idempotency_key = $1 AND project_id = $2 AND conversation_uuid = $3::uuid AND participant_id = $4
FOR UPDATE`, agentexecutionapp.IdleCompactionIdempotencyKey(turn.SourceExecutionID), turn.ProjectID,
		turn.ConversationUUID, turn.ParticipantID).Scan(&state, &source)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && state != "QUEUED") {
		return agentexecutionapp.ErrIdleCompactionNotQueued
	}
	if err != nil {
		return fmt.Errorf("lock idle compaction: %w", err)
	}
	var latest string
	var busy bool
	if err := tx.QueryRow(ctx, participantLatestSQL, turn.ConversationUUID, turn.ParticipantID, turn.ProjectID,
		executionID, true, !agentexecutionapp.IsParticipantSessionThread(turn.ThreadID, turn.ConversationUUID)).Scan(&latest, &busy); err != nil {
		return fmt.Errorf("recheck idle compaction agent turns: %w", err)
	}
	if latest != turn.SourceExecutionID {
		return agentexecutionapp.ErrIdleCompactionSuperseded
	}
	if busy {
		return agentexecutionapp.ErrIdleCompactionAgentBusy
	}
	// Authorization inside the effect transaction, after the locks: the
	// permission an ordinary chat turn of the actor needs (issue 1776).
	if err := checkInterruptPermission(ctx, tx, turn.ProjectID, turn.ActorUserID, agentexecutionapp.ChatTurnPermission); err != nil {
		if errors.Is(err, interruptdomain.ErrNotAllowed) {
			return agentexecutionapp.ErrIdleCompactionActorNotAllowed
		}
		return fmt.Errorf("recheck idle compaction actor: %w", err)
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET state = 'ADMITTED', idle_execution_id = $2, lease_until = NULL, updated_at = clock_timestamp()
WHERE idempotency_key = $1 AND state = 'QUEUED'`,
		agentexecutionapp.IdleCompactionIdempotencyKey(turn.SourceExecutionID), executionID)
	if err != nil {
		return fmt.Errorf("admit idle compaction: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return agentexecutionapp.ErrIdleCompactionNotQueued
	}
	// The run's own durable marker (shared 0240, #1921): the row above
	// CASCADE-deletes with the source's job, and the analytics must still
	// read the run's calls as an idle compaction's after that.
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs SET idle_compaction = true
WHERE execution_id = $1 AND NOT idle_compaction`, executionID); err != nil {
		return fmt.Errorf("mark the idle compaction run: %w", err)
	}
	return nil
}

// cancelParticipantIdleCompactions runs in the admission transaction of a
// turn that has a participant (a new question, an ad hoc chat, a regenerate
// or a continuation), after the conversation lock: the participant's QUEUED
// check ends CANCELLED, and a running idle run of it is asked to stop
// (desired_state CANCELLED, as a Stop does) and its row ends CANCELLED. Only
// the same participant's turn reaches here: a peer agent's or a person's
// message never touches the row.
func cancelParticipantIdleCompactions(ctx context.Context, tx sqlExecutor, projectID int64, conversationUUID string, participantID int64) error {
	if _, err := tx.Exec(ctx, `
WITH ended AS (
  UPDATE elitea_runtime.agent_idle_compactions
  SET state = 'CANCELLED', skip_code = $4, lease_until = NULL, updated_at = clock_timestamp()
  WHERE project_id = $1 AND conversation_uuid = $2::uuid AND participant_id = $3
    AND state IN ('QUEUED', 'ADMITTED')
  RETURNING idle_execution_id
)
UPDATE elitea_runtime.execution_jobs AS job
SET desired_state = 'CANCELLED'
FROM ended
WHERE job.execution_id = ended.idle_execution_id
  AND job.desired_state IN ('RUNNING', 'SUSPENDED')
  AND job.state IN `+nonTerminalJobStates, projectID, conversationUUID, participantID,
		string(idlecompaction.SkipSuperseded)); err != nil {
		return fmt.Errorf("cancel the agent's idle compaction: %w", err)
	}
	return nil
}

/* ── quiet projections of an idle run ────────────────────────────────────── */

// idleRunDetector tells an idle run from an ordinary turn inside a
// projection transaction.
type idleRunDetector interface {
	isIdleRun(ctx context.Context, tx sqlExecutor, executionID string) (bool, error)
}

type noopIdleRunDetector struct{}

func (noopIdleRunDetector) isIdleRun(context.Context, sqlExecutor, string) (bool, error) {
	return false, nil
}

type postgresIdleRunDetector struct{}

func (postgresIdleRunDetector) isIdleRun(ctx context.Context, tx sqlExecutor, executionID string) (bool, error) {
	return isIdleCompactionRun(ctx, tx, executionID)
}

// isIdleCompactionRun reports whether the execution is the idle run of a
// check (index agent_idle_compactions_idle_execution).
func isIdleCompactionRun(ctx context.Context, tx sqlExecutor, executionID string) (bool, error) {
	var idle bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.agent_idle_compactions WHERE idle_execution_id = $1)`,
		executionID).Scan(&idle); err != nil {
		return false, fmt.Errorf("load idle compaction binding: %w", err)
	}
	return idle, nil
}

// projectIdleCompactionContext stores the root agent's measurement of an
// idle run on its row (result_context), in the encoding agent_context.go
// writes on an answer. Every other frame of an idle run has no transcript
// projection: it is in the replay log only.
func projectIdleCompactionContext(ctx context.Context, tx sqlExecutor, projectID int64, frame outputapp.NodeEventFrame) error {
	event, measurement, ok, err := decodeRootAgentContextFrame(frame)
	if err != nil || !ok {
		return err
	}
	projectDatabaseID, valid := currentAgentDatabaseID(projectID)
	if !valid {
		return outputapp.ErrInvalidNodeEventOutput
	}
	binding, bound, err := loadCurrentAgentTraceBinding(ctx, tx, frame.Fence.ExecutionID, frame.Fence.Generation, projectDatabaseID)
	if err != nil {
		return err
	}
	if !bound || event.StreamID != binding.streamID || event.MessageID != binding.messageID ||
		event.ExecutionGeneration != binding.executionGeneration || event.SIOEvent != binding.sioEvent {
		return errors.New("idle compaction context conflicts with immutable admission")
	}
	raw, err := json.Marshal(contextsettings.RuntimeContext{
		Measurement: measurement, ExecutionID: frame.Fence.ExecutionID,
		Generation: frame.Fence.Generation, ExecutionGeneration: binding.executionGeneration,
		ResponseMessageID: binding.messageID,
	})
	if err != nil {
		return fmt.Errorf("encode idle compaction context: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_idle_compactions
SET result_context = $2::jsonb || jsonb_build_object('recorded_at', CURRENT_TIMESTAMP), updated_at = clock_timestamp()
WHERE idle_execution_id = $1 AND state IN ('ADMITTED', 'DONE')`, frame.Fence.ExecutionID, raw); err != nil {
		return fmt.Errorf("persist idle compaction context: %w", err)
	}
	return nil
}

/* ── the per-agent meter ─────────────────────────────────────────────────── */

// AgentContextRow is one agent participant's latest context measurement in a
// conversation: the runtime_context of its latest answer (with `active`), or
// the result_context of its latest DONE idle compaction when that compacted
// this very answer (Idle).
type AgentContextRow struct {
	ParticipantID  int64
	RuntimeContext json.RawMessage
	Idle           bool
}

// LatestAgentContexts returns, per agent participant of the conversation
// that has a measurement, the newer of its latest answer's runtime_context
// (bound to the answer's task_id and execution generation, as
// GetContextState reads the conversation's) and its latest DONE idle
// result_context. An idle result is the newer one exactly when its source
// is that latest answer's execution and the answer is not streaming: any
// later turn of the agent writes a later answer. The answer's agent is its
// job's participant_id, else its author_participant_id. Rows are in
// participant order. An unknown conversation answers no rows.
func (r *AgentIdleCompactionsRepository) LatestAgentContexts(ctx context.Context, projectID int64, conversationUUID string) ([]AgentContextRow, error) {
	if r == nil || r.pool == nil || projectID <= 0 || !validConversationUUID(conversationUUID) {
		return nil, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
WITH conversation AS (
    SELECT id FROM %[1]s.chat_conversations WHERE uuid = $1::uuid
), agents AS (
    SELECT DISTINCT participant.id
    FROM %[1]s.chat_participant_mapping AS mapping
    JOIN %[1]s.chat_participants AS participant ON participant.id = mapping.participant_id
    JOIN conversation ON conversation.id = mapping.conversation_id
    WHERE participant.entity_name <> 'user'
)
SELECT agents.id,
       answer.context,
       answer.task_id,
       COALESCE(answer.is_streaming, FALSE),
       idle.result_context,
       COALESCE(idle.source_execution_id, '')
FROM agents
CROSS JOIN conversation
LEFT JOIN LATERAL (
    SELECT CASE
               WHEN mg.meta -> 'runtime_context' ->> 'execution_id' = mg.task_id
                AND mg.meta -> 'runtime_context' ->> 'execution_generation' = mg.meta ->> 'execution_generation'
               THEN mg.meta -> 'runtime_context' || jsonb_build_object('active', mg.is_streaming)
           END AS context,
           mg.task_id, mg.is_streaming
    FROM %[1]s.chat_message_group AS mg
    WHERE mg.conversation_id = conversation.id AND NULLIF(mg.task_id, '') IS NOT NULL
      AND COALESCE((SELECT job.participant_id FROM elitea_runtime.agent_execution_jobs AS job
                    WHERE job.execution_id = mg.task_id
                    ORDER BY job.generation DESC LIMIT 1), mg.author_participant_id) = agents.id
    ORDER BY mg.is_streaming DESC, COALESCE(mg.updated_at, mg.created_at) DESC, mg.id DESC
    LIMIT 1
) AS answer ON TRUE
LEFT JOIN LATERAL (
    SELECT row.result_context || jsonb_build_object('active', FALSE) AS result_context, row.source_execution_id
    FROM elitea_runtime.agent_idle_compactions AS row
    WHERE row.project_id = $2 AND row.conversation_uuid = $1::uuid AND row.participant_id = agents.id
      AND row.state = 'DONE' AND row.result_context IS NOT NULL
    ORDER BY row.updated_at DESC, row.id DESC
    LIMIT 1
) AS idle ON TRUE
ORDER BY agents.id`, schema), conversationUUID, projectID)
	if err != nil {
		return nil, fmt.Errorf("idle compaction: read the agents' context: %w", err)
	}
	defer rows.Close()
	var out []AgentContextRow
	for rows.Next() {
		var (
			participant         int64
			answerContext, idle []byte
			taskID              *string
			streaming           bool
			idleSourceExecution string
		)
		if err := rows.Scan(&participant, &answerContext, &taskID, &streaming, &idle, &idleSourceExecution); err != nil {
			return nil, fmt.Errorf("idle compaction: read the agents' context: %w", err)
		}
		switch {
		case len(idle) != 0 && !streaming && taskID != nil && *taskID == idleSourceExecution:
			out = append(out, AgentContextRow{ParticipantID: participant, RuntimeContext: json.RawMessage(idle), Idle: true})
		case len(answerContext) != 0:
			out = append(out, AgentContextRow{ParticipantID: participant, RuntimeContext: json.RawMessage(answerContext)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("idle compaction: read the agents' context: %w", err)
	}
	return out, nil
}

// validConversationUUID admits any case spelling of a canonical UUID.
func validConversationUUID(value string) bool {
	_, err := currentPGUUID(value)
	return err == nil
}

// idleRunTerminalSkip is the row end of an idle run's runtime failure.
func idleRunTerminalSkip(failureCode string) string {
	if failureCode == "CANCELLED" {
		return agentexecutionapp.IdleCompactionSkipRunCancelled
	}
	return agentexecutionapp.IdleCompactionSkipRunFailed
}
