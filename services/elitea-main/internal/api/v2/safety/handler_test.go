package safety

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	safetyapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fakeService struct {
	actors   []safetyapp.Actor
	requests []safetyapp.CreateRequest
	err      error
}

func (f *fakeService) Create(_ context.Context, actor safetyapp.Actor, request safetyapp.CreateRequest) (safetyapp.Suspension, error) {
	f.actors, f.requests = append(f.actors, actor), append(f.requests, request)
	if f.err != nil {
		return safetyapp.Suspension{}, f.err
	}
	return safetyapp.Suspension{ID: 5, Target: request.Target, Reason: request.Reason, ActorID: actor.UserID,
		Source: safetyapp.SourceManual, ResumeRequires: safetyapp.ResumeAnyAdmin, CreatedAt: time.Unix(0, 0)}, nil
}

func (f *fakeService) Resume(_ context.Context, actor safetyapp.Actor, request safetyapp.ResumeRequest) (safetyapp.Suspension, error) {
	f.actors = append(f.actors, actor)
	if f.err != nil {
		return safetyapp.Suspension{}, f.err
	}
	now, by := time.Unix(10, 0), actor.UserID
	return safetyapp.Suspension{ID: request.ID, Target: safetyapp.Target{Scope: safetyapp.ScopePlatform},
		ResumedAt: &now, ResumedBy: &by}, nil
}

func (f *fakeService) List(_ context.Context, actor safetyapp.Actor, _ safetyapp.ListFilter) ([]safetyapp.Suspension, error) {
	f.actors = append(f.actors, actor)
	return nil, f.err
}

func router(t *testing.T, service Service) http.Handler {
	t.Helper()
	handler, err := NewHandler(service)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Get("/admin"+AdminPath, handler.AdminList)
	r.Post("/admin"+AdminPath, handler.AdminCreate)
	r.Post("/admin"+AdminResumePath, handler.AdminResume)
	r.Post("/core"+ProjectPath, handler.ProjectCreate)
	r.Post("/core"+ProjectResumePath, handler.ProjectResume)
	return r
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, audit.Annotation) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx, slot := audit.ContextWithAnnotationSlot(request.Context())
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "42", UserID: "42"})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request.WithContext(ctx))
	annotation, _ := slot.Read()
	return recorder, annotation
}

func TestAdminCreateAnnotatesTheAuditRecordAndPassesTheConfirmation(t *testing.T) {
	t.Parallel()
	service := &fakeService{}
	recorder, annotation := do(t, router(t, service), http.MethodPost, "/admin/execution_suspensions",
		`{"scope":"platform","reason":"incident","confirm":"STOP","resume_requires":"two_person"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body)
	}
	if !service.actors[0].PlatformAdmin || service.actors[0].UserID != 42 ||
		service.requests[0].Confirmation != "STOP" || service.requests[0].ResumeRequires != safetyapp.ResumeTwoPerson {
		t.Fatalf("actor = %+v, request = %+v", service.actors[0], service.requests[0])
	}
	if annotation.Action != AuditActionCreated || annotation.EntityType != "execution_suspension" ||
		annotation.EntityID == nil || *annotation.EntityID != 5 || annotation.EntityName != "platform" {
		t.Fatalf("annotation = %+v", annotation)
	}
	var body Suspension
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !body.Active || body.Scope != "platform" {
		t.Fatalf("body = %s", recorder.Body)
	}
}

func TestProjectSurfaceIsBoundToThePathProject(t *testing.T) {
	t.Parallel()
	service := &fakeService{}
	h := router(t, service)
	recorder, _ := do(t, h, http.MethodPost, "/core/execution_suspensions/prompt_lib/7",
		`{"scope":"agent","project_id":8,"application_id":3,"reason":"x"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("another project's id: %d", recorder.Code)
	}
	recorder, annotation := do(t, h, http.MethodPost, "/core/execution_suspensions/prompt_lib/7",
		`{"scope":"agent","application_id":3,"reason":"x"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body)
	}
	actor, request := service.actors[0], service.requests[0]
	if actor.PlatformAdmin || actor.ProjectID != 7 || request.Target.ProjectID != 7 || request.Target.ApplicationID != 3 {
		t.Fatalf("actor = %+v, target = %+v", actor, request.Target)
	}
	if annotation.ProjectID == nil || *annotation.ProjectID != 7 || annotation.EntityName != "agent:3" {
		t.Fatalf("annotation = %+v", annotation)
	}
	recorder, _ = do(t, h, http.MethodPost, "/core/execution_suspensions/prompt_lib/7",
		`{"scope":"agent","reason":"x"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an agent stop without application_id: %d", recorder.Code)
	}
}

func TestErrorsMapToTypedAnswers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{safetyapp.ErrConfirmationRequired, http.StatusBadRequest, "confirmation_required"},
		{safetyapp.ErrResumeForbidden, http.StatusForbidden, "resume_forbidden"},
		{&safetyapp.AlreadySuspendedError{Existing: safetyapp.Suspension{ID: 3, Target: safetyapp.Target{Scope: safetyapp.ScopePlatform}}}, http.StatusConflict, "already_suspended"},
		{safetyapp.ErrNotFound, http.StatusNotFound, ""},
		{safetyapp.ErrNotActive, http.StatusConflict, ""},
		{safetyapp.ErrScopeForbidden, http.StatusForbidden, ""},
	}
	for _, tc := range cases {
		recorder, annotation := do(t, router(t, &fakeService{err: tc.err}), http.MethodPost,
			"/admin/execution_suspensions/9:resume", `{"reason":"fixed"}`)
		if recorder.Code != tc.status {
			t.Errorf("%v: status = %d, want %d", tc.err, recorder.Code, tc.status)
		}
		if tc.code != "" && !strings.Contains(recorder.Body.String(), `"code":"`+tc.code+`"`) {
			t.Errorf("%v: body = %s", tc.err, recorder.Body)
		}
		if annotation.Action != "" {
			t.Errorf("%v: a refused resume must not be annotated as a resume", tc.err)
		}
	}
	recorder, annotation := do(t, router(t, &fakeService{}), http.MethodPost, "/admin/execution_suspensions/9:resume", "")
	if recorder.Code != http.StatusOK || annotation.Action != AuditActionResumed {
		t.Fatalf("resume with no body: %d %+v", recorder.Code, annotation)
	}
}

// The router mounts the routes unconditionally; a nil handler answers 503
// rather than panicking or admitting anything.
func TestNilHandlerAnswersUnavailable(t *testing.T) {
	t.Parallel()
	var handler *Handler
	r := chi.NewRouter()
	r.Post("/admin"+AdminPath, handler.AdminCreate)
	r.Get("/core"+ProjectPath, handler.ProjectList)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/admin/execution_suspensions", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, "/core/execution_suspensions/prompt_lib/7", nil),
	} {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d", request.Method, request.URL.Path, recorder.Code)
		}
	}
}

// Project and application ids are PostgreSQL integers: an id outside
// 1..MaxInt32 is a 400 and never reaches the service, where it would have
// been narrowed into another project's id.
func TestCreateRefusesIDsOutsideTheIntegerRange(t *testing.T) {
	t.Parallel()
	service := &fakeService{}
	h := router(t, service)
	for _, body := range []string{
		`{"scope":"project","project_id":4294967297,"reason":"x"}`,
		`{"scope":"project","project_id":2147483648,"reason":"x"}`,
		`{"scope":"project","project_id":0,"reason":"x"}`,
		`{"scope":"agent","project_id":7,"application_id":2147483648,"reason":"x"}`,
		`{"scope":"agent","project_id":7,"application_id":-1,"reason":"x"}`,
	} {
		recorder, _ := do(t, h, http.MethodPost, "/admin/execution_suspensions", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s", body, recorder.Code, recorder.Body)
		}
	}
	recorder, _ := do(t, h, http.MethodPost, "/core/execution_suspensions/prompt_lib/7",
		`{"scope":"agent","application_id":9223372036854775807,"reason":"x"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("project surface: status = %d", recorder.Code)
	}
	if len(service.requests) != 0 {
		t.Fatalf("an out-of-range id reached the service: %+v", service.requests)
	}
	recorder, _ = do(t, h, http.MethodPost, "/admin/execution_suspensions",
		`{"scope":"project","project_id":2147483647,"reason":"x"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("the largest integer id: status = %d %s", recorder.Code, recorder.Body)
	}
}

// approvingService answers a resume as the first approval of a system
// two-person stop: the suspension stays active (ADR-0041 decision 2).
type approvingService struct {
	fakeService
	err error
}

func (a *approvingService) Resume(_ context.Context, _ safetyapp.Actor, request safetyapp.ResumeRequest) (safetyapp.Suspension, error) {
	by, at := int64(42), time.Unix(20, 0)
	return safetyapp.Suspension{
		ID: request.ID, Target: safetyapp.Target{Scope: safetyapp.ScopeAgent, ProjectID: 7, ApplicationID: 31},
		Source: safetyapp.SourceBreaker, ResumeRequires: safetyapp.ResumeTwoPerson, CreatedAt: time.Unix(0, 0),
		Detail: map[string]any{"rule": "error_rate", "observed": 55.0, "threshold": 50.0, "unit": "percent",
			"tier": "critical", "window_runs": 20.0, "failed_runs": 11.0},
		ResumeApprovedBy: &by, ResumeApprovedAt: &at,
	}, a.err
}

func TestResumeOfASystemTwoPersonStop(t *testing.T) {
	t.Parallel()
	recorder, annotation := do(t, router(t, &approvingService{err: safetyapp.ErrResumeApprovalRecorded}),
		http.MethodPost, "/admin/execution_suspensions/9:resume", "")
	if recorder.Code != http.StatusAccepted || annotation.Action != AuditActionResumeApproved {
		t.Fatalf("first approval: %d %+v %s", recorder.Code, annotation, recorder.Body)
	}
	var body Suspension
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || !body.Active || body.Breaker == nil ||
		body.Breaker.Rule != "error_rate" || body.Breaker.Observed != 55 || body.Breaker.FailedRuns == nil ||
		*body.Breaker.FailedRuns != 11 || body.ResumeApprovedBy == nil || *body.ResumeApprovedBy != 42 {
		t.Fatalf("body = %s", recorder.Body)
	}
	recorder, annotation = do(t, router(t, &approvingService{err: safetyapp.ErrSecondPersonRequired}),
		http.MethodPost, "/admin/execution_suspensions/9:resume", "")
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), `"code":"second_person_required"`) ||
		annotation.Action != "" {
		t.Fatalf("same administrator again: %d %+v %s", recorder.Code, annotation, recorder.Body)
	}
}
