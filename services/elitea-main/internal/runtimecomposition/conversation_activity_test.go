package runtimecomposition

import (
	"context"
	"errors"
	"sync"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/conversationactivity"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
)

// activityLookups records which conversation the notifier was asked to read.
type activityLookups struct {
	mu    sync.Mutex
	calls []string
}

func (s *activityLookups) record(call string) (conversationactivity.Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	return conversationactivity.Snapshot{}, false, nil
}

func (s *activityLookups) ConversationByUUID(_ context.Context, project int64, uuid string) (conversationactivity.Snapshot, bool, error) {
	return s.record("conversation " + uuid)
}

func (s *activityLookups) ConversationByExecution(_ context.Context, execution string) (conversationactivity.Snapshot, bool, error) {
	return s.record("execution " + execution)
}

func (s *activityLookups) ConversationByMessage(_ context.Context, _ int64, message string) (conversationactivity.Snapshot, bool, error) {
	return s.record("message " + message)
}

type activityNoBus struct{}

func (activityNoBus) Publish(context.Context, int64, conversationactivity.Audience, conversationactivity.Frame) error {
	return nil
}

type admitStore struct{ err error }

func (s admitStore) AdmitAgentExecution(context.Context, agentexecutionapp.Admission) (executionapp.AdmissionOutcome, error) {
	return executionapp.AdmissionOutcome{}, s.err
}

type cancelStore struct {
	outcome agentexecutionapp.CurrentAgentCancelOutcome
	err     error
}

func (s cancelStore) CancelCurrentAgent(context.Context, agentexecutionapp.CurrentAgentCancelRequest) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	return s.outcome, s.err
}

// Each wrapper asks for the conversation's new state only after its inner
// call succeeded, and a stop that deleted the turn asks for nothing.
func TestConversationActivityWrappersNotifyAfterACommit(t *testing.T) {
	lookups := &activityLookups{}
	notifier := conversationactivity.New(lookups, activityNoBus{}, nil, conversationactivity.Synchronous())
	admission := agentexecutionapp.Admission{}
	admission.Record.Job.ProjectionProjectID = "4"
	admission.Binding.ClientStreamID = "conv-uuid"

	if _, err := (activityAdmissionStore{next: admitStore{}, activity: notifier}).AdmitAgentExecution(context.Background(), admission); err != nil {
		t.Fatal(err)
	}
	if _, err := (activityAdmissionStore{next: admitStore{err: errors.New("refused")}, activity: notifier}).AdmitAgentExecution(context.Background(), admission); err == nil {
		t.Fatal("the inner error was lost")
	}
	request := agentexecutionapp.CurrentAgentCancelRequest{ProjectID: 4, ResponseMessageID: "msg-uuid"}
	if _, err := (activityCancellationStore{next: cancelStore{}, activity: notifier}).CancelCurrentAgent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	deleted := cancelStore{outcome: agentexecutionapp.CurrentAgentCancelOutcome{Deleted: true}}
	if _, err := (activityCancellationStore{next: deleted, activity: notifier}).CancelCurrentAgent(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	hooks := activitySettlementHooks(notifier)
	if len(hooks) != 1 || activitySettlementHooks(nil) != nil {
		t.Fatalf("settlement hooks = %d, want 1 (and none without a notifier)", len(hooks))
	}

	lookups.mu.Lock()
	defer lookups.mu.Unlock()
	want := []string{"conversation conv-uuid", "message msg-uuid"}
	if len(lookups.calls) != len(want) || lookups.calls[0] != want[0] || lookups.calls[1] != want[1] {
		t.Fatalf("lookups = %v, want %v", lookups.calls, want)
	}
}
