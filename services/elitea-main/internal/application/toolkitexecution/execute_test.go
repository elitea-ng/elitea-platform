package toolkitexecution

import (
	"context"
	"errors"
	"testing"
	"time"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type executionFreezerStub struct {
	request FreezeCurrentReadToolRequest
	frozen  FrozenCurrentReadTool
	err     error
	calls   int
}

func (s *executionFreezerStub) Freeze(
	_ context.Context,
	request FreezeCurrentReadToolRequest,
) (FrozenCurrentReadTool, error) {
	s.calls++
	s.request = request
	return s.frozen.Clone(), s.err
}

type executionSubmitterStub struct {
	request SubmitRequest
	outcome executionapp.AdmissionOutcome
	err     error
	calls   int
}

func (s *executionSubmitterStub) Submit(
	_ context.Context,
	request SubmitRequest,
) (executionapp.AdmissionOutcome, error) {
	s.calls++
	s.request = request
	return s.outcome, s.err
}

type executionWaiterStub struct {
	executionID string
	generation  uint64
	completion  Completion
	err         error
	calls       int
}

func (s *executionWaiterStub) Wait(
	_ context.Context,
	executionID string,
	generation uint64,
) (Completion, error) {
	s.calls++
	s.executionID = executionID
	s.generation = generation
	return s.completion.Clone(), s.err
}

func TestCurrentReadToolExecutionRunsOneExactDurableInvocation(t *testing.T) {
	frozen := validFrozenCurrentReadTool()
	freezer := &executionFreezerStub{frozen: frozen}
	admittedAt := time.Now().UTC()
	submitter := &executionSubmitterStub{outcome: executionapp.AdmissionOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", Created: true,
		AdmittedAt: admittedAt, Deadline: admittedAt.Add(time.Minute),
	}}
	waiter := &executionWaiterStub{completion: Completion{
		State: executiondomain.JobSucceeded, ResultJSON: []byte(`{"ok":true}`),
		ToolkitType: frozen.ToolkitType, ToolkitName: frozen.ToolkitName, ToolName: frozen.ToolName,
	}}
	service, err := NewCurrentReadToolExecutionService(freezer, submitter, waiter)
	if err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{"issue": "EL-42"}
	outcome, err := service.Execute(context.Background(), validExecuteCurrentReadToolRequest(arguments))
	if err != nil {
		t.Fatal(err)
	}
	if freezer.calls != 1 || submitter.calls != 1 || waiter.calls != 1 ||
		freezer.request.ToolkitID != 73 || freezer.request.ToolName != "get_issue" ||
		submitter.request.IdempotencyKey != "mcp-call-1" ||
		waiter.executionID != "execution-1" || waiter.generation != 1 ||
		outcome.ExecutionID != "execution-1" || string(outcome.Completion.ResultJSON) != `{"ok":true}` {
		t.Fatalf("freezer=%#v submitter=%#v waiter=%#v outcome=%#v", freezer, submitter, waiter, outcome)
	}
	arguments["issue"] = "mutated"
	if freezer.request.Arguments["issue"] != "EL-42" {
		t.Fatal("caller mutation changed the frozen invocation request")
	}
}

func TestCurrentReadToolExecutionStopsAtEachFailedBoundary(t *testing.T) {
	freezeErr := errors.New("freeze failed")
	freezer := &executionFreezerStub{frozen: validFrozenCurrentReadTool(), err: freezeErr}
	submitter := &executionSubmitterStub{}
	waiter := &executionWaiterStub{}
	service, err := NewCurrentReadToolExecutionService(freezer, submitter, waiter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(
		context.Background(), validExecuteCurrentReadToolRequest(map[string]any{}),
	); !errors.Is(err, freezeErr) || submitter.calls != 0 || waiter.calls != 0 {
		t.Fatalf("freeze failure = %v, submit=%d wait=%d", err, submitter.calls, waiter.calls)
	}

	freezer.err = nil
	submitter.err = executionapp.ErrAdmissionCapacityExhausted
	if _, err := service.Execute(
		context.Background(), validExecuteCurrentReadToolRequest(map[string]any{}),
	); !errors.Is(err, executionapp.ErrAdmissionCapacityExhausted) || waiter.calls != 0 {
		t.Fatalf("admission failure = %v, wait=%d", err, waiter.calls)
	} else if stage := CurrentReadToolAdmissionStageOf(err); stage != CurrentReadToolAdmissionDurableWrite {
		t.Fatalf("admission failure stage = %q, want %q", stage, CurrentReadToolAdmissionDurableWrite)
	}
}

func TestCurrentReadToolExecutionRetainsExecutionIdentityOnWaitFailure(t *testing.T) {
	frozen := validFrozenCurrentReadTool()
	freezer := &executionFreezerStub{frozen: frozen}
	submitter := &executionSubmitterStub{outcome: executionapp.AdmissionOutcome{ExecutionID: "execution-7"}}
	waiter := &executionWaiterStub{err: context.DeadlineExceeded}
	service, err := NewCurrentReadToolExecutionService(freezer, submitter, waiter)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := service.Execute(
		context.Background(), validExecuteCurrentReadToolRequest(map[string]any{}),
	)
	if !errors.Is(err, context.DeadlineExceeded) || outcome.ExecutionID != "execution-7" {
		t.Fatalf("outcome=%#v error=%v", outcome, err)
	}

	waiter.err = nil
	waiter.completion = Completion{
		State: executiondomain.JobSucceeded, ResultJSON: []byte(`true`),
		ToolkitType: frozen.ToolkitType, ToolkitName: "another", ToolName: frozen.ToolName,
	}
	outcome, err = service.Execute(
		context.Background(), validExecuteCurrentReadToolRequest(map[string]any{}),
	)
	if !errors.Is(err, ErrToolkitExecuteReadResultMismatch) || outcome.ExecutionID != "execution-7" {
		t.Fatalf("mismatch outcome=%#v error=%v", outcome, err)
	}
}

func TestCurrentReadToolExecutionRejectsIdentityDriftBeforeDependencies(t *testing.T) {
	freezer := &executionFreezerStub{}
	service, err := NewCurrentReadToolExecutionService(
		freezer, &executionSubmitterStub{}, &executionWaiterStub{},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := validExecuteCurrentReadToolRequest(map[string]any{})
	request.Identity.ActorID = "8"
	if _, err := service.Execute(context.Background(), request); !errors.Is(err, ErrInvalidCurrentReadTool) {
		t.Fatalf("identity drift error = %v", err)
	}
	if freezer.calls != 0 {
		t.Fatal("identity drift reached the current toolkit reader")
	}
}

func validExecuteCurrentReadToolRequest(arguments map[string]any) ExecuteCurrentReadToolRequest {
	return ExecuteCurrentReadToolRequest{
		Identity: executionapp.AdmissionIdentity{
			TenantID: "17", ResourceProjectID: "17", ProjectionProjectID: "17", ActorID: "7",
		},
		IdempotencyKey: "mcp-call-1",
		ProjectID:      17, ActorID: 7, ToolkitID: 73,
		ToolName: "get_issue", Arguments: arguments,
	}
}
