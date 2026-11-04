package runfinished

import (
	"context"
	"testing"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
)

type settledRunsStub struct {
	calls []executionapp.SettlementOutcome
}

func (s *settledRunsStub) RecordSettledRun(_ context.Context, _ string, outcome executionapp.SettlementOutcome) error {
	s.calls = append(s.calls, outcome)
	return nil
}

func TestHookRecordsOnlyEndedAgentRuns(t *testing.T) {
	const execution = "0123456789abcdef0123456789abcdef"
	agent := agentOutputPrefix + execution
	cases := []struct {
		name    string
		outcome executionapp.SettlementOutcome
		output  string
		want    int
	}{
		{"succeeded agent run", executionapp.SettlementSucceeded, agent, 1},
		{"failed agent run", executionapp.SettlementFailed, agent, 1},
		{"cancelled agent run", executionapp.SettlementCancelled, agent, 0},
		{"unknown outcome", executionapp.SettlementOutcomeUnknown, agent, 0},
		{"another execution kind", executionapp.SettlementSucceeded, "index-ingest:" + execution, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := &settledRunsStub{}
			NewHook(runs, nil).AfterSettle(context.Background(), executionapp.SettlementProposal{
				Fence: runtimedomain.Fence{ExecutionID: execution}, Outcome: tc.outcome, TerminalLogicalOutputID: tc.output,
			}, executionapp.SettlementReceipt{})
			if len(runs.calls) != tc.want {
				t.Fatalf("calls = %d, want %d", len(runs.calls), tc.want)
			}
		})
	}
	if NewHook(nil, nil) != nil {
		t.Fatal("a hook without a store must be nil")
	}
	var nilHook *Hook
	nilHook.AfterSettle(context.Background(), executionapp.SettlementProposal{}, executionapp.SettlementReceipt{})
}
