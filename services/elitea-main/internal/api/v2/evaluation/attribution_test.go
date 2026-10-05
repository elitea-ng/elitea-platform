package evaluation

import (
	"context"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

func TestAttributionIDsNameTheRunTheCaseAndTheRole(t *testing.T) {
	t.Parallel()

	if got := AgentAttributionID("12", "34"); got != "eval:12:case:34" {
		t.Errorf("agent id = %q, want eval:12:case:34", got)
	}
	if got := JudgeAttributionID("12", "34"); got != "eval:12:judge:34" {
		t.Errorf("judge id = %q, want eval:12:judge:34", got)
	}
	for _, id := range []string{AgentAttributionID("12", "34"), JudgeAttributionID("12", "34")} {
		if !strings.HasPrefix(id, RunAttributionPrefix("12")) {
			t.Errorf("%q does not start with the run prefix", id)
		}
		if !predict.ValidAttributionID(id) {
			t.Errorf("%q is outside the edge's execution id charset; the completer would drop it", id)
		}
	}
	// Run 1's prefix must not also select run 12's calls.
	if strings.HasPrefix(AgentAttributionID("12", "1"), RunAttributionPrefix("1")) {
		t.Error("run 1's prefix matches run 12's ids")
	}
}

// Legacy issue 6677: every model call of a run carries the run's attribution,
// and the agent and the judge carry DIFFERENT roles. Before, neither sent an
// execution id, and the judge sent no user either.
func TestOrchestratorAttributesTheAgentTurnAndTheJudgeCall(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	createdBy := 42
	repo.mu.Lock()
	repo.runs["1"].CreatedBy = &createdBy
	repo.mu.Unlock()

	// ONE completer behind both the agent turn and the real AI judge, so the
	// requests are captured in the order the run makes them.
	completer := &stubCompleter{answer: `{"score": 4, "reason": "fine"}`}
	NewOrchestrator(repo, NewAIJudge(completer), completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if run := repo.run("1"); run.Status != RunStatusFinished {
		t.Fatalf("status = %q (error %q), want finished", run.Status, run.Error)
	}

	want := []string{
		"eval:1:case:11", "eval:1:judge:11",
		"eval:1:case:12", "eval:1:judge:12",
	}
	if len(completer.requests) != len(want) {
		t.Fatalf("made %d model calls, want %d (one agent turn and one judge call per case)",
			len(completer.requests), len(want))
	}
	for index, request := range completer.requests {
		if request.AttributionID != want[index] {
			t.Errorf("call %d: attribution = %q, want %q", index, request.AttributionID, want[index])
		}
		if request.UserID != "42" {
			t.Errorf("call %d (%s): user = %q, want the run's author 42", index, want[index], request.UserID)
		}
		if request.ProjectID != "1" {
			t.Errorf("call %d: project = %q, want 1", index, request.ProjectID)
		}
	}
}
