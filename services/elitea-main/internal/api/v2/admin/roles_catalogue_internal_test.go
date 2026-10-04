package admin

// The permission catalogue behind the admin Roles matrix.
//
// DEFECT. permissionCatalogue read the matrix rows back out of the grants:
// `SELECT permission FROM auth_core__role_permission UNION SELECT permission
// FROM auth_core__project_role_permission`. A permission that no role holds is
// then not a row, and parseMatrixBody rejects it with "unknown permission", so
// no API path can grant it. Two shapes follow.
//
//   - `configuration.service_descriptors` gates an admin Configuration section
//     (config_schemas.go, enforced by config_values.go). No migration seeds it,
//     so it was ungrantable from the first boot. `configuration.advanced` was
//     the second name in this pair until the Advanced section was removed; it
//     is no longer declared, because the section it gated is gone.
//   - Revocation was irreversible. `configuration.governance` is seeded by one
//     migration only (shared/0082). An operator who unchecks its last holder
//     deletes the row the catalogue came from, and the name can never be
//     granted again.
//
// EVIDENCE. Pylon does not have this failure: `auth.local_permissions` is a
// declaration registry, so both names are matrix rows there.

import (
	"encoding/json"
	"testing"
)

// TestDeclaredPermissionsHoldEveryConfigSectionGate keeps the declaration in
// step with the sections. A section that carries `required_permission` refuses
// itself to a caller who does not hold that name, so the name must be
// grantable.
func TestDeclaredPermissionsHoldEveryConfigSectionGate(t *testing.T) {
	declared := make(map[string]bool)
	for _, name := range declaredPermissions() {
		declared[name] = true
	}
	if len(declared) == 0 {
		t.Fatal("declaredPermissions() is empty; the section list or its key changed")
	}

	for _, section := range configSections() {
		permission, ok := section["required_permission"].(string)
		if !ok || permission == "" {
			continue
		}
		if !declared[permission] {
			t.Errorf("section %v gates on %q, which declaredPermissions() omits",
				section["id"], permission)
		}
	}
	if !declared["configuration.service_descriptors"] {
		t.Error(`declaredPermissions() omits "configuration.service_descriptors", which no migration grants`)
	}
	// The Advanced section is gone, so its gate must not be declared any more —
	// a declared name with no section behind it is a permission an operator can
	// grant and that nothing ever consults.
	if declared["configuration.advanced"] {
		t.Error(`declaredPermissions() still declares "configuration.advanced"; the Advanced section was removed`)
	}
}

// TestPermissionCatalogueKeepsDeclaredNamesWithoutAGrant is the read half: on a
// clean database no role holds either name, and the matrix must still show the
// row.
func TestPermissionCatalogueKeepsDeclaredNamesWithoutAGrant(t *testing.T) {
	catalogue := catalogueFrom(nil)
	present := make(map[string]bool, len(catalogue))
	for _, name := range catalogue {
		present[name] = true
	}
	if !present["configuration.service_descriptors"] {
		t.Error(`catalogue built from zero grants omits "configuration.service_descriptors"`)
	}
	// A deployment whose legacy roles still HOLD configuration.advanced keeps
	// the row: the catalogue is the union of granted and declared, so removing
	// the declaration invalidates no existing grant.
	granted := false
	for _, name := range catalogueFrom([]string{"configuration.advanced"}) {
		if name == "configuration.advanced" {
			granted = true
		}
	}
	if !granted {
		t.Error("a granted configuration.advanced disappeared from the catalogue")
	}

	sorted := mergePermissionCatalogue([]string{"b.two", "a.one", "b.two"}, []string{"a.one", "c.three"})
	want := []string{"a.one", "b.two", "c.three"}
	if len(sorted) != len(want) {
		t.Fatalf("merged catalogue = %v, want %v", sorted, want)
	}
	for index, name := range want {
		if sorted[index] != name {
			t.Fatalf("merged catalogue = %v, want %v", sorted, want)
		}
	}
}

// TestMatrixSaveAcceptsADeclaredPermissionNobodyHolds is the write half. It is
// the failure the operator meets: the PUT that grants the permission was
// refused 400 because the catalogue did not carry the name.
func TestMatrixSaveAcceptsADeclaredPermissionNobodyHolds(t *testing.T) {
	current := permissionMatrix{
		roles:     []string{"admin"},
		catalogue: catalogueFrom(nil),
	}
	body := []map[string]json.RawMessage{{
		"name":  json.RawMessage(`"configuration.service_descriptors"`),
		"admin": json.RawMessage(`true`),
	}}

	submission, err := parseMatrixBody(body, current)
	if err != nil {
		t.Fatalf("granting a declared permission was refused: %v", err)
	}
	if !submission.rows["configuration.service_descriptors"] {
		t.Fatal("the submitted row was dropped")
	}
	if !submission.desired[grant{role: "admin", permission: "configuration.service_descriptors"}] {
		t.Fatalf("the grant was not recorded: %+v", submission.desired)
	}
}

// TestPermissionCatalogueHidesRetiredNames covers #6874's recurrence guard. A
// retired string can still sit in a row: pylon re-seeds some on every start.
// The matrix must not show it, and a save that names it must fail, so the
// legacy group toggle can no longer write a prefix node back as a row.
func TestPermissionCatalogueHidesRetiredNames(t *testing.T) {
	granted := append([]string{"projects.projects.projects.view"}, RetiredPermissions()...)
	catalogue := catalogueFrom(granted)
	present := make(map[string]bool, len(catalogue))
	for _, name := range catalogue {
		present[name] = true
	}
	for _, name := range RetiredPermissions() {
		if present[name] {
			t.Errorf("retired permission %q is still in the catalogue", name)
		}
	}
	// The exact-match child of a retired prefix is a real permission.
	if !present["projects.projects.projects.view"] {
		t.Error("a live child of a retired prefix disappeared from the catalogue")
	}
	// No declared section gate may be retired: that would hide a real gate.
	for _, name := range declaredPermissions() {
		if _, retired := retiredPermissions[name]; retired {
			t.Errorf("declared permission %q is also retired", name)
		}
	}

	current := permissionMatrix{roles: []string{"admin"}, catalogue: catalogue}
	body := []map[string]json.RawMessage{{
		"name":  json.RawMessage(`"admin"`),
		"admin": json.RawMessage(`true`),
	}}
	if _, err := parseMatrixBody(body, current); err == nil {
		t.Fatal("a save that names the retired prefix node \"admin\" was accepted")
	}
}
