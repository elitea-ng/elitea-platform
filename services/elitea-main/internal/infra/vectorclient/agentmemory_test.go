package vectorclient

import (
	"context"
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"github.com/google/uuid"
)

func TestAgentMemoryNamespaceIsStableAndPerProjectAndAgent(t *testing.T) {
	first := AgentMemoryNamespace(7, 10)
	if _, err := uuid.Parse(first); err != nil || first != uuid.MustParse(first).String() {
		t.Fatalf("not a canonical lower-case uuid: %q (%v)", first, err)
	}
	if AgentMemoryNamespace(7, 10) != first {
		t.Fatal("the namespace is not stable")
	}
	for name, other := range map[string]string{
		"another agent":   AgentMemoryNamespace(7, 11),
		"another project": AgentMemoryNamespace(8, 10),
		// "7/10" and "71/0" must never collide through concatenation.
		"a shifted split": AgentMemoryNamespace(71, 0),
		"the inventory":   InventoryGraphNamespace(7, 10),
	} {
		if other == first {
			t.Errorf("%s shares the namespace", name)
		}
	}
}

func TestAgentMemoryCallsRefuseBadArgumentsBeforeTheWire(t *testing.T) {
	var client *Client
	if err := client.DeleteAgentMemoryEntries(context.Background(), 7, 10, []int64{1}); !errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("a nil client: %v", err)
	}
	live := &Client{}
	if err := live.DeleteAgentMemoryEntries(context.Background(), 0, 10, []int64{1}); !errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("no project: %v", err)
	}
	if err := live.DeleteAgentMemoryEntries(context.Background(), 7, 10, nil); !errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("no entries: %v", err)
	}
	if err := live.UpsertAgentMemory(context.Background(), "", 7, 10, agentmemoryvector.Space{ModelSlug: "m", Dimension: 3}, nil); !errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("no token or points: %v", err)
	}
	if _, err := live.SearchAgentMemory(context.Background(), "token", 7, 10, agentmemoryvector.Space{ModelSlug: "m", Dimension: 3}, nil, 5); !errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("no vector: %v", err)
	}
}
