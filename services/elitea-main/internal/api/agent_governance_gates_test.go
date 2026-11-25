package api

// The agent governance routes of ADR-0059 R0.15 (issue 2552): the agent
// registry, the delegation review list and scan and the worker egress hosts
// each accept the agent governance pair beside configuration.governance.
//
// Every request goes through the production router (NewRouter) with no
// database; the resolver grants exactly the holder's names. The answers of a
// holder of configuration.governance alone are pinned in
// testdata/admin_agent_governance_legacy.golden, captured on main before the
// change: such a holder sees no change.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const goldenAdminAgentGovernanceLegacy = "testdata/admin_agent_governance_legacy.golden"

// agentGovernanceRoutes are the routes of R0.15, each with the narrow names it
// accepts. A read accepts either name of the pair; the scan only the manage
// name.
var agentGovernanceRoutes = []struct {
	method, path string
	accepts      []string
}{
	{http.MethodGet, "/api/v2/admin/agent_registry", []string{adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage}},
	{http.MethodGet, "/api/v2/admin/delegation_review", []string{adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage}},
	{http.MethodPost, "/api/v2/admin/delegation_review/scan", []string{adminaccess.AgentsGovernanceManage}},
	{http.MethodGet, "/api/v2/admin/worker_egress/hosts", []string{adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage}},
}

// TestAgentGovernanceRoutesAnswerTheLegacyHolderAsBefore pins every touched
// route's answer to a holder of configuration.governance alone. Regenerate
// the file with UPDATE_GOLDEN=1 only for a change that means to alter what a
// legacy holder sees.
func TestAgentGovernanceRoutesAnswerTheLegacyHolderAsBefore(t *testing.T) {
	router := newPlatformGateRouter(t, adminaccess.ConfigurationGovernance)
	var b strings.Builder
	for _, route := range agentGovernanceRoutes {
		rec := servePlatformGate(t, router, route.method, route.path, "")
		fmt.Fprintf(&b, "%s\t%s\t%d\n", route.method, route.path, rec.Code)
	}
	got := b.String()
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenAdminAgentGovernanceLegacy), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenAdminAgentGovernanceLegacy, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenAdminAgentGovernanceLegacy)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_GOLDEN=1)", goldenAdminAgentGovernanceLegacy, err)
	}
	if got != string(want) {
		t.Errorf("a holder of configuration.governance gets different answers than before:\n got:\n%s\nwant:\n%s", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasSuffix(line, "\t403") {
			t.Errorf("%s: the legacy holder is refused", line)
		}
	}
}

// A holder of one name of the pair gets the legacy holder's answer on the
// routes that accept it and the gate's 403 on the others. A holder of nothing
// is refused everywhere.
func TestAgentGovernanceRoutesAdmitThePair(t *testing.T) {
	legacy := newPlatformGateRouter(t, adminaccess.ConfigurationGovernance)
	for _, holder := range []string{"", adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage} {
		router := newPlatformGateRouter(t, holder)
		for _, route := range agentGovernanceRoutes {
			got := servePlatformGate(t, router, route.method, route.path, "").Code
			accepted := false
			for _, name := range route.accepts {
				accepted = accepted || name == holder
			}
			if !accepted {
				if got != http.StatusForbidden {
					t.Errorf("holder %q: %s %s = %d, want 403", holder, route.method, route.path, got)
				}
				continue
			}
			want := servePlatformGate(t, legacy, route.method, route.path, "").Code
			if got != want || got == http.StatusForbidden {
				t.Errorf("holder %q: %s %s = %d, want the legacy holder's %d", holder, route.method, route.path, got, want)
			}
		}
	}
}

// The acceptance case of issue 2552, against a database
// (ELITEA_TEST_DATABASE_URL), so "passes the gate" is not mistaken for
// "works": a holder of only agents.governance.view reads the agent registry
// (200) and is refused the delegation review scan, the write route of the
// review under /admin (decisions on a grant are made on the project-plane
// grant routes).
func TestAgentGovernanceViewReadsTheRegistryAndCannotScan(t *testing.T) {
	router := newPlatformGateDatabaseRouter(t, adminaccess.AgentsGovernanceView)
	registry := servePlatformGate(t, router, http.MethodGet, "/api/v2/admin/agent_registry", "")
	if registry.Code != http.StatusOK {
		t.Errorf("agent registry = %d %s, want 200", registry.Code, registry.Body)
	}
	scan := servePlatformGate(t, router, http.MethodPost, "/api/v2/admin/delegation_review/scan", "")
	if scan.Code != http.StatusForbidden {
		t.Errorf("delegation review scan = %d, want 403", scan.Code)
	}
}
