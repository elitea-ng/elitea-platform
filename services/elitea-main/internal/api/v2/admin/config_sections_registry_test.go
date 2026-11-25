package admin

// Every Configuration section declares its console area and its narrow pair
// (ADR-0059 R0.5), and every name it declares is a row of the permission
// table of internal/adminaccess.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// pointerReadHalves are sections whose read half is the permission of the
// page the section points at rather than a name that narrows the legacy
// path, each with the reason.
var pointerReadHalves = map[string]string{
	"service_descriptors": "a fieldless pointer to /admin/app/service-descriptors, whose listing is gated on " +
		"runtime.airun.serviceproviders; configuration.service_descriptors is granted by no migration, so " +
		"without this every administrator would lose the pointer from the filtered list",
	"llm_proxy": "a fieldless pointer to the gateway editor, whose routes open on llm.catalogue.view and the " +
		"other llm.* names; with platform.config.view an LLM manager would never see the editor (R0.16)",
}

// sectionAccessProblems answers what is wrong with one section's
// declaration, or nothing.
func sectionAccessProblems(section configSection) []string {
	var problems []string
	access := section.access
	if !slices.Contains(adminaccess.Areas(), access.Area) {
		problems = append(problems, fmt.Sprintf("declares no console area (got %q)", access.Area))
	}
	if access.LegacyOnly {
		if access.Read != "" || access.Write != "" {
			problems = append(problems, "is legacy-only but names a narrow pair")
		}
		if section.legacyPermission == "" {
			problems = append(problems, "is legacy-only with no required_permission: runtime.plugins alone "+
				"would be its only path, so give it platform.config.view and .manage")
		}
		return problems
	}
	if access.Read == "" || access.Write == "" {
		return append(problems, fmt.Sprintf("declares no permission pair (read %q, write %q)", access.Read, access.Write))
	}
	for half, name := range map[string]string{"read": access.Read, "write": access.Write} {
		row, ok := adminaccess.Lookup(name)
		if !ok {
			problems = append(problems, fmt.Sprintf("names %s permission %q, which the registry does not have", half, name))
			continue
		}
		// The narrow name must narrow what the legacy path asks for, or be
		// the section's own single permission: no reach a legacy holder does
		// not already have opens on the narrow path by surprise.
		pointer := half == "read" && pointerReadHalves[section.id] != "" && len(section.fields) == 0
		if name != section.legacyPermission && !pointer {
			for _, legacy := range []string{adminaccess.RuntimePlugins, section.legacyPermission} {
				if legacy != "" && !slices.Contains(row.Narrows, legacy) {
					problems = append(problems, fmt.Sprintf("%s permission %q does not narrow %q", half, name, legacy))
				}
			}
		}
	}
	if row, ok := adminaccess.Lookup(access.Write); ok && row.Class != adminaccess.ClassWrite {
		problems = append(problems, fmt.Sprintf("write permission %q is of class %q", access.Write, row.Class))
	}
	if row, ok := adminaccess.Lookup(access.Read); ok && access.Read != access.Write && row.Class != adminaccess.ClassRead {
		problems = append(problems, fmt.Sprintf("read permission %q is of class %q", access.Read, row.Class))
	}
	return problems
}

func TestEverySectionDeclaresAnAreaAndARegisteredPair(t *testing.T) {
	t.Parallel()

	t.Run("fixtures", func(t *testing.T) {
		t.Parallel()
		for name, tc := range map[string]struct {
			section configSection
			want    string
		}{
			"no area": {
				configSection{access: sectionAccess{Read: adminaccess.PlatformConfigView, Write: adminaccess.PlatformConfigManage}},
				"declares no console area",
			},
			"no pair": {
				configSection{access: sectionAccess{Area: adminaccess.AreaPlatform}},
				"declares no permission pair",
			},
			"half a pair": {
				configSection{access: sectionAccess{Area: adminaccess.AreaPlatform, Read: adminaccess.PlatformConfigView}},
				"declares no permission pair",
			},
			"unregistered": {
				configSection{access: narrowAccess(adminaccess.AreaPlatform, adminaccess.PlatformConfigView, "fixture.not.registered")},
				`names write permission "fixture.not.registered", which the registry does not have`,
			},
			"does not narrow the legacy name": {
				configSection{legacyPermission: adminaccess.ConfigurationGovernance, access: platformConfigAccess(adminaccess.AreaPlatform)},
				`does not narrow "configuration.governance"`,
			},
			"read name as the write half": {
				configSection{access: narrowAccess(adminaccess.AreaPlatform, adminaccess.PlatformConfigView, adminaccess.PlatformConfigView)},
				"is of class",
			},
			"legacy-only without a legacy name": {
				configSection{access: legacyOnlyAccess(adminaccess.AreaPlatform)},
				"is legacy-only with no required_permission",
			},
		} {
			problems := sectionAccessProblems(tc.section)
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("%s: got %q, want a problem containing %q", name, problems, tc.want)
			}
		}
	})

	t.Run("sections", func(t *testing.T) {
		t.Parallel()
		sections := configurationSections()
		if len(sections) < 40 {
			t.Fatalf("read %d sections; the list was not found", len(sections))
		}
		for _, section := range sections {
			for _, problem := range sectionAccessProblems(section) {
				t.Errorf("section %s %s", section.id, problem)
			}
			if section.legacyPermission != "" {
				if _, ok := adminaccess.Lookup(section.legacyPermission); !ok {
					t.Errorf("section %s requires %q, which the registry does not have", section.id, section.legacyPermission)
				}
			}
		}
	})
}

// The route gate admits runtime.plugins and every narrow name a section
// declares, and nothing else: configuration.governance alone stays refused at
// the route, as before.
func TestTheRouteGateIsComputedFromTheSections(t *testing.T) {
	t.Parallel()
	admitted := configurationRoutePermissions()
	want := []string{adminaccess.RuntimePlugins}
	for _, section := range configurationSections() {
		if !section.access.LegacyOnly {
			want = append(want, section.access.Read, section.access.Write)
		}
	}
	for _, name := range want {
		if !slices.Contains(admitted, name) {
			t.Errorf("the route gate does not admit %q", name)
		}
	}
	for _, name := range admitted {
		if !slices.Contains(want, name) {
			t.Errorf("the route gate admits %q, which no section declares", name)
		}
	}
	if slices.Contains(admitted, "") {
		t.Error("the route gate admits the empty name")
	}
	// A section that forgot its `access`, or declared half of it, has no
	// narrow path: it falls back to the legacy one.
	for _, incomplete := range []sectionAccess{
		{},
		{Read: adminaccess.PlatformConfigView, Write: adminaccess.PlatformConfigManage},
		{Area: adminaccess.AreaPlatform, Read: adminaccess.PlatformConfigView},
	} {
		if got := incomplete.narrowPermissions(false); got != nil {
			t.Errorf("%+v admits %q on the narrow path, want nothing", incomplete, got)
		}
	}
	// entitlements.export is admitted since #3026: it is the read half of the
	// entitlement_export section, whose save takes entitlements.export.manage.
	for _, refused := range []string{adminaccess.ConfigurationGovernance} {
		if slices.Contains(admitted, refused) {
			t.Errorf("the route gate admits %q; its holder alone was refused at the route before", refused)
		}
	}
}

// The mapping of ADR-0059 decision 2, section by section where the decision
// names one.
func TestSectionPairsFollowDecisionTwo(t *testing.T) {
	t.Parallel()
	agentGovernance := 0
	want := map[string][2]string{
		"guardrails":      {adminaccess.LLMPolicyView, adminaccess.LLMGuardrailsManage},
		"circuit_breaker": {adminaccess.LLMPolicyView, adminaccess.LLMPolicyManage},
		"email":           {adminaccess.PlatformEmailManage, adminaccess.PlatformEmailManage},
		"maintenance":     {adminaccess.PlatformMaintenanceManage, adminaccess.PlatformMaintenanceManage},
		"auth":            {adminaccess.IdentitySSOView, adminaccess.IdentitySSOSettingsManage},
		// A write permission saves it, not the export read (#3026).
		"entitlement_export": {adminaccess.EntitlementsExport, adminaccess.EntitlementsExportManage},
		"analytics":          {adminaccess.PlatformConfigView, adminaccess.PlatformConfigManage},
		// The read half is the pointer's page (pointerReadHalves).
		"llm_proxy":      {adminaccess.LLMCatalogueView, adminaccess.PlatformConfigManage},
		"branding":       {adminaccess.ConfigurationBranding, adminaccess.ConfigurationBranding},
		"native_clients": {adminaccess.ConfigurationNativeClients, adminaccess.ConfigurationNativeClients},
		"microsoft_apps": {adminaccess.ConfigurationMicrosoftApps, adminaccess.ConfigurationMicrosoftApps},
		// The read half is the pointer's page (pointerReadHalves).
		"service_descriptors": {adminaccess.RuntimeAirunServiceProviders, adminaccess.ConfigurationServiceDescriptors},
	}
	for _, section := range configurationSections() {
		if pair, ok := want[section.id]; ok {
			if got := [2]string{section.access.Read, section.access.Write}; got != pair {
				t.Errorf("section %s declares %v, want %v", section.id, got, pair)
			}
			delete(want, section.id)
		}
		// The agent governance sections declare the pair of R0.15.
		if section.legacyPermission == adminaccess.ConfigurationGovernance &&
			section.access.Area == adminaccess.AreaAgentsGovernance {
			got := [2]string{section.access.Read, section.access.Write}
			if want := [2]string{adminaccess.AgentsGovernanceView, adminaccess.AgentsGovernanceManage}; got != want {
				t.Errorf("agent governance section %s declares %v, want %v (R0.15)", section.id, got, want)
			}
			agentGovernance++
		}
	}
	for id := range want {
		t.Errorf("section %s is not in the list", id)
	}
	if agentGovernance != 15 {
		t.Errorf("%d agent governance sections declare the pair, want the 15 of R0.15", agentGovernance)
	}
}

// The schema list serves the declaration as `access`, beside the
// `required_permission` the web still reads.
func TestTheSchemaListServesTheAccessDeclaration(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(configSections())
	if err != nil {
		t.Fatal(err)
	}
	var sections []struct {
		ID                 string         `json:"id"`
		RequiredPermission string         `json:"required_permission"`
		Access             map[string]any `json:"access"`
	}
	if err := json.Unmarshal(encoded, &sections); err != nil {
		t.Fatal(err)
	}
	for _, section := range sections {
		if section.ID != "guardrails" && section.ID != "maintenance" {
			continue
		}
		if section.Access["area"] == nil || section.Access["write_permission"] == nil {
			t.Errorf("section %s serves access %v", section.ID, section.Access)
		}
		if section.ID == "maintenance" && section.RequiredPermission != adminaccess.RuntimePlugins {
			t.Errorf("maintenance serves required_permission %q", section.RequiredPermission)
		}
	}
}
