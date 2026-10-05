package middleware

// Unit coverage for the audit EMITTER's policy and event shape.
//
// Every case asserts an EVENT — which one, with which fields — never "no error
// was returned". The defect this whole change removes is a table that nothing
// writes, and "the middleware ran without panicking" is exactly the assertion
// that would have passed against that table for the last year.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// captureRecorder keeps every event so a test can assert its columns.
type captureRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *captureRecorder) Record(_ context.Context, event audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *captureRecorder) all() []audit.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]audit.Event(nil), c.events...)
}

// auditTestRouter mounts the middleware over a handler that answers `status`
// and, if `annotate` is non-nil, annotates the request first.
func auditTestRouter(
	recorder audit.Recorder, pattern string, status int, annotate func(*http.Request),
) chi.Router {
	router := chi.NewRouter()
	router.Use(Audit(recorder))
	router.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if annotate != nil {
			annotate(r)
		}
		w.WriteHeader(status)
	}))
	return router
}

func authenticatedRequest(method, target string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	return request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID: "77", UserID: "77", Email: "admin@test.local", AuthType: "token",
	}))
}

/* ── DECISION 2: what is audited ────────────────────────────────────────── */

func TestAuditPolicySelectsAdministrativeMutationsAndRefusalsOnly(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		status  int
		audited bool
	}{
		{"admin mutation", http.MethodPut, "/api/v2/admin/user_suspend/administration/42", 200, true},
		{"admin delete", http.MethodDelete, "/api/v2/admin/users/administration/1", 200, true},
		{"secret write", http.MethodPost, "/api/v2/secrets/secret/administration/1/x", 200, true},
		{"credential config write", http.MethodPut, "/api/v2/configurations/configuration/1/9", 200, true},
		{"token issue", http.MethodPost, "/api/v2/auth/token/", 201, true},
		{"scim provisioning", http.MethodPost, "/api/v2/scim/v2/Users", 201, true},
		{"project lifecycle", http.MethodPost, "/api/v2/projects/project/administration", 200, true},
		{"budget change", http.MethodPut, "/api/v2/elitea_core/project_budget/administration/3/budget", 200, true},

		// Refusals on an audited surface are kept even though they are reads:
		// "who tried to reach something they could not" is the question.
		{"refused admin read", http.MethodGet, "/api/v2/admin/auth_users/administration", 403, true},
		{"unauthorized admin read", http.MethodGet, "/api/v2/admin/auth_users/administration", 401, true},

		// Successful reads are not. Highest volume, lowest information.
		{"successful admin read", http.MethodGet, "/api/v2/admin/auth_users/administration", 200, false},
		// Content CRUD is not administrative.
		{"content mutation", http.MethodPost, "/api/v2/elitea_core/pin/prompt_lib/1/agent/2", 200, false},
		{"notification write", http.MethodDelete, "/api/v2/notifications/notifications/prompt_lib/1", 200, false},
		// The model path is the gateway's request log, not this trail.
		{"llm proxy", http.MethodPost, "/llm/chat/completions", 200, false},
		// Auditing the telemetry intake would be a loop.
		{"tracing ingest", http.MethodPost, "/api/v2/tracing/otlp/prompt_lib/1", 200, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := &captureRecorder{}
			router := auditTestRouter(recorder, "/*", testCase.status, nil)

			response := httptest.NewRecorder()
			router.ServeHTTP(response, authenticatedRequest(testCase.method, testCase.path))

			events := recorder.all()
			if testCase.audited && len(events) != 1 {
				t.Fatalf("%s %s (%d) recorded %d events, want 1",
					testCase.method, testCase.path, testCase.status, len(events))
			}
			if !testCase.audited && len(events) != 0 {
				t.Fatalf("%s %s (%d) recorded %d events, want 0",
					testCase.method, testCase.path, testCase.status, len(events))
			}
		})
	}
}

/* ── the row the readers will render ────────────────────────────────────── */

func TestAuditEventCarriesTheColumnsTheAuditTrailReads(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder,
		"/api/v2/admin/user_suspend/{mode}/{userID}", http.StatusOK, nil)

	response := httptest.NewRecorder()
	router.ServeHTTP(response,
		authenticatedRequest(http.MethodPut, "/api/v2/admin/user_suspend/administration/42?project_id=8"))

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	event := events[0]

	// event_type must be a value the SPA's tabs and palette know, or the row
	// lands in the table and is filtered straight back out.
	if event.EventType != "api" {
		t.Errorf("event_type = %q, want %q", event.EventType, "api")
	}
	// The route PATTERN, not the raw target: the raw target carries user id 42.
	const wantRoute = "/api/v2/admin/user_suspend/{mode}/{userID}"
	if event.HTTPRoute != wantRoute {
		t.Errorf("http_route = %q, want %q", event.HTTPRoute, wantRoute)
	}
	if event.Action != "PUT "+wantRoute {
		t.Errorf("action = %q, want %q", event.Action, "PUT "+wantRoute)
	}
	if event.HTTPMethod != http.MethodPut {
		t.Errorf("http_method = %q, want PUT", event.HTTPMethod)
	}
	if event.StatusCode == nil || *event.StatusCode != 200 {
		t.Errorf("status_code = %v, want 200", event.StatusCode)
	}
	if event.IsError {
		t.Error("is_error = true on a 200")
	}
	// duration_ms must be present: the heatmap excludes rows without one, so a
	// NULL here silently removes the row from half the page.
	if event.DurationMS == nil {
		t.Error("duration_ms is nil; the heatmap would drop this row")
	}
	if event.UserID == nil || *event.UserID != 77 {
		t.Errorf("user_id = %v, want 77", event.UserID)
	}
	if event.UserEmail != "admin@test.local" {
		t.Errorf("user_email = %q", event.UserEmail)
	}
	if event.ProjectID == nil || *event.ProjectID != 8 {
		t.Errorf("project_id = %v, want 8 (from the query parameter)", event.ProjectID)
	}
	// The Traces view groups on trace_id IS NOT NULL. Empty here means that
	// whole view renders empty while Spans shows rows.
	if len(event.TraceID) != 32 || len(event.SpanID) != 16 {
		t.Errorf("trace/span identity = %q/%q, want 32/16 hex characters",
			event.TraceID, event.SpanID)
	}
}

// project_id is what feeds the admin Projects page's per-member activity strip
// (eliteacore/project_activity.go groups on it), so a row filed under no
// project disappears from that read entirely. The `{projectID}` URL parameter
// is the source 251 of this service's routes carry.
func TestAuditEventTakesProjectIDFromTheRouteParameter(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder,
		"/api/v2/secrets/secret/{mode}/{projectID}/{name}", http.StatusOK, nil)

	response := httptest.NewRecorder()
	router.ServeHTTP(response,
		authenticatedRequest(http.MethodPost, "/api/v2/secrets/secret/default/314/api-key"))

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].ProjectID == nil || *events[0].ProjectID != 314 {
		t.Fatalf("project_id = %v, want 314 from {projectID}", events[0].ProjectID)
	}
	// The secret's NAME must not leak into the row. http_route is the pattern,
	// and nothing here copies the raw target.
	if strings.Contains(events[0].HTTPRoute, "api-key") || strings.Contains(events[0].Action, "api-key") {
		t.Fatalf("the raw target leaked into the row: %q / %q", events[0].HTTPRoute, events[0].Action)
	}
}

func TestAuditEventMarksRefusalsAsErrors(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder, "/api/v2/admin/{rest}", http.StatusForbidden, nil)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/api/v2/admin/auth_users"))

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if !events[0].IsError {
		t.Error("is_error = false on a 403; the trail's error filter would hide the refusal")
	}
	if events[0].StatusCode == nil || *events[0].StatusCode != http.StatusForbidden {
		t.Errorf("status_code = %v, want 403", events[0].StatusCode)
	}
}

/* ── DECISION 1: the annotation escape hatch ────────────────────────────── */

func TestAuditAnnotationAddsDomainMeaningToTheRow(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder, "/api/v2/admin/user_suspend/{mode}/{userID}",
		http.StatusOK, func(r *http.Request) {
			audit.Annotate(r.Context(), audit.Annotation{
				Action:     "user.suspend",
				EntityType: "user",
				EntityID:   audit.ID(42),
				EntityName: "victim@test.local",
			})
		})

	response := httptest.NewRecorder()
	router.ServeHTTP(response,
		authenticatedRequest(http.MethodPut, "/api/v2/admin/user_suspend/administration/42"))

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	event := events[0]
	if event.Action != "user.suspend" {
		t.Errorf("action = %q, want the annotated %q", event.Action, "user.suspend")
	}
	if event.EntityType != "user" || event.EntityID == nil || *event.EntityID != 42 {
		t.Errorf("entity = %q/%v, want user/42", event.EntityType, event.EntityID)
	}
	if event.EntityName != "victim@test.local" {
		t.Errorf("entity_name = %q", event.EntityName)
	}
	// The HTTP shape survives the annotation — the page shows both columns.
	if event.HTTPMethod != http.MethodPut || event.HTTPRoute == "" {
		t.Errorf("annotation erased the HTTP shape: %q %q", event.HTTPMethod, event.HTTPRoute)
	}
}

// Annotate must be safe on a request the policy does not audit: a handler
// cannot be expected to know the policy, and a policy change must not be able
// to turn a handler into a panic.
func TestAnnotateOnAnUnauditedRequestIsANoOp(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder, "/*", http.StatusOK, func(r *http.Request) {
		audit.Annotate(r.Context(), audit.Annotation{Action: "content.pin"})
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response,
		authenticatedRequest(http.MethodPost, "/api/v2/elitea_core/pin/prompt_lib/1/agent/2"))

	if got := len(recorder.all()); got != 0 {
		t.Fatalf("recorded %d events on an unaudited surface, want 0", got)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

/* ── DECISION 3: the emitter can never break the request ────────────────── */

// panickingRecorder stands in for any recorder fault. Record is documented as
// unable to fail the request; a recorder that panics is the loudest version of
// a fault, and it must still not reach the client — the response is already
// written by the time Record is called, so a panic here would surface as a
// broken connection on a request that succeeded.
type panickingRecorder struct{}

func (panickingRecorder) Record(context.Context, audit.Event) { panic("recorder fault") }

func TestAuditMiddlewareDoesNotMountWithoutARecorder(t *testing.T) {
	for name, recorder := range map[string]audit.Recorder{
		"nil interface": nil,
		// The typed-nil case: NewPostgresRecorder(nil, nil) returns a nil
		// *PostgresRecorder, which is NOT == nil as an interface.
		"typed nil": audit.NewPostgresRecorder(nil, nil),
	} {
		t.Run(name, func(t *testing.T) {
			var served bool
			handler := Audit(recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served = true
				w.WriteHeader(http.StatusOK)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedRequest(http.MethodPut, "/api/v2/admin/x"))
			if !served || response.Code != http.StatusOK {
				t.Fatalf("served = %v, status = %d; a nil recorder must be transparent",
					served, response.Code)
			}
		})
	}
}

// The response is fully written before Record is called, so a request whose
// audit write faults is still a completed request as far as the client is
// concerned. This asserts the ordering that guarantees it.
func TestAuditRecordRunsAfterTheResponseIsWritten(t *testing.T) {
	router := chi.NewRouter()
	router.Use(Audit(panickingRecorder{}))
	router.Put("/api/v2/admin/x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	response := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("expected the recorder's panic to propagate to the Recover middleware")
		}
		if response.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204 written before the recorder ran", response.Code)
		}
	}()
	router.ServeHTTP(response, authenticatedRequest(http.MethodPut, "/api/v2/admin/x"))
}

func TestAuditedSurfacesIsACopy(t *testing.T) {
	surfaces := AuditedSurfaces()
	if len(surfaces) == 0 {
		t.Fatal("AuditedSurfaces is empty")
	}
	surfaces[0] = "/mutated/"
	if AuditedSurfaces()[0] == "/mutated/" {
		t.Fatal("AuditedSurfaces hands out the package's own slice")
	}
}

/* ── non-user actors (shared migration 0135) ────────────────────────────── */

// A SCIM client has no user id. Its change must still name an actor, or the
// trail shows a provisioning change made by nobody.
func TestAuditRecordsANonUserActorWhenNoUserIsPresent(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder, "/*", http.StatusCreated, func(r *http.Request) {
		audit.Annotate(r.Context(), audit.Annotation{Actor: "scim:Entra ID"})
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v2/scim/v2/Users", nil))

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].UserID != nil || events[0].UserEmail != "scim:Entra ID" {
		t.Fatalf("event actor = (%v, %q), want (nil, scim:Entra ID)", events[0].UserID, events[0].UserEmail)
	}
}

func TestAuditNeverReplacesAUserWithANonUserActor(t *testing.T) {
	recorder := &captureRecorder{}
	router := auditTestRouter(recorder, "/*", http.StatusOK, func(r *http.Request) {
		audit.Annotate(r.Context(), audit.Annotation{Actor: "scim:Entra ID"})
	})
	router.ServeHTTP(httptest.NewRecorder(), authenticatedRequest(http.MethodPost, "/api/v2/scim/v2/Users"))

	events := recorder.all()
	if len(events) != 1 || events[0].UserEmail != "admin@test.local" {
		t.Fatalf("events = %+v, want the user's own address", events)
	}
}
