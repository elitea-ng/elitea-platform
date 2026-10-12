package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// What an engine can do, from the engine itself.
//
// A rolling deploy runs a newer host against an older engine for a while. The
// host used to find out that the engine lacked a tool by calling it and
// matching the refusal's text ("Unknown tool"). The engine now LISTS the tools
// it serves in GET /engine/health (`tools`), the host reads the list when it
// starts and again every ToolsRefresh, and decides from the list.
//
// The list is a statement of the engine's release, so it is cached: a tool
// check is a map lookup, not a request. An engine that does not list its tools
// (a release from before the field) leaves the answer UNKNOWN, and the caller
// falls back to trying the call and recognising the refusal
// (ErrUnknownTool), as it did before. That fallback is the last resort, not
// the way.

// DefaultToolsRefresh is how often the host re-reads the engine's tool list.
const DefaultToolsRefresh = time.Minute

// toolsFetchTimeout bounds one health request: the list is read in the
// background, and an engine that does not answer must not stall a caller.
const toolsFetchTimeout = 3 * time.Second

type capabilities struct {
	mu    sync.RWMutex
	known bool
	tools []string
}

// Health is what GET /engine/health reports.
type Health struct {
	Status string   `json:"status"`
	Runner string   `json:"runner"`
	Active int      `json:"active"`
	Tools  []string `json:"tools"`
}

// Health reads the engine's health document.
func (c *Client) Health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, toolsFetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://engine/engine/health", nil)
	if err != nil {
		return Health{}, err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return Health{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("GET /engine/health answered %d", response.StatusCode)
	}
	var health Health
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health); err != nil {
		return Health{}, fmt.Errorf("the health document is not JSON: %w", err)
	}
	return health, nil
}

// RefreshTools reads the engine's tool list now and caches it. An engine that
// answers without a `tools` list (a release from before the field) makes the
// answer unknown again, so a downgrade is noticed too. An error leaves the
// last answer in place.
func (c *Client) RefreshTools(ctx context.Context) error {
	health, err := c.Health(ctx)
	if err != nil {
		return err
	}
	c.caps.mu.Lock()
	defer c.caps.mu.Unlock()
	c.caps.known = len(health.Tools) > 0
	c.caps.tools = slices.Clone(health.Tools)
	return nil
}

// WatchTools reads the tool list now and every `every` (DefaultToolsRefresh
// when not positive) until ctx ends. Call it once, in the background.
func (c *Client) WatchTools(ctx context.Context, every time.Duration, logger *slog.Logger) {
	if every <= 0 {
		every = DefaultToolsRefresh
	}
	if logger == nil {
		logger = slog.Default()
	}
	refresh := func() {
		if err := c.RefreshTools(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("could not read the engine's tool list", "error", err)
		}
	}
	refresh()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// Serves reports whether the engine lists `tool`. known is false when the
// engine's list has not been read (or the engine does not list its tools);
// served is then meaningless. With nothing cached it tries to read the list
// once before answering, so a host that started before its engine does not
// answer "unknown" for ever.
func (c *Client) Serves(ctx context.Context, tool string) (served, known bool) {
	c.caps.mu.RLock()
	known = c.caps.known
	c.caps.mu.RUnlock()
	if !known {
		_ = c.RefreshTools(ctx)
	}
	c.caps.mu.RLock()
	defer c.caps.mu.RUnlock()
	if !c.caps.known {
		return false, false
	}
	return slices.Contains(c.caps.tools, tool), true
}
