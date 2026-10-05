package runtimecomposition

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/noderecovery"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
)

type runtimeRecoveryStore struct {
	execution string
	receipt   json.RawMessage
}

func (s runtimeRecoveryStore) Read(context.Context, app.Selector) (app.State, error) {
	return app.State{Schema: "elitea.pipeline.node-recovery-state.v1", ExecutionID: s.execution, Generation: 1, Status: "suspended", Receipt: s.receipt}, nil
}
func (s runtimeRecoveryStore) Submit(_ context.Context, r app.Submission) (app.Outcome, error) {
	q := r.Request
	return app.Outcome{Schema: "elitea.pipeline.node-recovery-accepted.v1", ExecutionID: q.ExecutionID, Generation: q.Generation, RequestID: q.RequestID, ActivationID: q.ActivationID, ExpectedRevision: q.ExpectedRevision, Action: q.Action}, nil
}
func TestNodeRecoveryActualMainRuntimeGeneratorPassesRequestStateAndOperatorService(t *testing.T) {
	receipt, err := os.ReadFile("../../../../libs/jsonschema/runtime/v1/fixtures/node-recovery-required-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		execution, err := currentRuntimeID()
		if err != nil {
			t.Fatal(err)
		}
		if len(execution) != 32 || !domain.ValidExecutionID(execution) {
			t.Fatal("production generator rejected", execution)
		}
		service, err := app.New(runtimeRecoveryStore{execution: execution, receipt: receipt})
		if err != nil {
			t.Fatal(err)
		}
		selector := app.Selector{ProjectID: 7, ActorUserID: 11, ResponseMessageID: "10000000-0000-4000-8000-000000000043"}
		state, err := service.Read(t.Context(), selector)
		if err != nil || state.ExecutionID != execution {
			t.Fatal(state, err)
		}
		request := domain.Request{RequestID: strings.Repeat("2", 64), ExecutionID: execution, Generation: 1, ActivationID: strings.Repeat("1", 64), ExpectedRevision: 3, Action: "retry"}
		raw, _ := json.Marshal(request)
		decoded, err := domain.DecodeRequest(raw)
		if err != nil || decoded != request {
			t.Fatal(decoded, err)
		}
		outcome, err := service.Submit(t.Context(), app.Submission{Selector: selector, Request: decoded})
		if err != nil || outcome.ExecutionID != execution {
			t.Fatal(outcome, err)
		}
		selector.ResponseMessageID = execution
		if selector.Validate() == nil {
			t.Fatal("execution selector accepted as response UUID")
		}
	}
}
