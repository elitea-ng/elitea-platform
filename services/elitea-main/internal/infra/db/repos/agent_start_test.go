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
	// cloud: the thread is handed off to the cloud executor (ADR-0034).
	cloud      bool
	executedBy string
	notNewest  bool
	asked      pgtype.UUID
	project    int64
}

func (stub *localWorkRegenerationExecutorStub) LocalWorkThreadFence(_ context.Context, projectID int64, conversation pgtype.UUID) (bool, bool, error) {
	stub.asked, stub.project = conversation, projectID
	return stub.localWork, stub.cloud, nil
}

func (stub *localWorkRegenerationExecutorStub) CloudRegenerationCheck(context.Context, pgtype.UUID) (string, bool, error) {
	return stub.executedBy, !stub.notNewest, nil
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

// A handed-off Local work thread (ADR-0034 decision 7): the fence is open, so
// a regenerate of the newest cloud turn resolves; a turn the desktop ran and
// an older cloud turn are refused with their own errors.
func TestCurrentAgentRegenerationInAHandedOffThread(t *testing.T) {
	conversationUUID, err := currentPGUUID("10000000-0000-4000-8000-000000000034")
	if err != nil {
		t.Fatal(err)
	}
	questionID, err := currentPGUUID("20000000-0000-4000-8000-000000000034")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		executedBy string
		notNewest  bool
		want       error
	}{
		{"the newest cloud turn", "cloud_executor", false, nil},
		{"a desktop turn", "desktop", false, agentexecutionapp.ErrLocalWorkTurn},
		{"an older cloud turn", "cloud_executor", true, agentexecutionapp.ErrCloudTurnNotNewest},
	} {
		executor := &localWorkRegenerationExecutorStub{
			currentRegenerationExecutorStub: &currentRegenerationExecutorStub{
				scriptedExecutor: &scriptedExecutor{},
				row: sqlcgen.ResolveCurrentRegenerationRow{
					ConversationUuid: conversationUUID, QuestionID: questionID, TargetParticipantID: 21,
					RegenerationKind: "application", UserInput: "regenerate this",
				},
			},
			localWork: true, cloud: true, executedBy: tc.executedBy, notNewest: tc.notNewest,
		}
		repository, err := newCurrentAgentStartRepository(localWorkProjectStoreStub{executor: executor}, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = repository.ResolveCurrentRegeneration(t.Context(), agentexecutionapp.CurrentRegenerationResolveRequest{
			ProjectID: 7, ActorUserID: 11, ResponseMessageID: "40000000-0000-4000-8000-000000000034",
		})
		if !errors.Is(err, tc.want) && (tc.want != nil || err != nil) {
			t.Fatalf("%s: err=%v, want %v", tc.name, err, tc.want)
		}
		if executor.project != 7 {
			t.Fatalf("%s: the fence read project %d, want the request's", tc.name, executor.project)
		}
	}
}

// The continuation fence is open while the cloud owns the thread.
func TestCurrentAgentContinuationInAHandedOffThreadPassesTheFence(t *testing.T) {
	executor := &localWorkRegenerationExecutorStub{
		currentRegenerationExecutorStub: &currentRegenerationExecutorStub{scriptedExecutor: &scriptedExecutor{}},
		localWork:                       true, cloud: true,
	}
	repository, err := newCurrentAgentStartRepository(localWorkProjectStoreStub{executor: executor}, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.ResolveCurrentContinuation(t.Context(), agentexecutionapp.CurrentContinuationResolveRequest{
		ProjectID: 1, ActorUserID: 11,
		ConversationUUID:  "10000000-0000-4000-8000-000000000035",
		ResponseMessageID: "40000000-0000-4000-8000-000000000035",
		Kind:              agentexecutionapp.CurrentContinuationHITL,
	})
	if errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
		t.Fatalf("a handed-off thread was refused by the Local work fence")
	}
}

// All four start paths keep the Local work fence and open it only while the
// cloud executor owns the thread (ADR-0034 decision 7). With the fence shut
// each path answers ErrLocalWorkThread before reading the turn; with it open
// each path goes on to read the turn (the stub then has no turn to give).
func TestEveryStartPathOpensTheLocalWorkFenceOnlyForTheCloud(t *testing.T) {
	const conversation = "10000000-0000-4000-8000-000000000036"
	conversationUUID, err := currentPGUUID(conversation)
	if err != nil {
		t.Fatal(err)
	}
	questionID, err := currentPGUUID("20000000-0000-4000-8000-000000000036")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]func(*CurrentAgentStartRepository) error{
		"application": func(repository *CurrentAgentStartRepository) error {
			_, err := repository.ResolveCurrentApplication(t.Context(), agentexecutionapp.CurrentApplicationStartRequest{
				ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation, TargetParticipantID: 21,
				QuestionID: "30000000-0000-4000-8000-000000000036", UserInput: "go on",
			})
			return err
		},
		"adhoc": func(repository *CurrentAgentStartRepository) error {
			_, err := repository.ResolveCurrentAdhoc(t.Context(), agentexecutionapp.CurrentAdhocStartRequest{
				ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation,
				QuestionID: "30000000-0000-4000-8000-000000000037", UserInput: "go on",
				LLMSettings: []byte(`{}`),
			})
			return err
		},
		"regeneration": func(repository *CurrentAgentStartRepository) error {
			_, err := repository.ResolveCurrentRegeneration(t.Context(), agentexecutionapp.CurrentRegenerationResolveRequest{
				ProjectID: 1, ActorUserID: 11, ResponseMessageID: "40000000-0000-4000-8000-000000000036",
			})
			return err
		},
		"continuation": func(repository *CurrentAgentStartRepository) error {
			_, err := repository.ResolveCurrentContinuation(t.Context(), agentexecutionapp.CurrentContinuationResolveRequest{
				ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation,
				ResponseMessageID: "40000000-0000-4000-8000-000000000036", Kind: agentexecutionapp.CurrentContinuationHITL,
			})
			return err
		},
	}
	for name, run := range paths {
		for _, cloud := range []bool{false, true} {
			executor := &localWorkRegenerationExecutorStub{
				currentRegenerationExecutorStub: &currentRegenerationExecutorStub{
					scriptedExecutor: &scriptedExecutor{},
					row: sqlcgen.ResolveCurrentRegenerationRow{
						ConversationUuid: conversationUUID, QuestionID: questionID, TargetParticipantID: 21,
						RegenerationKind: "application", UserInput: "regenerate this",
					},
				},
				localWork: true, cloud: cloud, executedBy: "cloud_executor",
			}
			repository, err := newCurrentAgentStartRepository(localWorkProjectStoreStub{executor: executor}, 1)
			if err != nil {
				t.Fatal(err)
			}
			err = run(repository)
			if !cloud && !errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
				t.Fatalf("%s with the desktop owning the thread: err=%v, want ErrLocalWorkThread", name, err)
			}
			if cloud && errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
				t.Fatalf("%s with the cloud owning the thread was refused by the fence", name)
			}
			if executor.asked != conversationUUID {
				t.Fatalf("%s: the fence probed %v", name, executor.asked)
			}
		}
	}
}
