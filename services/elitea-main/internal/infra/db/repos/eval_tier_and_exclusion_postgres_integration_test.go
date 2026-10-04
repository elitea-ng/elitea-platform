package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// Legacy issue 6669: an agent dimension is PROMOTED to the project library by
// a PUT with `tier: project`, and its agent is cleared. The reverse move is
// refused with 409 and the stored row keeps its scope.
func TestEvalDimensionPromotionToTheProjectAndRefusedDemotion(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	adhoc := strings.Replace(evalCreateBody, `"tier": "project"`, `"tier": "agent_adhoc", "application_id": 77`, 1)
	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", adhoc)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}
	var createdRow evaluation.Dimension
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode the created dimension: %v", err)
	}

	promoted := callEvalRoute(t, router, http.MethodPut,
		"/eval_dimension/prompt_lib/1/"+createdRow.ID, evalCreateBody)
	if promoted.Code != http.StatusOK {
		t.Fatalf("promote: expected 200, got %d: %s", promoted.Code, promoted.Body.String())
	}
	rows := listEvalDimensions(t, router, "")
	if len(rows) != 1 || rows[0].Tier != evaluation.TierProject || rows[0].ApplicationID != nil {
		t.Fatalf("the project library = %+v, want the promoted dimension with no agent", rows)
	}

	demote := callEvalRoute(t, router, http.MethodPut, "/eval_dimension/prompt_lib/1/"+createdRow.ID, adhoc)
	if demote.Code != http.StatusConflict {
		t.Fatalf("demote: expected 409, got %d: %s", demote.Code, demote.Body.String())
	}
	rows = listEvalDimensions(t, router, "")
	if len(rows) != 1 || rows[0].Tier != evaluation.TierProject {
		t.Fatalf("a refused demotion changed the row: %+v", rows)
	}
}

// A promotion is a create: without dimension.create the real repository's
// transaction refuses it with 403, and the row keeps its agent.
func TestEvalDimensionPromotionNeedsTheCreatePermission(t *testing.T) {
	router := newEvalDimensionsRouterWith(t, false)

	adhoc := strings.Replace(evalCreateBody, `"tier": "project"`, `"tier": "agent_adhoc", "application_id": 77`, 1)
	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", adhoc)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}
	var createdRow evaluation.Dimension
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode the created dimension: %v", err)
	}

	promoted := callEvalRoute(t, router, http.MethodPut, "/eval_dimension/prompt_lib/1/"+createdRow.ID, evalCreateBody)
	if promoted.Code != http.StatusForbidden {
		t.Fatalf("promote without create: expected 403, got %d: %s", promoted.Code, promoted.Body.String())
	}
	if rows := listEvalDimensions(t, router, ""); len(rows) != 0 {
		t.Fatalf("the project library = %+v, want it empty", rows)
	}
	agentRows := listEvalDimensions(t, router, "?agent_id=77")
	if len(agentRows) != 1 || agentRows[0].Tier != evaluation.TierAgentAdhoc || agentRows[0].ApplicationID == nil {
		t.Fatalf("agent 77's dimensions = %+v, want the row still scoped to it", agentRows)
	}
}

// Legacy issue 6700: a case can be EXCLUDED and included again. The flag
// reads back through the route, a text edit without the key keeps it, and the
// run repository's case read carries it to the run start.
func TestEvalDatasetCaseExclusionRoundTrips(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newEvalDatasetsRouterOn(NewEvalDatasetsRepo(pool))
	dataset := createEvalDataset(t, router, `{"name":"Excluded","description":"","application_id":null,"is_shared":false}`)

	added := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, `{"input":"first","variables":{}}`)
	if added.Code != http.StatusCreated {
		t.Fatalf("add case: expected 201, got %d: %s", added.Code, added.Body.String())
	}
	var created evaluation.DatasetCase
	if err := json.Unmarshal(added.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode case: %v", err)
	}
	if created.Excluded {
		t.Fatal("a new case must be active")
	}
	casePath := "/eval_dataset_case/prompt_lib/1/" + dataset.ID + "/" + created.ID

	if code := callEvalRoute(t, router, http.MethodPut, casePath,
		`{"input":"first","variables":{},"excluded":true}`).Code; code != http.StatusOK {
		t.Fatalf("exclude: expected 200, got %d", code)
	}
	if stored := readEvalDataset(t, router, dataset.ID); len(stored.Cases) != 1 || !stored.Cases[0].Excluded {
		t.Fatalf("the exclusion did not read back: %+v", stored.Cases)
	}

	runCases, err := NewEvalRunsRepo(pool).DatasetCases(context.Background(), "1", dataset.ID)
	if err != nil {
		t.Fatalf("run repository case read: %v", err)
	}
	if len(runCases) != 1 || !runCases[0].Excluded {
		t.Fatalf("the run start cannot see the exclusion: %+v", runCases)
	}

	if code := callEvalRoute(t, router, http.MethodPut, casePath,
		`{"input":"first, edited","variables":{}}`).Code; code != http.StatusOK {
		t.Fatalf("edit: expected 200, got %d", code)
	}
	stored := readEvalDataset(t, router, dataset.ID)
	if !stored.Cases[0].Excluded || stored.Cases[0].Input != "first, edited" {
		t.Fatalf("a text edit without `excluded` changed the flag: %+v", stored.Cases[0])
	}

	if code := callEvalRoute(t, router, http.MethodPut, casePath,
		`{"input":"first, edited","variables":{},"excluded":false}`).Code; code != http.StatusOK {
		t.Fatalf("include: expected 200, got %d", code)
	}
	if stored := readEvalDataset(t, router, dataset.ID); stored.Cases[0].Excluded {
		t.Fatal("the case was not included again")
	}
}

// The include/exclude toggle sends `{"excluded": ...}` alone, and that writes
// the flag and NOTHING else: a newer edit to the case text survives a toggle
// sent from a stale view. The dataset's `active_case_count` follows the flag.
func TestEvalDatasetCaseExclusionOnlyBodyKeepsTheText(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newEvalDatasetsRouterOn(NewEvalDatasetsRepo(pool))
	dataset := createEvalDataset(t, router, `{"name":"Flag only","description":"","application_id":null,"is_shared":false}`)

	added := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, `{"input":"first","expected_output":"old"}`)
	if added.Code != http.StatusCreated {
		t.Fatalf("add case: expected 201, got %d: %s", added.Code, added.Body.String())
	}
	var created evaluation.DatasetCase
	if err := json.Unmarshal(added.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode case: %v", err)
	}
	casePath := "/eval_dataset_case/prompt_lib/1/" + dataset.ID + "/" + created.ID

	// Another editor corrects the expected answer.
	if code := callEvalRoute(t, router, http.MethodPut, casePath,
		`{"input":"first","expected_output":"corrected"}`).Code; code != http.StatusOK {
		t.Fatalf("edit: expected 200, got %d", code)
	}
	// The stale view toggles exclusion.
	if code := callEvalRoute(t, router, http.MethodPut, casePath, `{"excluded":true}`).Code; code != http.StatusOK {
		t.Fatalf("toggle: expected 200, got %d", code)
	}

	stored := readEvalDataset(t, router, dataset.ID)
	if len(stored.Cases) != 1 || !stored.Cases[0].Excluded {
		t.Fatalf("the flag did not read back: %+v", stored.Cases)
	}
	if got := stored.Cases[0].ExpectedOutput; got == nil || *got != "corrected" {
		t.Fatalf("the toggle reverted the expected answer to %v", got)
	}
	if stored.CaseCount != 1 || stored.ActiveCaseCount != 0 {
		t.Fatalf("case_count %d active_case_count %d, want 1 and 0", stored.CaseCount, stored.ActiveCaseCount)
	}

	missing := callEvalRoute(t, router, http.MethodPut,
		"/eval_dataset_case/prompt_lib/1/"+dataset.ID+"/999999", `{"excluded":false}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("toggle of a missing case: expected 404, got %d", missing.Code)
	}
}

// A case is reachable only through ITS dataset's path. Toggling or rewriting
// an EXISTING case of dataset A through dataset B's URL is a 404 and changes
// nothing: `dataset_id` is in both write predicates, and without it a
// dataset.update holder could include or exclude (or rewrite) the cases of a
// dataset by naming another one, changing what later runs execute.
func TestEvalDatasetCaseWritesAreBoundToTheirDataset(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	router := newEvalDatasetsRouterOn(NewEvalDatasetsRepo(pool))
	datasetA := createEvalDataset(t, router, `{"name":"A","description":"","application_id":null,"is_shared":false}`)
	datasetB := createEvalDataset(t, router, `{"name":"B","description":"","application_id":null,"is_shared":false}`)

	added := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+datasetA.ID, `{"input":"A's case","expected_output":"A's answer"}`)
	if added.Code != http.StatusCreated {
		t.Fatalf("add case: expected 201, got %d: %s", added.Code, added.Body.String())
	}
	var caseOfA evaluation.DatasetCase
	if err := json.Unmarshal(added.Body.Bytes(), &caseOfA); err != nil {
		t.Fatalf("decode case: %v", err)
	}
	crossPath := "/eval_dataset_case/prompt_lib/1/" + datasetB.ID + "/" + caseOfA.ID

	for name, body := range map[string]string{
		"flag-only toggle": `{"excluded":true}`,
		"full rewrite":     `{"input":"rewritten through B","variables":{},"expected_output":null,"excluded":true}`,
	} {
		if code := callEvalRoute(t, router, http.MethodPut, crossPath, body).Code; code != http.StatusNotFound {
			t.Errorf("%s through dataset B: expected 404, got %d", name, code)
		}
	}

	stored := readEvalDataset(t, router, datasetA.ID)
	if len(stored.Cases) != 1 {
		t.Fatalf("dataset A cases = %+v, want its one case", stored.Cases)
	}
	got := stored.Cases[0]
	if got.Excluded || got.Input != "A's case" || got.ExpectedOutput == nil || *got.ExpectedOutput != "A's answer" {
		t.Fatalf("a write through dataset B changed dataset A's case: %+v", got)
	}
	if stored.ActiveCaseCount != 1 {
		t.Fatalf("dataset A active_case_count = %d, want 1", stored.ActiveCaseCount)
	}
}

// The case cap is enforced under CONCURRENT adds. Before the dataset row was
// locked, two adds into a dataset holding cap-1 cases both saw cap-1 under
// READ COMMITTED and both inserted, and they took the same order_index.
func TestEvalDatasetCaseCapHoldsUnderConcurrentAdds(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewEvalDatasetsRepo(pool)
	router := newEvalDatasetsRouterOn(repo)
	dataset := createEvalDataset(t, router, `{"name":"Race","description":"","application_id":null,"is_shared":false}`)

	for i := 0; i < evaluation.MaxCasesPerDataset-1; i++ {
		if _, err := repo.AddCase(context.Background(), "1", dataset.ID, evaluation.CaseWriteInput{
			Input: "seed", Variables: map[string]any{},
		}); err != nil {
			t.Fatalf("seed case %d: %v", i, err)
		}
	}

	const writers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.AddCase(context.Background(), "1", dataset.ID, evaluation.CaseWriteInput{
				Input: "racer", Variables: map[string]any{},
			})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !strings.Contains(err.Error(), "at most") {
			t.Errorf("a refused add answered something other than the cap: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d concurrent adds succeeded, want exactly 1", succeeded)
	}

	stored := readEvalDataset(t, router, dataset.ID)
	if stored.CaseCount != evaluation.MaxCasesPerDataset {
		t.Fatalf("case_count = %d, want the cap %d", stored.CaseCount, evaluation.MaxCasesPerDataset)
	}
	seen := map[int]bool{}
	for _, datasetCase := range stored.Cases {
		if seen[datasetCase.OrderIndex] {
			t.Fatalf("duplicate order_index %d: %+v", datasetCase.OrderIndex, stored.Cases)
		}
		seen[datasetCase.OrderIndex] = true
	}
}

// The same race, made DETERMINISTIC. Another writer has inserted the cap-th
// case and not committed yet. Its foreign-key check holds a KEY SHARE lock on
// the dataset row, so AddCase's `FOR UPDATE` waits for that commit and then
// counts the row. Without the lock, AddCase cannot see the uncommitted row,
// counts cap-1, inserts, and the dataset ends one over the cap once the other
// writer commits. The concurrent test above can pass by luck; this one cannot.
func TestEvalDatasetCaseCapWaitsForAnUncommittedWriter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewEvalDatasetsRepo(pool)
	router := newEvalDatasetsRouterOn(repo)
	dataset := createEvalDataset(t, router, `{"name":"Held","description":"","application_id":null,"is_shared":false}`)
	ctx := context.Background()

	for i := 0; i < evaluation.MaxCasesPerDataset-1; i++ {
		if _, err := repo.AddCase(ctx, "1", dataset.ID, evaluation.CaseWriteInput{
			Input: "seed", Variables: map[string]any{},
		}); err != nil {
			t.Fatalf("seed case %d: %v", i, err)
		}
	}

	schema, err := tenantSchema("1")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	if _, err := other.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.eval_dataset_cases (dataset_id, input, variables, source_type, order_index)
		VALUES ($1::int, 'held', '{}'::jsonb, 'manual', 100)`, schema), dataset.ID); err != nil {
		t.Fatalf("held insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, addErr := repo.AddCase(ctx, "1", dataset.ID, evaluation.CaseWriteInput{
			Input: "late", Variables: map[string]any{},
		})
		done <- addErr
	}()

	select {
	case addErr := <-done:
		t.Fatalf("AddCase finished (%v) while another writer's case was uncommitted: it did not wait for the dataset lock", addErr)
	case <-time.After(500 * time.Millisecond):
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if addErr := <-done; addErr == nil || !strings.Contains(addErr.Error(), "at most") {
		t.Fatalf("AddCase after the cap-th commit answered %v, want the cap refusal", addErr)
	}
	if stored := readEvalDataset(t, router, dataset.ID); stored.CaseCount != evaluation.MaxCasesPerDataset {
		t.Fatalf("case_count = %d, want the cap %d", stored.CaseCount, evaluation.MaxCasesPerDataset)
	}
}

// An update body's `application_id` cannot move an agent dimension to another
// agent, with no tier or with the stored `agent_adhoc` tier.
func TestEvalDimensionUpdateCannotRebindTheAgent(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	adhoc := strings.Replace(evalCreateBody, `"tier": "project"`, `"tier": "agent_adhoc", "application_id": 11`, 1)
	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", adhoc)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}
	var row evaluation.Dimension
	if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}

	bodies := map[string]string{
		"no tier": strings.Replace(evalCreateBody, `"tier": "project"`, `"application_id": 22`, 1),
		"adhoc":   strings.Replace(evalCreateBody, `"tier": "project"`, `"tier": "agent_adhoc", "application_id": 22`, 1),
	}
	for name, body := range bodies {
		response := callEvalRoute(t, router, http.MethodPut, "/eval_dimension/prompt_lib/1/"+row.ID, body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", name, response.Code, response.Body.String())
		}
		if other := listEvalDimensions(t, router, "?agent_id=22"); len(other) != 0 {
			t.Fatalf("%s: the dimension moved to agent 22: %+v", name, other)
		}
		own := listEvalDimensions(t, router, "?agent_id=11")
		if len(own) != 1 || own[0].Tier != evaluation.TierAgentAdhoc || own[0].ApplicationID == nil || *own[0].ApplicationID != 11 {
			t.Fatalf("%s: agent 11's listing = %+v, want the dimension still bound to 11", name, own)
		}
	}
}
