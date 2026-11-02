package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	recoverydomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

// FU-269: the paused writer a continuation re-enters. The Worker re-runs a
// continuation lost before its first root save from the paused root row only
// when Main names that row's writer: the execution that paused the response
// (chat_message_group.task_id) and the generation its paused terminal settled
// under. The admitting transaction reads both under the response lock, before
// the admission consumes the pause, and stores them on the job; the claim
// receipt serves them back (control.proto ClaimReceiptV1.paused_writer).

// errNoPausedWriter: the response has no single settled paused terminal.
var errNoPausedWriter = errors.New("the response has no single settled paused terminal")

// readPausedWriter returns the paused execution and generation of the
// response, or errNoPausedWriter when there is not exactly one settled
// terminal of that execution. With lock, the response row is locked (the
// ledger's lock order: the response, then the ledger).
func readPausedWriter(ctx context.Context, tx sqlExecutor, responseMessageID pgtype.UUID, lock bool) (string, int64, error) {
	query := `
SELECT COALESCE(g.task_id, ''), s.generation
FROM chat_message_group g
LEFT JOIN elitea_runtime.execution_settlements s
  ON s.execution_id = g.task_id AND s.disposition = 'SUCCEEDED'
WHERE g.uuid = $1::uuid`
	if lock {
		query += `
FOR UPDATE OF g`
	}
	rows, err := tx.Query(ctx, query, responseMessageID)
	if err != nil {
		return "", 0, fmt.Errorf("read the paused writer: %w", err)
	}
	defer rows.Close()
	var execution string
	var generation *int64
	count := 0
	for rows.Next() {
		count++
		if err := rows.Scan(&execution, &generation); err != nil {
			return "", 0, fmt.Errorf("read the paused writer: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", 0, fmt.Errorf("read the paused writer: %w", err)
	}
	if count != 1 || execution == "" || generation == nil || *generation < 1 {
		return "", 0, errNoPausedWriter
	}
	return execution, *generation, nil
}

// recordPausedWriter stores the paused writer of a continuation on its job,
// in the admitting transaction and before the admission consumes the pause.
// An output-limit continuation continues a completed answer, not a pause, and
// names none. A response without exactly one settled paused terminal (one
// paused before settlements existed), or whose paused execution is not a
// runtime execution id, names none either: the continuation runs as before,
// and a reclaim lost before its first root save ends typed.
func recordPausedWriter(ctx context.Context, tx pgx.Tx, executionID string, generation int64, turn agentexecutionapp.CurrentContinueTurn) error {
	if turn.ContinuationKind == agentexecutionapp.CurrentContinuationOutputLimit {
		return nil
	}
	response, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return executionapp.ErrInvalidAdmission
	}
	pausedExecution, pausedGeneration, err := readPausedWriter(ctx, pgxExecutor{queryer: tx}, response, true)
	if errors.Is(err, errNoPausedWriter) {
		return nil
	}
	if err != nil {
		return err
	}
	if !recoverydomain.ValidExecutionID(pausedExecution) {
		// Not a runtime execution id (a pause from before the runtime kernel):
		// nothing the Worker could match, so name none.
		return nil
	}
	if pausedExecution == executionID {
		return errors.New("a continuation cannot re-enter its own execution")
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_runtime.agent_execution_jobs
SET paused_execution_id = $3, paused_generation = $4
WHERE execution_id = $1 AND generation = $2 AND paused_execution_id IS NULL`,
		executionID, generation, pausedExecution, pausedGeneration)
	if err != nil {
		return fmt.Errorf("record the paused writer: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("record the paused writer: the admitted job is missing")
	}
	return nil
}

// selectClaimPausedWriter serves the job's paused writer on an executable or
// recovering agent claim. Every other disposition carries none.
func selectClaimPausedWriter(ctx context.Context, tx sqlExecutor, request executionapp.ClaimRequest, decision *executionapp.ClaimDecision) error {
	if (request.CapabilityID != executiondomain.AgentApplicationCapability && request.CapabilityID != executiondomain.AgentAdhocCapability) ||
		(decision.Disposition != executionapp.ClaimAccepted && decision.Disposition != executionapp.ClaimRecoverAgentModelCheckpoint &&
			decision.Disposition != executionapp.ClaimRecoverNodeVisit) {
		return nil
	}
	var execution *string
	var generation *int64
	err := tx.QueryRow(ctx, `
SELECT paused_execution_id, paused_generation FROM elitea_runtime.agent_execution_jobs
WHERE execution_id = $1 AND generation = $2`, request.ExecutionID, int64(request.Generation)).Scan(&execution, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load the claim's paused writer: %w", err)
	}
	if execution == nil || generation == nil {
		return nil
	}
	decision.PausedWriter = &executionapp.PausedWriter{ExecutionID: *execution, Generation: uint64(*generation)}
	return nil
}
