package api

// The two run-level analytics routes (legacy issues 6667, 6816, 6817), in the
// router NewRouter really builds. The handler tests mount the handlers on a
// bare chi router, so they cannot see the gates. These tests fail when a gate
// is dropped, reordered out of the chain, or the route moves outside the
// analytics block.
//
//	GET /elitea_core/analytics_execution/prompt_lib/{projectID}/{executionID}
//	    analytics view
//	GET /elitea_core/eval_run_analytics/prompt_lib/{projectID}/{runID}
//	    analytics view AND evaluation run read

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// runAnalyticsRepo answers the two run reads for project 7 and records what
// reached it. Every other read panics through the embedded nil interface, so
// a request this test did not intend to send fails loudly.
type runAnalyticsRepo struct {
	v2analytics.Repository
	calls []string
}

func (r *runAnalyticsRepo) GetExecutionAnalytics(_ context.Context, projectID, executionID string) (domain.ExecutionAnalytics, error) {
	r.calls = append(r.calls, "execution:"+projectID+":"+executionID)
	if executionID != "exec-7" {
		// What the repository answers for an id the project does not hold,
		// another project's id included (executionHeader's project guard).
		return domain.ExecutionAnalytics{}, fmt.Errorf("%w: execution %q", domain.ErrNotFound, executionID)
	}
	return domain.ExecutionAnalytics{ExecutionID: executionID, Available: true, Totals: &domain.UsageFigures{LLMCalls: 1}}, nil
}

func (r *runAnalyticsRepo) GetEvaluationRunAnalytics(_ context.Context, projectID, runID string) (domain.EvaluationRunAnalytics, error) {
	r.calls = append(r.calls, "evaluation:"+projectID+":"+runID)
	return domain.EvaluationRunAnalytics{RunID: runID, Available: true, Totals: &domain.UsageFigures{LLMCalls: 1}}, nil
}

func newRunAnalyticsRouter(repo *runAnalyticsRepo, granted ...string) chi.Router {
	return NewRouter(RouterConfig{
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		AnalyticsRepo:             repo,
		ProjectAccessQuerier:      &memberOfProject{project: "7"},
		ProjectPermissionResolver: fakePermissionResolver{granted: granted, forProject: "7"},
	})
}

const (
	executionAnalyticsPath = "/api/v2/elitea_core/analytics_execution/prompt_lib/%s/%s"
	evaluationRunPath      = "/api/v2/elitea_core/eval_run_analytics/prompt_lib/%s/%s"
)

func serveRunAnalytics(router chi.Router, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, path, nil)))
	return recorder
}

func TestRunAnalyticsRoutesRequireTheirPermissions(t *testing.T) {
	both := []string{v2analytics.ViewPermission, v2evaluation.PermissionRunRead}
	cases := []struct {
		name    string
		path    string
		granted []string
		want    int
	}{
		{"execution with analytics view", fmt.Sprintf(executionAnalyticsPath, "7", "exec-7"), []string{v2analytics.ViewPermission}, http.StatusOK},
		{"execution without analytics view", fmt.Sprintf(executionAnalyticsPath, "7", "exec-7"), []string{v2evaluation.PermissionRunRead}, http.StatusForbidden},
		{"execution in another project", fmt.Sprintf(executionAnalyticsPath, "8", "exec-7"), both, http.StatusForbidden},
		{"evaluation run with both permissions", fmt.Sprintf(evaluationRunPath, "7", "12"), both, http.StatusOK},
		{"evaluation run without run read", fmt.Sprintf(evaluationRunPath, "7", "12"), []string{v2analytics.ViewPermission}, http.StatusForbidden},
		{"evaluation run without analytics view", fmt.Sprintf(evaluationRunPath, "7", "12"), []string{v2evaluation.PermissionRunRead}, http.StatusForbidden},
		{"evaluation run in another project", fmt.Sprintf(evaluationRunPath, "8", "12"), both, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &runAnalyticsRepo{}
			recorder := serveRunAnalytics(newRunAnalyticsRouter(repo, tc.granted...), tc.path)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.want == http.StatusForbidden && len(repo.calls) != 0 {
				t.Fatalf("a refused request reached the repository: %v", repo.calls)
			}
			if tc.want == http.StatusOK && len(repo.calls) != 1 {
				t.Fatalf("an admitted request reached the repository %d times: %v", len(repo.calls), repo.calls)
			}
		})
	}
}

// Project 7's analytics viewer names an execution id that belongs to another
// project. The repository's project guard finds nothing, and the route
// answers 404 run_not_found, not data and not a different status that would
// confirm the id exists elsewhere.
func TestExecutionAnalyticsRouteAnswersNotFoundForAnotherProjectsID(t *testing.T) {
	repo := &runAnalyticsRepo{}
	recorder := serveRunAnalytics(newRunAnalyticsRouter(repo, v2analytics.ViewPermission),
		fmt.Sprintf(executionAnalyticsPath, "7", "exec-of-project-8"))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["code"] != "run_not_found" {
		t.Fatalf("body = %s, want code run_not_found", recorder.Body.String())
	}
	if len(repo.calls) != 1 || repo.calls[0] != "execution:7:exec-of-project-8" {
		t.Fatalf("repository calls = %v, want the project from the path", repo.calls)
	}
}
