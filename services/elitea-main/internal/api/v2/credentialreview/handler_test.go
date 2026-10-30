package credentialreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type noAudit struct{}

func (noAudit) Append(context.Context, pgx.Tx, securityaudit.SecurityEvent) error { return nil }

func project(r *http.Request, id string) *http.Request {
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", id)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, routeContext))
}

// stores that never reach the database: every request below is refused
// before a store call.
func storedHandler(t *testing.T, check PermissionCheck) *Handler {
	t.Helper()
	review, err := repos.NewCredentialReview(&pgxpool.Pool{}, noAudit{}, func(context.Context, int64, string, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	trimming, err := repos.NewPermissionTrimming(review)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(review, trimming, check)
}

func TestNoStoreAnswers503(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewHandler(nil, nil, nil).Inventory(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"unavailable"`) {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
}

func TestInvalidRequestsAreRefusedBeforeTheStore(t *testing.T) {
	h := storedHandler(t, nil)
	cases := []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
		req   *http.Request
	}{
		{"unknown kind", h.Inventory, httptest.NewRequest(http.MethodGet, "/?kind=password", nil)},
		{"limit over 500", h.Inventory, httptest.NewRequest(http.MethodGet, "/?limit=501", nil)},
		{"stale not a bool", h.Inventory, httptest.NewRequest(http.MethodGet, "/?stale=yes", nil)},
		{"revoke unknown field", h.Revoke, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"kind":"pat","credential_id":"1","secret":"x"}`))},
		{"revoke without id", h.Revoke, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"kind":"pat"}`))},
		{"window over 365", h.AdminTrimming, httptest.NewRequest(http.MethodGet, "/?window_days=400", nil)},
		{"application without project", h.AdminTrimming, httptest.NewRequest(http.MethodGet, "/?application_id=3", nil)},
		{"admin remove without project", h.AdminRemove, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"kind":"tool","key":"1/2/x"}`))},
		{"project remove of a platform entry", h.ProjectRemove, project(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"kind":"egress_host","key":"platform:a.example"}`)), "4")},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		tc.serve(recorder, tc.req)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s", tc.name, recorder.Code, recorder.Body)
		}
	}
}

// A project egress entry is a project setting: removing it needs the
// configuration route's permission on top of agents.trimming.manage.
func TestProjectEgressRemovalNeedsTheSettingsPermission(t *testing.T) {
	h := storedHandler(t, func(*http.Request) (bool, error) { return false, nil })
	recorder := httptest.NewRecorder()
	h.ProjectRemove(recorder, project(httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"kind":"egress_host","key":"project:api.example.com"}`)), "4"))
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), ProjectEgressPermision) {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
}

// A removal of an item that is not a removable candidate now answers 409
// with the refusal's own code.
func TestTrimRefusalIs409WithItsCode(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeStoreError(recorder, httptest.NewRequest(http.MethodPost, "/", nil),
		&repos.TrimRefusal{Code: repos.TrimRefusalTooNew, Reason: "the item was granted for less than the window"})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"trim_item_too_new"`) ||
		strings.Contains(recorder.Body.String(), "permission trimming:") {
		t.Fatalf("status %d body %s", recorder.Code, recorder.Body)
	}
}
