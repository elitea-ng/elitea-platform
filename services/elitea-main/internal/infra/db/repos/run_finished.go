package repos

// The run_finished notification (ADR-0045 decision 5, #1437; package
// runfinished). Every writer runs in the transaction that ends the chain, so
// the row commits with the end state, and the uuid is derived from the chain
// root, so a replay writes nothing new. The row is best-effort: it is
// written in a savepoint, and a failure never holds the end state back.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runfinished"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/responsepause"
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
// rule asks for one. Best-effort: the write runs in a savepoint of tx, and a
// failure rolls back to it and is logged, so the caller's end state still
// commits (a chain end, a sweep batch or a local turn commit never waits on
// its notification).
func notifyChainEnd(ctx context.Context, tx pgx.Tx, end chainEnd) {
	err := pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
		return writeChainEndNotification(ctx, savepoint, end)
	})
	if err != nil {
		slog.WarnContext(ctx, "run finished: the chain end commits without its notification",
			"root_execution_id", end.Root, "end_state", end.State, "err", err)
	}
}

func writeChainEndNotification(ctx context.Context, tx pgx.Tx, end chainEnd) error {
	handedOff := false
	if end.Origin == runfinished.OriginCloud {
		var err error
		if handedOff, _, err = handOffOwner(ctx, tx, end.ProjectID, end.ConversationUUID); err != nil {
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
	if skip, err := waitAlreadyNotified(ctx, tx, end.State, end.LastExecutionID); err != nil || skip {
		return err
	}
	meta := runfinished.Meta{
		Outcome: end.State, Limit: end.Limit, ProjectID: end.ProjectID, ConversationUUID: end.ConversationUUID,
		ChainRootExecutionID: end.Root, ExecutionID: end.LastExecutionID, Origin: end.Origin,
		Continuations: end.Continuations,
	}
	if end.Origin == runfinished.OriginDesktop {
		var err error
		if meta.ThreadID, err = desktopThreadID(ctx, tx, end.ProjectID, end.ConversationUUID); err != nil {
			return err
		}
	}
	return insertRunFinished(ctx, tx, end.OwnerUserID, meta)
}

// desktopThreadID is the desktop app's thread id of a conversation: its
// numeric id in decimal (the app keeps a Local work thread under the id it
// created the conversation with). It reads the chain's own project and
// conversation, so it cannot name another tenant's row. "" when the
// conversation is gone.
func desktopThreadID(ctx context.Context, tx rowQueryer, projectID int64, conversationUUID string) (string, error) {
	if _, err := uuid.Parse(conversationUUID); err != nil {
		return "", nil
	}
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return "", nil
	}
	var id int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_conversations WHERE uuid = $1::uuid`, schema), conversationUUID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("run finished: read the desktop thread: %w", err)
	}
	return strconv.FormatInt(id, 10), nil
}

// waitAlreadyNotified reports a waiting run whose pause already told its
// owner: an unattended run under notify_and_wait gets its
// agent_approval_required row in the pause's transaction, together with the
// intent row read here (notifyApprovalOwner), so a run_finished "waiting"
// row would be a second "Run needs you" for the same pause.
func waitAlreadyNotified(ctx context.Context, tx rowQueryer, outcome, executionID string) (bool, error) {
	if outcome != runfinished.OutcomeWaiting || executionID == "" {
		return false, nil
	}
	var notified bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.unattended_approval_actions
    WHERE execution_id = $1 AND action = 'notify_and_wait')`, executionID).Scan(&notified); err != nil {
		return false, fmt.Errorf("run finished: read the approval wait: %w", err)
	}
	return notified, nil
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
		if !unattended.IsSystemTrigger(run.TriggerOrigin) {
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
		} else if facts, err := readResponseFacts(ctx, tx, run.ProjectID, run.ResponseMessageID, executionID); err != nil {
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
		if skip, err := waitAlreadyNotified(ctx, tx, state, executionID); err != nil || skip {
			return err
		}
		return insertRunFinished(ctx, tx, owner, runfinished.Meta{
			Outcome: state, ProjectID: run.ProjectID, ConversationUUID: run.ConversationUUID,
			ChainRootExecutionID: executionID, ExecutionID: executionID, Origin: runfinished.OriginCloud,
		})
	})
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
  -- An idle compaction (ADR-0050 decision 9b2) is no run a person waits
  -- for: it never notifies.
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.agent_idle_compactions AS idle WHERE idle.idle_execution_id = $1)
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

// handOffOwner answers whether the conversation is a handed-off thread (the
// cloud executor owns it after a hand-off, ADR-0034: its turns run in the
// cloud while the person is away from the desktop) and its owner.
func handOffOwner(ctx context.Context, tx rowQueryer, projectID int64, conversationUUID string) (bool, int64, error) {
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

// readResponseFacts reads the terminal facts of the response that
// executionID settled, from its meta. A response that does not exist answers
// no facts. Paused: it waits for a person (responsepause; a parked fan-out
// included, FU-284). When a fan-out wake of this very park already took the
// response (a decision was waiting when the run parked, so the wake was
// queued in the settlement and may have started or even finished before this
// read), the meta is the wake's: the settled run's facts are only that it
// parked. A response another execution took for any other reason (a
// regenerate, a new turn) keeps the facts its meta holds.
func readResponseFacts(ctx context.Context, queryer rowQueryer, projectID int64, messageID, executionID string) (responseFacts, error) {
	var facts responseFacts
	var takenByWake bool
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
       COALESCE(%s, FALSE),
       COALESCE(meta -> 'is_error' = 'true'::jsonb, FALSE),
       EXISTS (
           SELECT 1 FROM elitea_runtime.execution_interrupt_wakes AS wake
           WHERE wake.wake_execution_id = response.task_id
             AND wake.root_response_id = response.uuid
             AND wake.parked_execution_id = $2)
FROM %s.chat_message_group AS response
WHERE uuid = $1::uuid`, responsepause.SQL(responsepause.Unqualified), schema), messageID, executionID).Scan(
		&facts.OutputLimitReached, &facts.OutputLimitSequence, &facts.StepLimitReached, &facts.Paused, &facts.IsError,
		&takenByWake)
	if errors.Is(err, pgx.ErrNoRows) {
		return responseFacts{}, nil
	}
	if err != nil {
		return responseFacts{}, fmt.Errorf("read the response: %w", err)
	}
	if takenByWake {
		return responseFacts{Paused: true}, nil
	}
	return facts, nil
}
