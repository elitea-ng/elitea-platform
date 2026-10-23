package api

// Is the audit emitter actually MOUNTED on the production router?
//
// This is the layer the unit suite cannot see. Both halves of #597 were
// individually correct and the wiring between them was the bug, and 2475 green
// unit tests could not tell. internal/api/middleware/audit_internal_test.go
// proves the middleware records the right events; nothing there proves
// NewRouter installs it, or that it sits below authentication where the
// principal is reachable. That is what this file asserts.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

type routerAuditRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *routerAuditRecorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *routerAuditRecorder) all() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Event(nil), r.events...)
}

func auditWiringRouter(recorder audit.Recorder) http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AuditRecorder:      recorder,
	})
}

// An administrative write through the WHOLE production chain — CORS, request
// id, security headers, OTel, recover, compression, Auth, maintenance — leaves
// an audit event carrying the authenticated principal.
//
// With no pool the permission resolver denies, so the request ends in a 403.
// That is not a weakness of the test: a refusal is precisely one of the two
// things DECISION 2 audits, and it proves the middleware sits BELOW the
// per-route RBAC gate rather than above it.
func TestProductionRouterEmitsAnAuditEventForRefusedAdministrativeWrites(t *testing.T) {
	recorder := &routerAuditRecorder{}
	router := auditWiringRouter(recorder)

	response := httptest.NewRecorder()
	request := testAuthHeader(httptest.NewRequest(
		http.MethodPut, "/api/v2/admin/user_suspend/administration/42", nil))
	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no resolver can grant without a pool)", response.Code)
	}

	events := recorder.all()
	if len(events) != 1 {
		t.Fatalf("the production router recorded %d audit events, want 1", len(events))
	}
	event := events[0]
	if event.EventType != "api" {
		t.Errorf("event_type = %q, want api", event.EventType)
	}
	const wantRoute = "/api/v2/admin/user_suspend/{mode}/{userID}"
	if event.HTTPRoute != wantRoute {
		t.Errorf("http_route = %q, want %q", event.HTTPRoute, wantRoute)
	}
	// The principal is the whole reason the middleware is mounted below Auth.
	if event.UserID == nil || *event.UserID != 1 {
		t.Errorf("user_id = %v, want 1; the emitter cannot see the principal", event.UserID)
	}
	if event.UserEmail != "member@test.local" {
		t.Errorf("user_email = %q, want the authenticated principal's", event.UserEmail)
	}
	if !event.IsError || event.StatusCode == nil || *event.StatusCode != http.StatusForbidden {
		t.Errorf("is_error/%v status/%v, want true/403", event.IsError, event.StatusCode)
	}
}

// The negative half. Without it, a middleware that audited EVERY request would
// pass the test above and quietly build the unbounded table DECISION 2 forbids.
func TestProductionRouterDoesNotAuditContentTraffic(t *testing.T) {
	recorder := &routerAuditRecorder{}
	router := auditWiringRouter(recorder)

	for _, target := range []string{
		"/api/v2/elitea_core/pin/prompt_lib/1/agent/2",
		"/api/v2/notifications/notifications/prompt_lib/1",
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, testAuthHeader(httptest.NewRequest(http.MethodPost, target, nil)))
	}

	if events := recorder.all(); len(events) != 0 {
		t.Fatalf("content traffic produced %d audit events, want 0: %+v", len(events), events)
	}
}

// Every audited surface prefix must still match a route that is actually
// mounted.
//
// A prefix that matches nothing audits nothing and says nothing — it reads
// exactly like a surface with no traffic, which is the "absence reads as
// correctness" class this repo keeps producing (see AuditedSurfaces' doc).
// Walking the real router is the only way to tell the two apart.
func TestEveryAuditedSurfaceMatchesAMountedRoute(t *testing.T) {
	router, ok := NewRouter(RouterConfig{}).(chi.Routes)
	if !ok {
		t.Fatal("NewRouter no longer returns a chi.Routes; this test cannot walk it")
	}

	matched := map[string]bool{}
	surfaces := apimw.AuditedSurfaces()
	if err := chi.Walk(router, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		for _, surface := range surfaces {
			if strings.HasPrefix(route, surface) {
				matched[surface] = true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk the router: %v", err)
	}

	for _, surface := range surfaces {
		if !matched[surface] {
			t.Errorf("audited surface %q matches no mounted route; it audits nothing", surface)
		}
	}
}
