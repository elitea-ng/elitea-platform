package evaluation

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Legacy issue 6700: an excluded case stays in the dataset and out of the
// run. The snapshot is the list the orchestrator executes, so the filter has
// to be visible there and in `progress.total`.
func TestRunStartLeavesExcludedCasesOutOfTheSnapshot(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	repo.cases[1].Excluded = true
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
	if len(created.Snapshot.Cases) != 1 || created.Snapshot.Cases[0].ID != "11" {
		t.Fatalf("snapshot cases = %+v, want case 11 alone", created.Snapshot.Cases)
	}
	if created.Progress.Total != 1 {
		t.Errorf("progress.total = %d, want the ACTIVE case count 1", created.Progress.Total)
	}
	if len(queue.enqueued) != 1 {
		t.Errorf("enqueued %d runs, want 1", len(queue.enqueued))
	}
}

// Every case excluded is a 422 with a reason that names the fix, and no run
// row is enqueued. A 400 "no cases" would send the author to add a case that
// is already there.
func TestRunStartRefusesWhenEveryCaseIsExcluded(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	for index := range repo.cases {
		repo.cases[index].Excluded = true
	}
	queue := &recordingEnqueuer{}
	router := runRouter(t, repo, dimensionLibrary{rows: []Dimension{aiLibraryDimension()}}, queue)

	response := call(t, router, http.MethodPost, "/eval_runs/prompt_lib/1",
		`{"dataset_id":"5","application_version_id":9,"dimension_ids":["3"]}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("start: %d %s, want 422", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "excluded") {
		t.Errorf("body %s does not name the reason", response.Body.String())
	}
	if len(queue.enqueued) != 0 {
		t.Errorf("enqueued %+v, want nothing", queue.enqueued)
	}
}

// The orchestrator executes the SNAPSHOT's cases. A case included again after
// the start must not join a run that was counted without it.
func TestOrchestratorExecutesOnlyTheSnapshotCases(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Snapshot.Cases = []SnapshotCase{{ID: "12"}}
	run.Progress.Total = 1
	repo.seedRun(run)
	completer := &stubCompleter{answer: "an answer"}

	NewOrchestrator(repo, &scriptedJudge{answers: []JudgeVerdict{{Score: 5}}}, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if got := repo.run("1"); got.Status != RunStatusFinished || got.Progress.Done != 1 {
		t.Fatalf("run = %q done %d (error %q), want finished with 1 case", got.Status, got.Progress.Done, got.Error)
	}
	if len(completer.requests) != 1 {
		t.Fatalf("the agent was called %d times, want once", len(completer.requests))
	}
	results := repo.storedResults()
	if len(results) != 1 || results[0].DatasetCaseID != "12" {
		t.Fatalf("results = %+v, want case 12 alone", results)
	}
}

func TestSnapshotCasesWithoutFrozenCasesSkipsExcluded(t *testing.T) {
	t.Parallel()

	cases := []DatasetCase{{ID: "1"}, {ID: "2", Excluded: true}, {ID: "3"}}
	got := snapshotCases(cases, RunSnapshot{})
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("got %+v, want cases 1 and 3", got)
	}
}
