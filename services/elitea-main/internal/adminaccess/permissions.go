package adminaccess

// The administration-mode permissions elitea-main enforces. Every gate that
// resolves the `administration` mode names one of these constants; a string
// literal there fails internal/api/admin_permission_literal_test.go.
//
// The 27 narrow names of ADR-0059 decision 2 are not here yet. Each enters the
// table in the story whose migration grants it (R0.2, R0.15), so the grant
// gate stays green at every commit.
const (
	// People and access.
	AdminAuthUsers                         = "admin.auth.users"
	AdminAuthUsersSuperAdmin               = "admin.auth.users.super_admin"
	ModesUsers                             = "modes.users"
	InvitesBulkUsers                       = "invites.bulkusers"
	InvitesBulkProjects                    = "invites.bulkprojects"
	ConfigurationRolesPermissionsView      = "configuration.roles.permissions.view"
	ConfigurationRolesPermissionsEdit      = "configuration.roles.permissions.edit"
	ConfigurationRolesRolesView            = "configuration.roles.roles.view"
	ConfigurationRolesRolesCreate          = "configuration.roles.roles.create"
	ConfigurationRolesRolesEdit            = "configuration.roles.roles.edit"
	ConfigurationRolesRolesDelete          = "configuration.roles.roles.delete"
	ConfigurationRolesUserProjectPermsView = "configuration.roles.user_project_permissions.view"
	ConfigurationRolesUserProjectPermsEdit = "configuration.roles.user_project_permissions.edit"

	// Projects.
	ProjectsProjectsView          = "projects.projects.projects.view"
	ProjectsProjectsEdit          = "projects.projects.projects.edit"
	ProjectsProjectCreate         = "projects.projects.project.create"
	ProjectsProjectDelete         = "projects.projects.project.delete"
	ConfigurationUsersUsersView   = "configuration.users.users.view"
	ConfigurationUsersUsersCreate = "configuration.users.users.create"
	ConfigurationUsersUsersEdit   = "configuration.users.users.edit"
	AdminModeration               = "admin.moderation"
	AdminModerationEdit           = "admin.moderation.edit"
	RuntimeAdminPublishedAgents   = "runtime.admin.published_agents"

	// Models and spend.
	ModelsAdminProjectBudgetsView = "models.admin.project_budgets.view"
	ModelsAdminProjectBudgetsEdit = "models.admin.project_budgets.edit"

	// Agents and governance.
	ConfigurationGovernance      = "configuration.governance"
	ConnectorApprove             = "connector.approve"
	CredentialsInventoryRead     = "credentials.inventory.read"
	CredentialsInventoryManage   = "credentials.inventory.manage"
	AgentIdentityBindingsManage  = "models.agent_identity.bindings.manage"
	AgentIdentityBindingsApprove = "models.agent_identity.bindings.approve"
	AgentIdentityMandatesManage  = "models.agent_identity.mandates.manage"
	AgentIdentityMandatesApprove = "models.agent_identity.mandates.approve"
	AgentIdentityExternalManage  = "models.agent_identity.external.manage"
	SafetySuspend                = "safety.suspend"
	ToolkitCatalogueTypeManage   = "toolkit_catalogue.type.manage"

	// Security.
	ModelsAdminAuditTrailView = "models.admin.audit_trail.view"
	EntitlementsExport        = "entitlements.export"
	RetentionManage           = "retention.manage"
	LegalHoldManage           = "legal_hold.manage"

	// Platform.
	RuntimePlugins                       = "runtime.plugins"
	ConfigurationBranding                = "configuration.branding"
	ConfigurationNativeClients           = "configuration.native_clients"
	ConfigurationMicrosoftApps           = "configuration.microsoft_apps"
	ConfigurationServiceDescriptors      = "configuration.service_descriptors"
	ConfigurationSchedulingSchedulesView = "configuration.scheduling.schedules.view"
	ConfigurationSchedulingSchedulesEdit = "configuration.scheduling.schedules.edit"
	ConfigurationSecretsSecretView       = "configuration.secrets.secret.view"
	ConfigurationSecretsSecretCreate     = "configuration.secrets.secret.create"
	ConfigurationSecretsSecretEdit       = "configuration.secrets.secret.edit"
	ConfigurationSecretsSecretDelete     = "configuration.secrets.secret.delete"
	RuntimeAirunServiceProviders         = "runtime.airun.serviceproviders"
	ProviderHubDescriptorRegister        = "provider_hub.descriptor.register"
	ProviderHubDescriptorActivate        = "provider_hub.descriptor.activate"
)

// row is a row with the defaults of an existing name: grantable from the
// matrix, allowed as a token scope, not legacy, no narrower parent, not kept
// by a native session and no maintenance bypass.
func row(name string, area Area, class Class) Permission {
	return Permission{Name: name, Area: area, Class: class, Grantable: GrantableAny, Token: true}
}

// rows is the permission table. Every row other than the three set apart below
// takes the defaults of row.
func rows() []Permission {
	superAdmin := row(AdminAuthUsersSuperAdmin, AreaPeopleAccess, ClassWrite)
	// The global administrator's own permission. Shared 0060 grants it to
	// super_admin and rolepolicy refuses it to every other role, so only a
	// migration grants it. It gates identity trust (#2558), which is never
	// a token scope (ADR-0059 decision 7).
	superAdmin.Grantable = GrantableSeededOnly
	superAdmin.Token = false

	runtimePlugins := row(RuntimePlugins, AreaPlatform, ClassWrite)
	// The maintenance bypass (middleware/maintenance.go and the
	// platform_settings flag read). A native session keeps it, and today it
	// is the only permission that admits its holder during a window.
	runtimePlugins.NativeClient = true
	runtimePlugins.Maintenance = true

	publishedAgents := row(RuntimeAdminPublishedAgents, AreaProjects, ClassRead)
	// Marketplace moderation (eliteacore/marketplace_read.go), the second
	// in-application check a native session keeps.
	publishedAgents.NativeClient = true

	return []Permission{
		// People and access.
		row(AdminAuthUsers, AreaPeopleAccess, ClassWrite),
		superAdmin,
		row(ModesUsers, AreaPeopleAccess, ClassWrite),
		row(InvitesBulkUsers, AreaPeopleAccess, ClassWrite),
		row(InvitesBulkProjects, AreaPeopleAccess, ClassWrite),
		row(ConfigurationRolesPermissionsView, AreaPeopleAccess, ClassRead),
		row(ConfigurationRolesPermissionsEdit, AreaPeopleAccess, ClassWrite),
		row(ConfigurationRolesRolesView, AreaPeopleAccess, ClassRead),
		row(ConfigurationRolesRolesCreate, AreaPeopleAccess, ClassWrite),
		row(ConfigurationRolesRolesEdit, AreaPeopleAccess, ClassWrite),
		row(ConfigurationRolesRolesDelete, AreaPeopleAccess, ClassWrite),
		row(ConfigurationRolesUserProjectPermsView, AreaPeopleAccess, ClassRead),
		row(ConfigurationRolesUserProjectPermsEdit, AreaPeopleAccess, ClassWrite),

		// Projects.
		row(ProjectsProjectsView, AreaProjects, ClassRead),
		row(ProjectsProjectsEdit, AreaProjects, ClassWrite),
		row(ProjectsProjectCreate, AreaProjects, ClassWrite),
		row(ProjectsProjectDelete, AreaProjects, ClassWrite),
		row(ConfigurationUsersUsersView, AreaProjects, ClassRead),
		row(ConfigurationUsersUsersCreate, AreaProjects, ClassWrite),
		row(ConfigurationUsersUsersEdit, AreaProjects, ClassWrite),
		row(AdminModeration, AreaProjects, ClassRead),
		row(AdminModerationEdit, AreaProjects, ClassWrite),
		publishedAgents,

		// Models and spend.
		row(ModelsAdminProjectBudgetsView, AreaModelsSpend, ClassRead),
		row(ModelsAdminProjectBudgetsEdit, AreaModelsSpend, ClassWrite),

		// Agents and governance.
		row(ConfigurationGovernance, AreaAgentsGovernance, ClassWrite),
		row(ConnectorApprove, AreaAgentsGovernance, ClassApprove),
		row(CredentialsInventoryRead, AreaAgentsGovernance, ClassRead),
		row(CredentialsInventoryManage, AreaAgentsGovernance, ClassWrite),
		row(AgentIdentityBindingsManage, AreaAgentsGovernance, ClassWrite),
		row(AgentIdentityBindingsApprove, AreaAgentsGovernance, ClassApprove),
		row(AgentIdentityMandatesManage, AreaAgentsGovernance, ClassWrite),
		row(AgentIdentityMandatesApprove, AreaAgentsGovernance, ClassApprove),
		row(AgentIdentityExternalManage, AreaAgentsGovernance, ClassWrite),
		row(SafetySuspend, AreaAgentsGovernance, ClassWrite),
		row(ToolkitCatalogueTypeManage, AreaAgentsGovernance, ClassWrite),

		// Security.
		row(ModelsAdminAuditTrailView, AreaSecurity, ClassRead),
		row(EntitlementsExport, AreaSecurity, ClassRead),
		row(RetentionManage, AreaSecurity, ClassWrite),
		row(LegalHoldManage, AreaSecurity, ClassWrite),

		// Platform.
		runtimePlugins,
		row(ConfigurationBranding, AreaPlatform, ClassWrite),
		row(ConfigurationNativeClients, AreaPlatform, ClassWrite),
		row(ConfigurationMicrosoftApps, AreaPlatform, ClassWrite),
		row(ConfigurationServiceDescriptors, AreaPlatform, ClassWrite),
		row(ConfigurationSchedulingSchedulesView, AreaPlatform, ClassRead),
		row(ConfigurationSchedulingSchedulesEdit, AreaPlatform, ClassWrite),
		row(ConfigurationSecretsSecretView, AreaPlatform, ClassRead),
		row(ConfigurationSecretsSecretCreate, AreaPlatform, ClassWrite),
		row(ConfigurationSecretsSecretEdit, AreaPlatform, ClassWrite),
		row(ConfigurationSecretsSecretDelete, AreaPlatform, ClassWrite),
		row(RuntimeAirunServiceProviders, AreaPlatform, ClassRead),
		row(ProviderHubDescriptorRegister, AreaPlatform, ClassWrite),
		row(ProviderHubDescriptorActivate, AreaPlatform, ClassWrite),
	}
}
