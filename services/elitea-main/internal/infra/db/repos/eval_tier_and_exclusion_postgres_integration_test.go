package repos

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
