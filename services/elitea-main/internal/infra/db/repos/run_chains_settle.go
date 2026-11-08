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
	// ParticipantID is the agent the run answered as (shared 0227); 0 when
	// not recorded. A chain of a multi-agent message has links of several
	// agents (ADR-0050 decision 7c): a continuation goes to this one.
	ParticipantID int64
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
	var participant *int32
	if err := r.pool.QueryRow(ctx, `
SELECT participant_id FROM elitea_runtime.agent_execution_jobs
WHERE execution_id = $1 ORDER BY generation DESC LIMIT 1`, executionID).Scan(&participant); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return ChainSettleView{}, false, fmt.Errorf("unattended chain: read the run's participant: %w", err)
	}
	if participant != nil {
		view.ParticipantID = int64(*participant)
	}
	facts, err := readResponseFacts(ctx, r.pool, chain.ProjectID, chain.ResponseMessageID, executionID)
	if err != nil {
		return ChainSettleView{}, false, fmt.Errorf("unattended chain: %w", err)
	}
	view.OutputLimitReached, view.OutputLimitSequence = facts.OutputLimitReached, facts.OutputLimitSequence
	view.StepLimitReached, view.Paused, view.IsError = facts.StepLimitReached, facts.Paused, facts.IsError
	return view, true, nil
}
