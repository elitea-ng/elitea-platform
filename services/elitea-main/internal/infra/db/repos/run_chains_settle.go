package repos

// What the unattended chain hook reads when a chain's run settles
// (ADR-0045 decision 4).

import (
	"context"
	"fmt"

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
	facts, err := readResponseFacts(ctx, r.pool, chain.ProjectID, chain.ResponseMessageID)
	if err != nil {
		return ChainSettleView{}, false, fmt.Errorf("unattended chain: %w", err)
	}
	view.OutputLimitReached, view.OutputLimitSequence = facts.OutputLimitReached, facts.OutputLimitSequence
	view.StepLimitReached, view.Paused, view.IsError = facts.StepLimitReached, facts.Paused, facts.IsError
	return view, true, nil
}
