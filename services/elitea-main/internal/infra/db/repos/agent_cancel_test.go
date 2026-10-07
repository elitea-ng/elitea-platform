package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

type currentAgentCancelExecutorStub struct {
	*scriptedExecutor
	cancelRow     sqlcgen.CancelCurrentAgentExecutionRow
	cancelErr     error
	cancelParams  sqlcgen.CancelCurrentAgentExecutionParams
	projection    sqlcgen.ProjectCurrentAgentStopRow
	projectionErr error
	projectParams sqlcgen.ProjectCurrentAgentStopParams
	replay        bool
	replayErr     error
	replayParams  sqlcgen.IsCurrentAgentCancellationReplayParams
}

func (stub *currentAgentCancelExecutorStub) CancelCurrentAgentExecution(
	_ context.Context,
	params sqlcgen.CancelCurrentAgentExecutionParams,
) (sqlcgen.CancelCurrentAgentExecutionRow, error) {
	stub.cancelParams = params
	return stub.cancelRow, stub.cancelErr
}

func (stub *currentAgentCancelExecutorStub) ProjectCurrentAgentStop(
	_ context.Context,
	params sqlcgen.ProjectCurrentAgentStopParams,
) (sqlcgen.ProjectCurrentAgentStopRow, error) {
	stub.projectParams = params
	return stub.projection, stub.projectionErr
}

func (stub *currentAgentCancelExecutorStub) IsCurrentAgentCancellationReplay(
	_ context.Context,
	params sqlcgen.IsCurrentAgentCancellationReplayParams,
) (bool, error) {
	stub.replayParams = params
	return stub.replay, stub.replayErr
}

type currentAgentCancelProjectStoreStub struct {
	executor  *currentAgentCancelExecutorStub
	projectID int64
	options   pgx.TxOptions
}

func (stub *currentAgentCancelProjectStoreStub) WithinProjectTx(
	ctx context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	stub.projectID = projectID
	stub.options = options
	return fn(stub.executor)
}

func TestCurrentAgentCancelRepositoryProjectsRetainedSalvagedAndDeletedResponses(t *testing.T) {
	questionID := int32(41)
	request := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 2, ActorUserID: 7,
		ResponseMessageID: "10000000-0000-4000-8000-000000000031",
	}
	for name, projection := range map[string]sqlcgen.ProjectCurrentAgentStopRow{
		"retained": {Retained: true},
		"salvaged": {Retained: true, Salvaged: true},
		"deleted":  {Deleted: true, QuestionDeleted: true},
	} {
		t.Run(name, func(t *testing.T) {
			executor := &currentAgentCancelExecutorStub{
				scriptedExecutor: &scriptedExecutor{},
				cancelRow: sqlcgen.CancelCurrentAgentExecutionRow{
					ResponseMessageGroupID: 51,
					QuestionMessageGroupID: &questionID,
					ExecutionID:            "execution-1", Generation: 1,
					ClientExecutionGeneration: "20000000-0000-4000-8000-000000000031",
				},
				projection: projection,
			}
			projects := &currentAgentCancelProjectStoreStub{executor: executor}
			repository, err := newCurrentAgentCancelRepository(projects)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := repository.CancelCurrentAgent(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Deleted != projection.Deleted || outcome.Salvaged != projection.Salvaged || outcome.Replay {
				t.Fatalf("outcome=%+v projection=%+v", outcome, projection)
			}
			if projects.projectID != 2 || projects.options.AccessMode != pgx.ReadWrite ||
				executor.cancelParams.ProjectID != 2 || executor.cancelParams.ActorUserID != 7 ||
				executor.projectParams.ResponseMessageGroupID != 51 ||
				executor.projectParams.QuestionMessageGroupID != 41 ||
				executor.projectParams.ExecutionID != "execution-1" ||
				executor.projectParams.ExecutionGeneration != "20000000-0000-4000-8000-000000000031" {
				t.Fatalf("project=%d options=%+v cancel=%+v projection=%+v", projects.projectID, projects.options, executor.cancelParams, executor.projectParams)
			}
		})
	}
}

func TestCurrentAgentCancelRepositoryReplaysOnlyCancelledExecution(t *testing.T) {
	request := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 2, ActorUserID: 7,
		ResponseMessageID: "10000000-0000-4000-8000-000000000032",
	}
	for _, test := range []struct {
		name    string
		replay  bool
		wantErr error
	}{
		{name: "cancelled replay", replay: true},
		{name: "terminal or unauthorized target", wantErr: agentexecutionapp.ErrCurrentAgentCancelNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &currentAgentCancelExecutorStub{
				scriptedExecutor: &scriptedExecutor{},
				cancelErr:        pgx.ErrNoRows,
				replay:           test.replay,
			}
			repository, err := newCurrentAgentCancelRepository(
				&currentAgentCancelProjectStoreStub{executor: executor},
			)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := repository.CancelCurrentAgent(t.Context(), request)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
			if test.wantErr == nil && !outcome.Replay {
				t.Fatalf("outcome=%+v", outcome)
			}
			if executor.replayParams.ResponseMessageID != request.ResponseMessageID ||
				executor.replayParams.ProjectID != 2 || executor.replayParams.ActorUserID != 7 {
				t.Fatalf("replay params=%+v", executor.replayParams)
			}
		})
	}
}

func TestCurrentAgentCancelRepositoryRollsBackIncompleteProjection(t *testing.T) {
	questionID := int32(41)
	executor := &currentAgentCancelExecutorStub{
		scriptedExecutor: &scriptedExecutor{},
		cancelRow: sqlcgen.CancelCurrentAgentExecutionRow{
			ResponseMessageGroupID: 51, QuestionMessageGroupID: &questionID,
			ExecutionID: "execution-1", Generation: 1,
			ClientExecutionGeneration: "20000000-0000-4000-8000-000000000033",
		},
	}
	repository, err := newCurrentAgentCancelRepository(
		&currentAgentCancelProjectStoreStub{executor: executor},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.CancelCurrentAgent(t.Context(), agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 2, ActorUserID: 7,
		ResponseMessageID: "10000000-0000-4000-8000-000000000033",
	})
	if err == nil {
		t.Fatal("expected incomplete projection error")
	}
}

// Two stops of an empty turn sent together: the second one's target CTE took
// its snapshot before the first committed, so it still sees the answer, waits
// on the job row and then matches `desired_state = 'CANCELLED'`; by the time
// its projection runs the first stop has deleted both rows, so every flag is
// false. That is the same caller's repeated stop the contract answers 204, not
// a projection that "did not settle" (502). A projection that finds nothing
// for a caller the replay check does not admit is still an error.
func TestCurrentAgentCancelRepositoryTreatsAVanishedProjectionAsReplay(t *testing.T) {
	questionID := int32(41)
	request := agentexecutionapp.CurrentAgentCancelRequest{
		ProjectID: 2, ActorUserID: 7,
		ResponseMessageID: "10000000-0000-4000-8000-000000000034",
	}
	for _, test := range []struct {
		name    string
		replay  bool
		wantErr bool
	}{
		{name: "concurrent repeated stop", replay: true},
		{name: "not a replay", replay: false, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &currentAgentCancelExecutorStub{
				scriptedExecutor: &scriptedExecutor{},
				cancelRow: sqlcgen.CancelCurrentAgentExecutionRow{
					ResponseMessageGroupID: 51, QuestionMessageGroupID: &questionID,
					ExecutionID: "execution-1", Generation: 1,
					ClientExecutionGeneration: "20000000-0000-4000-8000-000000000034",
				},
				replay: test.replay,
			}
			repository, err := newCurrentAgentCancelRepository(
				&currentAgentCancelProjectStoreStub{executor: executor},
			)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := repository.CancelCurrentAgent(t.Context(), request)
			if test.wantErr {
				if err == nil {
					t.Fatalf("outcome=%+v, want an error", outcome)
				}
				return
			}
			if err != nil || !outcome.Replay || outcome.Deleted || outcome.Salvaged {
				t.Fatalf("outcome=%+v err=%v, want a replay", outcome, err)
			}
			if executor.replayParams.ResponseMessageID != request.ResponseMessageID ||
				executor.replayParams.ProjectID != 2 || executor.replayParams.ActorUserID != 7 {
				t.Fatalf("replay params=%+v", executor.replayParams)
			}
		})
	}
}
