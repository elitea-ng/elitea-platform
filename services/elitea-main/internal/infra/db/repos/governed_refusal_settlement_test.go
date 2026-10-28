package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

type governedRefusalExecutorStub struct {
	*currentAgentCancelExecutorStub
	refusalParams sqlcgen.ProjectGovernedContinuationRefusalParams
	refusalCalls  int
	refusalErr    error
}

func (stub *governedRefusalExecutorStub) ProjectGovernedContinuationRefusal(
	_ context.Context,
	params sqlcgen.ProjectGovernedContinuationRefusalParams,
) (int32, error) {
	stub.refusalCalls++
	stub.refusalParams = params
	return params.ResponseMessageGroupID, stub.refusalErr
}

type governedRefusalProjectStoreStub struct {
	executor *governedRefusalExecutorStub
}

func (stub *governedRefusalProjectStoreStub) WithinProjectTx(
	_ context.Context,
	_ int64,
	_ pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	return fn(stub.executor)
}

// A refused governed continuation (#1485) cancels the paused execution with
// the stop's ownership-checked statement, then projects the error onto the
// exact answer generation the cancel resolved. Nothing to cancel is not an
// error: the turn was settled already.
func TestCurrentAgentCancelRepositorySettlesAGovernedRefusal(t *testing.T) {
	questionID := int32(41)
	request := agentexecutionapp.GovernedRefusalSettlement{
		ProjectID: 2, ActorUserID: 7,
		ResponseMessageID: "10000000-0000-4000-8000-000000001485",
		Code:              "credential_grant_missing", Reason: "access revoked",
	}
	for _, test := range []struct {
		name        string
		cancelErr   error
		refusalErr  error
		wantSettled bool
		wantProject bool
	}{
		{name: "paused turn", wantSettled: true, wantProject: true},
		{name: "nothing to cancel", cancelErr: pgx.ErrNoRows},
		{name: "answer gone", refusalErr: pgx.ErrNoRows, wantProject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &governedRefusalExecutorStub{
				currentAgentCancelExecutorStub: &currentAgentCancelExecutorStub{
					scriptedExecutor: &scriptedExecutor{},
					cancelRow: sqlcgen.CancelCurrentAgentExecutionRow{
						ResponseMessageGroupID: 51, QuestionMessageGroupID: &questionID,
						ExecutionID: "execution-1", Generation: 1,
						ClientExecutionGeneration: "20000000-0000-4000-8000-000000001485",
					},
					cancelErr: test.cancelErr,
				},
				refusalErr: test.refusalErr,
			}
			repository, err := newCurrentAgentCancelRepository(&governedRefusalProjectStoreStub{executor: executor})
			if err != nil {
				t.Fatal(err)
			}
			settled, err := repository.SettleGovernedRefusal(t.Context(), request)
			if err != nil || settled != test.wantSettled {
				t.Fatalf("settled=%v err=%v", settled, err)
			}
			if executor.cancelParams.ProjectID != 2 || executor.cancelParams.ActorUserID != 7 {
				t.Fatalf("cancel params=%+v", executor.cancelParams)
			}
			if (executor.refusalCalls == 1) != test.wantProject {
				t.Fatalf("projection calls=%d", executor.refusalCalls)
			}
			if test.wantProject {
				got := executor.refusalParams
				if got.ResponseMessageGroupID != 51 || got.ExecutionID != "execution-1" ||
					got.ExecutionGeneration != "20000000-0000-4000-8000-000000001485" ||
					got.ErrorMessage != "access revoked" || got.RefusalCode != "credential_grant_missing" ||
					got.ErrorCode != agentexecutionapp.GovernedRefusalErrorCode {
					t.Fatalf("projection params=%+v", got)
				}
			}
		})
	}

	// A reason longer than the error a terminal carries, or none, is refused
	// before any statement.
	for _, reason := range []string{"", strings.Repeat("x", 257)} {
		executor := &governedRefusalExecutorStub{currentAgentCancelExecutorStub: &currentAgentCancelExecutorStub{scriptedExecutor: &scriptedExecutor{}}}
		repository, err := newCurrentAgentCancelRepository(&governedRefusalProjectStoreStub{executor: executor})
		if err != nil {
			t.Fatal(err)
		}
		bad := request
		bad.Reason = reason
		if _, err := repository.SettleGovernedRefusal(t.Context(), bad); !errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentCancel) {
			t.Fatalf("reason of %d bytes: err=%v", len(reason), err)
		}
	}
}
