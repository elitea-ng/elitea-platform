package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type fakeSuites struct {
	rows    map[string]Suite
	created []SuiteWrite
}

func newFakeSuites() *fakeSuites { return &fakeSuites{rows: map[string]Suite{}} }

func (f *fakeSuites) ListSuites(context.Context, string) ([]Suite, error) {
	out := []Suite{}
	for _, suite := range f.rows {
		out = append(out, suite)
	}
	return out, nil
}
func (f *fakeSuites) GetSuite(_ context.Context, _, id string) (Suite, error) {
	suite, ok := f.rows[id]
	if !ok {
		return Suite{}, apierr.NotFound("suite not found")
	}
	return suite, nil
}
func (f *fakeSuites) CreateSuite(_ context.Context, _ string, in SuiteWrite) (Suite, error) {
	f.created = append(f.created, in)
	suite := Suite{ID: "1", Name: in.Name, Description: in.Description, RunLimits: in.RunLimits}
	f.rows["1"] = suite
	return suite, nil
}
func (f *fakeSuites) UpdateSuite(_ context.Context, _, id string, in SuiteWrite) (Suite, error) {
	suite := Suite{ID: id, Name: in.Name, RunLimits: in.RunLimits}
	f.rows[id] = suite
	return suite, nil
}
func (f *fakeSuites) DeleteSuite(_ context.Context, _, id string) error {
	delete(f.rows, id)
	return nil
}

func suiteRouter(repo SuiteRepository) http.Handler {
	handler := NewSuiteHandler(repo)
	r := chi.NewRouter()
	r.Get("/eval_suites/prompt_lib/{projectID}", handler.List)
	r.Post("/eval_suites/prompt_lib/{projectID}", handler.Create)
	r.Put("/eval_suite/prompt_lib/{projectID}/{suiteID}", handler.Update)
	r.Delete("/eval_suite/prompt_lib/{projectID}/{suiteID}", handler.Delete)
	return r
}

func TestSuiteCreateDefaultsTheRunLimitActionToStop(t *testing.T) {
	t.Parallel()

	repo := newFakeSuites()
	response := call(t, suiteRouter(repo), http.MethodPost, "/eval_suites/prompt_lib/1",
		`{"name":"  Nightly  ","token_limit_per_run":50000,"cost_limit_per_case":0.25}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}
	got := repo.created[0]
	if got.Name != "Nightly" || got.OnRunLimit != RunLimitActionStop {
		t.Errorf("created = %+v", got)
	}
	if got.TokenLimitPerRun == nil || *got.TokenLimitPerRun != 50000 || got.TokenLimitPerCase != nil {
		t.Errorf("limits = %+v, want only the two that were sent", got.RunLimits)
	}
}

func TestSuiteWriteRefusesBadSettings(t *testing.T) {
	t.Parallel()

	router := suiteRouter(newFakeSuites())
	for name, body := range map[string]string{
		"no name":              `{"name":" "}`,
		"zero token limit":     `{"name":"a","token_limit_per_run":0}`,
		"negative cost":        `{"name":"a","cost_limit_per_case":-1}`,
		"fractional tokens":    `{"name":"a","token_limit_per_case":1.5}`,
		"huge tokens":          `{"name":"a","token_limit_per_run":99999999999}`,
		"unknown action":       `{"name":"a","on_run_limit":"explode"}`,
		"unknown field":        `{"name":"a","steps_limit":3}`,
		"name too long":        `{"name":"` + strings.Repeat("x", 129) + `"}`,
		"description too long": `{"name":"a","description":"` + strings.Repeat("x", 2001) + `"}`,
	} {
		if response := call(t, router, http.MethodPost, "/eval_suites/prompt_lib/1", body); response.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400: %s", name, response.Code, response.Body.String())
		}
	}
}

func TestSuiteListAnswersRowsEnvelopeAndDeleteIs204(t *testing.T) {
	t.Parallel()

	repo := newFakeSuites()
	router := suiteRouter(repo)
	call(t, router, http.MethodPost, "/eval_suites/prompt_lib/1", `{"name":"a","on_run_limit":"report"}`)

	response := call(t, router, http.MethodGet, "/eval_suites/prompt_lib/1", "")
	var body struct {
		Rows  []Suite `json:"rows"`
		Total int     `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Total != 1 || body.Rows[0].OnRunLimit != "report" {
		t.Fatalf("body = %s (%v)", response.Body.String(), err)
	}
	// The suite's limits are flat on the wire, not nested.
	if !strings.Contains(response.Body.String(), `"token_limit_per_run":null`) {
		t.Errorf("limits are not flat keys of the suite: %s", response.Body.String())
	}
	if response := call(t, router, http.MethodDelete, "/eval_suite/prompt_lib/1/1", ""); response.Code != http.StatusNoContent {
		t.Errorf("delete code = %d", response.Code)
	}
}

// The run start freezes the suite's limits into the snapshot: a later edit of
// the suite must not change a run that already started.
func TestRunStartFreezesTheSuiteLimitsIntoTheSnapshot(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	suites := newFakeSuites()
	tokens := int64(900)
	suites.rows["4"] = Suite{ID: "4", Name: "Cheap", RunLimits: RunLimits{TokenLimitPerRun: &tokens, OnRunLimit: RunLimitActionReport}}
	handler := NewRunHandler(repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, &recordingEnqueuer{}).WithSuites(suites)
	r := chi.NewRouter()
	r.Post("/eval_runs/prompt_lib/{projectID}", handler.Start)

	response := call(t, r, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"],"suite_id":"4"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}
	var created Run
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	limits := created.Snapshot.Limits
	if limits == nil || limits.TokenLimitPerRun == nil || *limits.TokenLimitPerRun != 900 || limits.OnRunLimit != RunLimitActionReport {
		t.Fatalf("snapshot limits = %+v", limits)
	}
	if created.Snapshot.SuiteID != "4" {
		t.Errorf("snapshot suite = %q", created.Snapshot.SuiteID)
	}

	// An unknown suite is a 404 and writes no run.
	before := len(repo.runs)
	if response := call(t, r, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"],"suite_id":"99"}`); response.Code != http.StatusNotFound {
		t.Errorf("unknown suite: code = %d, want 404", response.Code)
	}
	if len(repo.runs) != before {
		t.Error("a run was written for an unknown suite")
	}
}

func TestRunStartWithASuiteButNoSuiteStoreIsRefused(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, &recordingEnqueuer{})
	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"],"suite_id":"4"}`)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501: %s", response.Code, response.Body.String())
	}
}
