package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// humanDimension is a 1..5 ordinal dimension only a person can score.
func humanLibraryDimension() Dimension {
	target := 4.0
	return Dimension{
		ID: "7", Name: "Tone", AllowedEngines: []string{EngineHuman},
		ScaleType: ScaleOrdinal, ScaleMin: 1, ScaleMax: 5,
		Polarity: PolarityHigherBetter, DefaultWeight: 1,
		DefaultTarget: &target, DefaultTargetOperator: ">=",
	}
}

// humanRun is twoCaseRun with the dimension bound to the human engine.
func humanRun() (*fakeRunRepo, Run) {
	repo, run := twoCaseRun()
	run.Snapshot.Dimensions = map[string]SnapshotDimension{
		"7": {Name: "Tone", ScaleType: ScaleOrdinal, ScaleMin: 1, ScaleMax: 5, Polarity: PolarityHigherBetter},
	}
	target := 4.0
	run.Snapshot.Bindings = []SnapshotBinding{{
		DimensionID: "7", Engine: EngineHuman, Weight: 1, Target: &target, TargetOperator: ">=",
	}}
	repo.seedRun(run)
	return repo, run
}

// A dimension that allows only the human engine is no longer refused at run
// start: it is bound with the human engine.
func TestRunStartBindsAHumanOnlyDimensionToTheHumanEngine(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	queue := &recordingEnqueuer{}
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{humanLibraryDimension()}}, queue)

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["7"]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}
	var created Run
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Snapshot.Bindings) != 1 || created.Snapshot.Bindings[0].Engine != EngineHuman {
		t.Fatalf("bindings = %+v, want one human binding", created.Snapshot.Bindings)
	}
	if len(queue.enqueued) != 1 {
		t.Error("the run was not enqueued")
	}
}

// A dimension that allows both engines is scored by the AI judge, as before.
func TestRunStartPrefersTheAIEngineWhenBothAreAllowed(t *testing.T) {
	t.Parallel()

	dimension := aiLibraryDimension()
	dimension.AllowedEngines = []string{EngineHuman, EngineAI}
	if engine, ok := runEngine(dimension); !ok || engine != EngineAI {
		t.Fatalf("engine = %q %v, want ai", engine, ok)
	}
	if _, ok := runEngine(Dimension{AllowedEngines: []string{EngineCode}}); ok {
		t.Fatal("a code-only dimension must still be refused")
	}
}

// The orchestrator answers a human binding's case and then waits: no judge
// call, a pending_human result, no headline.
func TestOrchestratorLeavesAHumanBindingPending(t *testing.T) {
	t.Parallel()

	repo, _ := humanRun()
	judge := &scriptedJudge{}
	completer := &stubCompleter{answer: "an answer"}
	NewOrchestrator(repo, judge, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if judge.calls != 0 {
		t.Errorf("the judge was called %d times for a human dimension", judge.calls)
	}
	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q (%s), want finished", run.Status, run.Error)
	}
	if run.HeadlineScore != nil {
		t.Errorf("headline = %v, want none until a reviewer scores", *run.HeadlineScore)
	}
	results := repo.storedResults()
	if len(results) != 2 {
		t.Fatalf("stored %d results, want 2", len(results))
	}
	for _, result := range results {
		if result.Status != ResultStatusPendingHuman || result.NativeScore != nil || result.NormalizedScore != nil {
			t.Errorf("case %s: %+v, want pending_human with no score", result.DatasetCaseID, result)
		}
		if result.Evidence["output"] != "an answer" {
			t.Errorf("case %s: the answer the reviewer scores is not in the evidence", result.DatasetCaseID)
		}
	}
}

// A resumed run must not re-answer a pending case: that would store the
// pending status over a score a reviewer may have recorded since.
func TestResumedRunDoesNotRescoreAPendingHumanCase(t *testing.T) {
	t.Parallel()

	repo, _ := humanRun()
	repo.results = map[string]RunResult{
		"1/11/7": {RunID: "1", DatasetCaseID: "11", DimensionID: "7", Status: ResultStatusPendingHuman},
	}
	completer := &stubCompleter{answer: "an answer"}
	NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if len(completer.requests) != 1 {
		t.Errorf("the agent was called %d times, want once (case 12 only)", len(completer.requests))
	}
}

func TestPrepareHumanScore(t *testing.T) {
	t.Parallel()

	_, run := humanRun()
	good := HumanScoreInput{DatasetCaseID: "11", DimensionID: "7", Score: 4, Note: "  warm  "}
	write, err := PrepareHumanScore(run, good)
	if err != nil {
		t.Fatalf("a valid score was refused: %v", err)
	}
	// 4 on 1..5 is 75; it meets the >= 4 target; the note is trimmed.
	if write.NormalizedScore != 75 || write.TargetMet == nil || !*write.TargetMet || write.Note != "warm" {
		t.Errorf("write = %+v", write)
	}

	cases := map[string]HumanScoreInput{
		"above the scale":       {DatasetCaseID: "11", DimensionID: "7", Score: 6},
		"below the scale":       {DatasetCaseID: "11", DimensionID: "7", Score: 0},
		"a case not in the run": {DatasetCaseID: "99", DimensionID: "7", Score: 3},
		"a dimension not in it": {DatasetCaseID: "11", DimensionID: "8", Score: 3},
		"no case":               {DimensionID: "7", Score: 3},
		"note too long":         {DatasetCaseID: "11", DimensionID: "7", Score: 3, Note: strings.Repeat("x", MaxHumanNoteLength+1)},
	}
	for name, input := range cases {
		if _, err := PrepareHumanScore(run, input); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// An AI-scored dimension is not scored by a person.
	_, aiRun := twoCaseRun()
	if _, err := PrepareHumanScore(aiRun, HumanScoreInput{DatasetCaseID: "11", DimensionID: "3", Score: 3}); err == nil ||
		!strings.Contains(err.Error(), "not by a human") {
		t.Errorf("ai binding: %v", err)
	}
}

func TestPrepareHumanScoreBinaryAcceptsZeroAndOneOnly(t *testing.T) {
	t.Parallel()

	_, run := humanRun()
	run.Snapshot.Dimensions["7"] = SnapshotDimension{Name: "Safe", ScaleType: ScaleBinary, ScaleMin: 0, ScaleMax: 1, Polarity: PolarityHigherBetter}
	run.Snapshot.Bindings[0].Target = nil
	for _, score := range []float64{0, 1} {
		if _, err := PrepareHumanScore(run, HumanScoreInput{DatasetCaseID: "11", DimensionID: "7", Score: score}); err != nil {
			t.Errorf("%v refused: %v", score, err)
		}
	}
	if _, err := PrepareHumanScore(run, HumanScoreInput{DatasetCaseID: "11", DimensionID: "7", Score: 0.5}); err == nil {
		t.Error("0.5 accepted on a binary dimension")
	}
}

// fakeHumanScores records what the handler hands the repository.
type fakeHumanScores struct {
	mu       sync.Mutex
	written  []HumanScoreWrite
	stored   []HumanScore
	recorded HumanScoreRecorded
	err      error
}

func (f *fakeHumanScores) ListHumanScores(context.Context, string, string) ([]HumanScore, error) {
	return f.stored, nil
}

func (f *fakeHumanScores) RecordHumanScore(_ context.Context, _ string, in HumanScoreWrite) (HumanScoreRecorded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, in)
	return f.recorded, f.err
}

func humanRouter(repo RunRepository, scores HumanScoreRepository) http.Handler {
	handler := NewHumanScoreHandler(repo, scores)
	r := chi.NewRouter()
	r.Get("/eval_human_scores/prompt_lib/{projectID}/{runID}", handler.List)
	r.Post("/eval_human_scores/prompt_lib/{projectID}/{runID}", handler.Create)
	return r
}

func TestHumanScoreCreateStoresTheNormalisedScoreAgainstTheSnapshot(t *testing.T) {
	t.Parallel()

	repo, _ := humanRun()
	headline := 75.0
	scores := &fakeHumanScores{recorded: HumanScoreRecorded{
		Score: HumanScore{ID: "1", NativeScore: 4}, HeadlineScore: &headline,
	}}
	router := humanRouter(repo, scores)

	response := call(t, router, http.MethodPost, "/eval_human_scores/prompt_lib/1/1",
		`{"dataset_case_id":"11","dimension_id":"7","score":4,"note":"warm"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("code = %d: %s", response.Code, response.Body.String())
	}
	if len(scores.written) != 1 {
		t.Fatalf("repository got %d writes", len(scores.written))
	}
	write := scores.written[0]
	if write.RunID != "1" || write.NormalizedScore != 75 || write.Note != "warm" || write.TargetMet == nil || !*write.TargetMet {
		t.Errorf("write = %+v", write)
	}
	var body HumanScoreRecorded
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.HeadlineScore == nil || *body.HeadlineScore != 75 {
		t.Errorf("the response does not carry the recomputed headline: %+v", body)
	}
}

func TestHumanScoreCreateRefusesWhatTheSnapshotRefuses(t *testing.T) {
	t.Parallel()

	repo, _ := humanRun()
	scores := &fakeHumanScores{}
	router := humanRouter(repo, scores)

	for name, body := range map[string]string{
		"out of scale":  `{"dataset_case_id":"11","dimension_id":"7","score":9}`,
		"unknown field": `{"dataset_case_id":"11","dimension_id":"7","score":3,"rating":1}`,
		"wrong case":    `{"dataset_case_id":"77","dimension_id":"7","score":3}`,
	} {
		if response := call(t, router, http.MethodPost, "/eval_human_scores/prompt_lib/1/1", body); response.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400: %s", name, response.Code, response.Body.String())
		}
	}
	if response := call(t, router, http.MethodPost, "/eval_human_scores/prompt_lib/1/404",
		`{"dataset_case_id":"11","dimension_id":"7","score":3}`); response.Code == http.StatusCreated {
		t.Errorf("a score was recorded against a run that does not exist")
	}
	if len(scores.written) != 0 {
		t.Errorf("%d refused scores reached the repository", len(scores.written))
	}
}

func TestHumanScoreListAnswersRowsEnvelope(t *testing.T) {
	t.Parallel()

	repo, _ := humanRun()
	scores := &fakeHumanScores{stored: []HumanScore{{ID: "1", Note: "first"}, {ID: "2", Note: "second"}}}
	response := call(t, humanRouter(repo, scores), http.MethodGet, "/eval_human_scores/prompt_lib/1/1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("code = %d", response.Code)
	}
	var body struct {
		Rows  []HumanScore `json:"rows"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Total != 2 || len(body.Rows) != 2 {
		t.Fatalf("body = %s (%v)", response.Body.String(), err)
	}
}

func TestHeadlineFromScores(t *testing.T) {
	t.Parallel()

	if HeadlineFromScores(nil) != nil {
		t.Error("no scores must give no headline")
	}
	if got := HeadlineFromScores([]float64{75, 100}); got == nil || *got != 87.5 {
		t.Errorf("headline = %v, want 87.5", got)
	}
}
