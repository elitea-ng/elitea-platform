package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacehandoff"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func TestCloudExecutorResourcesParseTheChartQuantities(t *testing.T) {
	parsed, err := parseCloudExecutorResources(platformconfig.CloudExecutorResources{
		TurnWallSeconds: 900, CPU: "1.5", Memory: "4Gi", Disk: "10G",
	})
	if err != nil || parsed.TurnWallSeconds != 900 || parsed.CPUMillis != 1500 || parsed.MemoryBytes != 4<<30 || parsed.DiskBytes != 10e9 {
		t.Fatalf("parsed = %+v %v", parsed, err)
	}
	if parsed, err := parseCloudExecutorResources(platformconfig.CloudExecutorResources{CPU: "500m"}); err != nil || parsed.CPUMillis != 500 || parsed.MemoryBytes != 0 {
		t.Fatalf("millicores = %+v %v", parsed, err)
	}
	for _, bad := range []platformconfig.CloudExecutorResources{{CPU: "two"}, {Memory: "4 GiB"}, {Disk: "-1"}, {Memory: "0"}} {
		if _, err := parseCloudExecutorResources(bad); err == nil {
			t.Fatalf("%+v parsed", bad)
		}
	}
}

// The handoff is composed before the runtime, so its canceller is bound late;
// unbound it refuses instead of pretending to stop a turn.
func TestDeferredCancellerRefusesUntilBound(t *testing.T) {
	canceller := &deferredAgentCanceller{}
	if _, err := canceller.Cancel(context.Background(), agentexecutionapp.CurrentAgentCancelRequest{}); err == nil {
		t.Fatal("an unbound canceller succeeded")
	}
	bound := &countingCanceller{}
	canceller.bind(bound)
	if _, err := canceller.Cancel(context.Background(), agentexecutionapp.CurrentAgentCancelRequest{}); err != nil || bound.calls != 1 {
		t.Fatalf("bound cancel = %v (%d calls)", err, bound.calls)
	}
}

type countingCanceller struct{ calls int }

func (c *countingCanceller) Cancel(context.Context, agentexecutionapp.CurrentAgentCancelRequest) (agentexecutionapp.CurrentAgentCancelOutcome, error) {
	c.calls++
	return agentexecutionapp.CurrentAgentCancelOutcome{}, nil
}

// Without a composed handoff there is no executor: the runtime gets an empty
// seam (fence shut) and the sweeper no reconciler.
func TestNoHandoffComposesNoExecutor(t *testing.T) {
	composition, err := composeWorkspaceExecutor(nil, workspaceHandoffComposition{}, platformconfig.CloudExecutorResources{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seam := composition.dependencies(workspaceruntime.Egress{})
	if seam.Store != nil || seam.Turns != nil || seam.Policies != nil || seam.Lifecycle != nil || composition.lifecycle(true) != nil {
		t.Fatalf("seam = %+v", seam)
	}
}

type capturedBus struct {
	channel, eventType string
	payload            []byte
}

func (b *capturedBus) Publish(_ context.Context, channel, eventType string, payload any) error {
	b.channel, b.eventType = channel, eventType
	b.payload, _ = json.Marshal(payload)
	return nil
}

// The handoff's live state change goes onto the workspace family with its
// audience for the SSE relay; nothing else of the thread is in it.
func TestHandoffLiveEventsPublishTheStateWithItsAudience(t *testing.T) {
	if newHandoffLiveEvents(nil) != nil {
		t.Fatal("no bus must publish nothing")
	}
	bus := &capturedBus{}
	newHandoffLiveEvents(bus).HandoffChanged(context.Background(), 7, []int64{11}, workspacehandoff.StateEvent{
		ConversationUUID: "0b9c6f4e-9a7f-4d4e-8f0e-1d2c3b4a5f60", State: workspacestore.StateCloudRunning, Epoch: 2, ReclaimPending: true,
	})
	if bus.channel != "project:7:workspace" || bus.eventType != workspacehandoff.EventType {
		t.Fatalf("published on %q as %q", bus.channel, bus.eventType)
	}
	var fields map[string]any
	if err := json.Unmarshal(bus.payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || fields["state"] != "CLOUD_RUNNING" || fields["epoch"] != float64(2) || fields["reclaim_pending"] != true ||
		fields["conversation_uuid"] == nil || fmt.Sprint(fields["audience"]) != "[11]" {
		t.Fatalf("payload = %s", bus.payload)
	}
}

// The per-interrupt ledger's live frames go onto the conversation family
// (audience-scoped), never the workspace handoff's.
func TestInterruptLiveEventsPublishOnTheAudienceFamily(t *testing.T) {
	if newInterruptLiveEvents(nil) != nil {
		t.Fatal("no bus must publish nothing")
	}
	bus := &capturedBus{}
	newInterruptLiveEvents(bus).PublishInterruptEvent(context.Background(), 7, "agent_hitl_resolved",
		map[string]any{"conversation_id": "c-1", "root_response_id": "r-1", "audience": []int64{11}})
	if bus.channel != "project:7:conversation" || bus.eventType != "agent_hitl_resolved" ||
		string(bus.payload) != `{"audience":[11],"conversation_id":"c-1","root_response_id":"r-1"}` {
		t.Fatalf("published %q %q %s", bus.channel, bus.eventType, bus.payload)
	}
}
