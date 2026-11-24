package adminaccess

// The administration-mode permissions elitea-main enforces. Every gate that
// resolves the `administration` mode names one of these constants; a string
// literal there fails internal/api/admin_permission_literal_test.go.
//
// The narrow names of ADR-0059 decision 2 enter the table in the story whose
// migration grants them, so the grant gate stays green at every commit: 25 in
// R0.2 (shared 0261), the agents.governance.* pair in R0.15.
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
	// ConfigurationUsersUsersDelete gates the central member removal route
	// beside projects.members.manage (R0.6a). Shared 0076 grants it in the
	// default mode only, so no administration role holds it from a migration
	// (ungrantedByMigration); a deployment gives it to a custom role.
	ConfigurationUsersUsersDelete = "configuration.users.users.delete"
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

// The narrow permissions of ADR-0059 decision 2 that shared 0261 grants to
// super_admin and admin (identity.sso.trust.manage to super_admin alone).
// Stories R0.3 to R0.6 wire each beside the legacy name it narrows: R0.4a
// wired platform.email.manage, platform.operations.view and .manage, and
// mcp.catalogue.manage, each accepted beside runtime.plugins; R0.3 wired the
// seven llm.* names, each accepted beside configuration.governance on the
// /admin/gateway routes.
const (
	// Models and spend.
	LLMCatalogueView    = "llm.catalogue.view"
	LLMCatalogueManage  = "llm.catalogue.manage"
	LLMPolicyView       = "llm.policy.view"
	LLMPolicyManage     = "llm.policy.manage"
	LLMGuardrailsManage = "llm.guardrails.manage"
	LLMUsageView        = "llm.usage.view"
	LLMAlertsManage     = "llm.alerts.manage"

	// Platform.
	PlatformConfigView        = "platform.config.view"
	PlatformConfigManage      = "platform.config.manage"
	PlatformEmailManage       = "platform.email.manage"
	PlatformMaintenanceManage = "platform.maintenance.manage"
	PlatformOperationsView    = "platform.operations.view"
	PlatformOperationsManage  = "platform.operations.manage"
	MCPCatalogueManage        = "mcp.catalogue.manage"

	// People and access: identity.
	IdentitySSOView           = "identity.sso.view"
	IdentitySSOSettingsManage = "identity.sso.settings.manage"
	IdentitySSOTrustManage    = "identity.sso.trust.manage"

	// Projects.
	ProjectsMembersManage  = "projects.members.manage"
	ProjectsSettingsView   = "projects.settings.view"
	ProjectsSettingsManage = "projects.settings.manage"

	// People and access: roles and break-glass.
	AccessRolesView         = "access.roles.view"
	AccessBreakglassRequest = "access.breakglass.request"
	AccessBreakglassApprove = "access.breakglass.approve"
	AccessBreakglassReview  = "access.breakglass.review"
	AccessBreakglassManage  = "access.breakglass.manage"
)

// row is a row with the defaults of an existing name: grantable from the
// matrix, not legacy, no narrower parent, not kept by a native session, no
// maintenance bypass, and NOT a token scope.
//
// Token defaults to false so that a name becomes a token scope only by a
// deliberate opt-in. Nothing reads the field yet: the scoped token (ADR-0059
// decision 7, stories R1.1 and R1.2) opts rows in when it ships, and a row it
// does not name stays refused, which is the safe failure.
func row(name string, area Area, class Class) Permission {
	return Permission{Name: name, Area: area, Class: class, Grantable: GrantableAny}
}

// rows is the permission table. Every row other than the four set apart below
// takes the defaults of row.
func rows() []Permission {
	superAdmin := row(AdminAuthUsersSuperAdmin, AreaPeopleAccess, ClassWrite)
	// The global administrator's own permission. Shared 0060 grants it to
	// super_admin and the role policy refuses it to every other role, so only a
	// migration grants it. It gates identity trust (#2558), which is never
	// a token scope (ADR-0059 decision 7).
	superAdmin.Grantable = GrantableSeededOnly

	runtimePlugins := row(RuntimePlugins, AreaPlatform, ClassWrite)
	// The maintenance bypass (middleware/maintenance.go and the
	// platform_settings flag read), with its narrow name
	// platform.maintenance.manage. A native session keeps it.
	runtimePlugins.NativeClient = true
	runtimePlugins.Maintenance = true
	// Legacy: it ends on the sunset date (ADR-0059 decision 2). The Roles
	// matrix adds it to no role that does not already hold it, and the gates
	// that name it may only shrink (story R0.14a).
	runtimePlugins.Legacy = true

	governance := row(ConfigurationGovernance, AreaAgentsGovernance, ClassWrite)
	// Legacy, like runtime.plugins: the llm.* names and, from R0.15,
	// agents.governance.* replace it.
	governance.Legacy = true

	publishedAgents := row(RuntimeAdminPublishedAgents, AreaProjects, ClassRead)
	// Marketplace moderation (eliteacore/marketplace_read.go), the second
	// in-application check a native session keeps.
	publishedAgents.NativeClient = true

	return append([]Permission{
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
		row(ConfigurationUsersUsersDelete, AreaProjects, ClassWrite),
		row(AdminModeration, AreaProjects, ClassRead),
		row(AdminModerationEdit, AreaProjects, ClassWrite),
		publishedAgents,

		// Models and spend.
		row(ModelsAdminProjectBudgetsView, AreaModelsSpend, ClassRead),
		row(ModelsAdminProjectBudgetsEdit, AreaModelsSpend, ClassWrite),

		// Agents and governance.
		governance,
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
	}, narrowRows()...)
}

// narrow is a row of decision 2: the defaults of row, plus the legacy names
// it narrows (none for a new capability), as decision 2 lists them.
func narrow(name string, area Area, class Class, narrows ...string) Permission {
	p := row(name, area, class)
	p.Narrows = narrows
	return p
}

// seeded is a narrow row that only a migration grants.
func seeded(name string, area Area, class Class, narrows ...string) Permission {
	p := narrow(name, area, class, narrows...)
	p.Grantable = GrantableSeededOnly
	return p
}

// narrowRows are the 25 rows of shared 0261. None is a token scope (Token
// false): the scoped token of R1.1 and R1.2 opts rows in, and identity trust
// never is one. platform.maintenance.manage is the narrow name of the
// maintenance bypass: since R0.4b the bypass accepts it, so it carries
// Maintenance, and a native session keeps it (NativeClient) as it keeps
// runtime.plugins.
func narrowRows() []Permission {
	maintenance := narrow(PlatformMaintenanceManage, AreaPlatform, ClassWrite, RuntimePlugins)
	maintenance.NativeClient = true
	maintenance.Maintenance = true
	return []Permission{
		// Models and spend.
		narrow(LLMCatalogueView, AreaModelsSpend, ClassRead, ConfigurationGovernance),
		narrow(LLMCatalogueManage, AreaModelsSpend, ClassWrite, ConfigurationGovernance),
		narrow(LLMPolicyView, AreaModelsSpend, ClassRead, ConfigurationGovernance, RuntimePlugins),
		narrow(LLMPolicyManage, AreaModelsSpend, ClassWrite, ConfigurationGovernance, RuntimePlugins),
		// Guardrails are a safety control (ADR-0041), not a cost setting, but
		// they are gateway policy and live in the same area.
		narrow(LLMGuardrailsManage, AreaModelsSpend, ClassWrite, ConfigurationGovernance, RuntimePlugins),
		narrow(LLMUsageView, AreaModelsSpend, ClassRead, ConfigurationGovernance),
		narrow(LLMAlertsManage, AreaModelsSpend, ClassWrite, ConfigurationGovernance),

		// Platform.
		narrow(PlatformConfigView, AreaPlatform, ClassRead, RuntimePlugins),
		narrow(PlatformConfigManage, AreaPlatform, ClassWrite, RuntimePlugins),
		narrow(PlatformEmailManage, AreaPlatform, ClassWrite, RuntimePlugins),
		maintenance,
		narrow(PlatformOperationsView, AreaPlatform, ClassRead, RuntimePlugins),
		narrow(PlatformOperationsManage, AreaPlatform, ClassWrite, RuntimePlugins),
		// Prebuilt MCP servers are connectors: the Agents and governance
		// area, beside Toolkits and Connector requests (decision 6).
		narrow(MCPCatalogueManage, AreaAgentsGovernance, ClassWrite, RuntimePlugins),

		// People and access: identity. Trust is equivalent to super_admin:
		// seeded only, granted to super_admin alone (decision 3). It narrows
		// nothing; it names the admin.auth.users.super_admin gate of #2558.
		narrow(IdentitySSOView, AreaPeopleAccess, ClassRead,
			RuntimePlugins, AdminAuthUsers, ConfigurationRolesUserProjectPermsView),
		narrow(IdentitySSOSettingsManage, AreaPeopleAccess, ClassWrite,
			RuntimePlugins, AdminAuthUsers, ConfigurationRolesUserProjectPermsEdit),
		seeded(IdentitySSOTrustManage, AreaPeopleAccess, ClassWrite),

		// Projects. projects.members.manage also narrows
		// configuration.users.users.delete, on the central removal route of
		// R0.6a. The two settings names are new capabilities, not narrowings:
		// they let an administration permission reach the guarded settings of
		// a project, through the reviewed allowlist of central_override.go
		// (R0.6b).
		narrow(ProjectsMembersManage, AreaProjects, ClassWrite,
			ConfigurationUsersUsersCreate, ConfigurationUsersUsersEdit, ConfigurationUsersUsersDelete),
		narrow(ProjectsSettingsView, AreaProjects, ClassRead),
		narrow(ProjectsSettingsManage, AreaProjects, ClassWrite),

		// People and access: the Access page.
		narrow(AccessRolesView, AreaPeopleAccess, ClassRead, AdminAuthUsers, ModesUsers),

		// Security: Break-glass (decision 6). The four names are seeded
		// only: the elevation ledger is not something the Roles matrix hands
		// out (decision 3).
		seeded(AccessBreakglassRequest, AreaSecurity, ClassWrite),
		seeded(AccessBreakglassApprove, AreaSecurity, ClassApprove),
		seeded(AccessBreakglassReview, AreaSecurity, ClassApprove),
		seeded(AccessBreakglassManage, AreaSecurity, ClassWrite),
	}
}
