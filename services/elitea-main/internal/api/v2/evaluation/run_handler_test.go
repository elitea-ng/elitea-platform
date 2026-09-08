package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// dimensionLibrary is a Repository over a fixed slice, so a run test can state
// exactly what the project's library holds.
type dimensionLibrary struct{ rows []Dimension }

func (d dimensionLibrary) List(context.Context, string, ListFilter) ([]Dimension, error) {
	return d.rows, nil
}
func (d dimensionLibrary) Create(context.Context, string, Dimension) (Dimension, error) {
	return Dimension{}, nil
}
func (d dimensionLibrary) Update(context.Context, string, string, Dimension) (Dimension, error) {
	return Dimension{}, nil
}
func (d dimensionLibrary) Delete(context.Context, string, string) error { return nil }

// recordingEnqueuer proves the run was HANDED TO the orchestrator. Without this
// the route would write a `created` row that nothing ever executes, and answer
// 201 — a write that reports success and does nothing, which is the shape this
// repository's evidence bar is written about.
type recordingEnqueuer struct {
	enqueued  []RunRef
	cancelled []RunRef
}

func (e *recordingEnqueuer) Enqueue(ref RunRef) { e.enqueued = append(e.enqueued, ref) }
func (e *recordingEnqueuer) Cancel(ref RunRef)  { e.cancelled = append(e.cancelled, ref) }

func aiLibraryDimension() Dimension {
	target := 4.0
	return Dimension{
		ID: "3", Name: "Helpfulness", Description: "Does it help?",
		AllowedEngines: []string{EngineAI},
		ScaleType:      ScaleOrdinal, ScaleMin: 1, ScaleMax: 5,
		Polarity: PolarityHigherBetter, DefaultWeight: 2,
		DefaultTarget: &target, DefaultTargetOperator: ">=",
	}
}

func runRouter(t *testing.T, repo RunRepository, library Repository, queue Enqueuer) http.Handler {
	t.Helper()
	handler := NewRunHandler(repo, library, queue)
	r := chi.NewRouter()
	r.Get("/eval_runs/prompt_lib/{projectID}", handler.List)
	r.Post("/eval_runs/prompt_lib/{projectID}", handler.Start)
	r.Get("/eval_run/prompt_lib/{projectID}/{runID}", handler.Get)
	r.Post("/eval_run_cancel/prompt_lib/{projectID}/{runID}", handler.Cancel)
	r.Get("/eval_results/prompt_lib/{projectID}/{runID}", handler.Results)
	return r
}

func call(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// The snapshot is what a run is scored against, and it must carry the
// dimension's OWN defaults — a binding that invented a weight of 1 for a
// dimension authored at 2 would silently re-weight every scorecard.
func TestRunStartFreezesTheDimensionDefaultsIntoTheSnapshot(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	queue := &recordingEnqueuer{}
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, queue)

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}

	var created Run
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Status != RunStatusCreated {
		t.Errorf("status = %q, want created", created.Status)
	}
	if created.ExecutionMode != ExecutionModePredictBlocking {
		t.Errorf("execution_mode = %q — the read must say what actually ran", created.ExecutionMode)
	}
	if created.Progress.Total != 2 {
		t.Errorf("progress.total = %d, want the dataset's case count", created.Progress.Total)
	}
	if len(created.Snapshot.Bindings) != 1 {
		t.Fatalf("snapshot has %d bindings, want 1", len(created.Snapshot.Bindings))
	}
	binding := created.Snapshot.Bindings[0]
	if binding.Weight != 2 {
		t.Errorf("weight = %v, want the dimension's own default of 2", binding.Weight)
	}
	if binding.Target == nil || *binding.Target != 4 || binding.TargetOperator != ">=" {
		t.Errorf("target = %v %q, want >= 4 from the dimension", binding.Target, binding.TargetOperator)
	}
	if binding.Engine != EngineAI {
		t.Errorf("engine = %q, want ai", binding.Engine)
	}
	frozen, ok := created.Snapshot.Dimensions["3"]
	if !ok {
		t.Fatal("the snapshot carries no dimension for its own binding")
	}
	// The RUBRIC is frozen, because it is the judge's prompt. A run explained
	// by a rubric it was not scored against is worse than no explanation.
	if frozen.Description != "Does it help?" || frozen.ScaleMax != 5 {
		t.Errorf("frozen dimension = %+v, want the library row as it was", frozen)
	}

	// THE ENQUEUE. A row with nobody to execute it is a run that never starts.
	if len(queue.enqueued) != 1 || queue.enqueued[0].RunID != created.ID {
		t.Fatalf("enqueued %+v, want the created run", queue.enqueued)
	}
}

// A non-`ai` dimension is refused with 501 AND A NAMED REASON. It is not
// skipped and it is not scored some other way: there is no code sandbox in this
// service, and a code validation that silently passed would be the most
// dangerous fallback in the feature.
func TestRunStartRefusesANonAIDimensionWithAReason(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	codeDimension := Dimension{
		ID: "4", Name: "JSON shape", AllowedEngines: []string{EngineCode},
		ScaleType: ScaleBinary, ScaleMin: 0, ScaleMax: 1,
		Polarity: PolarityHigherBetter, Code: "return true", ReturnContract: ReturnContractBool,
	}
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{codeDimension}}, &recordingEnqueuer{})

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["4"]}`)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d, want 501: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, phrase := range []string{"sandbox", "ai engine"} {
		if !strings.Contains(body, phrase) {
			t.Errorf("the refusal does not mention %q: %s", phrase, body)
		}
	}
}

// A dimension the project's library does not hold is a 400, not a silent drop.
// A run that quietly scored fewer criteria than the request named would report
// a headline over a set nobody chose.
func TestRunStartRefusesADimensionOutsideTheLibrary(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, &recordingEnqueuer{})

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3","999"]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "999") {
		t.Errorf("the refusal does not name the missing dimension: %s", response.Body.String())
	}
}

func TestRunStartValidatesTheBody(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, &recordingEnqueuer{})

	cases := map[string]string{
		"no dataset":    `{"application_version_id":9,"dimension_ids":["3"]}`,
		"no dimensions": `{"dataset_id":"5","application_version_id":9,"dimension_ids":[]}`,
		"no version":    `{"dataset_id":"5","dimension_ids":["3"]}`,
		"bad trigger":   `{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"],"trigger_type":"whenever"}`,
		"unknown field": `{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"],"suite_id":"1"}`,
		"too many dims": `{"dataset_id":"5","application_version_id":9,"dimension_ids":["1","2","3","4","5","6"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1", body); response.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400: %s", response.Code, response.Body.String())
			}
		})
	}
}

// An empty dataset is refused BEFORE a run row is written. A run over nothing
// finishes instantly with no headline, which reads as a broken agent.
func TestRunStartRefusesAnEmptyDataset(t *testing.T) {
	t.Parallel()

	repo := newFakeRunRepo()
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, &recordingEnqueuer{})

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"]}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400: %s", response.Code, response.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Error("a run row was written for a dataset with no cases")
	}
}

// The scorecard OMITS `human_scores` and says why, rather than answering `[]`.
// An empty array tells a client "there are none"; the truth is "this deployment
// cannot have any".
func TestScorecardNamesWhatItCannotServeInsteadOfAnsweringEmpty(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	score := 80.0
	repo.seedRun(Run{ID: "1", DatasetID: "5", Status: RunStatusFinished, HeadlineScore: &score})
	router := runRouter(t, repo, dimensionLibrary{}, &recordingEnqueuer{})

	response := call(t, router, http.MethodGet, "/eval_results/prompt_lib/1/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := body["human_scores"]; present {
		t.Error("the scorecard answers `human_scores`, which this release cannot produce")
	}
	for _, key := range []string{"run", "results", "headline_score", "total", "offset", "unavailable"} {
		if _, present := body[key]; !present {
			t.Errorf("the scorecard has no %q key", key)
		}
	}
	var gaps []capabilityGap
	if err := json.Unmarshal(body["unavailable"], &gaps); err != nil {
		t.Fatalf("decode unavailable: %v", err)
	}
	found := false
	for _, gap := range gaps {
		if gap.Key == "human_scores" && gap.Reason != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("the omission of human_scores is not explained: %+v", gaps)
	}
}

// A scorecard for a run that does not exist is a 404. A 200 with an empty
// result list reads as "the run produced nothing", which is a different and
// much more alarming fact.
func TestScorecardForAMissingRunIs404(t *testing.T) {
	t.Parallel()

	router := runRouter(t, newFakeRunRepo(), dimensionLibrary{}, &recordingEnqueuer{})
	if response := call(t, router, http.MethodGet, "/eval_results/prompt_lib/1/404", ""); response.Code == http.StatusOK {
		t.Fatalf("code = %d, want a refusal: %s", response.Code, response.Body.String())
	}
}

// A cancel writes the row FIRST and signals the goroutine second, so a run
// being executed by another replica is still stopped.
func TestCancelWritesTheRowAndSignalsTheWorker(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Status = RunStatusRunning
	repo.seedRun(run)
	queue := &recordingEnqueuer{}
	router := runRouter(t, repo, dimensionLibrary{}, queue)

	response := call(t, router, http.MethodPost, "/eval_run_cancel/prompt_lib/1/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}
	if got := repo.run("1"); got.Status != RunStatusCancelled {
		t.Errorf("stored status = %q, want cancelled", got.Status)
	}
	if len(queue.cancelled) != 1 {
		t.Error("the in-flight worker was not signalled")
	}
}

func TestRunListRejectsAnOutOfRangeLimit(t *testing.T) {
	t.Parallel()

	router := runRouter(t, newFakeRunRepo(), dimensionLibrary{}, &recordingEnqueuer{})
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=many", "?application_id=x"} {
		if response := call(t, router, http.MethodGet, "/eval_runs/prompt_lib/1"+query, ""); response.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", query, response.Code)
		}
	}
}

// The list envelope is `{rows,total}` and NOT a bare array or `{items}`. A
// client reading the wrong key renders an empty page behind a 200 with nothing
// in the console (#132).
func TestRunListAnswersRowsEnvelope(t *testing.T) {
	t.Parallel()

	router := runRouter(t, newFakeRunRepo(), dimensionLibrary{}, &recordingEnqueuer{})
	response := call(t, router, http.MethodGet, "/eval_runs/prompt_lib/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d", response.Code)
	}
	var page struct {
		Rows  []Run `json:"rows"`
		Total int   `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Rows == nil {
		t.Error("an empty listing answered `rows: null` rather than an empty array")
	}
}
