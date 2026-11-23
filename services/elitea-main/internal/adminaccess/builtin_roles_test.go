package adminaccess

import (
	"slices"
	"testing"
)

// The declared sets name rows of the table, the marker says whether a set is
// declared, and the top permissions stay on the top role.
func TestBuiltInRolesAreConsistentWithTheTable(t *testing.T) {
	seen := map[string]bool{}
	for _, role := range BuiltInRoles() {
		if seen[role.Name] {
			t.Errorf("%s appears twice", role.Name)
		}
		seen[role.Name] = true
		if role.DisplayName == "" {
			t.Errorf("%s has no display name", role.Name)
		}
		if role.DeclaresSet() != (role.Marker == MarkerDeclared) {
			t.Errorf("%s: declares a set = %v, marker %q", role.Name, role.DeclaresSet(), role.Marker)
		}
		for _, name := range role.Set() {
			if _, ok := Lookup(name); !ok {
				t.Errorf("%s declares %q, which is not a row of the table", role.Name, name)
			}
		}
		if role.Name == RoleSuperAdmin {
			continue
		}
		for _, top := range []string{AdminAuthUsersSuperAdmin, IdentitySSOTrustManage} {
			if slices.Contains(role.Set(), top) {
				t.Errorf("%s declares %s, which only super_admin holds", role.Name, top)
			}
		}
	}
	for _, want := range []string{RoleSuperAdmin, RoleAdmin, RoleLLMManager, RoleProjectAdministrator,
		RoleSecurityAuditor, RoleEmergencyAccess, RoleEditor, RoleViewer, RoleSystem} {
		if !seen[want] {
			t.Errorf("%s is not a built-in role", want)
		}
	}
}

// The sizes decision 3 gives, so an edit to a set is deliberate.
func TestNarrowBuiltInRoleSetSizes(t *testing.T) {
	for name, want := range map[string]int{
		RoleLLMManager: 9, RoleProjectAdministrator: 15, RoleSecurityAuditor: 3, RoleEmergencyAccess: 1,
	} {
		role, ok := LookupBuiltInRole(name)
		if !ok {
			t.Fatalf("%s is not a built-in role", name)
		}
		if got := len(role.Set()); got != want {
			t.Errorf("%s declares %d permissions, want %d", name, got, want)
		}
	}
	if _, ok := LookupBuiltInRole("break_glass"); ok {
		t.Error("break_glass is a role name ADR-0059 reserves, not a built-in role")
	}
}

// A caller cannot write the registry through a returned set.
func TestBuiltInRoleSetIsACopy(t *testing.T) {
	role, _ := LookupBuiltInRole(RoleEmergencyAccess)
	role.Set()[0] = "changed"
	if again, _ := LookupBuiltInRole(RoleEmergencyAccess); again.Set()[0] != AccessBreakglassRequest {
		t.Fatal("writing a returned set changed the registry")
	}
}
