package adminaccess

// The console item table of ADR-0059 decision 6 (story R3.1a): the six areas,
// the items of each and the permissions that open each item. The capability
// document (GET /api/v2/admin/console, internal/api/v2/adminaccess) is
// computed from this table and the permission table, and from nothing else.
//
// # What an item maps
//
// An item names the permissions of the routes its page calls, as the router
// gates them: the narrow name and the legacy name it narrows, both, since a
// gate accepts either. The class of each name comes from the permission
// table, never from the item: an item is at `write` when the caller holds a
// mapped name of class write or approve, at `read` when the caller holds only
// mapped names of class read, and absent otherwise. An item whose page has no
// read half (Data retention, Branding, Kill switch, Toolkits, Client
// applications) maps no read name, so it is `write` or absent (decision 2,
// last rule).
//
// # Items whose page ships later
//
// Access, Access history, Break-glass, Security signals and Project settings
// are in the table from the start with Available false, and the document
// leaves them out. The story that ships the page sets Available. The item
// route (GET /api/v2/admin/console/items/{item_id}) answers for them already,
// so the access-denied page can name the roles that will open them.
//
// # The Configuration page today
//
// The Configuration page still carries the gateway, identity and client
// editors as tabs; story R3.2 makes them items. The table is the target of
// decision 6: those names map to their own items (Providers and models,
// Identity providers, Client applications, ...), and Configuration keeps the
// platform sections. internal/api/v2/admin holds the table to every name the
// Configuration routes admit, so no section is left without an item.

import "slices"

// ConsoleLevel is the access level of an item in the capability document.
type ConsoleLevel string

// The two levels. An item the caller cannot open has no level: it is absent.
const (
	ConsoleRead  ConsoleLevel = "read"
	ConsoleWrite ConsoleLevel = "write"
)

// MaxConsoleAreaItems is the most items an area may hold (decision 6).
const MaxConsoleAreaItems = 8

// ConsoleItem is one item of the console.
type ConsoleItem struct {
	// ID is stable: the label key and the web route are built from it.
	ID string
	// Area is the console area that shows it.
	Area Area
	// Permissions are the names that open it, each a row of the permission
	// table.
	Permissions []string
	// Available is false until the story that ships the item's page.
	Available bool
}

// LabelKey is the i18n key of the item's label.
func (i ConsoleItem) LabelKey() string { return "pages.admin.console.item." + i.ID }

// AreaLabelKey is the i18n key of an area's label.
func AreaLabelKey(area Area) string { return "pages.admin.console.area." + string(area) }

// Level answers the caller's level on the item for the administration set
// held, and false when the item is not open to the caller. A name the
// permission table does not know opens nothing.
func (i ConsoleItem) Level(held Set) (ConsoleLevel, bool) {
	level, open := ConsoleLevel(""), false
	for _, name := range i.Permissions {
		if !held[name] {
			continue
		}
		row, ok := Lookup(name)
		if !ok {
			continue
		}
		if row.Class == ClassWrite || row.Class == ClassApprove {
			return ConsoleWrite, true
		}
		level, open = ConsoleRead, true
	}
	return level, open
}

func consoleItem(id string, area Area, permissions ...string) ConsoleItem {
	return ConsoleItem{ID: id, Area: area, Permissions: permissions, Available: true}
}

// later is an item whose page ships in a later story.
func later(id string, area Area, permissions ...string) ConsoleItem {
	item := consoleItem(id, area, permissions...)
	item.Available = false
	return item
}

// consoleItems is the table, in console order: area by area, item by item.
var consoleItems = []ConsoleItem{
	// People and access.
	consoleItem("users", AreaPeopleAccess, AdminAuthUsers, ModesUsers),
	// Administrators, roles, tokens and the raw matrix as a tab (R3.6).
	later("access", AreaPeopleAccess,
		AccessRolesView, ConfigurationRolesPermissionsView, ConfigurationRolesPermissionsEdit,
		ConfigurationRolesRolesCreate, ConfigurationRolesRolesEdit, ConfigurationRolesRolesDelete),
	consoleItem("identity-providers", AreaPeopleAccess,
		IdentitySSOView, IdentitySSOSettingsManage, IdentitySSOTrustManage, RuntimePlugins),
	consoleItem("scim", AreaPeopleAccess,
		IdentitySSOView, IdentitySSOSettingsManage, IdentitySSOTrustManage, AdminAuthUsers,
		ConfigurationRolesUserProjectPermsView, ConfigurationRolesUserProjectPermsEdit),

	// Projects.
	consoleItem("projects", AreaProjects,
		ProjectsProjectsView, ProjectsProjectsEdit, ProjectsProjectCreate, ProjectsProjectDelete),
	consoleItem("app-requests", AreaProjects, AdminModeration, AdminModerationEdit),
	// The members and guarded settings of any project (R0.6a, R0.6b).
	later("project-settings", AreaProjects,
		ProjectsSettingsView, ProjectsSettingsManage, ProjectsMembersManage,
		ConfigurationUsersUsersView, ConfigurationUsersUsersCreate, ConfigurationUsersUsersEdit,
		ConfigurationUsersUsersDelete),

	// Models and spend: the /admin/gateway routes, each beside the legacy
	// configuration.governance its gate still accepts.
	consoleItem("providers-models", AreaModelsSpend, LLMCatalogueView, LLMCatalogueManage, ConfigurationGovernance),
	consoleItem("models-pricing", AreaModelsSpend, LLMCatalogueView, LLMCatalogueManage, ConfigurationGovernance),
	// The metered prices are catalogue routes (gateway/llm_proxy_models.go).
	consoleItem("tool-prices", AreaModelsSpend, LLMCatalogueView, LLMCatalogueManage, ConfigurationGovernance),
	consoleItem("usage", AreaModelsSpend, LLMUsageView, ConfigurationGovernance),
	consoleItem("logs", AreaModelsSpend, LLMUsageView, ConfigurationGovernance),
	consoleItem("llm-governance", AreaModelsSpend,
		LLMPolicyView, LLMPolicyManage, LLMGuardrailsManage, ConfigurationGovernance),
	// Budgets and the soft alert: the alert is read with llm.usage.view.
	consoleItem("budgets-alerts", AreaModelsSpend,
		ModelsAdminProjectBudgetsView, ModelsAdminProjectBudgetsEdit, LLMUsageView, LLMAlertsManage,
		ConfigurationGovernance),

	// Agents and governance.
	// The agent governance pair (R0.15) beside the legacy name its gates
	// still accept.
	consoleItem("agent-registry", AreaAgentsGovernance, AgentsGovernanceView, AgentsGovernanceManage,
		ConfigurationGovernance),
	consoleItem("connector-requests", AreaAgentsGovernance, ConnectorApprove),
	consoleItem("delegation-review", AreaAgentsGovernance, AgentsGovernanceView, AgentsGovernanceManage,
		ConfigurationGovernance),
	// The toolkit types and the prebuilt MCP servers, both connectors.
	consoleItem("toolkits", AreaAgentsGovernance, ToolkitCatalogueTypeManage, MCPCatalogueManage),
	consoleItem("credential-inventory", AreaAgentsGovernance, CredentialsInventoryRead, CredentialsInventoryManage),
	consoleItem("permission-trimming", AreaAgentsGovernance, CredentialsInventoryRead, CredentialsInventoryManage),
	consoleItem("kill-switch", AreaAgentsGovernance, SafetySuspend),

	// Security.
	// The Overview, Signals, Rules and Export pages of ADR-0058 as tabs.
	// ADR-0058 story E5.1 (issue 2403) adds security.signals.view to the
	// table; until then the Export tab's entitlements.export opens it.
	later("security-signals", AreaSecurity, EntitlementsExport),
	consoleItem("audit-trail", AreaSecurity, ModelsAdminAuditTrailView),
	// GET /admin/access/history, gated like GET /admin/access/roles.
	later("access-history", AreaSecurity, AccessRolesView, AdminAuthUsers, ModesUsers),
	// Any access.breakglass.* name opens it (decision 6, the state table).
	later("break-glass", AreaSecurity,
		AccessBreakglassRequest, AccessBreakglassApprove, AccessBreakglassReview, AccessBreakglassManage),
	consoleItem("data-retention", AreaSecurity, RetentionManage, LegalHoldManage),

	// Platform.
	// The break_glass section (R2.2a) is a section of this page, so its
	// permission opens it until the break-glass screens (R3.10) ship. The
	// entitlement_export section is one too: entitlements.export reads it and
	// entitlements.export.manage saves it (#3026).
	consoleItem("configuration", AreaPlatform,
		PlatformConfigView, PlatformConfigManage, PlatformMaintenanceManage, RuntimePlugins,
		AccessBreakglassManage, EntitlementsExport, EntitlementsExportManage),
	consoleItem("features", AreaPlatform, PlatformConfigView, PlatformConfigManage, RuntimePlugins),
	consoleItem("branding", AreaPlatform, ConfigurationBranding),
	consoleItem("email", AreaPlatform, PlatformEmailManage, RuntimePlugins),
	// "Tasks" and "Schedules & Tasks" are one item.
	consoleItem("schedules-tasks", AreaPlatform,
		ConfigurationSchedulingSchedulesView, ConfigurationSchedulingSchedulesEdit,
		PlatformOperationsView, PlatformOperationsManage, RuntimePlugins),
	consoleItem("service-descriptors", AreaPlatform,
		RuntimeAirunServiceProviders, ConfigurationServiceDescriptors,
		ProviderHubDescriptorRegister, ProviderHubDescriptorActivate),
	consoleItem("secrets", AreaPlatform,
		ConfigurationSecretsSecretView, ConfigurationSecretsSecretCreate,
		ConfigurationSecretsSecretEdit, ConfigurationSecretsSecretDelete),
	// Native clients and Microsoft apps.
	consoleItem("client-applications", AreaPlatform, ConfigurationNativeClients, ConfigurationMicrosoftApps),
}

// ConsoleItems returns the item table, in console order.
func ConsoleItems() []ConsoleItem {
	items := make([]ConsoleItem, len(consoleItems))
	for i, item := range consoleItems {
		item.Permissions = slices.Clone(item.Permissions)
		items[i] = item
	}
	return items
}

// LookupConsoleItem returns the item of id.
func LookupConsoleItem(id string) (ConsoleItem, bool) {
	for _, item := range ConsoleItems() {
		if item.ID == id {
			return item, true
		}
	}
	return ConsoleItem{}, false
}

// ConsoleAreaAccess is one area of the capability document.
type ConsoleAreaAccess struct {
	Area  Area
	Items []ConsoleItemAccess
}

// ConsoleItemAccess is one item the caller can open, at its level.
type ConsoleItemAccess struct {
	Item  ConsoleItem
	Level ConsoleLevel
}

// ConsoleAccess answers the areas and items the administration set held
// opens, in console order. An unavailable item, an item held opens nothing
// of, and an area with no open item are absent.
func ConsoleAccess(held Set) []ConsoleAreaAccess {
	areas := []ConsoleAreaAccess{}
	items := ConsoleItems()
	for _, area := range Areas() {
		access := ConsoleAreaAccess{Area: area}
		for _, item := range items {
			if item.Area != area || !item.Available {
				continue
			}
			if level, open := item.Level(held); open {
				access.Items = append(access.Items, ConsoleItemAccess{Item: item, Level: level})
			}
		}
		if len(access.Items) > 0 {
			areas = append(areas, access)
		}
	}
	return areas
}
