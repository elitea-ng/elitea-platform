package adminaccess

// The built-in administration roles of ADR-0059 decision 3 (story R0.7).
//
// A role is built-in when elitea_auth.admin_role_seeds holds a row for it, not
// because of its name: shared 0262 writes the rows, and a custom role that
// already used a built-in name gets none. This file is the other half: the
// set the registry declares for each built-in role, which the grant-parity
// test (migrations/builtin_admin_roles_postgres_integration_test.go) holds
// the database to, and the posture check of story R2.14 compares against.

import "slices"

// The names of the built-in administration roles.
const (
	RoleSuperAdmin           = "super_admin"
	RoleAdmin                = "admin"
	RoleLLMManager           = "llm_manager"
	RoleProjectAdministrator = "project_administrator"
	RoleSecurityAuditor      = "security_auditor"
	RoleEmergencyAccess      = "emergency_access"
	RoleEditor               = "editor"
	RoleViewer               = "viewer"
	RoleSystem               = "system"
)

// SeedMarker is the `marker` column of elitea_auth.admin_role_seeds.
type SeedMarker string

// The three markers.
const (
	// MarkerDeclared: the registry declares the role's set.
	MarkerDeclared SeedMarker = "declared"
	// MarkerAsFound: the registry declares no set; the role is locked to the
	// set it held when shared 0262 ran.
	MarkerAsFound SeedMarker = "as_found"
	// MarkerUnassignable: locked and never assigned (the administration
	// `system` role of a legacy-backed database).
	MarkerUnassignable SeedMarker = "unassignable"
)

// BuiltInRole is one built-in administration role.
type BuiltInRole struct {
	// Name is the auth_core__role name, in the `administration` mode.
	Name string
	// DisplayName is what the console shows.
	DisplayName string
	// Marker is the seed row's marker.
	Marker SeedMarker
	// set is the declared set, nil when the registry declares none.
	set func() []string
}

// DeclaresSet reports whether the registry declares the role's set.
func (r BuiltInRole) DeclaresSet() bool { return r.set != nil }

// Set returns the declared set, sorted, or nil when none is declared.
func (r BuiltInRole) Set() []string {
	if r.set == nil {
		return nil
	}
	set := r.set()
	slices.Sort(set)
	return slices.Compact(set)
}

// The sets of the narrow roles, as decision 3 lists them.
var (
	llmManagerSet = []string{
		LLMCatalogueView, LLMCatalogueManage, LLMPolicyView, LLMPolicyManage,
		LLMUsageView, LLMAlertsManage,
		ModelsAdminProjectBudgetsView, ModelsAdminProjectBudgetsEdit,
		ProjectsProjectsView,
	}
	projectAdministratorSet = []string{
		ProjectsProjectsView, ProjectsProjectsEdit, ProjectsProjectCreate, ProjectsProjectDelete,
		ProjectsMembersManage, ProjectsSettingsView, ProjectsSettingsManage,
		ConfigurationUsersUsersView, ConfigurationRolesRolesView, ConfigurationRolesUserProjectPermsView,
		InvitesBulkUsers, InvitesBulkProjects,
		AdminModeration, AdminModerationEdit,
		ModelsAdminProjectBudgetsView,
	}
	// securityAuditorSet is the part of ADR-0059. ADR-0058 story E5.1 (#2403)
	// adds security.signals.view, security.signals.project.view and
	// runs.evidence.read to the registry and to this set when it merges.
	securityAuditorSet = []string{
		ModelsAdminAuditTrailView, AccessRolesView, AccessBreakglassReview,
	}
	emergencyAccessSet = []string{AccessBreakglassRequest}
)

// administratorExclusions are the names an administrator role does not hold,
// by role. Every other row of the permission table goes to both (the rule of
// shared 0261).
var administratorExclusions = map[string][]string{
	RoleSuperAdmin: nil,
	// admin.auth.users.super_admin makes a global administrator, and identity
	// trust is equivalent to it (#2558).
	RoleAdmin: {AdminAuthUsersSuperAdmin, IdentitySSOTrustManage},
}

// ungrantedByMigration are rows of the table that no migration grants to any
// role, so neither administrator role holds them. A deployment that wants one
// gives it to a custom role. configuration.service_descriptors gates the
// Service Descriptors section, which answers 501 on this platform
// (internal/api/v2/admin/roles.go, declaredPermissions).
var ungrantedByMigration = []string{ConfigurationServiceDescriptors}

func administratorSet(role string) func() []string {
	return func() []string {
		excluded := append(slices.Clone(administratorExclusions[role]), ungrantedByMigration...)
		var set []string
		for _, name := range Names() {
			if !slices.Contains(excluded, name) {
				set = append(set, name)
			}
		}
		return set
	}
}

func fixed(names []string) func() []string {
	return func() []string { return slices.Clone(names) }
}

// builtInRoles is in console order.
var builtInRoles = []BuiltInRole{
	{Name: RoleSuperAdmin, DisplayName: "Global administrator", Marker: MarkerDeclared,
		set: administratorSet(RoleSuperAdmin)},
	{Name: RoleAdmin, DisplayName: "Platform administrator", Marker: MarkerDeclared,
		set: administratorSet(RoleAdmin)},
	{Name: RoleLLMManager, DisplayName: "LLM manager", Marker: MarkerDeclared,
		set: fixed(llmManagerSet)},
	{Name: RoleProjectAdministrator, DisplayName: "Project administrator (all projects)", Marker: MarkerDeclared,
		set: fixed(projectAdministratorSet)},
	{Name: RoleSecurityAuditor, DisplayName: "Security auditor", Marker: MarkerDeclared,
		set: fixed(securityAuditorSet)},
	{Name: RoleEmergencyAccess, DisplayName: "Emergency access", Marker: MarkerDeclared,
		set: fixed(emergencyAccessSet)},
	// No declared set: locked to what they held at the upgrade.
	{Name: RoleEditor, DisplayName: "Editor", Marker: MarkerAsFound},
	{Name: RoleViewer, DisplayName: "Viewer", Marker: MarkerAsFound},
	// Legacy-backed databases only.
	{Name: RoleSystem, DisplayName: "System", Marker: MarkerUnassignable},
}

// BuiltInRoles returns the built-in administration roles, in console order.
func BuiltInRoles() []BuiltInRole { return slices.Clone(builtInRoles) }

// LookupBuiltInRole returns the built-in role of name.
func LookupBuiltInRole(name string) (BuiltInRole, bool) {
	for _, role := range builtInRoles {
		if role.Name == name {
			return role, true
		}
	}
	return BuiltInRole{}, false
}
