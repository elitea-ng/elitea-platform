package agentexecution

import (
	"context"
	"errors"
	"math"
	"time"
)

// THE LEGACY CONTRACT THIS RESTORES.
//
// pylon served `application_task` from
// legacy/plugins/elitea_core/api/v2/application_task.py:
//
//	GET    /api/v2/elitea_core/application_task/prompt_lib/{project_id}/{task_id}
//	DELETE /api/v2/elitea_core/application_task/prompt_lib/{project_id}/{task_id}
//
// The GET answered `{"status": <arbiter task status>}` and, when the caller
// asked with `?meta=yes` or `?result=yes`, added the arbiter task meta and the
// unpickled task return value. It was gated on `models.applications.task.get`,
// which the legacy default-mode matrix grants to admin, editor AND viewer.
// The DELETE stopped the task and answered 204; its own docstring already
// called it DEPRECATED in favour of `task/{task_id}/stop`, and it is gated on
// `models.applications.task.delete`.
//
// Both raised the pylon exception text back to the caller as
// `{"ok": false, "error": str(e)}` with 400 for an unknown task id.
//
// WHAT THIS PORT KEEPS, AND WHAT IT REFUSES.
//
//   - `status` is kept, with the legacy vocabulary. The arbiter published
//     `pending`, `running`, `stopped`, `pruned` and `unknown`
//     (arbiter/tasknode/tasknode.py, watcher.py, housekeeper.py). The Go
//     runtime's execution_jobs.state is mapped onto the first three; there is
//     no pruning of a durable PostgreSQL job, so `pruned` cannot occur.
//   - `stopping` and `settled_at` are ADDED. A poller needs to distinguish "a
//     stop is requested but the worker has not settled yet" from "still
//     running", and the arbiter conflated the two.
//   - `meta` and `result` are REFUSED, not faked. The arbiter meta is
//     transport bookkeeping of a task node this repository does not have, and
//     the result was a gzipped pickle of the worker's return value —
//     the exact payload shape AGENTS.md forbids carrying forward. The answer
//     already exists as chat message items on the response message, which is
//     the identifier this route takes. The route answers 400 when a caller
//     asks for either, instead of answering 200 with an empty object: a 200
//     that silently drops what was asked for is the issue-128 pattern.
//   - The task identity is the RESPONSE MESSAGE UUID, not an arbiter task id.
//     The Go cancel route made the same substitution and for the same reason:
//     the server resolves the durable execution from a projection it owns
//     rather than trusting a client-held execution id.
//   - The error text is not echoed. An unknown, foreign or unbound id is one
//     404 with one message, so the route cannot be used to probe which
//     response messages exist in a project.
var (
	ErrInvalidCurrentAgentTaskStatus  = errors.New("invalid current agent task status request")
	ErrCurrentAgentTaskStatusNotFound = errors.New("current agent task is not readable by this actor")
	ErrCurrentAgentTaskStatusFailed   = errors.New("current agent task status is unavailable")
)

// Legacy status vocabulary. See the block comment above.
const (
	CurrentAgentTaskStatusPending = "pending"
	CurrentAgentTaskStatusRunning = "running"
	CurrentAgentTaskStatusStopped = "stopped"
)

// CurrentAgentTaskStatusRequest names the run through the response message the
// caller can already see, and the actor whose ownership the store re-checks.
type CurrentAgentTaskStatusRequest struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (request CurrentAgentTaskStatusRequest) Validate() error {
	if request.ProjectID <= 0 || request.ProjectID > math.MaxInt32 ||
		request.ActorUserID <= 0 || !validUUID(request.ResponseMessageID) {
		return ErrInvalidCurrentAgentTaskStatus
	}
	return nil
}

// CurrentAgentTaskState is the bounded runtime state the store returns. It
// carries no prompt, no tool payload and no upstream error text.
type CurrentAgentTaskState struct {
	State             string
	DesiredState      string
	SettledAt         *time.Time
	TerminalErrorCode string
}

// CurrentAgentTaskStatusOutcome is the exact answer the route serialises.
type CurrentAgentTaskStatusOutcome struct {
	Status    string
	Stopping  bool
	SettledAt *time.Time
	ErrorCode string
}

type CurrentAgentTaskStatusStore interface {
	ReadCurrentAgentTaskState(
		context.Context,
		CurrentAgentTaskStatusRequest,
	) (CurrentAgentTaskState, error)
}

type CurrentAgentTaskStatusService struct {
	store CurrentAgentTaskStatusStore
}

func NewCurrentAgentTaskStatusService(
	store CurrentAgentTaskStatusStore,
) (*CurrentAgentTaskStatusService, error) {
	if store == nil {
		return nil, errors.New("current agent task status store is required")
	}
	return &CurrentAgentTaskStatusService{store: store}, nil
}

func (service *CurrentAgentTaskStatusService) Status(
	ctx context.Context,
	request CurrentAgentTaskStatusRequest,
) (CurrentAgentTaskStatusOutcome, error) {
	if service == nil || service.store == nil || ctx == nil {
		return CurrentAgentTaskStatusOutcome{}, ErrInvalidCurrentAgentTaskStatus
	}
	if err := request.Validate(); err != nil {
		return CurrentAgentTaskStatusOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return CurrentAgentTaskStatusOutcome{}, err
	}

	state, err := service.store.ReadCurrentAgentTaskState(ctx, request)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return CurrentAgentTaskStatusOutcome{}, contextError
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrCurrentAgentTaskStatusNotFound) {
			return CurrentAgentTaskStatusOutcome{}, err
		}
		return CurrentAgentTaskStatusOutcome{}, ErrCurrentAgentTaskStatusFailed
	}

	status, mapped := currentAgentTaskStatus(state.State)
	if !mapped {
		// An unmapped state means the execution-state vocabulary moved and this
		// mapping did not. Answering `unknown` would let a poller wait forever
		// on a run that already settled, so the route fails loudly instead.
		return CurrentAgentTaskStatusOutcome{}, ErrCurrentAgentTaskStatusFailed
	}
	return CurrentAgentTaskStatusOutcome{
		Status:    status,
		Stopping:  state.DesiredState == "CANCELLED" && status != CurrentAgentTaskStatusStopped,
		SettledAt: state.SettledAt,
		ErrorCode: state.TerminalErrorCode,
	}, nil
}

// currentAgentTaskStatus maps elitea_runtime.execution_jobs.state onto the
// arbiter vocabulary the legacy clients parse. The eight states come from the
// execution_jobs_state CHECK constraint in migrations/shared/0030.
func currentAgentTaskStatus(state string) (string, bool) {
	switch state {
	case "PENDING", "DISPATCHED":
		return CurrentAgentTaskStatusPending, true
	case "CLAIMED", "RUNNING", "SETTLING":
		return CurrentAgentTaskStatusRunning, true
	case "SUCCEEDED", "FAILED", "CANCELLED":
		return CurrentAgentTaskStatusStopped, true
	default:
		return "", false
	}
}
