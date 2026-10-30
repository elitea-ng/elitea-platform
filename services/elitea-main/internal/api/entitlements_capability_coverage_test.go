package api_test

// Every permission is either an exported governance capability or named as
// not governance-relevant (ADR-0044 decision 5, issue 1501).
//
// The entitlement export maps PERMISSIONS to capability labels in one table,
// internal/domain/entitlements.Capabilities. A permission missing from that
// table is a grant the export never reports, and an access review then
// certifies a role without seeing what it allows. So a new permission must
// be classified when it is introduced, and this test is where that is
// forced: it reads the same two corpora as router_permission_grant_gate_test.go
// — every permission a route gates on (collectGates) and every permission a
// shared migration grants (grantsByMode, which also catches a permission
// checked inline rather than by a gate middleware) — and fails for any
// permission that is in neither Capabilities nor NotGovernanceRelevant.

import (
	"sort"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
)

func TestEveryPermissionIsClassifiedForTheEntitlementExport(t *testing.T) {
	t.Parallel()

	root := repoRootFrom(t)
	seen := map[string]bool{}
	for _, g := range collectGates(t, root) {
		for _, permission := range g.permissions {
			seen[permission] = true
		}
	}
	gated := len(seen)
	for _, permissions := range grantsByMode(t, root) {
		for permission := range permissions {
			seen[permission] = true
		}
	}
	// Premise guards: a corpus that reads as empty must not pass.
	if gated < 100 || len(seen) < gated {
		t.Fatalf("read %d gated and %d total permissions; the corpora were not found", gated, len(seen))
	}
	for _, required := range []string{
		entitlements.ExportPermission, "models.applications.governance", "lifecycle.attestations.write",
		"safety.suspend", "retention.manage", "legal_hold.manage", "runs.evidence.read",
	} {
		if !seen[required] {
			t.Errorf("%s is neither gated nor granted any more; the export maps it, so check the rename", required)
		}
	}

	var unclassified []string
	for permission := range seen {
		if len(entitlements.Capabilities[permission]) == 0 && !entitlements.NotGovernanceRelevant[permission] {
			unclassified = append(unclassified, permission)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("these permissions are gated or granted but not classified for the entitlement export:\n  %s\n"+
			"Add each to internal/domain/entitlements.Capabilities with the governance capability it carries,\n"+
			"or to NotGovernanceRelevant when an access review does not need to see it.",
			strings.Join(unclassified, "\n  "))
	}
}

// A permission is classified once: in both tables, the export and the
// exemption disagree about it.
func TestNoPermissionIsBothMappedAndExempt(t *testing.T) {
	t.Parallel()
	for permission := range entitlements.NotGovernanceRelevant {
		if _, mapped := entitlements.Capabilities[permission]; mapped {
			t.Errorf("%s is both mapped to a capability and marked not governance-relevant", permission)
		}
	}
	for permission, capabilities := range entitlements.Capabilities {
		if len(capabilities) == 0 {
			t.Errorf("%s maps to no capability; list it in NotGovernanceRelevant instead", permission)
		}
	}
	if len(entitlements.Capabilities[entitlements.ConnectorApprovePermission]) == 0 {
		t.Error("connector.approve must stay mapped to approve_connectors (ADR-0044 decision 1)")
	}
}
