package shadow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type Config struct {
	Enabled       bool
	LegacyBaseURL string
	Weight        float64
	Timeout       time.Duration
	LogDiffs      bool
}

type Comparator struct {
	cfg    Config
	client *http.Client
	mu     sync.RWMutex
}

func NewComparator(cfg Config) *Comparator {
	return &Comparator{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

func (c *Comparator) SetWeight(weight float64) {
	c.mu.Lock()
	c.cfg.Weight = weight
	c.mu.Unlock()
}

func (c *Comparator) Weight() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.Weight
}

type CompareResult struct {
	Endpoint      string        `json:"endpoint"`
	Method        string        `json:"method"`
	StatusMatch   bool          `json:"status_match"`
	BodyMatch     bool          `json:"body_match"`
	LegacyStatus  int           `json:"legacy_status"`
	NewStatus     int           `json:"new_status"`
	LegacyLatency time.Duration `json:"legacy_latency_ms"`
	NewLatency    time.Duration `json:"new_latency_ms"`
	Diffs         []Diff        `json:"diffs,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type Diff struct {
	Path   string `json:"path"`
	Legacy string `json:"legacy"`
	New    string `json:"new"`
}

func (c *Comparator) Compare(ctx context.Context, method, path string, newStatus int, newBody []byte, headers http.Header) CompareResult {
	result := CompareResult{
		Endpoint:  path,
		Method:    method,
		NewStatus: newStatus,
	}

	if !c.cfg.Enabled || c.cfg.LegacyBaseURL == "" {
		return result
	}

	legacyURL := c.cfg.LegacyBaseURL + path

	req, err := http.NewRequestWithContext(ctx, method, legacyURL, nil)
	if err != nil {
		result.Error = fmt.Sprintf("create request: %v", err)
		return result
	}

	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	start := time.Now()
	resp, err := c.client.Do(req)
	result.LegacyLatency = time.Since(start)

	if err != nil {
		result.Error = fmt.Sprintf("legacy request: %v", err)
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.LegacyStatus = resp.StatusCode
	result.StatusMatch = resp.StatusCode == newStatus

	legacyBody, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Error = fmt.Sprintf("read legacy body: %v", err)
		return result
	}

	result.BodyMatch = bytes.Equal(legacyBody, newBody)

	if !result.BodyMatch && c.cfg.LogDiffs {
		result.Diffs = computeJSONDiffs(legacyBody, newBody)
	}

	if c.cfg.LogDiffs && (!result.StatusMatch || !result.BodyMatch) {
		slog.Warn("shadow mode discrepancy",
			"endpoint", path,
			"method", method,
			"status_match", result.StatusMatch,
			"body_match", result.BodyMatch,
			"legacy_status", result.LegacyStatus,
			"new_status", newStatus,
			"legacy_latency_ms", result.LegacyLatency.Milliseconds(),
			"new_latency_ms", result.NewLatency.Milliseconds(),
		)
	}

	return result
}

func computeJSONDiffs(legacy, newBody []byte) []Diff {
	var legacyMap, newMap map[string]any
	if err := json.Unmarshal(legacy, &legacyMap); err != nil {
		return []Diff{{Path: "$", Legacy: string(legacy), New: string(newBody)}}
	}
	if err := json.Unmarshal(newBody, &newMap); err != nil {
		return []Diff{{Path: "$", Legacy: string(legacy), New: string(newBody)}}
	}

	var diffs []Diff
	diffMaps("$", legacyMap, newMap, &diffs)
	return diffs
}

func diffMaps(prefix string, a, b map[string]any, diffs *[]Diff) {
	for k, va := range a {
		path := prefix + "." + k
		vb, ok := b[k]
		if !ok {
			*diffs = append(*diffs, Diff{Path: path, Legacy: fmt.Sprintf("%v", va), New: "<missing>"})
			continue
		}
		if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
			*diffs = append(*diffs, Diff{Path: path, Legacy: fmt.Sprintf("%v", va), New: fmt.Sprintf("%v", vb)})
		}
	}
	for k, vb := range b {
		path := prefix + "." + k
		if _, ok := a[k]; !ok {
			*diffs = append(*diffs, Diff{Path: path, Legacy: "<missing>", New: fmt.Sprintf("%v", vb)})
		}
	}
}
