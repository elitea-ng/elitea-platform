package toolkitexecution

import (
	"context"
	"errors"
	"strconv"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

var ErrToolkitExecuteReadResultMismatch = errors.New("direct toolkit result does not match admitted invocation")

// ExecuteCurrentReadToolRequest is the authorized application-level request
// for one externally exposed toolkit operation. The identity is repeated in
// its typed numeric form so this boundary can prove that authorization,
// tenant routing, and durable admission all name the same actor and project.
type ExecuteCurrentReadToolRequest struct {
	Identity       executionapp.AdmissionIdentity
	IdempotencyKey string
	ProjectID      int32
	ActorID        int32
	ToolkitID      int32
	ToolName       string
	Arguments      map[string]any
}

type CurrentReadToolExecutionOutcome struct {
	ExecutionID string
	Completion  Completion
}

type currentReadToolFreezer interface {
	Freeze(context.Context, FreezeCurrentReadToolRequest) (FrozenCurrentReadTool, error)
}

type currentReadToolSubmitter interface {
	Submit(context.Context, SubmitRequest) (executionapp.AdmissionOutcome, error)
}

type currentReadToolWaiter interface {
	Wait(context.Context, string, uint64) (Completion, error)
}

// CurrentReadToolExecutionService composes the durable direct-tool lifecycle:
// re-read and freeze the exact current row, atomically admit its immutable
// input, then wait for the fenced terminal result. It deliberately contains no
// SQL and no transport concerns.
type CurrentReadToolExecutionService struct {
	freezer    currentReadToolFreezer
	admissions currentReadToolSubmitter
	results    currentReadToolWaiter
}

func NewCurrentReadToolExecutionService(
	freezer currentReadToolFreezer,
	admissions currentReadToolSubmitter,
	results currentReadToolWaiter,
) (*CurrentReadToolExecutionService, error) {
	if freezer == nil || admissions == nil || results == nil {
		return nil, errors.New("direct toolkit execution dependencies are required")
	}
	return &CurrentReadToolExecutionService{
		freezer: freezer, admissions: admissions, results: results,
	}, nil
}

func (s *CurrentReadToolExecutionService) Execute(
	ctx context.Context,
	request ExecuteCurrentReadToolRequest,
) (CurrentReadToolExecutionOutcome, error) {
	if s == nil || ctx == nil || !validExecutionRequest(request) {
		return CurrentReadToolExecutionOutcome{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionInput, ErrInvalidCurrentReadTool,
		)
	}
	if err := ctx.Err(); err != nil {
		return CurrentReadToolExecutionOutcome{}, err
	}

	frozen, err := s.freezer.Freeze(ctx, FreezeCurrentReadToolRequest{
		ProjectID: request.ProjectID,
		ActorID:   request.ActorID,
		ToolkitID: request.ToolkitID,
		ToolName:  request.ToolName,
		Arguments: cloneArguments(request.Arguments),
	})
	if err != nil {
		return CurrentReadToolExecutionOutcome{}, err
	}
	admitted, err := s.admissions.Submit(ctx, SubmitRequest{
		Identity:       request.Identity,
		IdempotencyKey: request.IdempotencyKey,
		Frozen:         frozen,
	})
	if err != nil {
		return CurrentReadToolExecutionOutcome{}, currentReadToolAdmissionError(
			CurrentReadToolAdmissionDurableWrite, err,
		)
	}
	outcome := CurrentReadToolExecutionOutcome{ExecutionID: admitted.ExecutionID}
	completion, err := s.results.Wait(ctx, admitted.ExecutionID, 1)
	if err != nil {
		return outcome, err
	}
	if completion.ToolkitType != frozen.ToolkitType ||
		completion.ToolkitName != frozen.ToolkitName ||
		completion.ToolName != frozen.ToolName {
		return outcome, ErrToolkitExecuteReadResultMismatch
	}
	outcome.Completion = completion.Clone()
	return outcome, nil
}

func validExecutionRequest(request ExecuteCurrentReadToolRequest) bool {
	if request.ProjectID <= 0 || request.ActorID <= 0 || request.ToolkitID <= 0 ||
		!validIdentity(request.ToolName) || request.Arguments == nil ||
		request.IdempotencyKey == "" {
		return false
	}
	projectID := strconv.FormatInt(int64(request.ProjectID), 10)
	actorID := strconv.FormatInt(int64(request.ActorID), 10)
	return request.Identity.TenantID == projectID &&
		request.Identity.ResourceProjectID == projectID &&
		request.Identity.ProjectionProjectID == projectID &&
		request.Identity.ActorID == actorID
}

func cloneArguments(arguments map[string]any) map[string]any {
	cloned := make(map[string]any, len(arguments))
	for key, value := range arguments {
		cloned[key] = value
	}
	return cloned
}
