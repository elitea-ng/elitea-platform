package toolkitcalltool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

var ErrToolRunNotFound = errors.New("tool run not found")

type ResultRequest struct {
	ProjectID   int64
	ActorUserID int64
	ToolkitID   int64
	ExecutionID string
}

func (r ResultRequest) Validate() error {
	if r.ProjectID <= 0 || r.ActorUserID <= 0 || r.ToolkitID <= 0 || r.ExecutionID == "" || len(r.ExecutionID) > 128 || !utf8.ValidString(r.ExecutionID) || strings.ContainsAny(r.ExecutionID, "\x00\r\n/") || strings.TrimSpace(r.ExecutionID) != r.ExecutionID {
		return ErrInvalidToolRun
	}
	return nil
}

// ResultBindingReader loads the original identity after checking caller ownership.
// It must not resolve current settings or admit another execution.
type ResultBindingReader interface {
	ReadToolkitCallToolResultBinding(context.Context, ResultRequest) (executiondomain.ToolkitCallToolBinding, string, error)
}

// AuthorizationRetry contains the original caller arguments, never toolkit settings.
type AuthorizationRetry struct {
	ToolName   string          `json:"tool_name"`
	ToolParams json.RawMessage `json:"tool_params"`
}
type ResultArgumentsReader interface {
	ReadToolkitCallToolAuthorizationRequest(context.Context, ResultRequest) (AuthorizationRetry, error)
}

// ReadToolRun returns a durable result, or pending=true while execution continues.
func (s *RunService) ReadToolRun(ctx context.Context, request ResultRequest) (RunOutcome, bool, error) {
	if s == nil || ctx == nil {
		return RunOutcome{}, false, ErrInvalidToolRun
	}
	if err := request.Validate(); err != nil {
		return RunOutcome{}, false, err
	}
	reader, ok := s.settlements.(ResultBindingReader)
	if !ok {
		return RunOutcome{}, false, errors.New("tool run result reader unavailable")
	}
	binding, state, err := reader.ReadToolkitCallToolResultBinding(ctx, request)
	if err != nil {
		return RunOutcome{}, false, err
	}
	settlement, found, err := s.settlements.ReadToolkitCallToolSettlement(ctx, request.ExecutionID, 1)
	if err != nil {
		return RunOutcome{}, false, err
	}
	if found {
		result, err := decodeSettlement(AdmittedRun{Outcome: executionapp.AdmissionOutcome{ExecutionID: request.ExecutionID}, Binding: binding}, settlement)
		if err == nil && result.Status == RunStatusAuthorizationRequired {
			retryReader, ok := s.settlements.(ResultArgumentsReader)
			if !ok {
				return RunOutcome{}, false, errors.New("tool authorization recovery reader unavailable")
			}
			retry, readErr := retryReader.ReadToolkitCallToolAuthorizationRequest(ctx, request)
			if readErr != nil {
				return RunOutcome{}, false, readErr
			}
			if retry.ToolName == "" || len(retry.ToolName) > 256 || !validBoundedJSONObject(retry.ToolParams) {
				return RunOutcome{}, false, ErrInvalidToolRun
			}
			result.AuthorizationRetry = &retry
		}
		return result, false, err
	}
	switch executiondomain.JobState(state) {
	case executiondomain.JobPending, executiondomain.JobDispatched, executiondomain.JobClaimed, executiondomain.JobRunning, executiondomain.JobSettling:
		return RunOutcome{ExecutionID: request.ExecutionID}, true, nil
	default:
		return RunOutcome{ExecutionID: request.ExecutionID, Status: RunStatusRuntimeFailure, ErrorMessage: "The tool execution ended without a recoverable result."}, false, nil
	}
}
