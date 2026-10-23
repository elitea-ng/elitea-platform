package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// models-parity (spec §3 / §10.3 / §7.3, gate BFF.3 / validator BFF.9c):
//
// The gateway synthesises /llm/v1/models per project from Postgres rather than
// routing through core (design §4.2). Before the big-bang cutover we must prove
// the synthesised set is EQUIVALENT to the legacy LiteLLM /v1/models set for
// each of a fixed corpus of >= N projects, order-insensitively (the two paths
// order rows differently, so only the id SET matters, not its sequence), and
// that fetching it is fast enough — p99 latency < M ms.
//
// This subcommand runs against elitea-main's public /llm (the gateway serves
// no HTTP /llm since GatewayService replaced that hop, #1359) and the legacy endpoint
// during pre-flight (BFF.9c), driven by an operator-seeded projects fixture
// (>= 5 projects). The two load-bearing decisions — set equivalence and the p99
// threshold — are pure functions (diffModelSets, percentile) exercised without
// live infra in modelsparity_test.go.
//
// The gateway id set is the source of truth for equivalence direction: a model
// present in legacy but absent from the gateway is a MISSING model (a caller
// would lose access); a model present in the gateway but absent from legacy is
// an EXTRA model (a caller gains access). Either breaks byte-compatible parity,
// so equivalence requires both diffs empty.

// modelsListEnvelope is the OpenAI /v1/models list shape returned by BOTH the
// gateway (synthesised) and legacy LiteLLM. Only the ids are compared; the
// object/created/owned_by fields are intentionally ignored (the gateway stamps
// its own owner and a fixed created=0, spec §3, which does not affect the set).
type modelsListEnvelope struct {
	Object string `json:"object"`
	Data   []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// idSet extracts the model-id set from a decoded /v1/models envelope, sorted and
// de-duplicated so downstream comparison and reporting are deterministic.
func (e modelsListEnvelope) idSet() []string {
	seen := make(map[string]struct{}, len(e.Data))
	ids := make([]string, 0, len(e.Data))
	for _, d := range e.Data {
		if d.ID == "" {
			continue
		}
		if _, dup := seen[d.ID]; dup {
			continue
		}
		seen[d.ID] = struct{}{}
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids
}

// toStringSet reduces a slice to a set, dropping empties and duplicates.
func toStringSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		set[id] = struct{}{}
	}
	return set
}

// diffModelSets compares the gateway and legacy id sets order-insensitively.
// missing = ids in legacy but NOT in the gateway (access lost); extra = ids in
// the gateway but NOT in legacy (access gained). Both slices are sorted. The two
// sets are equivalent iff both are empty.
func diffModelSets(gateway, legacy []string) (missing, extra []string) {
	gset := toStringSet(gateway)
	lset := toStringSet(legacy)
	for id := range lset {
		if _, ok := gset[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id := range gset {
		if _, ok := lset[id]; !ok {
			extra = append(extra, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// setsEquivalent reports whether two id sets are order-insensitively equal.
func setsEquivalent(gateway, legacy []string) bool {
	missing, extra := diffModelSets(gateway, legacy)
	return len(missing) == 0 && len(extra) == 0
}

// percentile returns the p-th percentile (0..100) of samples using the
// nearest-rank method: sort ascending, take the ceil(p/100 * n)-th value
// (1-indexed). For p99 of a small sample this yields the maximum, which is the
// conservative choice for a latency ceiling. The input is not mutated (a copy is
// sorted). An empty sample returns 0.
func percentile(samples []time.Duration, p float64) time.Duration {
	n := len(samples)
	if n == 0 {
		return 0
	}
	sorted := make([]time.Duration, n)
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// parityProject is one seeded project the gate probes. APIKey authenticates the
// project to BOTH endpoints unless LegacyAPIKey overrides it for the legacy hop
// (the two systems may issue different keys during staging co-existence).
type parityProject struct {
	ProjectID    string `json:"project_id"`
	APIKey       string `json:"api_key"`
	LegacyAPIKey string `json:"legacy_api_key,omitempty"`
}

// loadProjectsFixture reads the operator-seeded projects fixture (a JSON array of
// parityProject). The fixture is what BFF.9c seeds (>= 5 projects); its path
// comes from --projects-file or the LLM_PARITY_PROJECTS_FILE env.
func loadProjectsFixture(path string) ([]parityProject, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read projects fixture %q: %w", path, err)
	}
	var projects []parityProject
	if err := json.Unmarshal(raw, &projects); err != nil {
		return nil, fmt.Errorf("parse projects fixture %q: %w", path, err)
	}
	for i, p := range projects {
		if p.ProjectID == "" {
			return nil, fmt.Errorf("projects fixture entry %d has empty project_id", i)
		}
	}
	return projects, nil
}

// fetchModelIDs GETs /llm/v1/models from baseURL for one project, returning the
// sorted id set and the observed request latency. The project is identified by a
// bearer token (apiKey) and the X-Project-Id selector; the elapsed time
// spans the full request including body read (the p99 metric the gate bounds).
func fetchModelIDs(client *http.Client, baseURL, projectID, apiKey string) ([]string, time.Duration, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/llm/v1/models", nil)
	if err != nil {
		return nil, 0, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if projectID != "" {
		// The public edge's project selector. X-Elitea-Project-Id is the
		// signed internal identity header, which the edge strips.
		req.Header.Set("X-Project-Id", projectID)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request to %s failed: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s/llm/v1/models returned status %d (want 200)", baseURL, resp.StatusCode)
	}
	var env modelsListEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, 0, fmt.Errorf("decode %s models list: %w", baseURL, err)
	}
	return env.idSet(), time.Since(start), nil
}

// cmdModelsParity is the `cutover-ctl models-parity` entrypoint. For each seeded
// project it fetches the gateway and legacy model sets, asserts order-insensitive
// equivalence, and collects gateway latency samples; it exits 0 only when at
// least --min-projects projects were checked, EVERY set is equivalent, and the
// gateway p99 latency is below --max-p99-ms.
func cmdModelsParity(args []string) {
	fs := flag.NewFlagSet("models-parity", flag.ExitOnError)
	minProjects := fs.Int("min-projects", 5, "minimum number of projects that must be checked")
	maxP99MS := fs.Int("max-p99-ms", 200, "maximum acceptable p99 gateway /v1/models latency in ms")
	gatewayURL := fs.String("gateway-url", envOr("DEPLOYMENT_URL", defaultBaseURL), "elitea-main public origin serving /llm/v1/models (default $DEPLOYMENT_URL, else "+defaultBaseURL+")")
	legacyURL := fs.String("legacy-url", envOr("LEGACY_LLM_URL", "http://localhost:4000"), "legacy LiteLLM base URL for the reference model set")
	projectsFile := fs.String("projects-file", os.Getenv("LLM_PARITY_PROJECTS_FILE"), "path to the seeded projects fixture (JSON array)")
	samplesPerProject := fs.Int("samples-per-project", 10, "gateway /v1/models fetches per project used to build the p99 latency sample")
	timeoutS := fs.Int("timeout-s", 15, "per-request timeout in seconds")
	_ = fs.Parse(args)

	if *projectsFile == "" {
		fmt.Fprintln(os.Stderr, "models-parity: no projects fixture given (use --projects-file or set LLM_PARITY_PROJECTS_FILE)")
		os.Exit(2)
	}
	if *samplesPerProject < 1 {
		*samplesPerProject = 1
	}

	projects, err := loadProjectsFixture(*projectsFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models-parity: %v\n", err)
		os.Exit(2)
	}

	client := &http.Client{Timeout: time.Duration(*timeoutS) * time.Second}
	latencies := make([]time.Duration, 0, len(projects)*(*samplesPerProject))
	mismatches := 0
	checked := 0

	for _, p := range projects {
		legacyKey := p.LegacyAPIKey
		if legacyKey == "" {
			legacyKey = p.APIKey
		}

		legacyIDs, _, err := fetchModelIDs(client, *legacyURL, p.ProjectID, legacyKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ project %s: legacy fetch failed: %v\n", p.ProjectID, err)
			mismatches++
			continue
		}

		var (
			gatewayIDs []string
			fetchErr   error
		)
		for i := 0; i < *samplesPerProject; i++ {
			ids, elapsed, err := fetchModelIDs(client, *gatewayURL, p.ProjectID, p.APIKey)
			if err != nil {
				fetchErr = err
				break
			}
			gatewayIDs = ids
			latencies = append(latencies, elapsed)
		}
		if fetchErr != nil {
			fmt.Fprintf(os.Stderr, "✗ project %s: gateway fetch failed: %v\n", p.ProjectID, fetchErr)
			mismatches++
			continue
		}

		checked++
		missing, extra := diffModelSets(gatewayIDs, legacyIDs)
		if len(missing) == 0 && len(extra) == 0 {
			fmt.Printf("✓ project %s: %d models equivalent\n", p.ProjectID, len(gatewayIDs))
			continue
		}
		mismatches++
		fmt.Fprintf(os.Stderr, "✗ project %s: model sets differ (gateway %d / legacy %d)\n",
			p.ProjectID, len(gatewayIDs), len(legacyIDs))
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr, "    missing from gateway: %s\n", strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			fmt.Fprintf(os.Stderr, "    extra on gateway:     %s\n", strings.Join(extra, ", "))
		}
	}

	p99 := percentile(latencies, 99)
	maxP99 := time.Duration(*maxP99MS) * time.Millisecond

	fmt.Printf("\nchecked %d project(s); gateway p99 latency %s over %d sample(s)\n",
		checked, p99.Round(time.Millisecond), len(latencies))

	failed := false
	if checked < *minProjects {
		fmt.Fprintf(os.Stderr, "✗ models-parity: checked %d project(s) < required %d\n", checked, *minProjects)
		failed = true
	}
	if mismatches > 0 {
		fmt.Fprintf(os.Stderr, "✗ models-parity: %d project(s) failed set equivalence\n", mismatches)
		failed = true
	}
	if len(latencies) > 0 && p99 >= maxP99 {
		fmt.Fprintf(os.Stderr, "✗ models-parity: gateway p99 %s >= %s\n", p99.Round(time.Millisecond), maxP99)
		failed = true
	}

	if failed {
		fmt.Fprintln(os.Stderr, "\n/llm/v1/models parity not proven (spec §3/§10.3, gate BFF.3).")
		os.Exit(1)
	}
	fmt.Printf("✓ models-parity: %d project(s) set-equivalent, gateway p99 %s < %s\n",
		checked, p99.Round(time.Millisecond), maxP99)
}
