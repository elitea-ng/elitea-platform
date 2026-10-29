package evaluation

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The frozen agent holds the agent's instructions. The run reads do not
// answer it: the run read permission does not grant the instructions.
func TestRunReadsDoNotAnswerTheFrozenAgent(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Snapshot.Agent = &SnapshotAgent{Instructions: "secret instructions", ModelName: "m"}
	repo.seedRun(run)
	router := runRouter(t, repo, nil, nil)

	for _, path := range []string{"/eval_run/prompt_lib/1/1", "/eval_results/prompt_lib/1/1"} {
		response := call(t, router, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret instructions") {
			t.Fatalf("%s answers the frozen agent: %s", path, response.Body.String())
		}
	}
}

// A release gate run freezes the agent at start. The orchestrator then
// builds the agent turn from the snapshot and not from the version row,
// which may have been edited since.
func TestOrchestratorUsesTheFrozenAgentOverTheVersionRow(t *testing.T) {
	t.Parallel()

	repo, run := twoCaseRun()
	run.Snapshot.Agent = &SnapshotAgent{Instructions: "the released instructions", ModelName: "released-model"}
	repo.seedRun(run)
	completer := &stubCompleter{answer: "an answer"}

	NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if status := repo.run("1").Status; status != RunStatusFinished {
		t.Fatalf("status = %q, want finished", status)
	}
	if len(completer.requests) == 0 {
		t.Fatal("the agent was not called")
	}
	request := completer.requests[0]
	if request.Messages[0].Content != "the released instructions" {
		t.Errorf("system message = %q, want the frozen instructions", request.Messages[0].Content)
	}
	if request.Model != "released-model" {
		t.Errorf("model = %q, want the frozen model", request.Model)
	}
}
