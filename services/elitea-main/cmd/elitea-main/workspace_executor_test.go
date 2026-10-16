package main

import (
	"context"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
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
	seam := composition.dependencies()
	if seam.Store != nil || seam.Turns != nil || seam.Policies != nil || seam.Lifecycle != nil || composition.lifecycle(true) != nil {
		t.Fatalf("seam = %+v", seam)
	}
}
