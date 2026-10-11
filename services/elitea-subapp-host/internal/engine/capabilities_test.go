package engine

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// healthEngine is an engine on a Unix socket whose GET /engine/health lists
// the tools it is told to (nil: the field is left out, as an engine of an
// older release does).
type healthEngine struct {
	mu     sync.Mutex
	tools  []string
	status int
	calls  int
}

func (h *healthEngine) set(tools []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools = tools
}

func startHealthEngine(t *testing.T) (*healthEngine, *Client) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "eng")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "e.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	engine := &healthEngine{status: http.StatusOK}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.mu.Lock()
		engine.calls++
		tools, status := engine.tools, engine.status
		engine.mu.Unlock()
		if r.URL.Path != "/engine/health" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body := `{"status":"UP","runner":"native","active":0}`
		if tools != nil {
			body = `{"status":"UP","runner":"native","active":0,"tools":["` + joinTools(tools) + `"]}`
		}
		_, _ = w.Write([]byte(body))
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return engine, NewClient(socket, "DeepWiki")
}

func joinTools(tools []string) string {
	out := ""
	for i, tool := range tools {
		if i > 0 {
			out += `","`
		}
		out += tool
	}
	return out
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

// An engine of an older release does not list its tools: the answer is
// UNKNOWN (and the caller falls back to the call), not "absent".
func TestAnEngineThatDoesNotListItsToolsIsUnknownNotAbsent(t *testing.T) {
	_, client := startHealthEngine(t)
	if served, known := client.Serves(t.Context(), "delete_wiki_index"); known || served {
		t.Fatalf("served=%v known=%v", served, known)
	}
}

func TestAnEngineThatCannotBeReadIsUnknown(t *testing.T) {
	engine, client := startHealthEngine(t)
	engine.mu.Lock()
	engine.status = http.StatusServiceUnavailable
	engine.mu.Unlock()
	if err := client.RefreshTools(t.Context()); err == nil {
		t.Fatal("a 503 was taken for a health document")
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
