// Package preflight — BFF.9c hermetic test: models-parity pre-flight gate.
//
// Proves that the gateway's synthesised /llm/v1/models set is order-insensitively
// equivalent to the legacy StaticLegacyModels() set for >= 5 distinct projects,
// and that the per-call p99 latency of the /llm/v1/models endpoint is under
// 200 ms (trivially satisfied in-process; present as a guard against accidental
// synchronous-DB-call regressions).
//
// Design:
//   - MountedHandlerWithModels wires a ModelResolver backed by a
//     NewStaticModelResolver(StaticLegacyModels()) fake so every project
//     resolves the legacy ids without a live Postgres database.
//   - Per-project GET /llm/v1/models requests are signed via SignRequest
//     (HMAC identity headers) so the handler's signature check passes.
//   - Set-equality is verified by diffModelSets (copied inline per instructions;
//     cmd/cutover-ctl/modelsparity.go does not yet exist in the module).
package preflight_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/failmode"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/llmproxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/preflight"
)

// ── set-equivalence helpers ───────────────────────────────────────────────────
// Copied from the diffModelSets / setsEquivalent pattern described in
// cmd/cutover-ctl/modelsparity.go (not yet present in the module).  Using a
// local copy avoids importing a cmd package from a test.

// modelsDiff holds the result of comparing two id sets.
type modelsDiff struct {
	missing []string // ids present in want but absent in got
	extra   []string // ids present in got but absent in want
}

// diffModelSets computes the symmetric difference between got and want.
// Both slices may be in any order; the returned slices are sorted for
// deterministic output.
func diffModelSets(got, want []string) modelsDiff {
	wantSet := make(map[string]struct{}, len(want))
	for _, id := range want {
		wantSet[id] = struct{}{}
	}
	gotSet := make(map[string]struct{}, len(got))
	for _, id := range got {
		gotSet[id] = struct{}{}
	}
	var missing, extra []string
	for id := range wantSet {
		if _, ok := gotSet[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id := range gotSet {
		if _, ok := wantSet[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return modelsDiff{missing: missing, extra: extra}
}

// setsEquivalent reports whether two id sets are order-insensitively equal.
func setsEquivalent(got, want []string) bool {
	d := diffModelSets(got, want)
	return len(d.missing) == 0 && len(d.extra) == 0
}

// ── wire type for GET /llm/v1/models response ────────────────────────────────

type modelsListResponse struct {
	Object string            `json:"object"`
	Data   []modelObjectWire `json:"data"`
}

type modelObjectWire struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ── test projects ─────────────────────────────────────────────────────────────

// bff9cProjects is the set of project IDs exercised by TestBFF9C_ModelsParity.
// Five distinct projects are required by the gate specification.
var bff9cProjects = []struct {
	id    int
	idStr string
}{
	{id: 101, idStr: "101"},
	{id: 202, idStr: "202"},
	{id: 303, idStr: "303"},
	{id: 404, idStr: "404"},
	{id: 505, idStr: "505"},
}

// ── TestBFF9C_ModelsParity ────────────────────────────────────────────────────

// TestBFF9C_ModelsParity is the hermetic BFF.9c models-parity pre-flight gate.
// It asserts:
//
//  1. GET /llm/v1/models returns HTTP 200 for each of the 5 test projects.
//  2. The returned id set is order-insensitively equal to StaticLegacyModels()
//     (missing set empty AND extra set empty).
//  3. p99 per-call latency is under 200 ms.
//
// All calls are in-process (httptest.NewRecorder); no live NATS, Postgres, or
// external provider is required.
func TestBFF9C_ModelsParity(t *testing.T) {
	t.Parallel()

	const (
		hardLimitNano = int64(100) * failmode.NanoUSD
		spentNano     = int64(10) * failmode.NanoUSD // well under limit
	)

	legacy := preflight.StaticLegacyModels()
	if len(legacy) == 0 {
		t.Fatal("StaticLegacyModels() returned empty set — test precondition violated")
	}

	secret := []byte("bff9c-parity-secret")

	// Build a single static resolver pre-seeded with the legacy ids.
	// NewStaticModelResolver ignores the per-project schema name in the SQL, so
	// one resolver serves all test projects correctly.
	resolver := llmproxy.NewStaticModelResolver(legacy)

	// Wire >=5 projects, each with a real GovernanceStore (under-budget).
	type projectFixture struct {
		idStr   string
		handler http.Handler
	}
	fixtures := make([]projectFixture, 0, len(bff9cProjects))
	for _, p := range bff9cProjects {
		gov, _, _ := preflight.NewSeededGovernance(t, p.id, hardLimitNano, spentNano)
		router := preflight.NewMockRouter(preflight.MockRouterConfig{})
		h := preflight.MountedHandlerWithModels(t, router, gov, secret, resolver)
		fixtures = append(fixtures, projectFixture{idStr: p.idStr, handler: h})
	}

	// latencies collects per-call durations for the p99 assertion.
	// Guarded by latMu because the subtests below run with t.Parallel().
	latencies := make([]time.Duration, 0, len(fixtures))
	var latMu sync.Mutex

	for _, fix := range fixtures {
		fix := fix // capture loop variable
		t.Run(fmt.Sprintf("project_%s", fix.idStr), func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/llm/v1/models", nil)
			preflight.SignRequest(req, secret, fix.idStr, "user-parity", "tenant-parity")

			start := time.Now()
			rec := httptest.NewRecorder()
			fix.handler.ServeHTTP(rec, req)
			elapsed := time.Since(start)
			latMu.Lock()
			latencies = append(latencies, elapsed)
			latMu.Unlock()

			// 1. HTTP status must be 200.
			if rec.Code != http.StatusOK {
				t.Fatalf("project %s: GET /llm/v1/models returned HTTP %d, want 200\nbody: %s",
					fix.idStr, rec.Code, rec.Body.String())
			}

			// 2. Parse the returned models list.
			var list modelsListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
				t.Fatalf("project %s: decode models response: %v\nbody: %s",
					fix.idStr, err, rec.Body.String())
			}
			if list.Object != "list" {
				t.Errorf("project %s: response.object = %q, want \"list\"", fix.idStr, list.Object)
			}
			got := make([]string, 0, len(list.Data))
			for _, m := range list.Data {
				got = append(got, m.ID)
			}

			// 3. Assert order-insensitive set equivalence.
			if !setsEquivalent(got, legacy) {
				diff := diffModelSets(got, legacy)
				t.Errorf("project %s: model set is NOT equivalent to legacy set\n"+
					"  missing from gateway (%d): %v\n"+
					"  extra in gateway    (%d): %v\n"+
					"  gateway returned: %v",
					fix.idStr,
					len(diff.missing), diff.missing,
					len(diff.extra), diff.extra,
					got)
			}
		})
	}

	// p99 latency assertion — run after all subtests complete.
	// (The outer test waits for all parallel subtests here.)
	t.Cleanup(func() {
		if len(latencies) == 0 {
			return
		}
		sorted := make([]time.Duration, len(latencies))
		copy(sorted, latencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		// p99 index: ceiling of 0.99 * n (1-based), clamped to len-1.
		n := len(sorted)
		p99idx := int(float64(n)*0.99+0.5) - 1
		if p99idx < 0 {
			p99idx = 0
		}
		if p99idx >= n {
			p99idx = n - 1
		}
		p99 := sorted[p99idx]

		const threshold = 200 * time.Millisecond
		if p99 >= threshold {
			t.Errorf("BFF.9c latency gate FAILED: p99 = %v, must be < %v (in-process call should be well under threshold)",
				p99, threshold)
		} else {
			t.Logf("BFF.9c latency gate PASSED: p99 = %v (threshold %v)", p99, threshold)
		}
	})
}
