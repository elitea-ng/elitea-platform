package agentidentity

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	coreagentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

type fakeExternal struct {
	err       error
	lastID    string
	lastVerb  string
	lastActor string
}

func (f *fakeExternal) Create(_ context.Context, req domain.ExternalIdentityRequest, by string) (domain.ExternalAgentIdentity, error) {
	f.lastActor = by
	return domain.ExternalAgentIdentity{ID: bindingID, Mode: req.Mode, State: domain.ExternalIdentityProvisioning,
		Ref: domain.ExternalRef{Provider: req.Provider, ProviderTenant: req.ProviderTenant}}, f.err
}

func (f *fakeExternal) List(context.Context, domain.ExternalListFilter) ([]domain.ExternalAgentIdentity, error) {
	return nil, f.err
}

func (f *fakeExternal) run(verb string) func(context.Context, string, string) (domain.ExternalAgentIdentity, error) {
	return func(_ context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
		f.lastID, f.lastVerb, f.lastActor = id, verb, by
		return domain.ExternalAgentIdentity{ID: id, State: domain.ExternalIdentityActive}, f.err
	}
}

func (f *fakeExternal) Provision(ctx context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
	return f.run("provision")(ctx, id, by)
}
func (f *fakeExternal) Activate(ctx context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
	return f.run("activate")(ctx, id, by)
}
func (f *fakeExternal) Suspend(ctx context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
	return f.run("suspend")(ctx, id, by)
}
func (f *fakeExternal) Resume(ctx context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
	return f.run("resume")(ctx, id, by)
}
func (f *fakeExternal) Retire(ctx context.Context, id, by string) (domain.ExternalAgentIdentity, error) {
	return f.run("retire")(ctx, id, by)
}

// newExternalRouter mounts the handler with the gates router.go uses.
func newExternalRouter(t *testing.T, service ExternalService, permissions ...string) http.Handler {
	t.Helper()
	h, err := NewExternal(service)
	if err != nil {
		t.Fatal(err)
	}
	resolver := grants{userID: 42, permissions: permissions}
	gate := func(required string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(resolver, auth.PermissionModeAdministration, required)
	}
	manage, approve := gate(PermissionExternalManage), gate(PermissionBindingsApprove)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "token-7"})))
		})
	})
	r.Route("/api/v2/admin", func(r chi.Router) {
		r.With(manage).Get("/external_agent_identities", h.ListExternalIdentities)
		r.With(manage).Post("/external_agent_identities", h.CreateExternalIdentity)
		r.With(manage).Post("/external_agent_identities/{id}:provision", h.ProvisionExternalIdentity)
		r.With(manage, approve).Post("/external_agent_identities/{id}:activate", h.ActivateExternalIdentity)
		r.With(manage).Post("/external_agent_identities/{id}:suspend", h.SuspendExternalIdentity)
		r.With(manage).Post("/external_agent_identities/{id}:resume", h.ResumeExternalIdentity)
		r.With(manage).Post("/external_agent_identities/{id}:retire", h.RetireExternalIdentity)
	})
	return r
}

func TestActivationNeedsBothManageAndBindingApproval(t *testing.T) {
	path := "/api/v2/admin/external_agent_identities/" + bindingID + ":activate"
	for _, perms := range [][]string{{PermissionExternalManage}, {PermissionBindingsApprove}} {
		if got := serve(newExternalRouter(t, &fakeExternal{}, perms...), http.MethodPost, path, "").Code; got != http.StatusForbidden {
			t.Errorf("activate with only %v = %d, want 403", perms, got)
		}
	}
	service := &fakeExternal{}
	got := serve(newExternalRouter(t, service, PermissionExternalManage, PermissionBindingsApprove), http.MethodPost, path, "")
	if got.Code != http.StatusOK || service.lastID != bindingID || service.lastVerb != "activate" || service.lastActor != "42" {
		t.Fatalf("activate = %d %s; service saw %+v", got.Code, got.Body, service)
	}
}

func TestExternalRoutesMapTypedErrors(t *testing.T) {
	path := "/api/v2/admin/external_agent_identities/" + bindingID + ":provision"
	for err, want := range map[error]int{
		domain.ErrExternalTransition:               http.StatusConflict,
		domain.ErrNoApprovedBinding:                http.StatusConflict,
		domain.ErrDuplicateExternalIdentity:        http.StatusConflict,
		domain.ErrExternalIdentityNotFound:         http.StatusNotFound,
		domain.ErrInvalidExternalIdentity:          http.StatusBadRequest,
		domain.ErrIdentityModeNotAllowed:           http.StatusForbidden,
		coreagentidentity.ErrProviderNotConfigured: http.StatusServiceUnavailable,
		coreagentidentity.ErrProviderCall:          http.StatusBadGateway,
		context.DeadlineExceeded:                   http.StatusInternalServerError,
	} {
		got := serve(newExternalRouter(t, &fakeExternal{err: err}, PermissionExternalManage), http.MethodPost, path, "")
		if got.Code != want {
			t.Errorf("%v: status %d, want %d", err, got.Code, want)
		}
	}
}

func TestCreateExternalIdentityRefusesUnknownFieldsAndRecordsTheCaller(t *testing.T) {
	service := &fakeExternal{}
	router := newExternalRouter(t, service, PermissionExternalManage)
	bad := serve(router, http.MethodPost, "/api/v2/admin/external_agent_identities", `{"client_secret":"x"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", bad.Code)
	}
	got := serve(router, http.MethodPost, "/api/v2/admin/external_agent_identities",
		`{"tenant_project_id":7,"application_id":11,"mode":"dedicated_agent","provider":"entra","provider_tenant":"t1"}`)
	if got.Code != http.StatusCreated || service.lastActor != "42" {
		t.Fatalf("create = %d %s", got.Code, got.Body)
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["state"] != "provisioning" || view["external_id"] != nil || view["provider"] != "entra" {
		t.Fatalf("view = %v", view)
	}
}
