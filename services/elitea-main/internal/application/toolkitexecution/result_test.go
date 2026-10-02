package toolkitexecution

import (
	"context"
	"errors"
	"testing"
	"time"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type toolkitReadCompletionStoreStub struct {
	completions []Completion
	calls       int
}

func (s *toolkitReadCompletionStoreStub) LoadToolkitExecuteReadCompletion(
	context.Context,
	string,
	uint64,
) (Completion, error) {
	index := s.calls
	s.calls++
	if index >= len(s.completions) {
		index = len(s.completions) - 1
	}
	return s.completions[index].Clone(), nil
}

func TestToolkitExecuteReadResultWaiterReturnsOnlyDurableSuccess(t *testing.T) {
	store := &toolkitReadCompletionStoreStub{completions: []Completion{
		{State: executiondomain.JobRunning},
		{State: executiondomain.JobSucceeded, ResultJSON: []byte(`{"ok":true}`),
			ToolkitType: "github", ToolkitName: "source", ToolName: "get_issue"},
	}}
	waiter, err := NewResultWaiter(store, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	result, err := waiter.Wait(context.Background(), "execution-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.ResultJSON) != `{"ok":true}` || store.calls != 2 {
		t.Fatalf("result = %#v calls = %d", result, store.calls)
	}
}

func TestToolkitExecuteReadResultWaiterStopsOnTerminalFailureAndCancellation(t *testing.T) {
	for _, state := range []executiondomain.JobState{executiondomain.JobFailed, executiondomain.JobCancelled} {
		store := &toolkitReadCompletionStoreStub{completions: []Completion{{
			State: state, TerminalErrorCode: "PROVIDER_ERROR",
		}}}
		waiter, err := NewResultWaiter(store, 5*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		_, err = waiter.Wait(context.Background(), "execution-1", 1)
		if !errors.Is(err, ErrToolkitExecuteReadFailed) {
			t.Fatalf("state %s error = %v", state, err)
		}
	}
}

func TestToolkitExecuteReadResultWaiterHonorsContext(t *testing.T) {
	store := &toolkitReadCompletionStoreStub{completions: []Completion{{State: executiondomain.JobRunning}}}
	waiter, err := NewResultWaiter(store, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Millisecond)
	defer cancel()
	_, err = waiter.Wait(ctx, "execution-1", 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait() error = %v", err)
	}
}
