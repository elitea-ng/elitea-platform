package repos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// UnattendedApprovalsRepository is the store of the unattended approval
// worker (agentexecution.UnattendedApprovalWorker; shared 0211).
type UnattendedApprovalsRepository struct {
	pool     *pgxpool.Pool
	projects projectStore
}

var _ agentexecutionapp.UnattendedApprovalStore = (*UnattendedApprovalsRepository)(nil)

// NewUnattendedApprovalsRepository builds the store.
func NewUnattendedApprovalsRepository(pool *pgxpool.Pool) (*UnattendedApprovalsRepository, error) {
	projects, err := newPostgresProjectStore(pool)
	if err != nil {
		return nil, err
	}
	return &UnattendedApprovalsRepository{pool: pool, projects: projects}, nil
}

// claimSkipsSQL claims the QUEUED skip intents of at most $1 paused runs
// that no pass tried within $2 milliseconds, and counts the attempt.
const claimSkipsSQL = `
WITH due AS (
    SELECT execution_id
    FROM elitea_runtime.unattended_approval_actions
    WHERE state = 'QUEUED' AND action = 'skip' AND path = 'root'
      AND (attempts = 0 OR updated_at <= clock_timestamp() - $2::bigint * interval '1 millisecond')
    GROUP BY execution_id
    ORDER BY min(created_at)
    LIMIT $1
)
UPDATE elitea_runtime.unattended_approval_actions AS action
SET attempts = LEAST(action.attempts + 1, 1000), updated_at = clock_timestamp()
FROM due
WHERE action.execution_id = due.execution_id AND action.state = 'QUEUED'
RETURNING action.execution_id, action.interrupt_id, action.project_id, action.response_message_id,
          action.conversation_uuid::text, action.owner_user_id, action.trigger_origin,
          action.skip_action, action.attempts`

// ClaimUnattendedSkips implements UnattendedApprovalStore.
func (r *UnattendedApprovalsRepository) ClaimUnattendedSkips(
	ctx context.Context, limit int, lease time.Duration,
) ([]agentexecutionapp.UnattendedSkipTarget, error) {
	if r == nil || r.pool == nil || limit <= 0 {
		return nil, errors.New("unattended approvals store is not configured")
	}
	rows, err := r.pool.Query(ctx, claimSkipsSQL, limit, lease.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("claim unattended skips: %w", err)
	}
	defer rows.Close()
	byExecution := map[string]*agentexecutionapp.UnattendedSkipTarget{}
	order := []string{}
	for rows.Next() {
		var executionID, interruptID, responseID, conversationUUID, origin, refusal string
		var projectID, owner int64
		var attempts int
		if err := rows.Scan(&executionID, &interruptID, &projectID, &responseID, &conversationUUID, &owner,
			&origin, &refusal, &attempts); err != nil {
			return nil, fmt.Errorf("claim unattended skips: %w", err)
		}
		target, ok := byExecution[executionID]
		if !ok {
			target = &agentexecutionapp.UnattendedSkipTarget{
				ProjectID: projectID, OwnerUserID: owner, ConversationUUID: conversationUUID,
				ResponseMessageID: responseID, ExecutionID: executionID,
				TriggerOrigin: executiondomain.TriggerOrigin(origin),
			}
			byExecution[executionID] = target
			order = append(order, executionID)
		}
		target.Attempts = max(target.Attempts, attempts)
		decision := agentexecutionapp.CurrentHITLDecision{InterruptID: interruptID, Action: refusal}
		if refusal == "block_with_comment" {
			decision.Value = approvalSkipMessage
		}
		target.Decisions = append(target.Decisions, decision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim unattended skips: %w", err)
	}
	targets := make([]agentexecutionapp.UnattendedSkipTarget, 0, len(order))
	for _, executionID := range order {
		targets = append(targets, *byExecution[executionID])
	}
	return targets, nil
}

// CloseUnattendedSkip implements UnattendedApprovalStore.
func (r *UnattendedApprovalsRepository) CloseUnattendedSkip(ctx context.Context, target agentexecutionapp.UnattendedSkipTarget) error {
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.unattended_approval_actions
SET state = 'DONE', updated_at = clock_timestamp()
WHERE execution_id = $1 AND state = 'QUEUED'`, target.ExecutionID); err != nil {
		return fmt.Errorf("close the unattended skip: %w", err)
	}
	return nil
}

// FailUnattendedSkip implements UnattendedApprovalStore: in one project
// transaction it ends the intents FAILED and, when the pause is still
// pending, ends the run with approval_required_unattended and writes each
// pause's unattended_failed row.
func (r *UnattendedApprovalsRepository) FailUnattendedSkip(
	ctx context.Context, target agentexecutionapp.UnattendedSkipTarget, failureCode string,
) error {
	return r.projects.WithinProjectTx(ctx, target.ProjectID, interruptWriteTx, func(tx sqlExecutor) error {
		// Lock order: the response row first, as the continuation that
		// answers the pause takes it.
		pause, err := lockPausedResponse(ctx, tx, target.ResponseMessageID, target.ExecutionID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.unattended_approval_actions
SET state = 'FAILED', failure_code = $2, updated_at = clock_timestamp()
WHERE execution_id = $1 AND state = 'QUEUED'`, target.ExecutionID, failureCode); err != nil {
			return fmt.Errorf("fail the unattended skip: %w", err)
		}
		return endRootPauseForApproval(ctx, tx, pause, target.ResponseMessageID,
			ApprovalOutcomeUnattendedFailed, ApprovalRequiredUnattendedCode)
	})
}

// pausedResponse is a locked response row and whether it still holds the
// root pause of one execution.
type pausedResponse struct {
	groupID int64
	pending bool
}

// lockPausedResponse locks the response row (the ledger's lock order: the
// response, then the ledger rows).
func lockPausedResponse(ctx context.Context, tx sqlExecutor, responseMessageID, executionID string) (pausedResponse, error) {
	var locked pausedResponse
	err := tx.QueryRow(ctx, `
SELECT id, (COALESCE(task_id, '') = $2 AND (meta ? 'hitl_interrupts' OR meta ? 'hitl_interrupt'))
FROM chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, responseMessageID, executionID).
		Scan(&locked.groupID, &locked.pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return pausedResponse{}, nil
	}
	if err != nil {
		return pausedResponse{}, fmt.Errorf("lock the paused response: %w", err)
	}
	return locked, nil
}

// endRootPauseForApproval ends a root pause that is still pending on the
// locked response: each open pause row gets its system decision row
// (outcome), and the response becomes the typed error code. A pause that
// was answered meanwhile is left as it is.
func endRootPauseForApproval(ctx context.Context, tx sqlExecutor, pause pausedResponse, responseMessageID, outcome, code string) error {
	if pause.groupID == 0 || !pause.pending {
		return nil
	}
	if err := closeApprovalPausesOn(ctx, tx, responseMessageID, approvalPathRoot, outcome, 0, code); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = (meta - 'hitl_interrupt' - 'hitl_interrupts' - 'authorization_requests')
        || jsonb_build_object('is_error', TRUE, 'error', $2::text, 'error_code', $3::text),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, pause.groupID, approvalMessageOf(code), code); err != nil {
		return fmt.Errorf("end the paused response: %w", err)
	}
	return nil
}

// expiredWaitsSQL reads at most $1 runs with an expired wait.
const expiredWaitsSQL = `
SELECT execution_id, project_id, response_message_id, path
FROM elitea_runtime.unattended_approval_actions
WHERE state = 'WAITING' AND expires_at <= clock_timestamp()
GROUP BY execution_id, project_id, response_message_id, path
ORDER BY min(expires_at)
LIMIT $1`

// ExpireUnattendedWaits implements UnattendedApprovalStore.
func (r *UnattendedApprovalsRepository) ExpireUnattendedWaits(ctx context.Context, limit int) (int, error) {
	if r == nil || r.pool == nil || limit <= 0 {
		return 0, errors.New("unattended approvals store is not configured")
	}
	rows, err := r.pool.Query(ctx, expiredWaitsSQL, limit)
	if err != nil {
		return 0, fmt.Errorf("find expired unattended waits: %w", err)
	}
	type wait struct {
		executionID, responseID, path string
		projectID                     int64
	}
	var waits []wait
	for rows.Next() {
		var item wait
		if err := rows.Scan(&item.executionID, &item.projectID, &item.responseID, &item.path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("find expired unattended waits: %w", err)
		}
		waits = append(waits, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find expired unattended waits: %w", err)
	}
	// A wait that cannot be expired does not stop the others: it ends
	// FAILED (expiry_failed), so the next pass does not read it again ahead
	// of the later waits, and the pass goes on. The failures come back
	// joined, for the worker to log.
	expired := 0
	var failures []error
	for _, item := range waits {
		err := r.projects.WithinProjectTx(ctx, item.projectID, interruptWriteTx, func(tx sqlExecutor) error {
			return expireUnattendedWait(ctx, tx, item.responseID, item.executionID, item.path)
		})
		if err == nil {
			expired++
			continue
		}
		if ctx.Err() != nil {
			// A stopping worker is not a failing wait.
			return expired, errors.Join(append(failures, ctx.Err())...)
		}
		failure := fmt.Errorf("expire the unattended wait of execution %s: %w", item.executionID, err)
		slog.WarnContext(ctx, "unattended approvals: a wait could not be expired; it ends FAILED",
			"execution_id", item.executionID, "project_id", item.projectID, "err", err)
		if markErr := r.failUnattendedWait(ctx, item.executionID); markErr != nil {
			failure = errors.Join(failure, markErr)
		}
		failures = append(failures, failure)
	}
	return expired, errors.Join(failures...)
}

// unattendedExpiryFailedCode is the failure_code of a wait the sweep could
// not expire.
const unattendedExpiryFailedCode = "expiry_failed"

// failUnattendedWait ends the expired WAITING intents of one execution
// FAILED, outside the expiry transaction that failed.
func (r *UnattendedApprovalsRepository) failUnattendedWait(ctx context.Context, executionID string) error {
	if _, err := r.pool.Exec(ctx, `
UPDATE elitea_runtime.unattended_approval_actions
SET state = 'FAILED', failure_code = $2, updated_at = clock_timestamp()
WHERE execution_id = $1 AND state = 'WAITING' AND expires_at <= clock_timestamp()`,
		executionID, unattendedExpiryFailedCode); err != nil {
		return fmt.Errorf("end the unattended wait of execution %s failed: %w", executionID, err)
	}
	return nil
}

// expireUnattendedWait closes one run's expired wait: the intents end
// EXPIRED first (so the closes below find nothing to finish), then each
// still-open pause gets its timed_out row, and the run ends with
// approval_timed_out.
func expireUnattendedWait(ctx context.Context, tx sqlExecutor, responseMessageID, executionID, path string) error {
	// Lock order: the response row first.
	pause, err := lockPausedResponse(ctx, tx, responseMessageID, executionID)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.unattended_approval_actions
SET state = 'EXPIRED', updated_at = clock_timestamp()
WHERE execution_id = $1 AND state = 'WAITING' AND expires_at <= clock_timestamp()`, executionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // answered or expired by another pass
	}
	if path == approvalPathRoot {
		return endRootPauseForApproval(ctx, tx, pause, responseMessageID, ApprovalOutcomeTimedOut, ApprovalTimedOutCode)
	}
	// A fan-out run: the cards' pauses end timed_out before the cards
	// close, so the close changes nothing in the ledger.
	if err := closeApprovalPausesOn(ctx, tx, responseMessageID, approvalPathInterrupt, ApprovalOutcomeTimedOut, 0,
		ApprovalTimedOutCode); err != nil {
		return err
	}
	var claimID string
	err = tx.QueryRow(ctx, `SELECT claim_id FROM elitea_runtime.execution_claims
WHERE execution_id = $1 ORDER BY claimed_at DESC, claim_id DESC LIMIT 1`, executionID).Scan(&claimID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if claimID != "" {
		if _, _, err := closeOpenCards(ctx, tx, responseMessageID, domain.StateCancelled, domain.StateCancelled,
			domain.Closer{ClaimID: claimID}); err != nil {
			return err
		}
	}
	if err := cancelRunForApproval(ctx, tx, executionID, 0, ApprovalTimedOutCode); err != nil {
		return err
	}
	if pause.groupID == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = (meta - 'fanout_parked_v1') || jsonb_build_object('is_error', TRUE, 'error', $2::text, 'error_code', $3::text),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, pause.groupID, approvalTimedOutMessage, ApprovalTimedOutCode)
	return err
}
