package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	enginev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/engine/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/engine/enginetest"
)

// healthEngine is an engine on a Unix socket whose Health lists the tools it
// is told to (nil: none, as an engine that does not list them answers), or
// fails while down.
type healthEngine struct {
	enginev1.UnimplementedEngineServiceServer
	mu    sync.Mutex
	tools []string
	down  bool
	calls int
}

func (h *healthEngine) set(tools []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools = tools
}

func (h *healthEngine) Health(context.Context, *enginev1.HealthRequest) (*enginev1.HealthResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.down {
		return nil, status.Error(codes.Unavailable, "starting")
	}
	return &enginev1.HealthResponse{Status: "UP", Runner: "native", Tools: append([]string(nil), h.tools...)}, nil
}

func startHealthEngine(t *testing.T) (*healthEngine, *Client) {
	t.Helper()
	engine := &healthEngine{}
	return engine, NewClient(enginetest.Serve(t, engine), "DeepWiki")
}

func TestTheEngineListsItsToolsAndTheHostDecidesFromTheList(t *testing.T) {
	engine, client := startHealthEngine(t)
	engine.set([]string{"generate_wiki", "ask", "delete_wiki_index"})
	served, known := client.Serves(t.Context(), "delete_wiki_index")
	if !known || !served {
		t.Fatalf("served=%v known=%v", served, known)
	}
	if served, known := client.Serves(t.Context(), "delete_project_wikis"); !known || served {
		t.Fatalf("a tool the engine does not list: served=%v known=%v", served, known)
	}
	// Cached: a second question is not a second request.
	engine.mu.Lock()
	calls := engine.calls
	engine.mu.Unlock()
	client.Serves(t.Context(), "ask")
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.calls != calls {
		t.Fatalf("a cached answer cost %d request(s)", engine.calls-calls)
	}
}

// An engine that does not list its tools: the answer is UNKNOWN (and the
// caller falls back to the call), not "absent".
func TestAnEngineThatDoesNotListItsToolsIsUnknownNotAbsent(t *testing.T) {
	_, client := startHealthEngine(t)
	if served, known := client.Serves(t.Context(), "delete_wiki_index"); known || served {
		t.Fatalf("served=%v known=%v", served, known)
	}
}

func TestAnEngineThatCannotBeReadIsUnknown(t *testing.T) {
	engine, client := startHealthEngine(t)
	engine.mu.Lock()
	engine.down = true
	engine.mu.Unlock()
	if err := client.RefreshTools(t.Context()); err == nil {
		t.Fatal("a failed Health was taken for an answer")
	}
	if _, known := client.Serves(t.Context(), "ask"); known {
		t.Fatal("an unreadable engine was taken for one that lists its tools")
	}
}

// The host reads the list once at start, then again periodically: an engine
// upgraded under it (the rolling deploy finishing) is noticed, and so is a
// downgrade.
func TestTheHostRefreshesTheToolListPeriodically(t *testing.T) {
	engine, client := startHealthEngine(t)
	engine.set([]string{"generate_wiki"})
	done := make(chan struct{})
	go func() {
		client.WatchTools(t.Context(), 20*time.Millisecond, nil)
		close(done)
	}()

	waitFor := func(want bool, tool string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			client.caps.mu.RLock()
			known := client.caps.known
			list := append([]string(nil), client.caps.tools...)
			client.caps.mu.RUnlock()
			has := false
			for _, name := range list {
				has = has || name == tool
			}
			if known && has == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("the host never saw %q listed=%v", tool, want)
	}
	waitFor(false, "delete_wiki_index")
	engine.set([]string{"generate_wiki", "delete_wiki_index"}) // the engine is upgraded
	waitFor(true, "delete_wiki_index")
	engine.set(nil) // ... and downgraded: the list is gone, the answer unknown again
	deadline := time.Now().Add(5 * time.Second)
	for {
		client.caps.mu.RLock()
		known := client.caps.known
		client.caps.mu.RUnlock()
		if !known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a downgraded engine was still taken for one that lists its tools")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
