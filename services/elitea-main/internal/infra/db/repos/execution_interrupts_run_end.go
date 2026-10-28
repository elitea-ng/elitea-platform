package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
)

// Cards that outlive their run (fanout-interrupt-decisions §5, §8a). A run
// that ends without parking (On Error stop, fail-after-drain, a deadline, a
// joined run whose ACK was lost, a reclaim that ends the run) leaves no
// execution that could ever apply a decision. Its open cards are closed:
// PENDING → CANCELLED, DECIDED → SUPERSEDED (it may already have been
// applied), audited with the run's claim, with agent_hitl_resolved frames.

// strandedCardsBatch bounds one sweep pass.
const strandedCardsBatch = 16

// runEndedColumnsSQL reports, for the interrupt row aliased response joined
// to latestExecutionSQL, whether the response's run ended without parking:
// the latest execution bound to the response is terminal, the response is
// not parked, and no wake is queued. The second column is the latest claim
// that ran for the response, the closer of record.
const runEndedColumnsSQL = `
       COALESCE(latest.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED')
                AND response.parked_execution_id IS NULL
                AND NOT EXISTS (SELECT 1 FROM elitea_runtime.execution_interrupt_wakes wake
                                WHERE wake.root_response_id = response.root_response_id AND wake.state = 'QUEUED'),
                FALSE),
       COALESCE((SELECT claim.claim_id
                 FROM elitea_runtime.agent_execution_jobs binding
                 JOIN elitea_runtime.execution_claims claim
                   ON claim.execution_id = binding.execution_id AND claim.generation = binding.generation
                 WHERE binding.client_message_id = response.root_response_id::text
                 ORDER BY claim.claimed_at DESC, claim.claim_id DESC
                 LIMIT 1), '')`

// latestExecutionSQL joins the latest execution bound to the response.
const latestExecutionSQL = `
LEFT JOIN LATERAL (
  SELECT job.state
  FROM elitea_runtime.agent_execution_jobs binding
  JOIN elitea_runtime.execution_jobs job USING (execution_id, generation)
  WHERE binding.client_message_id = response.root_response_id::text
  ORDER BY job.admitted_at DESC, binding.generation DESC, binding.execution_id DESC
  LIMIT 1
) latest ON TRUE`

// closeCardsAtRunEnd closes the open cards of a response whose run ended
// without parking (runEndedColumnsSQL). The caller holds the
// chat_message_group response row lock and the execution_interrupt_responses
// row lock.
func closeCardsAtRunEnd(ctx context.Context, tx sqlExecutor, rootResponseID, claimID string) (resolvedNotice, error) {
	if claimID == "" {
		// A card is raised under a claim, so a response with cards has one.
		return resolvedNotice{}, domain.ErrLedgerFault
	}
	_, notice, err := closeOpenCards(ctx, tx, rootResponseID, domain.StateCancelled, domain.StateSuperseded,
		domain.Closer{ClaimID: claimID})
	return notice, err
}

// closeCardsIfRunEnded locks the response's interrupt row and closes its
// open cards when its run ended without parking. The caller holds the
// chat_message_group response row lock. It reports whether the run had
// ended.
func closeCardsIfRunEnded(ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID string) (bool, resolvedNotice, error) {
	var ended bool
	var claimID string
	err := tx.QueryRow(ctx, `SELECT `+runEndedColumnsSQL+`
FROM elitea_runtime.execution_interrupt_responses response`+latestExecutionSQL+`
WHERE response.root_response_id = $1::uuid AND response.project_id = $2
FOR UPDATE OF response`, rootResponseID, projectID).Scan(&ended, &claimID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, resolvedNotice{}, nil
	}
	if err != nil {
		return false, resolvedNotice{}, fmt.Errorf("execution interrupt run end: %w", err)
	}
	if !ended {
		return false, resolvedNotice{}, nil
	}
	notice, err := closeCardsAtRunEnd(ctx, tx, rootResponseID, claimID)
	return true, notice, err
}

// CloseStrandedCards closes the open cards of at most strandedCardsBatch
// responses whose run ended without parking. It runs on every wake sweep
// (after each settlement and every FanoutWakeSweepInterval), so a crash
// between a run's settlement and the close is repaired by the next pass. It
// reports how many responses it closed.
func (r *ExecutionInterruptRepository) CloseStrandedCards(ctx context.Context) (int, error) {
	if r == nil || r.shared == nil || r.projects == nil {
		return 0, errors.New("stranded card close is not configured")
	}
	type candidate struct {
		responseID string
		projectID  int64
	}
	var candidates []candidate
	// Driven by the open-card partial index; a parked response or one with a
	// queued wake is not a candidate.
	rows, err := r.shared.Query(ctx, `
SELECT DISTINCT card.root_response_id::text, card.project_id
FROM elitea_runtime.execution_interrupts card
JOIN elitea_runtime.execution_interrupt_responses response ON response.root_response_id = card.root_response_id
WHERE card.state IN ('PENDING', 'DECIDED') AND response.parked_execution_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.execution_interrupt_wakes wake
                  WHERE wake.root_response_id = card.root_response_id AND wake.state = 'QUEUED')
  AND EXISTS (
    SELECT 1 FROM (
      SELECT job.state
      FROM elitea_runtime.agent_execution_jobs binding
      JOIN elitea_runtime.execution_jobs job USING (execution_id, generation)
      WHERE binding.client_message_id = card.root_response_id::text
      ORDER BY job.admitted_at DESC, binding.generation DESC, binding.execution_id DESC
      LIMIT 1) latest
    WHERE latest.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'QUARANTINED'))
ORDER BY 1
LIMIT $1`, strandedCardsBatch)
	if err != nil {
		return 0, fmt.Errorf("find stranded cards: %w", err)
	}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.responseID, &c.projectID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("find stranded cards: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("find stranded cards: %w", err)
	}
	closed := 0
	for _, c := range candidates {
		var notice resolvedNotice
		var ended bool
		err := r.projects.WithinProjectTx(ctx, c.projectID, interruptWriteTx, func(tx sqlExecutor) error {
			notice, ended = resolvedNotice{}, false
			// The ledger's lock order: the response row, then its interrupt row.
			var id int64
			err := tx.QueryRow(ctx, `SELECT id FROM chat_message_group WHERE uuid = $1::uuid FOR UPDATE`, c.responseID).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // the response was deleted; its cards went with it
			}
			if err != nil {
				return err
			}
			ended, notice, err = closeCardsIfRunEnded(ctx, tx, c.projectID, c.responseID)
			return err
		})
		if err != nil {
			return closed, fmt.Errorf("close stranded cards: %w", err)
		}
		if ended {
			closed++
			r.signals.resolved(ctx, notice)
		}
	}
	return closed, nil
}
