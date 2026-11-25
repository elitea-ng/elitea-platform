package admin_test

// The agent governance Configuration sections on the narrow path (ADR-0059
// R0.15, issue 2552): agents.governance.view reads them, agents.governance.manage
// saves them, and neither reaches a section of another area. The legacy
// holders' answers over every section stay pinned by
// TestConfigurationRoutesAnswerTheLegacyHoldersAsBefore.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// agentGovernanceSections are the fifteen sections of the pair.
var agentGovernanceSections = []string{
	"agent_lifecycle", "agents", "ai_disclosure", "approval_policy", "approvals", "browser", "catalogue",
	"credential_review", "delegation", "notifications", "platform_preamble", "proactivity", "remote_agents",
	"run_caps", "worker_egress",
}

// The sections of other areas the pair never opens: the guardrails (llm.*),
// the platform, identity and break-glass.
var notAgentGovernanceSections = []string{"guardrails", "circuit_breaker", "governance", "maintenance", "auth", "break_glass"}

// A holder of only agents.governance.manage gets the legacy administrator's
// answer on every agent governance section, read and write, and 403 on the
// sections of other areas, the guardrails among them. The router has no
// store, so a passed request answers what the legacy administrator gets; the
// save itself is TestAnAgentGovernanceManagerSavesItsSection.
func TestAnAgentGovernanceManagerReachesItsSectionsAndNotGuardrails(t *testing.T) {
	legacy := newConfigRouter(legacyAdministratorNames...)
	router := newConfigRouter(adminaccess.AgentsGovernanceManage)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		for _, id := range agentGovernanceSections {
			got, want := sectionAnswer(t, router, method, id), sectionAnswer(t, legacy, method, id)
			if got == http.StatusForbidden || got != want {
				t.Errorf("%s %s = %d, want the legacy administrator's %d", method, id, got, want)
			}
		}
		for _, id := range notAgentGovernanceSections {
			if got := sectionAnswer(t, router, method, id); got != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", method, id, got)
			}
		}
	}
}

// A holder of only agents.governance.view reads every agent governance
// section and saves none.
func TestAnAgentGovernanceViewerReadsItsSectionsAndSavesNone(t *testing.T) {
	legacy := newConfigRouter(legacyAdministratorNames...)
	router := newConfigRouter(adminaccess.AgentsGovernanceView)
	for _, id := range agentGovernanceSections {
		got, want := sectionAnswer(t, router, http.MethodGet, id), sectionAnswer(t, legacy, http.MethodGet, id)
		if got == http.StatusForbidden || got != want {
			t.Errorf("GET %s = %d, want the legacy administrator's %d", id, got, want)
		}
		if got := sectionAnswer(t, router, http.MethodPut, id); got != http.StatusForbidden {
			t.Errorf("PUT %s = %d, want 403", id, got)
		}
	}
	for _, id := range notAgentGovernanceSections {
		if got := sectionAnswer(t, router, http.MethodGet, id); got != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", id, got)
		}
	}
}

// The save itself, against a database (ELITEA_TEST_DATABASE_URL): a holder of
// only agents.governance.manage saves the remote_agents section through the
// route gate and the handler router.go composes, reads it back, and is refused
// the guardrails section.
func TestAnAgentGovernanceManagerSavesItsSection(t *testing.T) {
	pool := newConfigPool(t)
	handler := admin.NewHandler(pool, admin.WithPermissionResolver(grantingResolver(adminaccess.AgentsGovernanceManage)))
	principal := auth.User{ID: "7", UserID: "7", Email: "operator@example.com"}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(handler.RequireConfigurationAccess).
		Get("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValues)
	router.With(handler.RequireConfigurationAccess).
		Put("/admin/plugin_config_values/administration/{plugin}", handler.AdministrationPluginConfigValuesSave)

	const remoteAgents = "/admin/plugin_config_values/administration/" + platformconfig.SectionRemoteAgents
	saved := configDo(t, router, http.MethodPut, remoteAgents, map[string]any{"values": map[string]any{
		platformconfig.KeyRemoteAgentsOutboundAssertions: string(platformconfig.OutboundAssertionsOn),
	}})
	if saved.Code != http.StatusOK {
		t.Fatalf("PUT remote_agents = %d, want 200 (body %s)", saved.Code, saved.Body.String())
	}
	read := configDo(t, router, http.MethodGet, remoteAgents, nil)
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"`+string(platformconfig.OutboundAssertionsOn)+`"`) {
		t.Fatalf("GET remote_agents = %d %s, want 200 with the saved value", read.Code, read.Body.String())
	}
	refused := configDo(t, router, http.MethodPut, "/admin/plugin_config_values/administration/guardrails",
		map[string]any{"values": map[string]any{"blocked_toolkits": []any{"shell"}}})
	if refused.Code != http.StatusForbidden {
		t.Errorf("PUT guardrails = %d, want 403 (body %s)", refused.Code, refused.Body.String())
	}
}
