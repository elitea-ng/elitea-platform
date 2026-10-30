package connectorrequests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type createStore struct {
	Store
	created []connectorapproval.Draft
}

func (s *createStore) Create(_ context.Context, projectID, _ int64, draft connectorapproval.Draft, _ bool) (connectorapproval.Request, error) {
	s.created = append(s.created, draft)
	return connectorapproval.Request{ID: 1, ProjectID: projectID, RequestedBy: 5}, nil
}

func (s *createStore) UserNames(context.Context, []int64) (map[int64]repos.ConnectorRequestUser, error) {
	return nil, nil
}

func postCreate(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/connector_requests/prompt_lib/1", strings.NewReader(body))
	routes := chi.NewRouteContext()
	routes.URLParams.Add("projectID", "1")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routes)
	ctx = auth.ContextWithUser(ctx, auth.User{ID: "5", UserID: "5"})
	recorder := httptest.NewRecorder()
	handler.Create(recorder, request.WithContext(ctx))
	return recorder
}

const createBodyFor = `{"target_kind":"toolkit_type","target":"jira","justification":"triage",%s}`

func body(extra string) string { return strings.Replace(createBodyFor, "%s", extra, 1) }

// #1537 review item 7: a requester names only projects where it holds the
// requesting permission, and the whole platform only as a connector
// approver.
func TestCreateRefusesProjectsAndPlatformBeyondTheRequesterScope(t *testing.T) {
	store := &createStore{}
	handler := NewHandler(store, nil).WithRequestScope(RequestScope{
		InProject:    func(_ *http.Request, projectID int64) (bool, error) { return projectID == 2, nil },
		PlatformWide: func(*http.Request) (bool, error) { return false, nil },
	})

	refused := postCreate(t, handler, body(`"project_ids":[1,2,3]`))
	if refused.Code != http.StatusForbidden {
		t.Fatalf("a foreign project: status %d %s", refused.Code, refused.Body)
	}
	var answer map[string]any
	_ = json.Unmarshal(refused.Body.Bytes(), &answer)
	if answer["code"] != connectorapproval.RequestScopeCode {
		t.Fatalf("code = %v", answer["code"])
	}
	if ids, _ := answer["refused_project_ids"].([]any); len(ids) != 1 || ids[0] != float64(3) {
		t.Fatalf("refused_project_ids = %v", answer["refused_project_ids"])
	}

	if response := postCreate(t, handler, body(`"platform_wide":true`)); response.Code != http.StatusForbidden {
		t.Fatalf("platform-wide from a member: status %d", response.Code)
	}

	if response := postCreate(t, handler, body(`"project_ids":[1,2]`)); response.Code != http.StatusCreated {
		t.Fatalf("held projects: status %d %s", response.Code, response.Body)
	}

	approver := NewHandler(store, nil).WithRequestScope(RequestScope{
		PlatformWide: func(*http.Request) (bool, error) { return true, nil },
	})
	if response := postCreate(t, approver, body(`"platform_wide":true`)); response.Code != http.StatusCreated {
		t.Fatalf("platform-wide from an approver: status %d %s", response.Code, response.Body)
	}
	if len(store.created) != 2 {
		t.Fatalf("created %d requests, want 2", len(store.created))
	}

	// No scope configured: the path's project only.
	if response := postCreate(t, NewHandler(store, nil), body(`"project_ids":[2]`)); response.Code != http.StatusForbidden {
		t.Fatalf("no scope, another project: status %d", response.Code)
	}
}
