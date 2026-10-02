package toolkitexecution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

var (
	ErrToolkitExecuteReadPending = errors.New("direct toolkit execution is still pending")
	ErrToolkitExecuteReadFailed  = errors.New("direct toolkit execution failed")
)

type Completion struct {
	State             executiondomain.JobState
	TerminalErrorCode string
	ResultJSON        json.RawMessage
	ToolkitType       string
	ToolkitName       string
	ToolName          string
}

func (c Completion) Validate() error {
	if !c.State.Valid() {
		return ErrToolkitExecuteReadFailed
	}
	hasResult := len(c.ResultJSON) != 0 || c.ToolkitType != "" || c.ToolkitName != "" || c.ToolName != ""
	if hasResult {
		if len(c.ResultJSON) == 0 || len(c.ResultJSON) > 48*1024 || !json.Valid(c.ResultJSON) ||
			!validIdentity(c.ToolkitType) || !validIdentity(c.ToolkitName) || !validIdentity(c.ToolName) {
			return ErrToolkitExecuteReadFailed
		}
	}
	if c.State == executiondomain.JobSucceeded && (!hasResult || c.TerminalErrorCode != "") {
		return ErrToolkitExecuteReadFailed
	}
	if (c.State == executiondomain.JobFailed || c.State == executiondomain.JobCancelled ||
		c.State == executiondomain.JobQuarantined) && hasResult {
		return ErrToolkitExecuteReadFailed
	}
	return nil
}

type CompletionStore interface {
	LoadToolkitExecuteReadCompletion(context.Context, string, uint64) (Completion, error)
}

type ResultWaiter struct {
	store        CompletionStore
	pollInterval time.Duration
}

func NewResultWaiter(store CompletionStore, pollInterval time.Duration) (*ResultWaiter, error) {
	if store == nil || pollInterval < 5*time.Millisecond || pollInterval > time.Second {
		return nil, errors.New("direct toolkit result waiter dependencies are invalid")
	}
	return &ResultWaiter{store: store, pollInterval: pollInterval}, nil
}

func (w *ResultWaiter) Wait(
	ctx context.Context,
	executionID string,
	generation uint64,
) (Completion, error) {
	if !validIdentity(executionID) || generation == 0 {
		return Completion{}, ErrInvalidCurrentReadTool
	}
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		completion, err := w.store.LoadToolkitExecuteReadCompletion(ctx, executionID, generation)
		if err != nil {
			return Completion{}, fmt.Errorf("load direct toolkit completion: %w", err)
		}
		if err := completion.Validate(); err != nil {
			return Completion{}, err
		}
		switch completion.State {
		case executiondomain.JobSucceeded:
			return completion.Clone(), nil
		case executiondomain.JobFailed, executiondomain.JobCancelled, executiondomain.JobQuarantined:
			if completion.TerminalErrorCode == "" && completion.State != executiondomain.JobCancelled {
				return Completion{}, ErrToolkitExecuteReadFailed
			}
			return Completion{}, fmt.Errorf("%w: %s", ErrToolkitExecuteReadFailed, completion.State)
		}
		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c Completion) Clone() Completion {
	c.ResultJSON = append(json.RawMessage(nil), c.ResultJSON...)
	return c
}
