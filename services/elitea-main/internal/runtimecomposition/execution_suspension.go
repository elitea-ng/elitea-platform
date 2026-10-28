package runtimecomposition

import (
	"context"
	"strconv"

	"github.com/nats-io/nats.go"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	toolkitcalltoolapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

// The kill switch at admission (ADR-0041 decision 1, #1493).
//
// Every run start that writes an execution job passes through one of the
// four admission stores below, so a decorator on each is the one place the
// check needs: agent runs (start, regenerate, ad hoc, every continue and
// resume, pipeline triggers and schedules, MCP, the support assistant and
// desktop child turns all reach AgentExecutionJobsRepository), index ingest
// (manual and scheduled), and the two direct toolkit runs. The check runs
// BEFORE the store, so a refused run writes nothing. A retry of a run
// admitted before the suspension is refused too: the switch wins over
// idempotent replay.
//
// Not decorated: toolkit discovery (a catalogue read of a toolkit's tool
// list, no agent and no tool call) and configuration validation.

func projectOf(value string) int64 {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// agentAdmissionApplicationID is the agent of an agent admission; 0 for an
// ad hoc chat.
func agentAdmissionApplicationID(admission agentexecutionapp.Admission) int64 {
	switch {
	case admission.CurrentTurn != nil:
		return admission.CurrentTurn.ApplicationID
	case admission.CurrentRegenerateTurn != nil:
		return admission.CurrentRegenerateTurn.ApplicationID
	case admission.CurrentContinueTurn != nil:
		return admission.CurrentContinueTurn.ApplicationID
	}
	return 0
}

type suspensionAgentAdmissionStore struct {
	next agentexecutionapp.AtomicAdmissionStore
	gate *safety.Gate
}

func (s suspensionAgentAdmissionStore) AdmitAgentExecution(
	ctx context.Context, admission agentexecutionapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := s.gate.Check(ctx, safety.Subject{
		ProjectID:     projectOf(admission.Record.Job.ResourceProjectID),
		ApplicationID: agentAdmissionApplicationID(admission),
	}); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	return s.next.AdmitAgentExecution(ctx, admission)
}

type suspensionIndexAdmissionStore struct {
	next indexingapp.AtomicAdmissionStore
	gate *safety.Gate
}

func (s suspensionIndexAdmissionStore) AdmitIndexIngest(
	ctx context.Context, admission indexingapp.Admission,
) (indexingapp.AdmissionOutcome, error) {
	if err := s.gate.Check(ctx, safety.Subject{ProjectID: projectOf(admission.Record.Job.ResourceProjectID)}); err != nil {
		return indexingapp.AdmissionOutcome{}, err
	}
	return s.next.AdmitIndexIngest(ctx, admission)
}

type suspensionToolkitExecuteStore struct {
	next toolkitexecutionapp.AtomicAdmissionStore
	gate *safety.Gate
}

// AdmitToolkitExecuteRead checks the platform and project scopes. The
// direct read's binding does not name its toolkit type; the type scope
// reaches it through the frozen guardrails, refused by the worker.
func (s suspensionToolkitExecuteStore) AdmitToolkitExecuteRead(
	ctx context.Context, admission toolkitexecutionapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := s.gate.Check(ctx, safety.Subject{ProjectID: projectOf(admission.Record.Job.ResourceProjectID)}); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	return s.next.AdmitToolkitExecuteRead(ctx, admission)
}

type suspensionToolkitCallToolStore struct {
	next toolkitcalltoolapp.AtomicAdmissionStore
	gate *safety.Gate
}

func (s suspensionToolkitCallToolStore) AdmitToolkitCallTool(
	ctx context.Context, admission toolkitcalltoolapp.Admission,
) (executionapp.AdmissionOutcome, error) {
	if err := s.gate.Check(ctx, safety.Subject{
		ProjectID:   projectOf(admission.Record.Job.ResourceProjectID),
		ToolkitType: admission.Binding.ToolkitType,
	}); err != nil {
		return executionapp.AdmissionOutcome{}, err
	}
	return s.next.AdmitToolkitCallTool(ctx, admission)
}

// guardAgentAdmissions, guardIndexAdmissions, guardToolkitExecute and
// guardToolkitCallTool wrap a store when the gate is composed; a nil gate
// leaves the store as it was.
func guardAgentAdmissions(store agentexecutionapp.AtomicAdmissionStore, gate *safety.Gate) agentexecutionapp.AtomicAdmissionStore {
	if gate == nil {
		return store
	}
	return suspensionAgentAdmissionStore{next: store, gate: gate}
}

func guardIndexAdmissions(store indexingapp.AtomicAdmissionStore, gate *safety.Gate) indexingapp.AtomicAdmissionStore {
	if gate == nil {
		return store
	}
	return suspensionIndexAdmissionStore{next: store, gate: gate}
}

func guardToolkitExecute(store toolkitexecutionapp.AtomicAdmissionStore, gate *safety.Gate) toolkitexecutionapp.AtomicAdmissionStore {
	if gate == nil {
		return store
	}
	return suspensionToolkitExecuteStore{next: store, gate: gate}
}

func guardToolkitCallTool(store toolkitcalltoolapp.AtomicAdmissionStore, gate *safety.Gate) toolkitcalltoolapp.AtomicAdmissionStore {
	if gate == nil {
		return store
	}
	return suspensionToolkitCallToolStore{next: store, gate: gate}
}

const (
	suspensionInvalidationPendingMessages = 256
	suspensionInvalidationPendingBytes    = 256 << 10
)

// natsSuspensionTransport carries the suspension invalidation fan-out on
// core NATS (commandbus.SafetySuspensionSubject, the elitea-main-runtime
// identity, which both publishes and subscribes). Reference-only: a
// suspension id, never a reason or an actor.
type natsSuspensionTransport struct{ conn *nats.Conn }

func (t natsSuspensionTransport) Publish(data []byte) error {
	return t.conn.Publish(commandbus.SafetySuspensionSubject, data)
}

func (t natsSuspensionTransport) Subscribe(deliver func([]byte)) (func() error, error) {
	sub, err := t.conn.Subscribe(commandbus.SafetySuspensionSubject, func(msg *nats.Msg) { deliver(msg.Data) })
	if err != nil {
		return nil, err
	}
	if err := sub.SetPendingLimits(suspensionInvalidationPendingMessages, suspensionInvalidationPendingBytes); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub.Unsubscribe, nil
}

// suspensionInvalidationRunner runs the gate's fan-out as a runtime
// publisher.
type suspensionInvalidationRunner struct {
	gate      *safety.Gate
	transport safety.Transport
}

func (r suspensionInvalidationRunner) Run(ctx context.Context) error {
	return r.gate.Run(ctx, r.transport)
}
