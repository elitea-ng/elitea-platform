package admin

// The admin Tasks page's handler: the filters it forwards, the shape it
// serves, and which cancel each kind reaches.
//
// The store is a double, so these tests judge the HANDLER. The SQL that builds
// the union and the UPDATE that stops a job are proved against a real database
// in internal/infra/db/repos/admin_background_jobs_postgres_integration_test.go
// — a fake that answered the wrong rows would still let this file pass, and a
// real database that answered the wrong rows would still let a fake-only suite
// pass. Both halves are needed and neither substitutes for the other.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type fakeBackgroundJobs struct {
	page       repos.BackgroundJobPage
	listErr    error
	seen       repos.BackgroundJobFilter
	cancelled  []string
	cancelErr  error
	cancelSeen bool
}

func (f *fakeBackgroundJobs) ListBackgroundJobs(
	_ context.Context, filter repos.BackgroundJobFilter,
) (repos.BackgroundJobPage, error) {
	f.seen = filter
	return f.page, f.listErr
}

func (f *fakeBackgroundJobs) CancelRuntimeJob(_ context.Context, executionID string) error {
	f.cancelSeen = true
	f.cancelled = append(f.cancelled, executionID)
	return f.cancelErr
}

type fakeEvalCancel struct {
	projectID string
	runID     string
	err       error
	called    bool
}

func (f *fakeEvalCancel) CancelRun(_ context.Context, projectID, runID string) error {
	f.called = true
	f.projectID = projectID
	f.runID = runID
	return f.err
}

func newBackgroundJobsRouter(options ...Option) chi.Router {
	handler := NewHandler(nil, options...)
	router := chi.NewRouter()
	router.Get("/admin/background_jobs/administration", handler.BackgroundJobs)
	router.Post("/admin/background_jobs/administration/{kind}/{jobID}:cancel", handler.CancelBackgroundJob)
	return router
}

func serve(t *testing.T, router chi.Router, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func decodeJobsBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, recorder.Body.String())
	}
	return body
}

/* ── the read ─────────────────────────────────────────────────────────── */

// An unconfigured deployment says so. It must never answer an empty list: that
// is the exact correction arbiterTaskNodeUnavailable records, and repeating it
// here would reintroduce it under a new route.
func TestBackgroundJobs_NoStoreAnswers503(t *testing.T) {
	recorder := serve(t, newBackgroundJobsRouter(), http.MethodGet, "/admin/background_jobs/administration")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if _, ok := decodeJobsBody(t, recorder)["error"]; !ok {
		t.Error("the 503 carried no reason")
	}
}

func TestBackgroundJobs_ServesTheLegacyRowKeys(t *testing.T) {
	started := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	projectID := int64(7)
	store := &fakeBackgroundJobs{page: repos.BackgroundJobPage{
		Total: 1,
		Rows: []repos.BackgroundJobRow{{
			TaskID: "exec-1", Kind: repos.BackgroundJobKindIndex, Name: "index.ingest.v1",
			Status: "RUNNING", StartedAt: &started, ProjectID: &projectID,
			Principal: "42", Cancellable: true,
		}},
	}}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)),
		http.MethodGet, "/admin/background_jobs/administration")
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeJobsBody(t, recorder)
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("rows = %v", body["rows"])
	}
	row := rows[0].(map[string]any)
	// The four legacy keys, by name. A struct with different tags would still
	// marshal successfully and break every existing operator script.
	for key, want := range map[string]any{
		"task_id": "exec-1", "status": "RUNNING", "user": "42",
		"kind": "index", "name": "index.ingest.v1", "cancellable": true,
	} {
		if row[key] != want {
			t.Errorf("row[%q] = %v, want %v", key, row[key], want)
		}
	}
	if row["started_at"] == nil {
		t.Error("started_at is null for a job that has started")
	}
	// finished_at is null, not absent: the column is nullable and the page
	// renders a dash for it.
	if value, present := row["finished_at"]; !present || value != nil {
		t.Errorf("finished_at = %v (present=%v), want an explicit null", value, present)
	}
	if body["truncated"] != false {
		t.Errorf("truncated = %v, want false", body["truncated"])
	}
}

func TestBackgroundJobs_ForwardsEveryFilter(t *testing.T) {
	store := &fakeBackgroundJobs{}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodGet,
		"/admin/background_jobs/administration?kind=agent&status=RUNNING&project_id=9&limit=25&offset=50")
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if store.seen.Kind != "agent" || store.seen.Status != "RUNNING" {
		t.Errorf("filter = %+v, want kind=agent status=RUNNING", store.seen)
	}
	if store.seen.ProjectID == nil || *store.seen.ProjectID != 9 {
		t.Errorf("project filter = %v, want 9", store.seen.ProjectID)
	}
	if store.seen.Limit != 25 || store.seen.Offset != 50 {
		t.Errorf("page = %d/%d, want 25/50", store.seen.Limit, store.seen.Offset)
	}
}

func TestBackgroundJobs_RejectsANonNumericProjectFilter(t *testing.T) {
	store := &fakeBackgroundJobs{}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodGet,
		"/admin/background_jobs/administration?project_id=all")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// A failed read is a 500 with no cause in it. The store's error carries the
// database host and the SQLSTATE.
func TestBackgroundJobs_FailedReadDoesNotLeakTheCause(t *testing.T) {
	store := &fakeBackgroundJobs{listErr: errors.New("dial tcp 10.0.0.1:5432: refused")}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodGet,
		"/admin/background_jobs/administration")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "10.0.0.1") {
		t.Errorf("the 500 carried the cause: %s", body)
	}
}

/* ── the cancel, per kind ─────────────────────────────────────────────── */

func TestCancelBackgroundJob_RuntimeKindsReachTheRuntimeCancel(t *testing.T) {
	for _, kind := range []string{"index", "agent", "toolkit", "execution"} {
		t.Run(kind, func(t *testing.T) {
			store := &fakeBackgroundJobs{}
			recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodPost,
				"/admin/background_jobs/administration/"+kind+"/exec-1:cancel")
			if recorder.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if len(store.cancelled) != 1 || store.cancelled[0] != "exec-1" {
				t.Errorf("cancelled %v, want [exec-1]", store.cancelled)
			}
		})
	}
}

// A settled job answers 409, NOT 404: the row is on the operator's screen, and
// telling them it does not exist sends them looking for it.
func TestCancelBackgroundJob_SettledRuntimeJobIs409(t *testing.T) {
	store := &fakeBackgroundJobs{cancelErr: repos.ErrBackgroundJobNotCancellable}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodPost,
		"/admin/background_jobs/administration/agent/exec-1:cancel")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCancelBackgroundJob_EvalReachesTheEvaluationCancel(t *testing.T) {
	store := &fakeBackgroundJobs{}
	eval := &fakeEvalCancel{}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store), WithEvalRunCancel(eval)),
		http.MethodPost, "/admin/background_jobs/administration/eval/31:cancel?project_id=7")
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !eval.called || eval.projectID != "7" || eval.runID != "31" {
		t.Fatalf("eval cancel got project=%q run=%q (called=%v), want 7/31", eval.projectID, eval.runID, eval.called)
	}
	// And the runtime cancel was NOT reached: one route, two stores, and the
	// wrong one would stop nothing while answering 200.
	if store.cancelSeen {
		t.Error("an eval cancel reached the runtime store")
	}
}

func TestCancelBackgroundJob_EvalNeedsItsProject(t *testing.T) {
	eval := &fakeEvalCancel{}
	recorder := serve(t, newBackgroundJobsRouter(WithEvalRunCancel(eval)), http.MethodPost,
		"/admin/background_jobs/administration/eval/31:cancel")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if eval.called {
		t.Error("a malformed id still reached the store")
	}
}

// The typed conflict's message reaches the operator; an untyped failure does
// not.
func TestCancelBackgroundJob_EvalRendersTheTypedConflictOnly(t *testing.T) {
	typed := &fakeEvalCancel{err: apierr.Conflict("this run is already finished and cannot be cancelled")}
	recorder := serve(t, newBackgroundJobsRouter(WithEvalRunCancel(typed)), http.MethodPost,
		"/admin/background_jobs/administration/eval/31:cancel?project_id=7")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "already finished") {
		t.Errorf("the typed message was lost: %s", recorder.Body.String())
	}

	untyped := &fakeEvalCancel{err: errors.New("dial tcp 10.0.0.1:5432: refused")}
	recorder = serve(t, newBackgroundJobsRouter(WithEvalRunCancel(untyped)), http.MethodPost,
		"/admin/background_jobs/administration/eval/31:cancel?project_id=7")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "10.0.0.1") {
		t.Errorf("the 500 carried the cause: %s", recorder.Body.String())
	}
}

// A scheduled occurrence has no cancel, and the refusal names the control that
// does change something.
func TestCancelBackgroundJob_ScheduleIs409WithTheRealControl(t *testing.T) {
	store := &fakeBackgroundJobs{}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodPost,
		"/admin/background_jobs/administration/schedule/abc:cancel")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Schedules") {
		t.Errorf("the refusal did not name the schedule control: %s", recorder.Body.String())
	}
	if store.cancelSeen {
		t.Error("a schedule cancel reached the runtime store")
	}
}

func TestCancelBackgroundJob_UnknownKindIs400(t *testing.T) {
	store := &fakeBackgroundJobs{}
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(store)), http.MethodPost,
		"/admin/background_jobs/administration/nonsense/abc:cancel")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCancelBackgroundJob_EvalWithNoStoreIs503(t *testing.T) {
	recorder := serve(t, newBackgroundJobsRouter(WithBackgroundJobs(&fakeBackgroundJobs{})),
		http.MethodPost, "/admin/background_jobs/administration/eval/31:cancel?project_id=7")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
