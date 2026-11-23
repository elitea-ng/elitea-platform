package adminaccess

import "net/http"

// The route prefixes of the administration surface.
const (
	adminPrefix = "/api/v2/admin"
	corePrefix  = "/api/v2/elitea_core"
)

// ConfigurationValuesPattern is the Configuration value route. Its PUT has one
// action per section, selected by the {plugin} segment.
const ConfigurationValuesPattern = adminPrefix + "/plugin_config_values/administration/{plugin}"

// AuthUsersActionPattern is the user action route. Its POST has one action per
// `action` of the body (AuthUsersAction in internal/api/v2/admin/users.go).
const AuthUsersActionPattern = adminPrefix + "/auth_users/{mode}"

// write is an entry with no danger: no reason, elevation `allow`, token
// `allowed`.
func write(method, pattern, permission string) Action {
	return Action{Method: method, Pattern: pattern, Permission: permission}
}

func post(pattern, permission string) Action  { return write(http.MethodPost, pattern, permission) }
func put(pattern, permission string) Action   { return write(http.MethodPut, pattern, permission) }
func del(pattern, permission string) Action   { return write(http.MethodDelete, pattern, permission) }
func admin(path string) string                { return adminPrefix + path }
func core(path string) string                 { return corePrefix + path }
func (a Action) dangerous() Action            { a.Danger = DangerDangerous; return a }
func (a Action) elevation(e Elevation) Action { a.Elevation = e; return a }
func (a Action) critical(e Elevation) Action  { a.Danger = DangerCritical; a.Elevation = e; return a }
func (a Action) selected(s Selector) Action   { a.Selector = s; a.Name = s.Value; return a }
func (a Action) outcome(c Classifier, outcome string) Action {
	a.Classifier = c
	a.Name = ActionName(a.Selector.Value, outcome)
	return a
}

// section is the PUT of one Configuration section. permission is the
// section's own required_permission where it declares one, otherwise the
// route gate's runtime.plugins.
func section(id, permission string) Action {
	return put(ConfigurationValuesPattern, permission).selected(Selector{In: SelectPath, Field: "plugin", Value: id})
}

// userAction is one `action` of POST /admin/auth_users/{mode}.
func userAction(action string) Action {
	return post(AuthUsersActionPattern, AdminAuthUsers).selected(Selector{In: SelectBody, Field: "action", Value: action})
}

// actionRows is the action table. The first danger list of ADR-0059 decision 6
// is encoded here, with the elevation values of decision 5. Every entry with
// no danger has the defaults of write.
//
// Keep it in step with the router: internal/api/administration_surface_test.go
// fails on a write route of the surface with no entry, and on an entry whose
// route the surface does not have.
func actionRows() []Action {
	const (
		sealed = ElevationSealed
		deny   = ElevationDeny
		flag   = ElevationFlag
		allow  = ElevationAllow
	)
	retention := func(a Action) []Action {
		return []Action{
			a.outcome(ClassifyRetention, OutcomeShorten).critical(deny),
			a.outcome(ClassifyRetention, OutcomeLengthen),
		}
	}
	governanceRow := func(a Action) []Action {
		return []Action{
			a.outcome(ClassifyGovernanceRow, OutcomeGuardrail).critical(allow),
			a.outcome(ClassifyGovernanceRow, OutcomeOther),
		}
	}

	rows := []Action{
		// People and access: users.
		userAction("delete").dangerous().elevation(flag),
		// Assigns or removes an administrator role. Provisional under an
		// elevation (decision 5), so flagged rather than refused.
		userAction("set_admin_role").critical(flag),
		put(admin("/user_suspend/{mode}/{userID}"), AdminAuthUsers).
			outcome(ClassifyUserSuspend, OutcomeSuspend).dangerous().elevation(flag),
		put(admin("/user_suspend/{mode}/{userID}"), AdminAuthUsers).
			outcome(ClassifyUserSuspend, OutcomeUnsuspend).elevation(flag),
		del(admin("/native_devices/administration/{deviceID}"), AdminAuthUsers).elevation(flag),
		post(admin("/user_invite/administration"), ModesUsers).elevation(flag),
		// The second assignment route: writes auth_core__user_role like
		// set_admin_role.
		post(admin("/modes/administration"), ModesUsers).critical(flag),
		del(admin("/modes/administration"), ModesUsers).critical(flag),
		// Bulk membership invites across projects.
		post(admin("/invites_bulk/administration"), InvitesBulkProjects).dangerous().elevation(flag),

		// People and access: the role matrix and the roles.
		put(admin("/permissions/{scope}/{mode}"), ConfigurationRolesPermissionsEdit).critical(deny),
		// "Apply to Projects": pushes the central matrix onto every shared
		// project.
		post(admin("/permissions/{scope}/{mode}"), ConfigurationRolesPermissionsEdit).critical(deny),
		// The matrix of every personal or team project at once.
		put(admin("/user_project_permissions/administration"), ConfigurationRolesUserProjectPermsEdit).critical(deny),
		post(admin("/roles/{scope}/{mode}"), ConfigurationRolesRolesCreate).elevation(deny),
		put(admin("/roles/{scope}/{mode}"), ConfigurationRolesRolesEdit),
		// Deleting a role removes it, an administrator role included, from
		// every holder.
		del(admin("/roles/{scope}/{mode}"), ConfigurationRolesRolesDelete).critical(deny),

		// People and access: SCIM.
		post(admin("/scim_clients/administration"), AdminAuthUsersSuperAdmin).critical(sealed),
		post(admin("/scim_clients/administration/{id}/rotate"), AdminAuthUsersSuperAdmin).critical(sealed),
		post(admin("/scim_clients/administration/{id}/revoke"), AdminAuthUsers).dangerous(),
		del(admin("/scim_clients/administration/{id}"), AdminAuthUsersSuperAdmin).dangerous(),
		post(admin("/scim_group_bindings/administration"), ConfigurationRolesUserProjectPermsEdit).critical(deny),
		put(admin("/scim_group_bindings/administration/{id}"), ConfigurationRolesUserProjectPermsEdit).critical(deny),
		del(admin("/scim_group_bindings/administration/{id}"), ConfigurationRolesUserProjectPermsEdit).critical(deny),

		// Security: identity providers. The PUT is split by the fields it
		// changes; story R0.4c wires the split at the gate. A trust change and
		// turning a provider off need super_admin in the handler today (issue
		// 2558); the delete needs only the route gate until R0.4c moves it to
		// the trust permission. The entries name what each requires today.
		put(admin("/identity_providers/administration/{key}"), AdminAuthUsersSuperAdmin).
			outcome(ClassifyIdentityProvider, OutcomeTrust).critical(sealed),
		put(admin("/identity_providers/administration/{key}"), AdminAuthUsersSuperAdmin).
			outcome(ClassifyIdentityProvider, OutcomeDisable).critical(flag),
		put(admin("/identity_providers/administration/{key}"), RuntimePlugins).
			outcome(ClassifyIdentityProvider, OutcomeSettings).elevation(flag),
		del(admin("/identity_providers/administration/{key}"), RuntimePlugins).critical(sealed),
		// Moves linked accounts onto the provider's issuer: the identity
		// mapping.
		post(admin("/identity_providers/administration/{key}/rebind"), AdminAuthUsersSuperAdmin).critical(sealed),

		// Security: retention and legal holds.
		post(admin("/legal_holds"), LegalHoldManage),
		post(admin("/legal_holds/{id}/release"), LegalHoldManage).critical(deny),

		// Projects.
		post("/api/v2/projects/project/{mode}", ProjectsProjectCreate),
		// The administration mode only; the user-facing delete in the default
		// mode is not on the surface.
		del("/api/v2/projects/project/{mode}/{projectID}", ProjectsProjectDelete).critical(allow),
		put(admin("/project_suspend/{mode}/{projectID}"), ProjectsProjectsEdit),
		put(admin("/project_cloud_executor/administration/{projectID}"), ProjectsProjectsEdit).elevation(flag),
		post(admin("/users/administration/{projectID}"), ConfigurationUsersUsersCreate).elevation(flag),
		put(admin("/users/administration/{projectID}"), ConfigurationUsersUsersEdit).elevation(flag),
		post(admin("/users/{mode}/{projectID}"), ConfigurationUsersUsersCreate).elevation(flag),
		put(admin("/users/{mode}/{projectID}"), ConfigurationUsersUsersEdit).elevation(flag),
		put(admin("/moderation_status/administration"), AdminModerationEdit),

		// Models and spend: budgets.
		put(core("/project_budget/administration/{projectID}/budget"), ModelsAdminProjectBudgetsEdit),
		del(core("/project_budget/administration/{projectID}/budget"), ModelsAdminProjectBudgetsEdit),
		put(core("/user_budget/administration/{projectID}/user_budget/{userID}"), ModelsAdminProjectBudgetsEdit),
		del(core("/user_budget/administration/{projectID}/user_budget/{userID}"), ModelsAdminProjectBudgetsEdit),

		// Models and spend: the gateway.
		post(admin("/gateway/providers/"), ConfigurationGovernance),
		put(admin("/gateway/providers/{configID}"), ConfigurationGovernance).
			outcome(ClassifyGlobalProvider, OutcomeRoute).critical(flag),
		put(admin("/gateway/providers/{configID}"), ConfigurationGovernance).
			outcome(ClassifyGlobalProvider, OutcomeSettings),
		del(admin("/gateway/providers/{configID}"), ConfigurationGovernance).dangerous(),
		post(admin("/gateway/providers/{configID}/check"), ConfigurationGovernance),
		post(admin("/gateway/providers/{configID}/models"), ConfigurationGovernance),
		post(admin("/gateway/providers/{configID}/revalidate"), ConfigurationGovernance),
		post(admin("/gateway/platform_models/"), ConfigurationGovernance),
		put(admin("/gateway/platform_models/{configID}"), ConfigurationGovernance),
		del(admin("/gateway/platform_models/{configID}"), ConfigurationGovernance).dangerous(),
		put(admin("/gateway/default_model/"), ConfigurationGovernance),
		del(admin("/gateway/default_model/"), ConfigurationGovernance),
		put(admin("/gateway/models"), ConfigurationGovernance),
		del(admin("/gateway/models/{id}"), ConfigurationGovernance),
		put(admin("/gateway/metered-prices"), ConfigurationGovernance),
		put(admin("/gateway/*/budget-alerts"), ConfigurationGovernance),
		post(admin("/gateway/governance/validate-cel"), ConfigurationGovernance),

		// Agents and governance.
		post(admin("/execution_suspensions"), SafetySuspend).
			outcome(ClassifySuspensionScope, OutcomePlatform).dangerous(),
		post(admin("/execution_suspensions"), SafetySuspend).outcome(ClassifySuspensionScope, OutcomeScoped),
		post(admin("/execution_suspensions/{suspension_id}:resume"), SafetySuspend),
		post(admin("/connector_inventory/requests"), ConnectorApprove),
		post(admin("/connector_requests/{requestID}/approve"), ConnectorApprove),
		post(admin("/connector_requests/{requestID}/reject"), ConnectorApprove),
		post(admin("/credential_inventory/revoke"), CredentialsInventoryManage).dangerous(),
		post(admin("/permission_trimming/remove"), CredentialsInventoryManage),
		post(admin("/delegation_review/scan"), ConfigurationGovernance),
		post(admin("/toolkit_types/administration/bulk"), ToolkitCatalogueTypeManage),
		put(admin("/toolkit_types/administration/{type}"), ToolkitCatalogueTypeManage),
		put(admin("/toolkit_types/administration/{type}/projects/{projectID}"), ToolkitCatalogueTypeManage),
		del(admin("/toolkit_types/administration/{type}/projects/{projectID}"), ToolkitCatalogueTypeManage),
		put(admin("/mcp_prebuilt_servers/administration/{key}"), RuntimePlugins),
		del(admin("/mcp_prebuilt_servers/administration/{key}"), RuntimePlugins),

		// Agents and governance: agent identities (mounted with the
		// governance flag on).
		post(admin("/agent_identities"), AgentIdentityBindingsManage),
		post(admin("/agent_identities/{id}:approve"), AgentIdentityBindingsApprove),
		post(admin("/agent_identities/{id}:repin"), AgentIdentityBindingsManage),
		post(admin("/agent_identities/{id}:revoke"), AgentIdentityBindingsManage),
		post(admin("/agent_identities/{id}:suspend"), AgentIdentityBindingsManage),
		put(admin("/agent_identities/{id}/toolkits/{toolkitID}"), AgentIdentityBindingsManage),
		del(admin("/agent_identities/{id}/toolkits/{toolkitID}"), AgentIdentityBindingsManage),
		post(admin("/mandates"), AgentIdentityMandatesManage),
		post(admin("/mandates/{id}:approve"), AgentIdentityMandatesApprove),
		post(admin("/mandates/{id}:revoke"), AgentIdentityMandatesManage),
		post(admin("/mandates/{id}:suspend"), AgentIdentityMandatesManage),
		put(admin("/mandate_attachments/pipeline_schedules/{projectID}/{versionID}"), AgentIdentityMandatesManage),
		del(admin("/mandate_attachments/pipeline_schedules/{projectID}/{versionID}"), AgentIdentityMandatesManage),
		put(admin("/mandate_attachments/pipeline_triggers/{projectID}/{versionID}"), AgentIdentityMandatesManage),
		del(admin("/mandate_attachments/pipeline_triggers/{projectID}/{versionID}"), AgentIdentityMandatesManage),
		put(admin("/mandate_attachments/index_schedules/{projectID}/{toolkitID}/{indexName}"), AgentIdentityMandatesManage),
		del(admin("/mandate_attachments/index_schedules/{projectID}/{toolkitID}/{indexName}"), AgentIdentityMandatesManage),
		post(admin("/credential_references"), AgentIdentityBindingsManage),
		post(admin("/credential_references/{id}:activate"), AgentIdentityBindingsApprove),
		post(admin("/credential_references/{id}:attach"), AgentIdentityBindingsManage),
		post(admin("/credential_references/{id}:detach"), AgentIdentityBindingsManage),
		post(admin("/credential_references/{id}:retire"), AgentIdentityBindingsManage),
		post(admin("/credential_references/{id}:rotate"), AgentIdentityBindingsManage),
		post(admin("/credential_references/{id}:cancel_rotation"), AgentIdentityBindingsManage),
		post(admin("/external_agent_identities"), AgentIdentityExternalManage),
		post(admin("/external_agent_identities/{id}:provision"), AgentIdentityExternalManage),
		post(admin("/external_agent_identities/{id}:activate"), AgentIdentityBindingsApprove),
		post(admin("/external_agent_identities/{id}:suspend"), AgentIdentityExternalManage),
		post(admin("/external_agent_identities/{id}:resume"), AgentIdentityExternalManage),
		post(admin("/external_agent_identities/{id}:retire"), AgentIdentityExternalManage),

		// Platform.
		put(admin("/branding/administration"), ConfigurationBranding),
		del(admin("/branding/administration"), ConfigurationBranding).dangerous(),
		post(admin("/branding/assets/{kind}"), ConfigurationBranding),
		post(admin("/branding/package/administration"), ConfigurationBranding),
		post(admin("/branding/package/administration/versions/{digest}/restore"), ConfigurationBranding),
		post(admin("/branding/test_email/administration"), ConfigurationBranding),
		put(admin("/email/administration"), RuntimePlugins).elevation(deny),
		post(admin("/email/test/administration"), RuntimePlugins),
		// Answers 405: maintenance is authored in the Maintenance section.
		put(admin("/maintenance/{mode}"), RuntimePlugins),
		post(admin("/background_jobs/administration/pat_expiry_notices:run"), RuntimePlugins),
		post(admin("/background_jobs/administration/{kind}/{jobID}:cancel"), RuntimePlugins),
		put(admin("/platform_routines/administration/{id}"), ConfigurationSchedulingSchedulesEdit),
		put(admin("/native_clients/administration/{clientID}"), ConfigurationNativeClients),
		del(admin("/native_clients/administration/{clientID}"), ConfigurationNativeClients),
		post(admin("/microsoft_oauth_apps"), ConfigurationMicrosoftApps),
		put(admin("/microsoft_oauth_apps/settings"), ConfigurationMicrosoftApps),
		put(admin("/microsoft_oauth_apps/{id}"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/disable"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/enable"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/test"), ConfigurationMicrosoftApps),
		post(core("/register_descriptor/{projectID}"), ProviderHubDescriptorRegister),
		del(core("/register_descriptor/{projectID}"), ProviderHubDescriptorRegister),
		post(core("/register_descriptor/{projectID}/activate"), ProviderHubDescriptorActivate),
		post(core("/register_descriptor/{projectID}/deactivate"), ProviderHubDescriptorActivate),
		post("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretCreate),
		put("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretEdit),
		del("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretDelete),
		post("/api/v2/secrets/hide/{mode}/{projectID}/{name}", ConfigurationSecretsSecretEdit),
	}

	rows = append(rows, retention(put(admin("/retention/policies/{dataClass}"), RetentionManage))...)
	rows = append(rows, retention(put(admin("/retention/policies/{dataClass}/projects/{projectID}"), RetentionManage))...)
	// Deleting an override falls back to the platform maximum, which can be
	// shorter.
	rows = append(rows, retention(del(admin("/retention/policies/{dataClass}/projects/{projectID}"), RetentionManage))...)
	rows = append(rows, governanceRow(post(admin("/gateway/governance"), ConfigurationGovernance))...)
	rows = append(rows, governanceRow(put(admin("/gateway/governance/{id}"), ConfigurationGovernance))...)
	rows = append(rows, governanceRow(del(admin("/gateway/governance/{id}"), ConfigurationGovernance))...)

	// The Configuration sections.
	maintenance := section("maintenance", RuntimePlugins)
	rows = append(rows,
		section("agent_lifecycle", ConfigurationGovernance),
		section("agent_publishing", RuntimePlugins),
		section("agents", ConfigurationGovernance),
		section("ai_disclosure", ConfigurationGovernance),
		section("analytics", RuntimePlugins),
		section("approval_policy", ConfigurationGovernance),
		section("approvals", ConfigurationGovernance),
		section("auth", RuntimePlugins),
		section("branding", ConfigurationBranding),
		section("browser", ConfigurationGovernance),
		section("catalogue", ConfigurationGovernance),
		section("circuit_breaker", ConfigurationGovernance),
		section("cloud_executor", ConfigurationNativeClients),
		section("credential_review", ConfigurationGovernance),
		section("dedicated_banner", RuntimePlugins),
		section("default_secrets", RuntimePlugins),
		section("delegation", ConfigurationGovernance),
		section("email", RuntimePlugins).elevation(deny),
		section("entitlement_export", EntitlementsExport),
		section("governance", ConfigurationGovernance),
		section("guardrails", RuntimePlugins),
		section("llm_proxy", RuntimePlugins),
		maintenance.outcome(ClassifyMaintenance, OutcomeOn).dangerous().elevation(flag),
		maintenance.outcome(ClassifyMaintenance, OutcomeOther).elevation(flag),
		section("mcp_configuration", RuntimePlugins),
		section("mcp_servers", RuntimePlugins),
		section("microsoft_apps", ConfigurationMicrosoftApps),
		section("native_client_policy", ConfigurationNativeClients),
		section("native_clients", ConfigurationNativeClients),
		section("notifications", ConfigurationGovernance),
		section("platform_preamble", ConfigurationGovernance),
		section("proactivity", ConfigurationGovernance),
		section("remote_agents", ConfigurationGovernance),
		section("resources", RuntimePlugins),
		section("run_caps", ConfigurationGovernance),
		section("service_descriptors", ConfigurationServiceDescriptors),
		section("session_policy", RuntimePlugins),
		section("skill_publishing", RuntimePlugins),
		section("support_assistant", RuntimePlugins),
		section("voice_features", RuntimePlugins),
		section("web_search", RuntimePlugins),
		section("worker_egress", ConfigurationGovernance),
	)
	return rows
}
