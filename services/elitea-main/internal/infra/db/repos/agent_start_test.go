package repos

import (
	"context"
	"errors"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// localWorkRegenerationExecutorStub answers the Local work probe as well.
type localWorkRegenerationExecutorStub struct {
	*currentRegenerationExecutorStub
	localWork bool
	asked     pgtype.UUID
}

func (stub *localWorkRegenerationExecutorStub) ConversationIsLocalWork(_ context.Context, conversation pgtype.UUID) (bool, error) {
	stub.asked = conversation
	return stub.localWork, nil
}

type localWorkProjectStoreStub struct{ executor sqlExecutor }

func (stub localWorkProjectStoreStub) WithinProjectTx(_ context.Context, _ int64, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	return fn(stub.executor)
}

// A regenerate names a message, not a conversation: the Local work probe runs
// on the conversation the response row resolves to, and refuses the turn with
// ErrLocalWorkThread (the route's 409 `local_work_thread`). An ordinary
// conversation is resolved exactly as before.
func TestCurrentAgentRegenerationRefusesALocalWorkThread(t *testing.T) {
	conversationUUID, err := currentPGUUID("10000000-0000-4000-8000-000000000032")
	if err != nil {
		t.Fatal(err)
	}
	questionID, err := currentPGUUID("20000000-0000-4000-8000-000000000032")
	if err != nil {
		t.Fatal(err)
	}
	for _, localWork := range []bool{true, false} {
		executor := &localWorkRegenerationExecutorStub{
			currentRegenerationExecutorStub: &currentRegenerationExecutorStub{
				scriptedExecutor: &scriptedExecutor{},
				row: sqlcgen.ResolveCurrentRegenerationRow{
					ConversationUuid: conversationUUID, QuestionID: questionID, TargetParticipantID: 21,
					RegenerationKind: "application", UserInput: "regenerate this",
				},
			},
			localWork: localWork,
		}
		repository, err := newCurrentAgentStartRepository(localWorkProjectStoreStub{executor: executor}, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = repository.ResolveCurrentRegeneration(t.Context(), agentexecutionapp.CurrentRegenerationResolveRequest{
			ProjectID: 1, ActorUserID: 11, ResponseMessageID: "40000000-0000-4000-8000-000000000032",
		})
		if executor.asked != conversationUUID {
			t.Fatalf("localWork=%v: probed %v, want the response's conversation", localWork, executor.asked)
		}
		if localWork && !errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
			t.Fatalf("a local work thread regenerated: err=%v", err)
		}
		if !localWork && err != nil {
			t.Fatalf("an ordinary conversation was refused: %v", err)
		}
	}
}

// Every continuation kind runs the probe on the conversation it names, before
// reading the paused turn.
func TestCurrentAgentContinuationRefusesALocalWorkThread(t *testing.T) {
	executor := &localWorkRegenerationExecutorStub{
		currentRegenerationExecutorStub: &currentRegenerationExecutorStub{scriptedExecutor: &scriptedExecutor{}},
		localWork:                       true,
	}
	repository, err := newCurrentAgentStartRepository(localWorkProjectStoreStub{executor: executor}, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ResolveCurrentContinuation(t.Context(), agentexecutionapp.CurrentContinuationResolveRequest{
		ProjectID: 1, ActorUserID: 11,
		ConversationUUID:  "10000000-0000-4000-8000-000000000033",
		ResponseMessageID: "40000000-0000-4000-8000-000000000033",
		Kind:              agentexecutionapp.CurrentContinuationHITL,
	})
	if !errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
		t.Fatalf("err=%v, want ErrLocalWorkThread", err)
	}
}
