package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
)

type fakeMarketplaceSink struct {
	run    ProposeDismissalRun
	runErr error
	calls  []string
}

func (f *fakeMarketplaceSink) ResolveProposeDismissalRun(context.Context, int64, string, uint64) (ProposeDismissalRun, error) {
	return f.run, f.runErr
}

func (f *fakeMarketplaceSink) MarketplaceWorkforce(_ context.Context, _ ProposeDismissalRun, tool string, _ json.RawMessage) (map[string]any, error) {
	f.calls = append(f.calls, tool)
	return map[string]any{"status": "ok", "message": "fine", "echo": tool}, nil
}

func marketplaceService(t *testing.T, sink *fakeMarketplaceSink) *RuntimeMarketplaceWorkforceService {
	t.Helper()
	authorizer := agentRuntimeContextAuthorizerFunc(func(context.Context, ContentClaim) (RuntimeContextAuthorization, error) {
		return RuntimeContextAuthorization{ResourceProjectID: 7, ActorID: "3", ConversationID: "c-1"}, nil
	})
	service, err := NewRuntimeMarketplaceWorkforceService(authorizer, sink)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func managerRun() ProposeDismissalRun {
	return ProposeDismissalRun{ProjectID: 7, ExecutionID: "e-1", ConversationUUID: "c-1", ApplicationID: 11, ActorID: 3, IsWorkforceManager: true}
}

func TestMarketplaceWorkforceRunsEachToolForAWorkforceManager(t *testing.T) {
	sink := &fakeMarketplaceSink{run: managerRun()}
	service := marketplaceService(t, sink)
	for _, tool := range []string{MarketplaceToolSearch, MarketplaceToolListing, MarketplaceToolProposeHire, MarketplaceToolSuggestFeedback} {
		got, err := service.Call(context.Background(), ContentClaim{ExecutionID: "e-1", Generation: 1},
			MarketplaceWorkforceRequest{Tool: tool, Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if got["status"] != "ok" || got["echo"] != tool || got["schema_version"] != RuntimeMarketplaceWorkforceSchemaVersion {
			t.Fatalf("%s: %v", tool, got)
		}
	}
	if len(sink.calls) != 4 {
		t.Fatalf("calls %v", sink.calls)
	}
}

func TestMarketplaceWorkforceRefusesAnAgentThatIsNotAManagerAndAnUnknownTool(t *testing.T) {
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	for name, mutate := range map[string]func(*ProposeDismissalRun){
		"no capability": func(run *ProposeDismissalRun) { run.IsWorkforceManager = false },
		"ad hoc run":    func(run *ProposeDismissalRun) { run.ApplicationID = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			run := managerRun()
			mutate(&run)
			sink := &fakeMarketplaceSink{run: run}
			got, err := marketplaceService(t, sink).Call(context.Background(), claim,
				MarketplaceWorkforceRequest{Tool: MarketplaceToolProposeHire, Arguments: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			if got["status"] != agentworkforce.OutcomeNotAllowed || len(sink.calls) != 0 {
				t.Fatalf("got %v, calls %v", got, sink.calls)
			}
		})
	}
	sink := &fakeMarketplaceSink{run: managerRun()}
	got, err := marketplaceService(t, sink).Call(context.Background(), claim, MarketplaceWorkforceRequest{Tool: "write_review"})
	if err != nil {
		t.Fatal(err)
	}
	if got["status"] != agentworkforce.OutcomeInvalid || len(sink.calls) != 0 {
		t.Fatalf("an unknown tool (reviews are people's) must be refused: %v, calls %v", got, sink.calls)
	}
}

func TestMarketplaceWorkforceTakesTheRunFromTheClaimNotTheWorker(t *testing.T) {
	claim := ContentClaim{ExecutionID: "e-1", Generation: 1}
	for name, mutate := range map[string]func(*ProposeDismissalRun){
		"another project":      func(run *ProposeDismissalRun) { run.ProjectID = 8 },
		"another conversation": func(run *ProposeDismissalRun) { run.ConversationUUID = "c-2" },
		"another person":       func(run *ProposeDismissalRun) { run.ActorID = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			run := managerRun()
			mutate(&run)
			sink := &fakeMarketplaceSink{run: run}
			_, err := marketplaceService(t, sink).Call(context.Background(), claim,
				MarketplaceWorkforceRequest{Tool: MarketplaceToolSearch})
			if !errors.Is(err, ErrContentUnauthorized) || len(sink.calls) != 0 {
				t.Fatalf("err %v, calls %v", err, sink.calls)
			}
		})
	}
	sink := &fakeMarketplaceSink{runErr: ErrContentUnauthorized}
	if _, err := marketplaceService(t, sink).Call(context.Background(), claim, MarketplaceWorkforceRequest{Tool: MarketplaceToolSearch}); !errors.Is(err, ErrContentUnauthorized) {
		t.Fatalf("a run the claim does not name: %v", err)
	}
}
