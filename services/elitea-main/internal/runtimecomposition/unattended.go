package runtimecomposition

// Unattended runs (ADR-0045, #1435, #1436): the grant admission of the
// start service, and the after-settle hook that continues or ends a cloud
// chain.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runfinished"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattendedchain"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// unattendedGrantPolicy is what the start service reads to admit a grant.
func unattendedGrantPolicy(
	pool *pgxpool.Pool,
	approval agentexecutionapp.UnattendedApprovalPolicy,
	runCaps agentexecutionapp.UnattendedRunCaps,
	tiers agentexecutionapp.UnattendedTierReader,
) *agentexecutionapp.UnattendedGrantPolicy {
	settings := platformconfig.NewCachedSection(func(ctx context.Context) (platformconfig.UnattendedSettings, error) {
		return platformconfig.LoadUnattendedSettings(ctx, pool)
	}, 0)
	return &agentexecutionapp.UnattendedGrantPolicy{
		Settings: settings, Approval: approval, RunCaps: runCaps, Tiers: tiers,
	}
}

// unattendedSettlementHooks continues or ends a cloud chain when its run
// settles. Without a start service there is no chain to continue.
func unattendedSettlementHooks(
	pool *pgxpool.Pool, start *agentexecutionapp.CurrentApplicationStartService, briefs *runbrief.Service,
	logger *slog.Logger,
) []executionapp.SettlementOption {
	if pool == nil || start == nil {
		return nil
	}
	hook := unattendedchain.New(chainSettleViews{repos.NewRunChainsRepository(pool)}, start, logger)
	if hook == nil {
		return nil
	}
	if briefs != nil {
		hook.WithGoals(briefs)
	}
	return []executionapp.SettlementOption{executionapp.WithAfterSettleHooks(hook.AfterSettle)}
}

// runFinishedSettlementHooks writes the run_finished notification of a
// schedule or webhook run, or a cloud turn of a handed-off thread, that has
// no chain (ADR-0045 decision 5). A chain's row comes with its end state.
func runFinishedSettlementHooks(pool *pgxpool.Pool, logger *slog.Logger) []executionapp.SettlementOption {
	runs, err := repos.NewRunFinishedRepository(pool)
	if err != nil {
		return nil
	}
	return []executionapp.SettlementOption{executionapp.WithAfterSettleHooks(runfinished.NewHook(runs, logger).AfterSettle)}
}

// runBriefStarter adapts runbrief.Service to agentexecution.RunBriefStarter.
type runBriefStarter struct{ *runbrief.Service }

func (s runBriefStarter) Begin(ctx context.Context, start agentexecutionapp.RunBriefStart) {
	s.Service.Begin(ctx, start.RootExecutionID, start.Request)
}

// chainSettleViews adapts the repository to unattendedchain.Chains.
type chainSettleViews struct{ *repos.RunChainsRepository }

func (c chainSettleViews) SettleView(ctx context.Context, executionID string) (unattendedchain.SettleView, bool, error) {
	view, found, err := c.ChainSettleView(ctx, executionID)
	if err != nil || !found {
		return unattendedchain.SettleView{}, found, err
	}
	return unattendedchain.SettleView{
		Chain: view.Chain, Latest: view.Latest, OutputLimitReached: view.OutputLimitReached,
		OutputLimitSequence: view.OutputLimitSequence, StepLimitReached: view.StepLimitReached,
		Paused: view.Paused, IsError: view.IsError,
	}, true, nil
}

var _ unattendedchain.Chains = chainSettleViews{}

// chainSweepStore adapts the repository to unattendedchain.SweepStore.
type chainSweepStore struct{ *repos.RunChainsRepository }

func (c chainSweepStore) SweepChains(ctx context.Context, quiet time.Duration, limit int) (int, error) {
	swept, err := c.RunChainsRepository.SweepChains(ctx, quiet, limit)
	return len(swept), err
}

// unattendedChainSweeper ends the chains that nothing else will end.
func unattendedChainSweeper(pool *pgxpool.Pool, logger *slog.Logger) (*unattendedchain.Sweeper, error) {
	return unattendedchain.NewSweeper(chainSweepStore{repos.NewRunChainsRepository(pool)}, logger)
}
