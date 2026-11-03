package repos

// What the unattended chain hook reads when a chain's run settles
// (ADR-0045 decision 4).

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// ChainSettleView is a settled chain run: its link, whether it is still
// the chain's latest run, and the facts of its response.
type ChainSettleView struct {
	Chain unattended.Chain
	// Latest is true when no later run of the chain exists.
	Latest bool
	// The response's terminal facts.
	OutputLimitReached  bool
	OutputLimitSequence int64
	StepLimitReached    bool
	Paused              bool
	IsError             bool
}

// ChainSettleView reads a settled run's chain and response. It answers
// false when the run is not a link of a chain.
func (r *RunChainsRepository) ChainSettleView(ctx context.Context, executionID string) (ChainSettleView, bool, error) {
	chain, found, err := readRunChainLink(ctx, r.pool, executionID)
	if err != nil || !found {
		return ChainSettleView{}, false, err
	}
	view := ChainSettleView{Chain: chain}
	var last *string
	if err := r.pool.QueryRow(ctx, `
SELECT last_execution_id FROM elitea_runtime.run_chains WHERE root_execution_id = $1`,
		chain.RootExecutionID).Scan(&last); err != nil {
		return ChainSettleView{}, false, fmt.Errorf("unattended chain: read the latest run: %w", err)
	}
	view.Latest = last == nil || *last == executionID
	if chain.ResponseMessageID == "" {
		return view, true, nil
	}
	schema, err := tenantSchema(fmt.Sprint(chain.ProjectID))
	if err != nil {
		return ChainSettleView{}, false, err
	}
	err = r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(meta -> 'output_limit_reached' = 'true'::jsonb, FALSE),
       CASE WHEN COALESCE(meta ->> 'output_limit_sequence', '') ~ '^[0-9]{1,18}$'
            THEN (meta ->> 'output_limit_sequence')::bigint ELSE 0 END,
       COALESCE(meta -> 'step_limit_reached' = 'true'::jsonb, FALSE),
       (meta ? 'hitl_interrupt' OR meta ? 'hitl_interrupts' OR meta ? 'authorization_requests'),
       COALESCE(meta -> 'is_error' = 'true'::jsonb, FALSE)
FROM %s.chat_message_group
WHERE uuid = $1::uuid`, schema), chain.ResponseMessageID).Scan(
		&view.OutputLimitReached, &view.OutputLimitSequence, &view.StepLimitReached, &view.Paused, &view.IsError)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, true, nil
	}
	if err != nil {
		return ChainSettleView{}, false, fmt.Errorf("unattended chain: read the response: %w", err)
	}
	return view, true, nil
}
