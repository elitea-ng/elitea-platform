package agentworkforce

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakePolicyStore struct {
	fakeStore
	set      agentworkforce.Policy
	setActor repos.WorkforceActor
	undo     struct {
		action  int64
		actor   repos.WorkforceActor
		confirm bool
	}
	filter repos.ActionFilter
}

func (f *fakePolicyStore) Policy(_ context.Context, projectID int64) (repos.WorkforcePolicy, error) {
	return repos.WorkforcePolicy{Policy: agentworkforce.DefaultPolicy(), ProjectID: projectID}, nil
}

func (f *fakePolicyStore) SetPolicy(_ context.Context, projectID int64, actor repos.WorkforceActor, policy agentworkforce.Policy) (repos.WorkforcePolicy, error) {
	f.set, f.setActor = policy, actor
	if !actor.Manager {
		return repos.WorkforcePolicy{}, agentworkforce.Refuse(agentworkforce.ForbiddenCode, "no")
	}
	return repos.WorkforcePolicy{Policy: policy, ProjectID: projectID, Saved: true}, nil
}

func (f *fakePolicyStore) ListAutomaticActions(_ context.Context, _ int64, filter repos.ActionFilter) ([]repos.AutomaticAction, int, error) {
	f.filter = filter
	return []repos.AutomaticAction{}, 0, nil
}

func (f *fakePolicyStore) UndoAutoHire(_ context.Context, _, actionID int64, actor repos.WorkforceActor, _ string, confirm bool) (repos.AutomaticAction, error) {
	f.undo.action, f.undo.actor, f.undo.confirm = actionID, actor, confirm
	return repos.AutomaticAction{ID: actionID, Status: agentworkforce.ActionUndone}, nil
}

func (f *fakePolicyStore) KeepAutoHire(_ context.Context, _, actionID int64, _ repos.WorkforceActor, _ string) (repos.AutomaticAction, error) {
	return repos.AutomaticAction{ID: actionID, Status: agentworkforce.ActionKept}, nil
}

func (f *fakePolicyStore) ObjectToAction(_ context.Context, _, actionID int64, _ repos.WorkforceActor, _ string) (repos.AutomaticAction, error) {
	return repos.AutomaticAction{}, agentworkforce.Refuse(agentworkforce.ActionClosedCode, "closed")
}

func servePolicy(t *testing.T, store *fakePolicyStore, manager bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(store, func(_ *http.Request, projectID, permission string) (bool, error) {
		return manager && projectID == "7" && permission == ManagePermission, nil
	})
	router := chi.NewRouter()
	router.Get(PolicyPath, handler.GetPolicy)
	router.Put(PolicyPath, handler.SetPolicy)
	router.Get(ActionsPath, handler.ListActions)
	router.Post(ActionUndoPath, handler.UndoAction)
	router.Post(ActionKeepPath, handler.KeepAction)
	router.Post(ActionObjectPath, handler.ObjectAction)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "4", UserID: "4", AuthType: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestPolicyReadIsOffByDefaultAndSaysWhoMayManage(t *testing.T) {
	store := &fakePolicyStore{}
	recorder := servePolicy(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/7/policy", "")
	var body map[string]any
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
	}
	if body["auto_hire_enabled"] != false || body["auto_dismiss_enabled"] != false || body["can_manage"] != true {
		t.Fatalf("body = %v", body)
	}
}

func TestPolicyAndActionsAreReadByAgentManagersOnly(t *testing.T) {
	store := &fakePolicyStore{}
	for _, path := range []string{"/agent_workforce/prompt_lib/7/policy", "/agent_workforce/prompt_lib/7/automatic_actions"} {
		recorder := servePolicy(t, store, false, http.MethodGet, path, "")
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), agentworkforce.ForbiddenCode) {
			t.Fatalf("%s as a member: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if recorder := servePolicy(t, store, true, http.MethodGet, path, ""); recorder.Code != http.StatusOK {
			t.Fatalf("%s as a manager: %d", path, recorder.Code)
		}
	}
}

func TestSetPolicyCarriesTheManagerFlagAndRefusesUnknownFields(t *testing.T) {
	store := &fakePolicyStore{}
	body := `{"auto_hire_enabled":true,"min_successful_runs":3,"max_auto_hired":5,"auto_dismiss_enabled":false,"auto_dismiss_rules":[],"grace_days":7}`
	recorder := servePolicy(t, store, false, http.MethodPut, "/agent_workforce/prompt_lib/7/policy", body)
	if recorder.Code != http.StatusForbidden || store.setActor.Manager || !store.set.AutoHireEnabled {
		t.Fatalf("%d %+v", recorder.Code, store.setActor)
	}
	recorder = servePolicy(t, store, true, http.MethodPut, "/agent_workforce/prompt_lib/7/policy", body)
	if recorder.Code != http.StatusOK || !store.setActor.Manager {
		t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
	}
	recorder = servePolicy(t, store, true, http.MethodPut, "/agent_workforce/prompt_lib/7/policy", `{"auto_delete":true}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", recorder.Code)
	}
}

func TestActionRoutes(t *testing.T) {
	store := &fakePolicyStore{}
	if recorder := servePolicy(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/7/automatic_actions?status=open&application_id=3", ""); recorder.Code != http.StatusOK ||
		store.filter.Status != "open" || store.filter.ApplicationID != 3 {
		t.Fatalf("list: %d %+v", recorder.Code, store.filter)
	}
	if recorder := servePolicy(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/7/automatic_actions?status=bogus", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("bad status: %d", recorder.Code)
	}
	recorder := servePolicy(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/automatic_actions/12/undo", `{"confirm_nested":true}`)
	if recorder.Code != http.StatusOK || store.undo.action != 12 || !store.undo.confirm || !store.undo.actor.Manager {
		t.Fatalf("undo: %d %+v", recorder.Code, store.undo)
	}
	if recorder := servePolicy(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/automatic_actions/12/keep", ""); recorder.Code != http.StatusOK {
		t.Fatalf("keep: %d", recorder.Code)
	}
	recorder = servePolicy(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/automatic_actions/12/object", "")
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), agentworkforce.ActionClosedCode) {
		t.Fatalf("object: %d %s", recorder.Code, recorder.Body.String())
	}
}
