package repos

// The run_finished notification (ADR-0045 decision 5, #1437; package
// runfinished). Every writer runs in the transaction that ends the chain, so
// the row commits with the end state, and the uuid is derived from the chain
// root, so a replay writes nothing new.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runfinished"
)

// chainExecutor is what a chain end needs: a transaction (or, in a test, a
// pool).
type chainExecutor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// chainEndReturning is what an UPDATE that ends a chain (alias c) returns.
const chainEndReturning = `
RETURNING c.root_execution_id, c.project_id, c.owner_user_id, c.conversation_uuid::text, c.origin,
          c.trigger_origin, c.continuations, c.last_execution_id, c.end_state, COALESCE(c.end_limit, ''),
          EXTRACT(EPOCH FROM (c.ended_at - c.created_at))::float8`

// chainEnd is one ended chain.
type chainEnd struct {
	Root, ConversationUUID, Origin, TriggerOrigin, LastExecutionID, State, Limit string
	ProjectID, OwnerUserID                                                       int64
	Continuations                                                                int
	Duration                                                                     time.Duration
}

func scanChainEnd(row pgx.Row) (chainEnd, error) {
	var end chainEnd
	var seconds float64
	err := row.Scan(&end.Root, &end.ProjectID, &end.OwnerUserID, &end.ConversationUUID, &end.Origin,
		&end.TriggerOrigin, &end.Continuations, &end.LastExecutionID, &end.State, &end.Limit, &seconds)
	end.Duration = time.Duration(seconds * float64(time.Second))
	return end, err
}

// notifyChainEnd writes the run_finished row of an ended chain when the noise
// rule asks for one.
func notifyChainEnd(ctx context.Context, tx chainExecutor, end chainEnd) error {
	handedOff := false
	if end.Origin == runfinished.OriginCloud {
		var err error
		if handedOff, err = handedOffThread(ctx, tx, end.ProjectID, end.ConversationUUID); err != nil {
			return err
		}
	}
	facts := runfinished.Facts{
		Outcome: end.State, Continuations: end.Continuations, Duration: end.Duration,
		TriggerOrigin: end.TriggerOrigin, HandedOff: handedOff,
	}
	if !runfinished.Notify(facts) {
		return nil
	}
	return insertRunFinished(ctx, tx, end.OwnerUserID, runfinished.Meta{
		Outcome: end.State, Limit: end.Limit, ProjectID: end.ProjectID, ConversationUUID: end.ConversationUUID,
		ChainRootExecutionID: end.Root, ExecutionID: end.LastExecutionID, Origin: end.Origin,
		Continuations: end.Continuations,
	})
}

// handedOffThread reports a conversation that the cloud executor owns after
// a hand-off (ADR-0034): its turns run in the cloud while the person is
// away from the desktop.
func handedOffThread(ctx context.Context, tx chainExecutor, projectID int64, conversationUUID string) (bool, error) {
	if _, err := uuid.Parse(conversationUUID); err != nil {
		return false, nil
	}
	var handedOff bool
	err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.workspace_handoffs
    WHERE project_id = $1 AND conversation_uuid = $2::uuid
      AND state IN ('CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING') AND deleted_at IS NULL)`,
		projectID, conversationUUID).Scan(&handedOff)
	if err != nil {
		return false, fmt.Errorf("run finished: read the hand-off: %w", err)
	}
	return handedOff, nil
}

// insertRunFinished writes one run_finished row. The uuid comes from the
// chain root, so a second write of the same chain is a no-op.
func insertRunFinished(ctx context.Context, tx chainExecutor, owner int64, meta runfinished.Meta) error {
	if owner <= 0 || owner > math.MaxInt32 || meta.ProjectID <= 0 || meta.ProjectID > math.MaxInt32 ||
		meta.ChainRootExecutionID == "" {
		return nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("run finished: encode the meta: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, $5, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`,
		reviewNotificationUUID(runfinished.NotificationKey(meta.ChainRootExecutionID)),
		int32(meta.ProjectID), int32(owner), string(raw), runfinished.EventType); err != nil {
		return fmt.Errorf("run finished: write the notification: %w", err)
	}
	return nil
}

// RunFinishedRepository writes the run_finished row of a run that has no
// chain: a schedule or webhook run, or a cloud turn of a handed-off thread.
type RunFinishedRepository struct {
	pool *pgxpool.Pool
}

// NewRunFinishedRepository builds the repository.
func NewRunFinishedRepository(pool *pgxpool.Pool) (*RunFinishedRepository, error) {
	if pool == nil {
		return nil, errors.New("run finished repository requires a pool")
	}
	return &RunFinishedRepository{pool: pool}, nil
}

// RecordSettledRun writes the row of a settled agent run that is not a link
// of a chain (a chain's row comes with its end state). It writes nothing for
// a run that a person started on a thread they did not hand off, and nothing
// for a cancelled run. Idempotent: the row's uuid comes from the execution.
func (r *RunFinishedRepository) RecordSettledRun(
	ctx context.Context, executionID string, outcome executionapp.SettlementOutcome,
) error {
	if r == nil || executionID == "" || outcome == executionapp.SettlementOutcomeUnknown ||
		outcome == executionapp.SettlementCancelled {
		return nil
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		run, found, err := readStandaloneRun(ctx, tx, executionID)
		if err != nil || !found {
			return err
		}
		handedOff, owner := false, run.ActorUserID
		if !runfinishedUnattended(run.TriggerOrigin) {
			if handedOff, owner, err = handOffOwner(ctx, tx, run.ProjectID, run.ConversationUUID); err != nil || !handedOff {
				return err
			}
		} else if policyOwner, err := unattendedRunOwner(ctx, tx, run.ProjectID, run.ConversationUUID); err != nil {
			return err
		} else if policyOwner > 0 {
			owner = policyOwner
		}
		state := runfinished.OutcomeCompleted
		if outcome == executionapp.SettlementFailed {
			state = runfinished.OutcomeFailed
		} else if facts, err := readResponseFacts(ctx, tx, run.ProjectID, run.ResponseMessageID); err != nil {
			return err
		} else if facts.IsError {
			state = runfinished.OutcomeFailed
		} else if facts.Paused {
			state = runfinished.OutcomeWaiting
		}
		if !runfinished.Notify(runfinished.Facts{
			Outcome: state, TriggerOrigin: run.TriggerOrigin, HandedOff: handedOff,
		}) {
			return nil
		}
		return insertRunFinished(ctx, tx, owner, runfinished.Meta{
			Outcome: state, ProjectID: run.ProjectID, ConversationUUID: run.ConversationUUID,
			ChainRootExecutionID: executionID, ExecutionID: executionID, Origin: runfinished.OriginCloud,
		})
	})
}

func runfinishedUnattended(origin string) bool {
	return origin == "schedule" || origin == "webhook"
}

// standaloneRun is a settled agent run that is not a chain link.
type standaloneRun struct {
	ProjectID, ActorUserID                             int64
	TriggerOrigin, ConversationUUID, ResponseMessageID string
}

func readStandaloneRun(ctx context.Context, tx pgx.Tx, executionID string) (standaloneRun, bool, error) {
	var (
		run     standaloneRun
		actor   string
		project int32
	)
	err := tx.QueryRow(ctx, `
SELECT job.trigger_origin, job.actor_id, job.projection_project_id, binding.client_stream_id,
       binding.client_message_id
FROM elitea_runtime.agent_execution_jobs AS binding
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = binding.execution_id AND job.generation = binding.generation
WHERE binding.execution_id = $1
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.run_chain_links AS link WHERE link.execution_id = $1)
ORDER BY binding.generation DESC
LIMIT 1`, executionID).Scan(&run.TriggerOrigin, &actor, &project, &run.ConversationUUID, &run.ResponseMessageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return standaloneRun{}, false, nil
	}
	if err != nil {
		return standaloneRun{}, false, fmt.Errorf("run finished: read the run: %w", err)
	}
	run.ProjectID = int64(project)
	run.ActorUserID, _ = strconv.ParseInt(actor, 10, 64)
	return run, true, nil
}

// handOffOwner answers whether the conversation is a handed-off thread and
// its owner.
func handOffOwner(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string) (bool, int64, error) {
	if _, err := uuid.Parse(conversationUUID); err != nil {
		return false, 0, nil
	}
	var owner int64
	err := tx.QueryRow(ctx, `
SELECT owner_user_id FROM elitea_runtime.workspace_handoffs
WHERE project_id = $1 AND conversation_uuid = $2::uuid
  AND state IN ('CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING') AND deleted_at IS NULL`,
		projectID, conversationUUID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("run finished: read the hand-off: %w", err)
	}
	return true, owner, nil
}

// unattendedRunOwner is the owner a schedule or trigger names for its
// conversation (unattended_run_policies), or 0.
func unattendedRunOwner(ctx context.Context, tx pgx.Tx, projectID int64, conversationUUID string) (int64, error) {
	if _, err := uuid.Parse(conversationUUID); err != nil {
		return 0, nil
	}
	var owner int64
	err := tx.QueryRow(ctx, `
SELECT owner_user_id FROM elitea_runtime.unattended_run_policies
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, projectID, conversationUUID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("run finished: read the run's owner: %w", err)
	}
	return owner, nil
}

// responseFacts are the terminal facts of a chat response.
type responseFacts struct {
	OutputLimitReached  bool
	OutputLimitSequence int64
	StepLimitReached    bool
	Paused              bool
	IsError             bool
}

// readResponseFacts reads a response's terminal facts from its meta. A
// response that does not exist answers no facts.
func readResponseFacts(ctx context.Context, queryer rowQueryer, projectID int64, messageID string) (responseFacts, error) {
	var facts responseFacts
	if _, err := uuid.Parse(messageID); err != nil {
		return facts, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return facts, err
	}
	err = queryer.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(meta -> 'output_limit_reached' = 'true'::jsonb, FALSE),
       CASE WHEN COALESCE(meta ->> 'output_limit_sequence', '') ~ '^[0-9]{1,18}$'
            THEN (meta ->> 'output_limit_sequence')::bigint ELSE 0 END,
       COALESCE(meta -> 'step_limit_reached' = 'true'::jsonb, FALSE),
       COALESCE(meta ? 'hitl_interrupt' OR meta ? 'hitl_interrupts' OR meta ? 'authorization_requests', FALSE),
       COALESCE(meta -> 'is_error' = 'true'::jsonb, FALSE)
FROM %s.chat_message_group
WHERE uuid = $1::uuid`, schema), messageID).Scan(
		&facts.OutputLimitReached, &facts.OutputLimitSequence, &facts.StepLimitReached, &facts.Paused, &facts.IsError)
	if errors.Is(err, pgx.ErrNoRows) {
		return responseFacts{}, nil
	}
	if err != nil {
		return responseFacts{}, fmt.Errorf("read the response: %w", err)
	}
	return facts, nil
}
