package agentidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity/governance"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

type grants struct {
	userID      int64
	permissions []string
}

func (g grants) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: g.userID, Permissions: g.permissions}, nil
}

// fakeService records the last call and returns a canned answer or error.
type fakeService struct {
	err        error
	lastID     string
	lastAction string
	lastActor  string
}

var fixedTime = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func (f *fakeService) binding() agentidentity.AgentIdentityBinding {
	return agentidentity.AgentIdentityBinding{ID: f.lastID, Mode: agentidentity.IdentityModeDedicatedAgent,
		State: agentidentity.BindingActive, CreatedAt: fixedTime, UpdatedAt: fixedTime}
}

func (f *fakeService) ProposeBinding(_ context.Context, p agentidentity.Proposal, by string) (agentidentity.AgentIdentityBinding, error) {
	f.lastActor = by
	return agentidentity.AgentIdentityBinding{ID: "new", Mode: p.Mode, DefinitionDigest: p.DefinitionDigest,
		State: agentidentity.BindingProposed, ProposedBy: by, CreatedAt: fixedTime, UpdatedAt: fixedTime}, f.err
}

func (f *fakeService) ListBindings(context.Context, agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	return []agentidentity.AgentIdentityBinding{f.binding()}, f.err
}

func (f *fakeService) TransitionBinding(_ context.Context, id string, action agentidentity.Action, by string) (agentidentity.AgentIdentityBinding, error) {
	f.lastID, f.lastAction, f.lastActor = id, string(action), by
	return f.binding(), f.err
}

func (f *fakeService) RepinBinding(ctx context.Context, id, by string,
	digestOf func(context.Context, agentidentity.AgentIdentityBinding) ([32]byte, error)) (agentidentity.AgentIdentityBinding, error) {
	f.lastID, f.lastAction, f.lastActor = id, "repin", by
	if f.err != nil {
		return agentidentity.AgentIdentityBinding{}, f.err
	}
	current := f.binding()
	current.TenantProjectID, current.ApplicationID, current.ApplicationVersionID = 7, 3, 4
	digest, err := digestOf(ctx, current)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	return current.Repin(digest, by, fixedTime)
}

func (f *fakeService) IssueMandate(_ context.Context, req mandate.Request, by string) (mandate.Mandate, error) {
	f.lastActor = by
	return mandate.Mandate{ID: "m", Kind: req.Kind, ExpiresAt: req.ExpiresAt, State: mandate.StateProposed}, f.err
}

func (f *fakeService) ListMandates(context.Context, mandate.ListFilter) ([]mandate.Mandate, error) {
	return nil, f.err
}

func (f *fakeService) TransitionMandate(_ context.Context, id string, action mandate.Action, by string) (mandate.Mandate, error) {
	f.lastID, f.lastAction, f.lastActor = id, string(action), by
	return mandate.Mandate{ID: id, State: mandate.StateActive, ExpiresAt: time.Now().Add(time.Hour)}, f.err
}

// newRouter mounts the handler with the same gates router.go uses.
func newRouter(t *testing.T, service Service, permissions ...string) http.Handler {
	t.Helper()
	h, err := New(service)
	if err != nil {
		t.Fatal(err)
	}
	resolver := grants{userID: 42, permissions: permissions}
	gate := func(required ...string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, required...)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "token-7"})))
		})
	})
	r.Route("/api/v2/admin", func(r chi.Router) {
		r.With(gate(PermissionBindingsManage, PermissionBindingsApprove)).Get("/agent_identities", h.ListBindings)
		r.With(gate(PermissionBindingsManage)).Post("/agent_identities", h.CreateBinding)
		r.With(gate(PermissionBindingsApprove)).Post("/agent_identities/{id}:approve", h.ApproveBinding)
		r.With(gate(PermissionBindingsManage)).Post("/agent_identities/{id}:suspend", h.SuspendBinding)
		r.With(gate(PermissionBindingsManage)).Post("/agent_identities/{id}:revoke", h.RevokeBinding)
		r.With(gate(PermissionBindingsManage)).Post("/agent_identities/{id}:repin", h.RepinBinding)
		r.With(gate(PermissionMandatesManage, PermissionMandatesApprove)).Get("/mandates", h.ListMandates)
		r.With(gate(PermissionMandatesManage)).Post("/mandates", h.CreateMandate)
		r.With(gate(PermissionMandatesApprove)).Post("/mandates/{id}:approve", h.ApproveMandate)
		r.With(gate(PermissionMandatesManage)).Post("/mandates/{id}:suspend", h.SuspendMandate)
		r.With(gate(PermissionMandatesManage)).Post("/mandates/{id}:revoke", h.RevokeMandate)
	})
	return r
}

func serve(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

const bindingID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11"

func TestEveryRouteRefusesACallerWithoutItsPermission(t *testing.T) {
	routes := []struct{ method, path, allowedBy string }{
		{http.MethodGet, "/api/v2/admin/agent_identities", PermissionBindingsManage},
		{http.MethodPost, "/api/v2/admin/agent_identities", PermissionBindingsManage},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":approve", PermissionBindingsApprove},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":suspend", PermissionBindingsManage},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":revoke", PermissionBindingsManage},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":repin", PermissionBindingsManage},
		{http.MethodGet, "/api/v2/admin/mandates", PermissionMandatesManage},
		{http.MethodPost, "/api/v2/admin/mandates", PermissionMandatesManage},
		{http.MethodPost, "/api/v2/admin/mandates/" + bindingID + ":approve", PermissionMandatesApprove},
		{http.MethodPost, "/api/v2/admin/mandates/" + bindingID + ":suspend", PermissionMandatesManage},
		{http.MethodPost, "/api/v2/admin/mandates/" + bindingID + ":revoke", PermissionMandatesManage},
	}
	// A project member's ordinary grants, and the sibling family's grants.
	member := newRouter(t, &fakeService{}, "models.applications.applications.view", "models.chat.messages.create")
	for _, route := range routes {
		if got := serve(member, route.method, route.path, "{}").Code; got != http.StatusForbidden {
			t.Errorf("member: %s %s = %d, want 403", route.method, route.path, got)
		}
	}
	// Approve is not implied by manage, and the reverse holds too.
	manager := newRouter(t, &fakeService{}, PermissionBindingsManage, PermissionMandatesManage)
	approver := newRouter(t, &fakeService{}, PermissionBindingsApprove, PermissionMandatesApprove)
	for _, route := range routes {
		isApprove := strings.HasSuffix(route.path, ":approve")
		managerCode := serve(manager, route.method, route.path, "{}").Code
		approverCode := serve(approver, route.method, route.path, "{}").Code
		if isApprove && managerCode != http.StatusForbidden {
			t.Errorf("manage-only reached %s: %d", route.path, managerCode)
		}
		if !isApprove && route.method == http.MethodPost && approverCode != http.StatusForbidden {
			t.Errorf("approve-only reached %s %s: %d", route.method, route.path, approverCode)
		}
	}
}

func TestColonVerbRoutesReachTheirOwnAction(t *testing.T) {
	service := &fakeService{}
	router := newRouter(t, service, PermissionBindingsManage, PermissionBindingsApprove, PermissionMandatesManage, PermissionMandatesApprove)
	for _, verb := range []string{"approve", "suspend", "revoke"} {
		for _, family := range []string{"agent_identities", "mandates"} {
			response := serve(router, http.MethodPost, "/api/v2/admin/"+family+"/"+bindingID+":"+verb, "")
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s = %d: %s", family, verb, response.Code, response.Body.String())
			}
			if service.lastID != bindingID || service.lastAction != verb || service.lastActor != "42" {
				t.Errorf("%s %s reached id=%q action=%q actor=%q", family, verb, service.lastID, service.lastAction, service.lastActor)
			}
		}
	}
	if got := serve(router, http.MethodPost, "/api/v2/admin/agent_identities/"+bindingID+":delete", "").Code; got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
		t.Errorf("an unknown verb = %d, want 404/405", got)
	}
}

func TestTypedErrorsMapToSafeStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{agentidentity.ErrInvalidTransition, http.StatusConflict},
		{agentidentity.ErrDuplicateBinding, http.StatusConflict},
		{agentidentity.ErrExternalIdentityNotActive, http.StatusConflict},
		{agentidentity.ErrSelfApproval, http.StatusForbidden},
		{agentidentity.ErrIdentityModeNotAllowed, http.StatusForbidden},
		{agentidentity.ErrBindingNotFound, http.StatusNotFound},
		{governance.ErrPolicyUnavailable, http.StatusServiceUnavailable},
		{context.DeadlineExceeded, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		router := newRouter(t, &fakeService{err: tc.err}, PermissionBindingsApprove)
		response := serve(router, http.MethodPost, "/api/v2/admin/agent_identities/"+bindingID+":approve", "")
		if response.Code != tc.want {
			t.Errorf("%v = %d, want %d", tc.err, response.Code, tc.want)
		}
		if tc.want == http.StatusInternalServerError && strings.Contains(response.Body.String(), "deadline") {
			t.Errorf("an unknown error leaked its text: %s", response.Body.String())
		}
	}
	mandateCases := []struct {
		err  error
		want int
	}{
		{mandate.ErrExpiresAtRequired, http.StatusBadRequest},
		{mandate.ErrTTLExceedsPolicy, http.StatusBadRequest},
		{mandate.ErrBindingNotUsable, http.StatusConflict},
	}
	for _, tc := range mandateCases {
		router := newRouter(t, &fakeService{err: tc.err}, PermissionMandatesManage)
		body := `{"tenant_project_id":7,"kind":"autonomous","agent_binding_id":"` + bindingID + `","operation_scopes":["a"]}`
		if got := serve(router, http.MethodPost, "/api/v2/admin/mandates", body).Code; got != tc.want {
			t.Errorf("%v = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestCreateBindingDecodesStrictly(t *testing.T) {
	service := &fakeService{}
	router := newRouter(t, service, PermissionBindingsManage)
	digest := strings.Repeat("ab", 32)
	ok := serve(router, http.MethodPost, "/api/v2/admin/agent_identities",
		`{"tenant_project_id":7,"application_id":1,"application_version_id":2,"definition_digest":"`+digest+`","mode":"dedicated_agent"}`)
	if ok.Code != http.StatusCreated || service.lastActor != "42" {
		t.Fatalf("create = %d actor %q: %s", ok.Code, service.lastActor, ok.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(ok.Body.Bytes(), &view); err != nil || view["definition_digest"] != digest {
		t.Fatalf("view: %v %v", view, err)
	}
	for _, body := range []string{
		`{"definition_digest":"zz"}`,
		`{"definition_digest":"` + digest + `","token":"x"}`,
		`not json`,
	} {
		if got := serve(router, http.MethodPost, "/api/v2/admin/agent_identities", body).Code; got != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", body, got)
		}
	}
}

func TestNewRefusesANilService(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
}

// The actor is only the resolved auth_core user id (auth.User.UserID). A
// caller that reaches the handler without one (no gate stamped it) is
// refused before the service runs, never attributed to auth.User.ID.
func TestActorIsOnlyTheResolvedUserID(t *testing.T) {
	h, err := New(&fakeService{})
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		method, path string
		serve        http.HandlerFunc
		body         string
	}{
		{http.MethodPost, "/api/v2/admin/agent_identities", h.CreateBinding,
			`{"tenant_project_id":7,"application_id":1,"application_version_id":2,"definition_digest":"` + strings.Repeat("ab", 32) + `","mode":"dedicated_agent"}`},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":approve", h.ApproveBinding, ""},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":revoke", h.RevokeBinding, ""},
		{http.MethodPost, "/api/v2/admin/agent_identities/" + bindingID + ":repin", h.RepinBinding, ""},
		{http.MethodPost, "/api/v2/admin/mandates", h.CreateMandate,
			`{"tenant_project_id":7,"kind":"autonomous","agent_binding_id":"` + bindingID + `","operation_scopes":["a"]}`},
		{http.MethodPost, "/api/v2/admin/mandates/" + bindingID + ":approve", h.ApproveMandate, ""},
		{http.MethodPost, "/api/v2/admin/mandates/" + bindingID + ":suspend", h.SuspendMandate, ""},
	}
	for _, route := range routes {
		for _, tc := range []struct {
			name string
			user *auth.User
			want int
		}{
			{"no user", nil, http.StatusUnauthorized},
			{"compatibility id only", &auth.User{ID: "token-7"}, http.StatusForbidden},
			{"blank user id", &auth.User{ID: "42", UserID: "  "}, http.StatusForbidden},
		} {
			service := &fakeService{}
			h.service = service
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			if tc.user != nil {
				request = request.WithContext(auth.ContextWithUser(request.Context(), *tc.user))
			}
			response := httptest.NewRecorder()
			route.serve(response, request)
			if response.Code != tc.want {
				t.Errorf("%s %s (%s) = %d, want %d", route.method, route.path, tc.name, response.Code, tc.want)
			}
			if service.lastActor != "" {
				t.Errorf("%s %s (%s) reached the service as %q", route.method, route.path, tc.name, service.lastActor)
			}
		}
	}
}

func TestDelegationNotUsableIsAConflict(t *testing.T) {
	router := newRouter(t, &fakeService{err: mandate.ErrDelegationNotUsable}, PermissionMandatesManage)
	body := `{"tenant_project_id":7,"kind":"delegated_background","agent_binding_id":"` + bindingID +
		`","subject_user_id":5,"delegation_id":"` + bindingID + `","operation_scopes":["a"]}`
	if got := serve(router, http.MethodPost, "/api/v2/admin/mandates", body).Code; got != http.StatusConflict {
		t.Fatalf("ErrDelegationNotUsable = %d, want 409", got)
	}
}
