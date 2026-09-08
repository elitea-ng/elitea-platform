package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
)

type currentRegenerationExecutorStub struct {
	*scriptedExecutor
	row    sqlcgen.ResolveCurrentRegenerationRow
	err    error
	params sqlcgen.ResolveCurrentRegenerationParams
}

func (stub *currentRegenerationExecutorStub) ResolveCurrentRegeneration(
	_ context.Context,
	params sqlcgen.ResolveCurrentRegenerationParams,
) (sqlcgen.ResolveCurrentRegenerationRow, error) {
	stub.params = params
	return stub.row, stub.err
}

type currentRegenerationProjectStoreStub struct {
	executor  *currentRegenerationExecutorStub
	projectID int64
	options   pgx.TxOptions
}

func (stub *currentRegenerationProjectStoreStub) WithinProjectTx(
	ctx context.Context,
	projectID int64,
	options pgx.TxOptions,
	fn func(sqlExecutor) error,
) error {
	stub.projectID = projectID
	stub.options = options
	return fn(stub.executor)
}

func TestCurrentAgentRegenerationResolverDistinguishesFinalizingResponse(t *testing.T) {
	conversationUUID, err := currentPGUUID("10000000-0000-4000-8000-000000000031")
	if err != nil {
		t.Fatal(err)
	}
	questionID, err := currentPGUUID("20000000-0000-4000-8000-000000000031")
	if err != nil {
		t.Fatal(err)
	}
	executor := &currentRegenerationExecutorStub{
		scriptedExecutor: &scriptedExecutor{},
		row: sqlcgen.ResolveCurrentRegenerationRow{
			ConversationUuid: conversationUUID,
			QuestionID:       questionID, TargetParticipantID: 21,
			ResponseIsStreaming: true,
			RegenerationKind:    "application", UserInput: "regenerate this",
		},
	}
	projects := &currentRegenerationProjectStoreStub{executor: executor}
	repository, err := newCurrentAgentStartRepository(projects, 1)
	if err != nil {
		t.Fatal(err)
	}

	_, err = repository.ResolveCurrentRegeneration(t.Context(), agentexecutionapp.CurrentRegenerationResolveRequest{
		ProjectID: 1, ActorUserID: 11,
		ResponseMessageID: "40000000-0000-4000-8000-000000000031",
	})
	if !errors.Is(err, agentexecutionapp.ErrCurrentAgentRegenerationStillFinalizing) {
		t.Fatalf("error=%v", err)
	}
	if projects.projectID != 1 || projects.options.AccessMode != pgx.ReadOnly ||
		executor.params.ActorUserID != 11 || executor.params.ProjectID != 1 {
		t.Fatalf("project=%d options=%+v params=%+v", projects.projectID, projects.options, executor.params)
	}
}
