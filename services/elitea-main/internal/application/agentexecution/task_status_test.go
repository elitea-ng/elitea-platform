package agentexecution

import (
	"context"
	"errors"
	"testing"
	"time"
)

type currentAgentTaskStatusStoreStub struct {
	request CurrentAgentTaskStatusRequest
	state   CurrentAgentTaskState
	err     error
	calls   int
}

func (stub *currentAgentTaskStatusStoreStub) ReadCurrentAgentTaskState(
	_ context.Context,
	request CurrentAgentTaskStatusRequest,
) (CurrentAgentTaskState, error) {
	stub.calls++
	stub.request = request
	return stub.state, stub.err
}

// TestCurrentAgentTaskStatusServiceMapsEveryExecutionState walks the COMPLETE
// execution_jobs_state CHECK constraint from migrations/shared/0030. A state
// this switch does not name would answer 502 rather than silently reporting a
// settled run as still running, and this test is what proves no state is
// missing.
func TestCurrentAgentTaskStatusServiceMapsEveryExecutionState(t *testing.T) {
	for state, want := range map[string]string{
		"PENDING":    CurrentAgentTaskStatusPending,
		"DISPATCHED": CurrentAgentTaskStatusPending,
		"CLAIMED":    CurrentAgentTaskStatusRunning,
		"RUNNING":    CurrentAgentTaskStatusRunning,
		"SETTLING":   CurrentAgentTaskStatusRunning,
		"SUCCEEDED":  CurrentAgentTaskStatusStopped,
		"FAILED":     CurrentAgentTaskStatusStopped,
		"CANCELLED":  CurrentAgentTaskStatusStopped,
	} {
		t.Run(state, func(t *testing.T) {
			store := &currentAgentTaskStatusStoreStub{
				state: CurrentAgentTaskState{State: state, DesiredState: "RUNNING"},
			}
			service := newCurrentAgentTaskStatusService(t, store)
			outcome, err := service.Status(t.Context(), validCurrentAgentTaskStatusRequest())
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Status != want || outcome.Stopping {
				t.Fatalf("outcome=%+v want status=%q", outcome, want)
			}
		})
	}
}

// TestCurrentAgentTaskStatusServiceReportsStoppingOnlyBeforeTheRunSettles is
// the distinction the arbiter could not make. `desired_state = CANCELLED` on a
// still-running job is a stop in flight; on a settled job it is history.
func TestCurrentAgentTaskStatusServiceReportsStoppingOnlyBeforeTheRunSettles(t *testing.T) {
	for _, test := range []struct {
		state        string
		desiredState string
		wantStatus   string
		wantStopping bool
	}{
		{state: "RUNNING", desiredState: "CANCELLED", wantStatus: "running", wantStopping: true},
		{state: "PENDING", desiredState: "CANCELLED", wantStatus: "pending", wantStopping: true},
		{state: "CANCELLED", desiredState: "CANCELLED", wantStatus: "stopped", wantStopping: false},
		{state: "SUCCEEDED", desiredState: "CANCELLED", wantStatus: "stopped", wantStopping: false},
		{state: "RUNNING", desiredState: "DRAINING", wantStatus: "running", wantStopping: false},
	} {
		t.Run(test.state+"/"+test.desiredState, func(t *testing.T) {
			store := &currentAgentTaskStatusStoreStub{
				state: CurrentAgentTaskState{State: test.state, DesiredState: test.desiredState},
			}
			service := newCurrentAgentTaskStatusService(t, store)
			outcome, err := service.Status(t.Context(), validCurrentAgentTaskStatusRequest())
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Status != test.wantStatus || outcome.Stopping != test.wantStopping {
				t.Fatalf("outcome=%+v want status=%q stopping=%v",
					outcome, test.wantStatus, test.wantStopping)
			}
		})
	}
}

func TestCurrentAgentTaskStatusServiceCarriesSettlementAndTerminalCode(t *testing.T) {
	settledAt := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	store := &currentAgentTaskStatusStoreStub{
		state: CurrentAgentTaskState{
			State:             "FAILED",
			DesiredState:      "RUNNING",
			SettledAt:         &settledAt,
			TerminalErrorCode: "DEADLINE_EXCEEDED",
		},
	}
	service := newCurrentAgentTaskStatusService(t, store)
	outcome, err := service.Status(t.Context(), validCurrentAgentTaskStatusRequest())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "stopped" || outcome.SettledAt == nil ||
		!outcome.SettledAt.Equal(settledAt) || outcome.ErrorCode != "DEADLINE_EXCEEDED" {
		t.Fatalf("outcome=%+v", outcome)
	}
}

// TestCurrentAgentTaskStatusServiceRefusesAnUnmappedState is the guard against
// a state vocabulary that moves without this mapping. Answering a made-up
// status would leave a poller waiting on a run that already ended.
func TestCurrentAgentTaskStatusServiceRefusesAnUnmappedState(t *testing.T) {
	store := &currentAgentTaskStatusStoreStub{
		state: CurrentAgentTaskState{State: "QUARANTINED", DesiredState: "RUNNING"},
	}
	service := newCurrentAgentTaskStatusService(t, store)
	if _, err := service.Status(t.Context(), validCurrentAgentTaskStatusRequest()); !errors.Is(
		err, ErrCurrentAgentTaskStatusFailed) {
		t.Fatalf("error=%v", err)
	}
}

func TestCurrentAgentTaskStatusServiceValidatesBeforeReadingAndKeepsNotFoundDistinct(t *testing.T) {
	for name, test := range map[string]struct {
		request   CurrentAgentTaskStatusRequest
		storeErr  error
		wantErr   error
		wantCalls int
	}{
		"no project": {
			request: CurrentAgentTaskStatusRequest{
				ActorUserID: 11, ResponseMessageID: "10000000-0000-4000-8000-000000000061"},
			wantErr: ErrInvalidCurrentAgentTaskStatus,
		},
		"no actor": {
			request: CurrentAgentTaskStatusRequest{
				ProjectID: 7, ResponseMessageID: "10000000-0000-4000-8000-000000000061"},
			wantErr: ErrInvalidCurrentAgentTaskStatus,
		},
		"malformed response id": {
			request: CurrentAgentTaskStatusRequest{
				ProjectID: 7, ActorUserID: 11, ResponseMessageID: "10000000"},
			wantErr: ErrInvalidCurrentAgentTaskStatus,
		},
		"project above int32": {
			request: CurrentAgentTaskStatusRequest{
				ProjectID: 1 << 40, ActorUserID: 11,
				ResponseMessageID: "10000000-0000-4000-8000-000000000061"},
			wantErr: ErrInvalidCurrentAgentTaskStatus,
		},
		"not found stays not found": {
			request:   validCurrentAgentTaskStatusRequest(),
			storeErr:  ErrCurrentAgentTaskStatusNotFound,
			wantErr:   ErrCurrentAgentTaskStatusNotFound,
			wantCalls: 1,
		},
		"any other store failure is generalised": {
			request:   validCurrentAgentTaskStatusRequest(),
			storeErr:  errors.New("pool exhausted"),
			wantErr:   ErrCurrentAgentTaskStatusFailed,
			wantCalls: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &currentAgentTaskStatusStoreStub{err: test.storeErr}
			service := newCurrentAgentTaskStatusService(t, store)
			if _, err := service.Status(t.Context(), test.request); !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
			if store.calls != test.wantCalls {
				t.Fatalf("store calls=%d want=%d", store.calls, test.wantCalls)
			}
		})
	}
}

func TestNewCurrentAgentTaskStatusServiceRequiresAStore(t *testing.T) {
	if _, err := NewCurrentAgentTaskStatusService(nil); err == nil {
		t.Fatal("a service with no store was constructed")
	}
	var service *CurrentAgentTaskStatusService
	if _, err := service.Status(context.Background(), validCurrentAgentTaskStatusRequest()); !errors.Is(
		err, ErrInvalidCurrentAgentTaskStatus) {
		t.Fatalf("error=%v", err)
	}
}

func newCurrentAgentTaskStatusService(
	t *testing.T,
	store CurrentAgentTaskStatusStore,
) *CurrentAgentTaskStatusService {
	t.Helper()
	service, err := NewCurrentAgentTaskStatusService(store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func validCurrentAgentTaskStatusRequest() CurrentAgentTaskStatusRequest {
	return CurrentAgentTaskStatusRequest{
		ProjectID: 7, ActorUserID: 11,
		ResponseMessageID: "10000000-0000-4000-8000-000000000061",
	}
}
