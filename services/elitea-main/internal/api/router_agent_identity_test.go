package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	v2skills "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// unusedAgentIdentityService is never reached: the routes are probed for
// registration and for the permission gate only.
type unusedAgentIdentityService struct{}

func (unusedAgentIdentityService) ProposeBinding(context.Context, agentidentity.Proposal, string) (agentidentity.AgentIdentityBinding, error) {
	panic("unreachable")
}
func (unusedAgentIdentityService) ListBindings(context.Context, agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	panic("unreachable")
}
func (unusedAgentIdentityService) TransitionBinding(context.Context, string, agentidentity.Action, string) (agentidentity.AgentIdentityBinding, error) {
	panic("unreachable")
}
func (unusedAgentIdentityService) IssueMandate(context.Context, mandate.Request, string) (mandate.Mandate, error) {
	panic("unreachable")
}
func (unusedAgentIdentityService) ListMandates(context.Context, mandate.ListFilter) ([]mandate.Mandate, error) {
	panic("unreachable")
}
func (unusedAgentIdentityService) TransitionMandate(context.Context, string, mandate.Action, string) (mandate.Mandate, error) {
	panic("unreachable")
}

var agentIdentityRoutes = []string{
	"GET /api/v2/admin/agent_identities",
	"POST /api/v2/admin/agent_identities",
	"POST /api/v2/admin/agent_identities/{id}:approve",
	"POST /api/v2/admin/agent_identities/{id}:suspend",
	"POST /api/v2/admin/agent_identities/{id}:revoke",
	"GET /api/v2/admin/mandates",
	"POST /api/v2/admin/mandates",
	"POST /api/v2/admin/mandates/{id}:approve",
	"POST /api/v2/admin/mandates/{id}:suspend",
	"POST /api/v2/admin/mandates/{id}:revoke",
}

func newAgentIdentityRouter(t *testing.T, admin *v2agentidentity.Handler) chi.Router {
	t.Helper()
	return NewRouter(RouterConfig{
		SkillsRepo:         struct{ v2skills.Repository }{},
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		AgentIdentityAdmin: admin,
	})
}

// Flag off (nil field): none of the routes exists, and a probe answers
// exactly what an unknown admin path answers.
func TestAgentIdentityRoutesAreUnmountedWhenTheDependencyIsNil(t *testing.T) {
	router := newAgentIdentityRouter(t, nil)
	routes := walkRoutes(t, router)
	for _, route := range agentIdentityRoutes {
		if _, ok := routes[route]; ok {
			t.Errorf("%s is mounted with AgentIdentityAdmin nil", route)
		}
	}
	unknown := serveStatus(t, router, http.MethodGet, "/api/v2/admin/no_such_surface_1112")
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/admin/agent_identities"},
		{http.MethodPost, "/api/v2/admin/agent_identities"},
		{http.MethodPost, "/api/v2/admin/agent_identities/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:approve"},
		{http.MethodGet, "/api/v2/admin/mandates"},
		{http.MethodPost, "/api/v2/admin/mandates/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:revoke"},
	} {
		if got := serveStatus(t, router, probe.method, probe.path); got != unknown {
			t.Errorf("flag off: %s %s = %d, an unknown route answers %d", probe.method, probe.path, got, unknown)
		}
	}
}

// Flag on: every route is mounted behind its gate. The test router has no
// pool, so the central resolver cannot grant anything; a caller is refused
// before the handler runs.
func TestAgentIdentityRoutesAreMountedAndGatedWhenTheDependencyIsSet(t *testing.T) {
	admin, err := v2agentidentity.New(unusedAgentIdentityService{})
	if err != nil {
		t.Fatal(err)
	}
	router := newAgentIdentityRouter(t, admin)
	routes := walkRoutes(t, router)
	for _, route := range agentIdentityRoutes {
		if _, ok := routes[route]; !ok {
			t.Errorf("%s is not mounted", route)
		}
	}
	got := serveStatus(t, router, http.MethodGet, "/api/v2/admin/agent_identities")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Errorf("GET /admin/agent_identities without a grant = %d, want a gate refusal", got)
	}
}

var externalAgentIdentityRoutes = []string{
	"GET /api/v2/admin/external_agent_identities",
	"POST /api/v2/admin/external_agent_identities",
	"GET /api/v2/admin/external_agent_identities/drift",
	"POST /api/v2/admin/external_agent_identities/{id}:provision",
	"POST /api/v2/admin/external_agent_identities/{id}:activate",
	"POST /api/v2/admin/external_agent_identities/{id}:suspend",
	"POST /api/v2/admin/external_agent_identities/{id}:resume",
	"POST /api/v2/admin/external_agent_identities/{id}:retire",
}

// unusedExternalService is never reached, like unusedAgentIdentityService.
type unusedExternalService struct{}

func (unusedExternalService) Create(context.Context, agentidentity.ExternalIdentityRequest, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) List(context.Context, agentidentity.ExternalListFilter) ([]agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) Provision(context.Context, string, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) Activate(context.Context, string, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) Suspend(context.Context, string, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) Resume(context.Context, string, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}
func (unusedExternalService) Retire(context.Context, string, string) (agentidentity.ExternalAgentIdentity, error) {
	panic("unreachable")
}

// Flag off (nil field): no external identity route exists, and a probe
// answers what an unknown admin path answers. Flag on: every route is
// mounted behind its gate.
func TestExternalAgentIdentityRoutesFollowTheDependency(t *testing.T) {
	off := NewRouter(RouterConfig{
		SkillsRepo: struct{ v2skills.Repository }{}, AuthValidator: testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
	})
	routes := walkRoutes(t, off)
	for _, route := range externalAgentIdentityRoutes {
		if _, ok := routes[route]; ok {
			t.Errorf("%s is mounted with ExternalAgentIdentityAdmin nil", route)
		}
	}
	unknown := serveStatus(t, off, http.MethodGet, "/api/v2/admin/no_such_surface_1151")
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/admin/external_agent_identities"},
		{http.MethodPost, "/api/v2/admin/external_agent_identities"},
		{http.MethodGet, "/api/v2/admin/external_agent_identities/drift"},
		{http.MethodPost, "/api/v2/admin/external_agent_identities/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:activate"},
		{http.MethodPost, "/api/v2/admin/external_agent_identities/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:retire"},
	} {
		if got := serveStatus(t, off, probe.method, probe.path); got != unknown {
			t.Errorf("flag off: %s %s = %d, an unknown route answers %d", probe.method, probe.path, got, unknown)
		}
	}

	admin, err := v2agentidentity.NewExternal(unusedExternalService{})
	if err != nil {
		t.Fatal(err)
	}
	on := NewRouter(RouterConfig{
		SkillsRepo: struct{ v2skills.Repository }{}, AuthValidator: testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{}, ExternalAgentIdentityAdmin: admin,
	})
	routes = walkRoutes(t, on)
	for _, route := range externalAgentIdentityRoutes {
		if _, ok := routes[route]; !ok {
			t.Errorf("%s is not mounted", route)
		}
	}
	got := serveStatus(t, on, http.MethodPost, "/api/v2/admin/external_agent_identities/0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f11:activate")
	if got == http.StatusNotFound || got == http.StatusOK || got == http.StatusInternalServerError {
		t.Errorf("activate without a grant = %d, want a gate refusal", got)
	}
}
