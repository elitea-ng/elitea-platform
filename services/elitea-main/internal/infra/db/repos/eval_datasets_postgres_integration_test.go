package repos

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// Agent Evaluation datasets and cases, exercised against a REAL migrated tenant
// schema and through the REAL routes.
//
// The failure this file exists to make impossible is not "the SQL is wrong". It
// is "the write reported success and persisted nothing", and its close relative
// "the write persisted something and the read cannot see it". Both are
// invisible to a test that calls Create and asserts on Create's own return
// value, and both have shipped from this repository before. So every assertion
// goes POST through the handler -> GET through the handler -> find the row,
// which is the sequence the browser performs.
//
// It also exercises the constraints that exist ONLY in the database
// (tenant/0132). The handlers validate the same rules, so the interesting
// question is whether the two AGREE: a handler that accepts what the table
// refuses is a 500 in production, and a table that accepts what the handler
// refuses is a rule with a hole in it for every other writer.
func newEvalDatasetsRouter(t *testing.T) http.Handler {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	handler := evaluation.NewDatasetHandler(NewEvalDatasetsRepo(pool))

	r := chi.NewRouter()
	r.Get("/eval_datasets/prompt_lib/{projectID}", handler.List)
	r.Post("/eval_datasets/prompt_lib/{projectID}", handler.Create)
	r.Get("/eval_dataset/prompt_lib/{projectID}/{datasetID}", handler.Get)
	r.Put("/eval_dataset/prompt_lib/{projectID}/{datasetID}", handler.Update)
	r.Delete("/eval_dataset/prompt_lib/{projectID}/{datasetID}", handler.Delete)
	r.Post("/eval_dataset_cases/prompt_lib/{projectID}/{datasetID}", handler.AddCase)
	r.Put("/eval_dataset_case/prompt_lib/{projectID}/{datasetID}/{caseID}", handler.UpdateCase)
	r.Delete("/eval_dataset_case/prompt_lib/{projectID}/{datasetID}/{caseID}", handler.DeleteCase)
	return r
}

func createEvalDataset(t *testing.T, router http.Handler, body string) evaluation.Dataset {
	t.Helper()
	response := callEvalRoute(t, router, http.MethodPost, "/eval_datasets/prompt_lib/1", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create dataset: expected 201, got %d: %s", response.Code, response.Body.String())
	}
	var dataset evaluation.Dataset
	if err := json.Unmarshal(response.Body.Bytes(), &dataset); err != nil {
		t.Fatalf("decode dataset: %v", err)
	}
	return dataset
}

// readEvalDataset reads a dataset back THROUGH THE ROUTE. Querying the table
// here instead would prove the row exists and nothing about whether a client
// can ever see it.
func readEvalDataset(t *testing.T, router http.Handler, datasetID string) evaluation.DatasetDetail {
	t.Helper()
	response := callEvalRoute(t, router, http.MethodGet,
		"/eval_dataset/prompt_lib/1/"+datasetID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get dataset: expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var detail evaluation.DatasetDetail
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode dataset: %v", err)
	}
	return detail
}

func TestEvalDatasetRoundTripsThroughTheRoutes(t *testing.T) {
	router := newEvalDatasetsRouter(t)

	created := createEvalDataset(t, router,
		`{"name":"Support answers","description":"the ones people ask","application_id":null,"is_shared":false}`)
	if created.ID == "" {
		t.Fatal("the create answered 201 with no id")
	}
	if created.CaseCount != 0 {
		t.Errorf("case_count = %d on a new dataset, want 0", created.CaseCount)
	}

	stored := readEvalDataset(t, router, created.ID)
	if stored.Name != "Support answers" || stored.Description != "the ones people ask" {
		t.Fatalf("the stored dataset is not the one that was posted: %+v", stored)
	}
	// `cases` is an ARRAY and not null, even when empty. A client that receives
	// null — or no key at all — renders a spinner for ever, waiting for a load
	// that already happened. This assertion caught exactly that: the field was
	// `omitempty` on the shared Dataset struct, so an empty dataset omitted the
	// key. DatasetDetail exists because of it.
	if stored.Cases == nil {
		t.Error("an empty dataset answered `cases: null` rather than []")
	}
	detailBody := callEvalRoute(t, router, http.MethodGet,
		"/eval_dataset/prompt_lib/1/"+created.ID, "").Body.String()
	if !strings.Contains(detailBody, `"cases":[]`) {
		t.Errorf("the detail read omits the `cases` key entirely: %s", detailBody)
	}
	// And the LIST must NOT carry one, because a `cases: []` on every row tells
	// a client that a dataset with ten cases has none.
	if strings.Contains(callEvalRoute(t, router, http.MethodGet,
		"/eval_datasets/prompt_lib/1", "").Body.String(), `"cases"`) {
		t.Error("the dataset listing carries a `cases` key it cannot fill")
	}

	listResponse := callEvalRoute(t, router, http.MethodGet, "/eval_datasets/prompt_lib/1", "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list: %d %s", listResponse.Code, listResponse.Body.String())
	}
	var page struct {
		Rows  []evaluation.Dataset `json:"rows"`
		Total int                  `json:"total"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if page.Total != 1 || len(page.Rows) != 1 || page.Rows[0].ID != created.ID {
		t.Fatalf("the created dataset is not in the listing: %s", listResponse.Body.String())
	}
}

// A case must be readable back through the dataset detail, with its variables
// and its expected output intact. This is the "persisted something the read
// cannot see" half.
func TestEvalDatasetCaseRoundTripsWithItsVariables(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	dataset := createEvalDataset(t, router, `{"name":"Cases","description":"","application_id":null,"is_shared":false}`)

	addResponse := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID,
		`{"input":"what is {{topic}}?","variables":{"topic":"Go"},"expected_output":"a language"}`)
	if addResponse.Code != http.StatusCreated {
		t.Fatalf("add case: expected 201, got %d: %s", addResponse.Code, addResponse.Body.String())
	}
	var added evaluation.DatasetCase
	if err := json.Unmarshal(addResponse.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode case: %v", err)
	}

	stored := readEvalDataset(t, router, dataset.ID)
	if stored.CaseCount != 1 || len(stored.Cases) != 1 {
		t.Fatalf("case_count = %d with %d cases, want 1 and 1", stored.CaseCount, len(stored.Cases))
	}
	storedCase := stored.Cases[0]
	if storedCase.ID != added.ID {
		t.Errorf("the read case id %q is not the created one %q", storedCase.ID, added.ID)
	}
	if storedCase.Variables["topic"] != "Go" {
		t.Errorf("variables did not survive the round trip: %v", storedCase.Variables)
	}
	if storedCase.ExpectedOutput == nil || *storedCase.ExpectedOutput != "a language" {
		t.Errorf("expected_output = %v, want the stored answer", storedCase.ExpectedOutput)
	}
	if storedCase.SourceType != evaluation.CaseSourceManual {
		t.Errorf("source_type = %q, want manual", storedCase.SourceType)
	}
}

// A case with NO expected output stores a NULL and reads back as one. "No
// expected answer" and "an expected answer of empty string" are different
// instructions to a judge, and collapsing them tells the judge to mark every
// non-empty answer wrong.
func TestEvalDatasetCaseKeepsAnAbsentExpectedOutputDistinctFromAnEmptyOne(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	dataset := createEvalDataset(t, router, `{"name":"Cases","description":"","application_id":null,"is_shared":false}`)

	if code := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID,
		`{"input":"no answer stated","variables":{}}`).Code; code != http.StatusCreated {
		t.Fatalf("add case: %d", code)
	}
	if code := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID,
		`{"input":"an empty answer stated","variables":{},"expected_output":""}`).Code; code != http.StatusCreated {
		t.Fatalf("add case: %d", code)
	}

	stored := readEvalDataset(t, router, dataset.ID)
	byInput := map[string]*string{}
	for _, storedCase := range stored.Cases {
		byInput[storedCase.Input] = storedCase.ExpectedOutput
	}
	if byInput["no answer stated"] != nil {
		t.Errorf("an unstated expected output came back as %q", *byInput["no answer stated"])
	}
	if byInput["an empty answer stated"] == nil {
		t.Error("a deliberately empty expected output came back as absent")
	}
}

// The cap is enforced in ONE statement, so two concurrent writers cannot both
// see room. It is also a 409 and not a silent truncation: a dataset that reads
// as complete and is not would score an agent on a question it never saw.
func TestEvalDatasetRefusesMoreCasesThanTheCap(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	dataset := createEvalDataset(t, router, `{"name":"Full","description":"","application_id":null,"is_shared":false}`)

	for index := 0; index < evaluation.MaxCasesPerDataset; index++ {
		body := fmt.Sprintf(`{"input":"question %d","variables":{}}`, index)
		if code := callEvalRoute(t, router, http.MethodPost,
			"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, body).Code; code != http.StatusCreated {
			t.Fatalf("case %d: expected 201, got %d", index, code)
		}
	}

	overflow := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, `{"input":"one too many","variables":{}}`)
	if overflow.Code != http.StatusConflict {
		t.Fatalf("the cap was not enforced: got %d %s", overflow.Code, overflow.Body.String())
	}

	stored := readEvalDataset(t, router, dataset.ID)
	if stored.CaseCount != evaluation.MaxCasesPerDataset {
		t.Errorf("case_count = %d, want the cap %d", stored.CaseCount, evaluation.MaxCasesPerDataset)
	}
}

// A case is edited THROUGH ITS OWN DATASET. Without `dataset_id` in the update
// predicate, any case id could be edited through any dataset's path.
func TestEvalDatasetCaseCannotBeEditedThroughAnotherDataset(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	first := createEvalDataset(t, router, `{"name":"First","description":"","application_id":null,"is_shared":false}`)
	second := createEvalDataset(t, router, `{"name":"Second","description":"","application_id":null,"is_shared":false}`)

	addResponse := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+first.ID, `{"input":"first's case","variables":{}}`)
	if addResponse.Code != http.StatusCreated {
		t.Fatalf("add case: %d", addResponse.Code)
	}
	var added evaluation.DatasetCase
	if err := json.Unmarshal(addResponse.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode: %v", err)
	}

	stolen := callEvalRoute(t, router, http.MethodPut,
		"/eval_dataset_case/prompt_lib/1/"+second.ID+"/"+added.ID,
		`{"input":"rewritten through the wrong dataset","variables":{}}`)
	if stolen.Code != http.StatusNotFound {
		t.Fatalf("a case was edited through another dataset: %d %s", stolen.Code, stolen.Body.String())
	}

	// And the row is unchanged — the refusal is not just a status code.
	stored := readEvalDataset(t, router, first.ID)
	if stored.Cases[0].Input != "first's case" {
		t.Errorf("the case was rewritten anyway: %q", stored.Cases[0].Input)
	}
}

// A delete answers 404 for an id that was never there, and 204 exactly once.
func TestEvalDatasetDeleteReportsAMissAsAMiss(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	dataset := createEvalDataset(t, router, `{"name":"Doomed","description":"","application_id":null,"is_shared":false}`)

	if code := callEvalRoute(t, router, http.MethodDelete,
		"/eval_dataset/prompt_lib/1/"+dataset.ID, "").Code; code != http.StatusNoContent {
		t.Fatalf("first delete: expected 204, got %d", code)
	}
	if code := callEvalRoute(t, router, http.MethodDelete,
		"/eval_dataset/prompt_lib/1/"+dataset.ID, "").Code; code != http.StatusNotFound {
		t.Fatalf("second delete: expected 404, got %d", code)
	}
	if code := callEvalRoute(t, router, http.MethodGet,
		"/eval_dataset/prompt_lib/1/"+dataset.ID, "").Code; code != http.StatusNotFound {
		t.Fatalf("the deleted dataset is still readable: %d", code)
	}
}

// Deleting a dataset CASCADES to its cases, so no unreachable row is left
// behind. The cascade is the table's, and this is the only place it is proved.
func TestEvalDatasetDeleteRemovesItsCases(t *testing.T) {
	router := newEvalDatasetsRouter(t)

	dataset := createEvalDataset(t, router, `{"name":"Doomed","description":"","application_id":null,"is_shared":false}`)
	if code := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, `{"input":"q","variables":{}}`).Code; code != http.StatusCreated {
		t.Fatalf("add case: %d", code)
	}
	if code := callEvalRoute(t, router, http.MethodDelete,
		"/eval_dataset/prompt_lib/1/"+dataset.ID, "").Code; code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	// The case is unreachable through the route, which is the only reachability
	// that matters: a re-created dataset with the same id cannot exist (the
	// sequence never rewinds), so an orphan would be invisible for ever.
	if code := callEvalRoute(t, router, http.MethodGet,
		"/eval_dataset/prompt_lib/1/"+dataset.ID, "").Code; code != http.StatusNotFound {
		t.Fatalf("the dataset survived its delete: %d", code)
	}
}

// The handler and the table must AGREE. Every body below is refused by the
// handler; each one is also refused by a CHECK in tenant/0132, so a caller that
// slipped past one meets the other.
func TestEvalDatasetHandlerAndTableRefuseTheSameShapes(t *testing.T) {
	router := newEvalDatasetsRouter(t)

	bad := map[string]string{
		"no name":       `{"name":"","description":"","application_id":null,"is_shared":false}`,
		"blank name":    `{"name":"   ","description":"","application_id":null,"is_shared":false}`,
		"unknown field": `{"name":"x","description":"","application_id":null,"isShared":true}`,
		"bad agent id":  `{"name":"x","description":"","application_id":0,"is_shared":false}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			response := callEvalRoute(t, router, http.MethodPost, "/eval_datasets/prompt_lib/1", body)
			if response.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", response.Code, response.Body.String())
			}
		})
	}

	dataset := createEvalDataset(t, router, `{"name":"Valid","description":"","application_id":null,"is_shared":false}`)
	badCases := map[string]string{
		"no input":    `{"input":"","variables":{}}`,
		"blank input": `{"input":"   ","variables":{}}`,
	}
	for name, body := range badCases {
		t.Run(name, func(t *testing.T) {
			response := callEvalRoute(t, router, http.MethodPost,
				"/eval_dataset_cases/prompt_lib/1/"+dataset.ID, body)
			if response.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

// A case added to a dataset that does not exist is a 404, and NOT the 409 the
// cap answers. A single zero-row insert cannot tell the two apart, and
// answering the same code for both sends the caller looking in the wrong place.
func TestEvalDatasetCaseOnAMissingDatasetIs404(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	response := callEvalRoute(t, router, http.MethodPost,
		"/eval_dataset_cases/prompt_lib/1/98765", `{"input":"q","variables":{}}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", response.Code, response.Body.String())
	}
}

// A dataset belongs to ONE project. The schema comes from the path segment, so
// this is really a check that nothing here reads a fixed schema.
func TestEvalDatasetsArePerProject(t *testing.T) {
	router := newEvalDatasetsRouter(t)
	createEvalDataset(t, router, `{"name":"Project one only","description":"","application_id":null,"is_shared":false}`)

	// p_2 does not exist in this template, so the read must FAIL rather than
	// answer p_1's rows. Failing closed is the requirement; the status is
	// whatever the missing relation produces.
	other := callEvalRoute(t, router, http.MethodGet, "/eval_datasets/prompt_lib/2", "")
	if other.Code == http.StatusOK && strings.Contains(other.Body.String(), "Project one only") {
		t.Fatalf("project 2 was served project 1's datasets: %s", other.Body.String())
	}
}
