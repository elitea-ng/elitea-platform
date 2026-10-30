package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

// FU-265: the paused writer a continuation re-enters. The Worker accepts the
// paused root row only for exactly this execution and generation, so Main
// names it from its own records: the execution that paused the response
// (chat_message_group.task_id) and the generation its paused terminal settled
// under. No settlement, or more than one, refuses the continuation.

// pausedWriterQuery reads the paused execution and the generation of its
// settled terminal. With lock, the response row is locked first (the ledger's
// lock order: the response, then the ledger).
func pausedWriterQuery(lock bool) string {
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
	return query
}

// readPausedWriter returns the paused execution and generation of the
// response, refusing when there is not exactly one settled paused terminal.
func readPausedWriter(ctx context.Context, tx sqlExecutor, responseMessageID pgtype.UUID, lock bool) (string, int64, error) {
	rows, err := tx.Query(ctx, pausedWriterQuery(lock), responseMessageID)
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
		return "", 0, agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	return execution, *generation, nil
}

// continuationNamesPausedWriter reports whether a continuation kind re-enters
// a paused root row. An output-limit continuation continues a completed
// answer, not a pause.
func continuationNamesPausedWriter(kind agentexecutionapp.CurrentContinuationKind) bool {
	return kind != agentexecutionapp.CurrentContinuationOutputLimit
}

// withPausedWriter runs resolve, then names the paused writer on target in the
// same transaction. A response without exactly one settled paused terminal
// (one paused before settlements existed, or an ambiguous one) names none;
// the service refuses that continuation only while it names paused writers.
func withPausedWriter(
	ctx context.Context, target *agentexecutionapp.CurrentContinuationTarget, responseMessageID pgtype.UUID,
	kind agentexecutionapp.CurrentContinuationKind, resolve func(sqlExecutor) error,
) func(sqlExecutor) error {
	return func(tx sqlExecutor) error {
		if err := resolve(tx); err != nil {
			return err
		}
		if !continuationNamesPausedWriter(kind) {
			return nil
		}
		execution, generation, err := readPausedWriter(ctx, tx, responseMessageID, false)
		if errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
			return nil
		}
		if err != nil {
			return err
		}
		target.PausedExecutionID, target.PausedGeneration = execution, generation
		return nil
	}
}

// verifyPausedWriter re-reads the paused writer under the response row lock
// in the admission transaction and refuses a continuation whose input names
// another (FU-265, C5): the marker is bound to the decision this
// transaction admits.
func verifyPausedWriter(ctx context.Context, tx pgx.Tx, turn agentexecutionapp.CurrentContinueTurn) error {
	if !continuationNamesPausedWriter(turn.ContinuationKind) || turn.PausedExecutionID == "" {
		return nil
	}
	response, err := currentPGUUID(turn.ResponseMessageID)
	if err != nil {
		return agentexecutionapp.ErrUnsupportedCurrentAgentStart
	}
	execution, generation, err := readPausedWriter(ctx, pgxExecutor{queryer: tx}, response, true)
	if err != nil {
		return err
	}
	if execution != turn.PausedExecutionID || generation != turn.PausedGeneration {
		return errors.Join(agentexecutionapp.ErrUnsupportedCurrentAgentStart, errPausedWriterMoved)
	}
	return nil
}

var errPausedWriterMoved = errors.New("the paused writer changed after the continuation was resolved")
