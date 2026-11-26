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

// The Access reads with an action entry (actions.go, "A GET or HEAD changes
// nothing"): the sealed credential list (ADR-0059 R2.17) and the access
// posture (R2.14a).
const (
	SealedCredentialsPattern = adminPrefix + "/access/sealed_credentials"
	AccessPosturePattern     = adminPrefix + "/access/posture"
)

// The elevation request routes (ADR-0059 R2.2b): the request, its answers,
// the caller's own request, the approval queue and the history.
const (
	ElevationsPattern        = adminPrefix + "/access/elevations"
	ElevationsMinePattern    = adminPrefix + "/access/elevations/mine"
	ElevationsPendingPattern = adminPrefix + "/access/elevations/pending"
	ElevationApprovePattern  = adminPrefix + "/access/elevations/{elevationID}/approve"
	ElevationDenyPattern     = adminPrefix + "/access/elevations/{elevationID}/deny"
)

// The review after use (ADR-0059 R2.9a): the review of one elevation, its
// attestation, and the revert list of the platform administrators.
const (
	ElevationReviewPattern = adminPrefix + "/access/elevations/{elevationID}/review"
	RevertsPattern         = adminPrefix + "/access/reverts"
	RevertHandledPattern   = adminPrefix + "/access/reverts/{itemID}/handled"
)

// IdentityProviderSavePattern is the identity provider save, whose handler
// classifies the save and applies the token rule itself (TokenRuleInHandler).
const IdentityProviderSavePattern = adminPrefix + "/identity_providers/administration/{key}"

// The routes whose handler classifies the write and applies the token rule
// itself (TokenRuleInHandler, issue 2993): the gateway governance rows
// (GovernanceRowAction), the global provider update
// (GlobalProviderUpdateAction) and the retention writes (RetentionAction).
const (
	GovernanceCreatePattern       = adminPrefix + "/gateway/governance"
	GovernanceRowPattern          = adminPrefix + "/gateway/governance/{id}"
	GlobalProviderUpdatePattern   = adminPrefix + "/gateway/providers/{configID}"
	RetentionPolicyPattern        = adminPrefix + "/retention/policies/{dataClass}"
	RetentionProjectPolicyPattern = adminPrefix + "/retention/policies/{dataClass}/projects/{projectID}"
)

// write is an entry with no danger: no reason, elevation `allow`, token
// `allowed`.
func write(method, pattern, permission string) Action {
	return Action{Method: method, Pattern: pattern, Permission: permission}
}

// read is the entry of a GET the ADR restricts beyond its gate (actions.go,
// "A GET or HEAD changes nothing").
func read(pattern, permission string) Action { return write(http.MethodGet, pattern, permission) }

func post(pattern, permission string) Action  { return write(http.MethodPost, pattern, permission) }
func put(pattern, permission string) Action   { return write(http.MethodPut, pattern, permission) }
func del(pattern, permission string) Action   { return write(http.MethodDelete, pattern, permission) }
func admin(path string) string                { return adminPrefix + path }
func core(path string) string                 { return corePrefix + path }
func (a Action) dangerous() Action            { a.Danger = DangerDangerous; return a }
func (a Action) elevation(e Elevation) Action { a.Elevation = e; return a }
func (a Action) critical(e Elevation) Action  { a.Danger = DangerCritical; a.Elevation = e; return a }
func (a Action) selected(s Selector) Action   { a.Selector = s; a.Name = s.Value; return a }
func (a Action) tokenNever() Action           { a.Token = TokenNever; return a }

// tokenAllowed lets a token call a critical action, with the written reason
// ADR-0059 decision 7 requires. The token rule (token_rule.go) still admits
// only an inherit token of a standing global administrator. The identity
// trust entries use it (R1.2b); R4.2 turns them to never.
func (a Action) tokenAllowed(reason string) Action {
	a.Token, a.TokenReason = TokenAllowed, reason
	return a
}
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
	// A governance row write: the route gate admits llm.policy.manage or
	// llm.guardrails.manage (or configuration.governance), and the handler
	// decides by the kind of the row (internal/api/gateway/gates.go).
	//
	// The `other` outcome names llm.policy.manage, the narrowest name that
	// writes any of its kinds. It is an upper bound: the handler narrows it
	// further, and the egress allowlist, the MCP allowlist, the credential
	// policy and the budget alert row still take configuration.governance
	// alone. A reader of this table that decides admission must not treat
	// llm.policy.manage as enough for those kinds; splitting the outcome per
	// kind is a change to ClassifyGovernanceRow.
	governanceRow := func(method, pattern string) []Action {
		return []Action{
			write(method, pattern, LLMGuardrailsManage).outcome(ClassifyGovernanceRow, OutcomeGuardrail).critical(allow),
			write(method, pattern, LLMPolicyManage).outcome(ClassifyGovernanceRow, OutcomeOther),
		}
	}

	// The identity trust repair path (ADR-0059 decision 5, "Emergency
	// mechanisms that exist today"): an automation that holds an inherit
	// token of a standing global administrator can still repair a broken
	// sign-in until the sunset of decision 7 (R4.2 removes the exception).
	const trustRepair = "identity trust repair path until the sunset: an inherit token of a standing " +
		"global administrator (ADR-0059 decision 5)"

	rows := []Action{
		// The two Access reads the ADR restricts beyond their gate. The
		// sealed credential list (R2.17) names who holds emergency access:
		// an elevation cannot read it (decision 5) and a token never does.
		// The access posture (R2.14a) is an ordinary read, and its entry
		// says so: a scoped token that holds access.roles.view reads it.
		read(SealedCredentialsPattern, AccessBreakglassManage).elevation(deny).tokenNever(),
		read(AccessPosturePattern, AccessRolesView),

		// The elevation requests (R2.2b). No token calls an elevation
		// route (decision 5, "Tokens"; decision 7, the credentials table),
		// whatever its scopes: every entry says `token: never`, and the
		// routes refuse every credential but a browser session before
		// their gate. The ledger's writes are `deny` under an elevation
		// (R2.8, "the elevation ledger (approve, deny, review)"): the
		// request, too, since more time is a new request from a standing
		// holder. The reads change nothing an elevation could misuse.
		post(ElevationsPattern, AccessBreakglassRequest).elevation(deny).tokenNever(),
		post(ElevationApprovePattern, AccessBreakglassApprove).elevation(deny).tokenNever(),
		post(ElevationDenyPattern, AccessBreakglassApprove).elevation(deny).tokenNever(),
		read(ElevationsMinePattern, AccessBreakglassRequest).tokenNever(),
		read(ElevationsPendingPattern, AccessBreakglassApprove).tokenNever(),
		read(ElevationsPattern, AccessRolesView).tokenNever(),
		// The review after use (R2.9a): an attestation and the revert
		// list. The review is the elevation ledger's, so `deny` under an
		// elevation; marking a revert entry handled too, since the revert
		// list is the record of what an elevation did. No token.
		read(ElevationReviewPattern, AccessBreakglassReview).tokenNever(),
		post(ElevationReviewPattern, AccessBreakglassReview).elevation(deny).tokenNever(),
		read(RevertsPattern, AccessBreakglassManage).tokenNever(),
		post(RevertHandledPattern, AccessBreakglassManage).elevation(deny).tokenNever(),

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

		// People and access: SCIM. Split by action (R0.4c): minting, rotating
		// and deleting a client are trust actions (identity.sso.trust.manage or
		// super_admin, in the handler); a revoke and the bindings are settings.
		// A binding write also meets the role rule of #2555.
		post(admin("/scim_clients/administration"), IdentitySSOTrustManage).critical(sealed).tokenAllowed(trustRepair),
		post(admin("/scim_clients/administration/{id}/rotate"), IdentitySSOTrustManage).critical(sealed).
			tokenAllowed(trustRepair),
		post(admin("/scim_clients/administration/{id}/revoke"), IdentitySSOSettingsManage).dangerous(),
		del(admin("/scim_clients/administration/{id}"), IdentitySSOTrustManage).dangerous(),
		post(admin("/scim_group_bindings/administration"), IdentitySSOSettingsManage).critical(deny),
		put(admin("/scim_group_bindings/administration/{id}"), IdentitySSOSettingsManage).critical(deny),
		del(admin("/scim_group_bindings/administration/{id}"), IdentitySSOSettingsManage).critical(deny),

		// Security: identity providers. The PUT is split by the fields it
		// changes (R0.4c): a trust change and turning a provider off need
		// identity.sso.trust.manage or super_admin in the handler (issue 2558),
		// as do the delete and the re-bind; the rest is settings.
		//
		// The trust entries here and above name identity.sso.trust.manage.
		// The route gate also admits the settings name and the legacy name, so
		// that the handler (requireIdentityTrust) writes the refusal; the
		// handler accepts identity.sso.trust.manage or
		// admin.auth.users.super_admin, which only super_admin holds.
		put(IdentityProviderSavePattern, IdentitySSOTrustManage).
			outcome(ClassifyIdentityProvider, OutcomeTrust).critical(sealed).tokenAllowed(trustRepair),
		put(admin("/identity_providers/administration/{key}"), IdentitySSOTrustManage).
			outcome(ClassifyIdentityProvider, OutcomeDisable).critical(flag).tokenAllowed(trustRepair),
		put(admin("/identity_providers/administration/{key}"), IdentitySSOSettingsManage).
			outcome(ClassifyIdentityProvider, OutcomeSettings).elevation(flag),
		del(admin("/identity_providers/administration/{key}"), IdentitySSOTrustManage).critical(sealed).
			tokenAllowed(trustRepair),
		// Moves linked accounts onto the provider's issuer: the identity
		// mapping.
		post(admin("/identity_providers/administration/{key}/rebind"), IdentitySSOTrustManage).critical(sealed).
			tokenAllowed(trustRepair),

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
		// The central member routes (ADR-0059 R0.6a): the gate accepts
		// projects.members.manage beside each legacy name.
		post(admin("/users/administration/{projectID}"), ProjectsMembersManage).elevation(flag),
		put(admin("/users/administration/{projectID}"), ProjectsMembersManage).elevation(flag),
		del(admin("/users/administration/{projectID}"), ProjectsMembersManage).elevation(flag),
		post(admin("/users/{mode}/{projectID}"), ConfigurationUsersUsersCreate).elevation(flag),
		put(admin("/users/{mode}/{projectID}"), ConfigurationUsersUsersEdit).elevation(flag),
		// The `{mode}` pattern with mode `administration` reaches the static
		// central removal route above (R0.6a).
		del(admin("/users/{mode}/{projectID}"), ConfigurationUsersUsersDelete).elevation(flag),
		// A guarded project setting of any project through the central
		// override (ADR-0059 R0.6b): only the allowlisted pairs of
		// central_override.go. Flagged under an elevation (decision 5).
		post("/api/v2/configurations/configurations/{projectID}", ProjectsSettingsManage).elevation(flag),
		post("/api/v2/configurations/configurations/{mode}/{projectID}", ProjectsSettingsManage).elevation(flag),
		put("/api/v2/configurations/configuration/{projectID}/{configID}", ProjectsSettingsManage).elevation(flag),
		put("/api/v2/configurations/configuration/{mode}/{projectID}/{configID}", ProjectsSettingsManage).elevation(flag),
		del("/api/v2/configurations/configuration/{projectID}/{configID}", ProjectsSettingsManage).elevation(flag),
		del("/api/v2/configurations/configuration/{mode}/{projectID}/{configID}", ProjectsSettingsManage).elevation(flag),
		put(admin("/moderation_status/administration"), AdminModerationEdit),

		// Models and spend: budgets.
		put(core("/project_budget/administration/{projectID}/budget"), ModelsAdminProjectBudgetsEdit),
		del(core("/project_budget/administration/{projectID}/budget"), ModelsAdminProjectBudgetsEdit),
		put(core("/user_budget/administration/{projectID}/user_budget/{userID}"), ModelsAdminProjectBudgetsEdit),
		del(core("/user_budget/administration/{projectID}/user_budget/{userID}"), ModelsAdminProjectBudgetsEdit),

		// Models and spend: the gateway (ADR-0059 R0.3). Each route's gate
		// accepts the narrow name below or configuration.governance.
		// Creating a global provider sets its endpoint, credential and keyless
		// identity, so it is critical like changing them.
		post(admin("/gateway/providers"), LLMCatalogueManage).critical(flag),
		put(GlobalProviderUpdatePattern, LLMCatalogueManage).
			outcome(ClassifyGlobalProvider, OutcomeRoute).critical(flag),
		put(GlobalProviderUpdatePattern, LLMCatalogueManage).
			outcome(ClassifyGlobalProvider, OutcomeSettings),
		del(admin("/gateway/providers/{configID}"), LLMCatalogueManage).dangerous(),
		// check, revalidate and the provider model listing call out with the
		// stored credential, so they take the manage name, not the view.
		post(admin("/gateway/providers/{configID}/check"), LLMCatalogueManage),
		post(admin("/gateway/providers/{configID}/models"), LLMCatalogueManage),
		post(admin("/gateway/providers/{configID}/revalidate"), LLMCatalogueManage),
		post(admin("/gateway/platform_models"), LLMCatalogueManage),
		put(admin("/gateway/platform_models/{configID}"), LLMCatalogueManage),
		del(admin("/gateway/platform_models/{configID}"), LLMCatalogueManage).dangerous(),
		put(admin("/gateway/default_model"), LLMCatalogueManage),
		del(admin("/gateway/default_model"), LLMCatalogueManage),
		put(admin("/gateway/models"), LLMCatalogueManage),
		del(admin("/gateway/models/{id}"), LLMCatalogueManage),
		put(admin("/gateway/metered-prices"), LLMCatalogueManage),
		put(admin("/gateway/budget-alerts"), LLMAlertsManage),
		// Changes nothing: a read behind the policy view gate.
		post(admin("/gateway/governance/validate-cel"), LLMPolicyView),

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
		// The scan writes the delegation grants a migration missed (R0.15).
		post(admin("/delegation_review/scan"), AgentsGovernanceManage),
		post(admin("/toolkit_types/administration/bulk"), ToolkitCatalogueTypeManage),
		put(admin("/toolkit_types/administration/{type}"), ToolkitCatalogueTypeManage),
		put(admin("/toolkit_types/administration/{type}/projects/{projectID}"), ToolkitCatalogueTypeManage),
		del(admin("/toolkit_types/administration/{type}/projects/{projectID}"), ToolkitCatalogueTypeManage),
		put(admin("/mcp_prebuilt_servers/administration/{key}"), MCPCatalogueManage),
		del(admin("/mcp_prebuilt_servers/administration/{key}"), MCPCatalogueManage),

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
		put(admin("/email/administration"), PlatformEmailManage).elevation(deny),
		post(admin("/email/test/administration"), PlatformEmailManage),
		// Answers 405: maintenance is authored in the Maintenance section.
		put(admin("/maintenance/{mode}"), PlatformMaintenanceManage),
		post(admin("/background_jobs/administration/pat_expiry_notices:run"), PlatformOperationsManage),
		post(admin("/background_jobs/administration/{kind}/{jobID}:cancel"), PlatformOperationsManage),
		put(admin("/platform_routines/administration/{id}"), ConfigurationSchedulingSchedulesEdit),
		put(admin("/native_clients/administration/{clientID}"), ConfigurationNativeClients),
		del(admin("/native_clients/administration/{clientID}"), ConfigurationNativeClients),
		post(admin("/microsoft_oauth_apps"), ConfigurationMicrosoftApps),
		put(admin("/microsoft_oauth_apps/settings"), ConfigurationMicrosoftApps),
		put(admin("/microsoft_oauth_apps/{id}"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/disable"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/enable"), ConfigurationMicrosoftApps),
		post(admin("/microsoft_oauth_apps/{id}/test"), ConfigurationMicrosoftApps),
		// Feature or delist a Marketplace listing: the moderation check in
		// the handler (eliteacore holdsModerationPermission), behind the
		// project's agent list gate. The surface reaches it since #2925.
		write(http.MethodPatch, core("/marketplace_listing/prompt_lib/{projectID}/{listingID}"), RuntimeAdminPublishedAgents),
		post(core("/register_descriptor/{projectID}"), ProviderHubDescriptorRegister),
		del(core("/register_descriptor/{projectID}"), ProviderHubDescriptorRegister),
		post(core("/register_descriptor/{projectID}/activate"), ProviderHubDescriptorActivate),
		post(core("/register_descriptor/{projectID}/deactivate"), ProviderHubDescriptorActivate),
		post("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretCreate),
		put("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretEdit),
		del("/api/v2/secrets/secret/{mode}/{projectID}/{name}", ConfigurationSecretsSecretDelete),
		post("/api/v2/secrets/hide/{mode}/{projectID}/{name}", ConfigurationSecretsSecretEdit),
	}

	rows = append(rows, retention(put(RetentionPolicyPattern, RetentionManage))...)
	rows = append(rows, retention(put(RetentionProjectPolicyPattern, RetentionManage))...)
	// Deleting an override falls back to the platform maximum, which can be
	// shorter.
	rows = append(rows, retention(del(RetentionProjectPolicyPattern, RetentionManage))...)
	rows = append(rows, governanceRow(http.MethodPost, GovernanceCreatePattern)...)
	rows = append(rows, governanceRow(http.MethodPut, GovernanceRowPattern)...)
	rows = append(rows, governanceRow(http.MethodDelete, GovernanceRowPattern)...)

	// The Configuration sections. The agent governance sections are saved
	// with agents.governance.manage (R0.15) or on the legacy path.
	maintenance := section("maintenance", RuntimePlugins)
	rows = append(rows,
		section("admin_console", RuntimePlugins),
		section("agent_lifecycle", AgentsGovernanceManage),
		section("agent_publishing", RuntimePlugins),
		section("agents", AgentsGovernanceManage),
		section("ai_disclosure", AgentsGovernanceManage),
		section("analytics", RuntimePlugins),
		section("approval_policy", AgentsGovernanceManage),
		section("approvals", AgentsGovernanceManage),
		section("auth", RuntimePlugins),
		// The break-glass policy (ADR-0059 R2.2a): durations, caps and the
		// approval switch. An elevation never changes its own controls.
		section("break_glass", AccessBreakglassManage).critical(deny),
		section("branding", ConfigurationBranding),
		section("browser", AgentsGovernanceManage),
		section("catalogue", AgentsGovernanceManage),
		section("circuit_breaker", ConfigurationGovernance),
		section("cloud_executor", ConfigurationNativeClients),
		section("credential_review", AgentsGovernanceManage),
		section("dedicated_banner", RuntimePlugins),
		section("default_secrets", RuntimePlugins),
		section("delegation", AgentsGovernanceManage),
		section("email", RuntimePlugins).elevation(deny),
		section("entitlement_export", EntitlementsExportManage),
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
		section("notifications", AgentsGovernanceManage),
		section("platform_preamble", AgentsGovernanceManage),
		section("proactivity", AgentsGovernanceManage),
		section("remote_agents", AgentsGovernanceManage),
		section("resources", RuntimePlugins),
		section("run_caps", AgentsGovernanceManage),
		section("service_descriptors", ConfigurationServiceDescriptors),
		section("session_policy", RuntimePlugins),
		section("skill_publishing", RuntimePlugins),
		section("support_assistant", RuntimePlugins),
		section("voice_features", RuntimePlugins),
		section("web_search", RuntimePlugins),
		section("worker_egress", AgentsGovernanceManage),
	)
	return rows
}
