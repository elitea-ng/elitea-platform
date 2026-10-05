package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// runRepo answers the two run reads with fixed values and keeps the ids it
// was asked for.
type runRepo struct {
	stubRepo
	execution domain.ExecutionAnalytics
	run       domain.EvaluationRunAnalytics
	err       error
	asked     []string
}

func (r *runRepo) GetExecutionAnalytics(_ context.Context, projectID, executionID string) (domain.ExecutionAnalytics, error) {
	r.asked = append(r.asked, projectID, executionID)
	return r.execution, r.err
}

func (r *runRepo) GetEvaluationRunAnalytics(_ context.Context, projectID, runID string) (domain.EvaluationRunAnalytics, error) {
	r.asked = append(r.asked, projectID, runID)
	return r.run, r.err
}

func serveRun(t *testing.T, repo Repository, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	handler := NewHandler(repo)
	router := chi.NewRouter()
	router.Get("/analytics_execution/prompt_lib/{projectID}/{executionID}", handler.Execution)
	router.Get("/eval_run_analytics/prompt_lib/{projectID}/{runID}", handler.EvaluationRun)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON (%q): %v", recorder.Body.String(), err)
	}
	return recorder, body
}

func TestExecutionRoutePassesThePathAndPublishesTheFigures(t *testing.T) {
	t.Parallel()

	repo := &runRepo{execution: domain.ExecutionAnalytics{
		ExecutionID: "exec-1", TriggerOrigin: "schedule", Available: true,
		Totals: &domain.UsageFigures{LLMCalls: 3, TotalTokens: 30},
	}}
	recorder, body := serveRun(t, repo, "/analytics_execution/prompt_lib/7/exec-1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if len(repo.asked) != 2 || repo.asked[0] != "7" || repo.asked[1] != "exec-1" {
		t.Fatalf("repository asked for %v, want [7 exec-1]", repo.asked)
	}
	if body["trigger_origin"] != "schedule" || body["available"] != true {
		t.Fatalf("body = %v", body)
	}
	totals, ok := body["totals"].(map[string]any)
	if !ok || totals["llm_calls"] != float64(3) {
		t.Fatalf("totals = %v, want llm_calls 3", body["totals"])
	}
}

// Unavailable is not zero: the body carries the reason and NO figure keys.
func TestExecutionRouteOmitsFiguresWhenUnavailable(t *testing.T) {
	t.Parallel()

	repo := &runRepo{execution: domain.ExecutionAnalytics{
		ExecutionID: "exec-old", UnavailableReason: domain.UnavailableBeforeAttribution,
	}}
	recorder, body := serveRun(t, repo, "/analytics_execution/prompt_lib/7/exec-old")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if body["available"] != false || body["unavailable_reason"] != "before_attribution" {
		t.Fatalf("body = %v", body)
	}
	for _, key := range []string{"totals", "by_model", "by_user", "tools"} {
		if _, present := body[key]; present {
			t.Errorf("an unavailable run publishes %q", key)
		}
	}
}

func TestRunRoutesMapRepositoryFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", errors.Join(domain.ErrNotFound), http.StatusNotFound, "run_not_found"},
		{"bad id", domain.BadIDError("execution", "a b"), http.StatusBadRequest, "bad_run_id"},
		{"bad project", domain.BadProjectError("x"), http.StatusBadRequest, "bad_project_id"},
		{"no source", domain.NoSourceError("execution analytics", "absent"), http.StatusNotImplemented, "no_data_source"},
		{"query failed", errors.New("connection reset"), http.StatusInternalServerError, "query_failed"},
	} {
		for _, target := range []string{
			"/analytics_execution/prompt_lib/7/exec-1",
			"/eval_run_analytics/prompt_lib/7/3",
		} {
			recorder, body := serveRun(t, &runRepo{err: test.err}, target)
			if recorder.Code != test.status || body["code"] != test.code {
				t.Errorf("%s %s: status %d code %v, want %d %s",
					test.name, target, recorder.Code, body["code"], test.status, test.code)
			}
		}
	}
}

func TestEvaluationRunRouteKeepsTheRolesApart(t *testing.T) {
	t.Parallel()

	repo := &runRepo{run: domain.EvaluationRunAnalytics{
		RunID: "3", Available: true,
		Totals: &domain.UsageFigures{LLMCalls: 3},
		Agent:  &domain.UsageFigures{LLMCalls: 2},
		Judge:  &domain.UsageFigures{LLMCalls: 1},
		ByCase: []domain.EvaluationCaseFigures{{CaseID: "21",
			Agent: domain.UsageFigures{LLMCalls: 2}, Judge: domain.UsageFigures{LLMCalls: 1}}},
	}}
	recorder, body := serveRun(t, repo, "/eval_run_analytics/prompt_lib/7/3")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if repo.asked[1] != "3" {
		t.Fatalf("asked for run %q, want 3", repo.asked[1])
	}
	agent := body["agent"].(map[string]any)
	judge := body["judge"].(map[string]any)
	if agent["llm_calls"] != float64(2) || judge["llm_calls"] != float64(1) {
		t.Fatalf("agent = %v, judge = %v", agent, judge)
	}
	cases := body["by_case"].([]any)
	if len(cases) != 1 {
		t.Fatalf("by_case = %v", cases)
	}
}

// The Overview publishes the automated bucket when the repository measured
// it, and omits the key when it could not.
func TestUsagePublishesTheAutomatedBucketOnlyWhenMeasured(t *testing.T) {
	t.Parallel()

	_, measured := do(t, stubRepo{summary: domain.UsageSummary{
		Automated: []domain.AutomatedActivity{{TriggerOrigin: "schedule", Executions: 1,
			UsageFigures: domain.UsageFigures{LLMCalls: 4}}},
	}}, "/")
	rows, ok := measured["automated_activity"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("automated_activity = %v, want one row", measured["automated_activity"])
	}
	row := rows[0].(map[string]any)
	if row["trigger_origin"] != "schedule" || row["llm_calls"] != float64(4) {
		t.Fatalf("row = %v", row)
	}

	_, unmeasured := do(t, stubRepo{}, "/")
	if _, present := unmeasured["automated_activity"]; present {
		t.Fatal("automated_activity is published when the origins could not be read")
	}
}
