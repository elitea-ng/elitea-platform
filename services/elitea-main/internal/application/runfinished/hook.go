package runfinished

import (
	"context"
	"log/slog"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
)

// SettledRuns writes the row of a settled run that is not a chain link
// (repos.RunFinishedRepository).
type SettledRuns interface {
	RecordSettledRun(ctx context.Context, executionID string, outcome executionapp.SettlementOutcome) error
}

// agentOutputPrefix is the terminal output prefix of an agent execution.
const agentOutputPrefix = "agent-execution:"

// Hook writes the run_finished row of a schedule or webhook run, or of a
// cloud turn of a handed-off thread, that has no unattended chain. A chain's
// row comes with its end state (the chain hook and the sweep), so the store
// skips every chain link. The hook fires on replays too: the store is
// idempotent.
type Hook struct {
	runs   SettledRuns
	logger *slog.Logger
}

// NewHook builds the hook; nil without a store.
func NewHook(runs SettledRuns, logger *slog.Logger) *Hook {
	if runs == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Hook{runs: runs, logger: logger}
}

// AfterSettle is the executionapp.AfterSettleHook.
func (h *Hook) AfterSettle(ctx context.Context, proposal executionapp.SettlementProposal, _ executionapp.SettlementReceipt) {
	if h == nil || proposal.Outcome == executionapp.SettlementOutcomeUnknown ||
		proposal.Outcome == executionapp.SettlementCancelled ||
		proposal.TerminalLogicalOutputID != agentOutputPrefix+proposal.Fence.ExecutionID {
		return
	}
	if err := h.runs.RecordSettledRun(ctx, proposal.Fence.ExecutionID, proposal.Outcome); err != nil {
		h.logger.ErrorContext(ctx, "run finished: record the settled run",
			"execution_id", proposal.Fence.ExecutionID, "err", err)
	}
}
