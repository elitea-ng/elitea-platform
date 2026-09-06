package admin

// Why most of these sections carry an `unavailable_reason` — unit A14, #200.
//
// This file describes what the admin Configuration page can offer. In pylon
// those descriptions are assembled at request time from every plugin's
// `admin_schema.json` as announced over the Arbiter bus, and each field's `path`
// addresses a key inside that plugin's YAML config; saving one re-serialises the
// YAML and ships it back to the pylon that owns it
// (legacy/plugins/admin/api/v2/plugin_config_values.py — see the header of
// config_values.go for the whole chain).
//
// None of that exists here, and AGENTS.md says it is not meant to: "Do not
// preserve Pylon plugin loading … as target architecture." A field whose only
// consumer is a Pylon plugin descriptor therefore has nowhere to go, and the
// honest answer is to say so ON THE SECTION rather than to render a form.
//
// `unavailable_reason` is declared HERE, on the server, for the same reason the
// permission is: it is a fact about the deployment, and a page that decided it
// locally would drift from what the endpoints actually do. `config_values.go`
// answers 501 with this exact string, and the ported page renders it.
//
// Removing a reason is how a section becomes live — and it is only correct to
// remove one once something in this platform READS the values. `resources` is
// the section that passes that test: apps/elitea-web's Help Center reads it back
// through `GET /admin/plugin_config_values/prompt_lib/resources`.

const (
	// extraUIConfigUnavailable used to cover the `extra_ui_config.*` sections
	// other than `resources` — settings the legacy UI received by injecting
	// elitea_core's `extra_ui_config` into the page, withheld here for the
	// narrower reason that nothing read them YET. It was described as a gap to
	// close rather than a boundary to respect, and every section that held it
	// has now closed its gap: `resources` first (the Help Center),
	// `support_assistant` by acquiring the specific reason below, and
	// `dedicated_banner` by acquiring a consumer. The constant is gone rather
	// than kept for a future section, so the next one to need it has to state
	// what reads it before it can be written.

	// governanceElsewhereUnavailable — the LLM-governance fields are NOT saved
	// through this endpoint even in the reference: elitea-main has its own CRUD
	// at `/admin/gateway/governance` writing `gateway.governance_config`, and
	// the SPA's page for it is `pages/admin/GatewayGovernance.tsx`.
	//
	// The reason this section is unavailable has CHANGED, and the change is the
	// point of #218. It used to be withheld because nothing read the table: the
	// gateway's budget engine never queried it, so a rule saved anywhere was
	// enforced nowhere. That is no longer true — the gateway now reads every
	// enabled row and enforces it on the /llm path (elitea-llm-gateway
	// internal/policy). The section stays unavailable HERE for the narrower and
	// older reason: this page is a flat form over one value document, and a
	// governance corpus is a list of rows with per-row scope. The row editor is
	// the surface that can express that; this form is not.
	//
	// The stale claim in the header of
	// migrations/shared/0067_gateway_budget_schema.sql — which said the gateway
	// reads these rows at load, and was wrong when it was written — has become
	// accurate by the gateway catching up with it. Migration 0093 records that,
	// because 0067 is checksum-immutable once applied
	// (internal/infra/db/migrate/manifest.go) and cannot be edited.
	governanceElsewhereUnavailable = "LLM governance is authored through /admin/gateway/governance, not through this " +
		"page, because a governance corpus is a list of scoped rows and this page is a flat form over a single " +
		"value document. Definitions saved there ARE enforced: the LLM gateway reads gateway.governance_config and " +
		"applies budgets, rate limits, model and MCP allowlists, credential rate policy and CEL routing rules on " +
		"every /llm request."

	// serviceDescriptorsElsewhereUnavailable — the section defers to the page,
	// and no longer borrows that page's old refusal.
	//
	// THREE WORDINGS, IN ORDER, AND EACH ONE WAS TRUE WHEN WRITTEN. First it
	// said only "service descriptors are a page of their own in the admin port
	// (issue #200)", deferring to a surface that did not exist yet. Then it
	// became `eliteacore.ServiceDescriptorsUnavailableReason`, because the page
	// existed and refused, and pointing an operator at a page that said
	// something different would have been worse than saying nothing.
	//
	// Migration 0107 made that page ANSWER. Reusing the refusal now would tell
	// an operator this deployment has no descriptor store while the page it
	// points at lists one — a section whose text outlived its subject, which is
	// the failure the shared constant was adopted to prevent, arriving from the
	// other direction. So the section states what is still true: authoring
	// happens somewhere else.
	//
	// The refusal constant is NOT retired. It remains this deployment's answer
	// when the admission plane is absent — no database, or 0107 unapplied — and
	// it is still the sentence those endpoints return.
	serviceDescriptorsElsewhereUnavailable = "provider registrations are administered on their own page " +
		"(/admin/app/service-descriptors), not through this form: a registration is a descriptor plus an " +
		"admission decision, and this page is a flat form over a single value document. Registrations recorded " +
		"there are stored and listed; a recorded descriptor is NOT in force until an admission overlay is issued, " +
		"and its page says which state each one is in."

	// skillPublishValidationRulesUnavailable is the skill-side twin of
	// publishValidationRulesUnavailable below, and it is withheld for the same
	// reason and at the same level: the FIELD, not the section.
	//
	// `internal/api/v2/skillpublish/validate.go` is deterministic end to end —
	// version-name collisions, empty instructions, the draft/published status —
	// and says so on the wire (`ai_validation_available: false`). The reference
	// feeds this textarea to an LLM evaluator that this service has no transport
	// for (#126/#194), so a custom rules prompt would have nothing to reach.
	skillPublishValidationRulesUnavailable = "publish validation for skills in this service is deterministic " +
		"(version name collisions, empty instructions, publish status); there is no AI evaluator for custom " +
		"criteria to reach, so these rules would never be applied."

	// The support assistant section USED TO carry an `unavailable_reason` here,
	// saying that the switch had a wire (`GET /support_assistant/config`) and no
	// rendered consumer: `SupportAssistantWidget` had no render site and
	// `@eliteaai/elitea-assistant` was not a dependency, so enabling it "would
	// change a flag no rendered surface reads".
	//
	// Both halves are now false, which is why the constant is DELETED rather
	// than kept for reuse. The widget is mounted in `AppShell`, ported into
	// `apps/elitea-web/src/widgets/support-assistant/` rather than taken as a
	// dependency (the published package streams over socket.io, which this
	// service does not serve), and the switch is read by
	// `internal/api/v2/supportassistant`, which serves the whole surface —
	// config, conversations, attachments and one agent turn per question.
	//
	// Deleting the constant is deliberate: the next section that wants to be
	// withheld has to state what reads it before it can be written.

	// authProvidersElsewhereUnavailable — the Authentication section is now a
	// pointer to a real surface, in the same way `mcp_servers` is.
	//
	// The reason it is not a form HERE has not changed and cannot: every field
	// it declares addresses a key inside a pylon plugin's YAML, and two of them
	// (`client_secret`, and a SAML service provider key) are credentials that
	// `rejectCredentialField` refuses into a plaintext `centry.platform_config`
	// row — correctly, because every holder of `runtime.plugins` can read those
	// rows. A flat list of field values also cannot express "this document is
	// an OIDC provider and these are its invariants", which is the shape the
	// configuration provenance specification requires by name.
	//
	// What HAS changed is that the values now have somewhere to go and
	// something that reads them. `elitea_auth.identity_providers` (shared
	// migration 0095) holds one typed revision per provider, the admin surface
	// at `/admin/identity_providers/administration` authors it, and the browser
	// login path resolves the enabled definition on every login.
	//
	// FORM USERS ARE NOT PART OF THAT, and this says so. The form provider's
	// user list is a file the deployment mounts
	// (`authcomposition.FormProviderConfig.UsersJSONFile`), read once at
	// startup after the deployment's own environment and vault resolution. It
	// is not a row this service may rewrite, and an editor here would present
	// a control whose saves the next restart discards.
	authProvidersElsewhereUnavailable = "identity providers are authored on the Authentication editor on this page, " +
		"which writes typed OIDC and SAML definitions rather than plugin configuration. The plugin-config value " +
		"endpoints cannot serve this section: two of its fields are credentials, and those are sealed in the " +
		"platform vault instead of stored in a settings row. Form users stay in the deployment's mounted user file " +
		"and are not editable here."

	// emailElsewhereUnavailable — the E-mail section is a pointer to a real
	// surface, in the same way `auth` and `mcp_servers` are (gap G7).
	//
	// Outbound mail used to have no section at all: the transport was read from
	// the environment at boot (SMTP_HOST and seven siblings), so an operator
	// with no access to the deployment's environment could not turn e-mail on,
	// and every invitation answered `invitation_delivered: false`. The values
	// now live in `centry.platform_config`'s `email` section and the resolver
	// reads them per send.
	//
	// The plugin-config VALUE endpoints still cannot serve them, and that will
	// not change: the SMTP password is a credential, and
	// `rejectCredentialField` refuses a credential into a plaintext row every
	// holder of `runtime.plugins` can read. The document also has invariants a
	// flat value list cannot express — a user name without a password is a
	// session the relay refuses at AUTH. So the section says where the real
	// editor is, and declares no fields of its own.
	emailElsewhereUnavailable = "outbound e-mail is configured on the E-mail editor, which stores the relay " +
		"settings and seals the SMTP password in the platform vault. The plugin-config value endpoints cannot " +
		"serve this section: the password is a credential, and it is sealed rather than stored in a settings row. " +
		"The environment variables (SMTP_HOST and its siblings) remain the bootstrap defaults, and the editor " +
		"shows which layer decides each field."

	// publishValidationRulesUnavailable is a FIELD-level reason, not a section
	// one. The rest of `agent_publishing` is enforced for real; this one field
	// alone has nothing behind it. `runPublishValidation` in
	// internal/api/v2/eliteacore/handler.go is entirely deterministic — version
	// name collisions, generic names, sub-agent cycles and depth — with no model
	// call anywhere in it, so a custom evaluation prompt has no evaluator to
	// reach. A section-level reason would have withheld three working controls
	// to disclose one broken one.
	publishValidationRulesUnavailable = "publish validation in this service is deterministic (name collisions, " +
		"sub-agent cycles and depth); there is no AI evaluator for custom criteria to reach, so these rules would " +
		"never be applied."
)

// Which page a section belongs to.
//
// The reference decides this IN THE CLIENT: `FeaturesPage.jsx` hardcodes a list
// of six sections and `ConfigurationPage.jsx` hardcodes the complementary
// `MOVED_TO_FEATURES` array plus three config-path prefixes to subtract from
// Guardrails. Two client-side lists that must stay each other's complement is a
// drift waiting to happen, and it is the same mistake #217 removed when it moved
// `service_descriptors` out of the client's section list and onto the server.
//
// So placement is declared here, next to the fields, and both pages filter on
// it. A section with no `page` belongs to Configuration — the default keeps the
// eight sections that were already there exactly where they were.
const (
	configPageFeatures = "features"
)

// ## The Advanced section is GONE, not withheld
//
// It used to be declared here with an `unavailable_reason` saying that it edits
// raw plugin YAML, tails pylon logs and reloads plugins on live Pylon runtimes,
// and that those runtimes are not part of this platform. Every word of that was
// true, and it is why the section is now absent instead.
//
// A withheld section is a promise: it tells an operator that the capability
// belongs on this page and is coming. Advanced is the one section on this page
// whose subject the target architecture removes ON PURPOSE — AGENTS.md names
// Pylon plugin loading and the Arbiter bus as things this platform does not
// preserve — so there is nothing to arrive. Leaving the row in the sidebar
// spends a permanent line of the operator's attention on a control that will
// never exist, which is the same failure mode as a form that saves into a void,
// one step removed.
//
// `configuration.advanced` therefore leaves declaredPermissions() with it
// (roles.go). A deployment whose legacy roles still grant the name keeps it in
// the matrix — the catalogue is the union of granted and declared — so no
// existing grant is invalidated; the name simply stops being one this service
// declares a gate for.
//
// ## Observability, Runtime and Admin Panel are GONE too, for the same reason
//
// All three carried the generic "these settings configure Pylon plugin
// runtimes" reason, and in every one of the three that reason was permanent
// rather than a gap this platform intended to close:
//
//   - `observability` held two fields. `tracing_enabled` is owned by each
//     process's own environment (OTEL_SDK_DISABLED / OTEL_EXPORTER_OTLP_ENDPOINT,
//     read independently by four separate binaries —
//     libs/go/observability/observability.go), so a single DB row can never
//     govern it without becoming a second, disagreeing source of truth.
//     `audit_trail_enabled` toggled a writer that does not exist:
//     centry.audit_events has only readers in this repo — see the "READ-ONLY
//     from this service. Never emitted today." note on
//     internal/api/generated/api.gen.go's audit fields. There was nothing this
//     platform could ever wire either field to.
//   - `runtime` held ten fields across three owners, none of them this page.
//     `ai_project_id` is read from the environment independently by elitea-main
//     AND by the LLM gateway (services/elitea-llm-gateway/internal/config —
//     a SEPARATE PROCESS that cannot see a DB row this service writes).
//     `ai_project_allowed_domains` is deployment-mounted YAML read once at
//     startup (internal/authcomposition/config.go). The four
//     `*_scheduling_{enabled,cron}` fields are centry.schedule rows the admin
//     "Schedules & Tasks" page already edits
//     (internal/api/v2/scheduling/schedules.go). The rest belong to
//     pylon-indexer's mounted YAML. A save on this page could reach none of
//     them.
//   - `admin_panel` declared zero fields — the same subject as Advanced above,
//     with nothing even to describe.
//
// The one field worth keeping, `analytics_enabled`, was never actually
// Pylon-plugin-shaped: it is a `centry.platform_config` flag of exactly the
// same kind as `mcp_in_menu` or the voice-feature switches, and this platform
// already has the whole chain to serve it. It now lives on the Features page —
// see analyticsSection() below and platformconfig.KeyAnalyticsEnabled.

func configSections() []map[string]any {
	return []map[string]any{
		guardrailsSection(),
		mcpConfigurationSection(),
		agentPublishingSection(),
		skillPublishingSection(),
		mcpServersSection(),
		llmProxySection(),
		governanceSection(),
		authSection(),
		resourcesSection(),
		dedicatedBannerSection(),
		brandingSection(),
		supportAssistantSection(),
		voiceFeaturesSection(),
		serviceDescriptorsSection(),
		maintenanceSection(),
		analyticsSection(),
		emailSection(),
	}
}

// serviceDescriptorsSection is declared HERE rather than appended by the page.
//
// The reference client injects `{id: "service_descriptors", title: …}` into the
// section list itself, so the sidebar shows an entry the server never described.
// Every other section — including its permission and, now, its availability — is
// server-declared, and one that is not is one the page can get wrong on its own.
func serviceDescriptorsSection() map[string]any {
	return map[string]any{
		"id":                  "service_descriptors",
		"unavailable_reason":  serviceDescriptorsElsewhereUnavailable,
		"title":               "Service Descriptors",
		"description":         "Registered provider service descriptors.",
		"order":               8,
		"icon":                "settings_input_component",
		"always_visible":      true,
		"required_permission": "configuration.service_descriptors",
		"fields":              []map[string]any{},
	}
}

// guardrailsSection — the platform-wide toolkit security policy. LIVE.
//
// Every field here is read, and the reason the section carried an
// unavailable_reason until now was that none of them were. The four
// consumers, in the order an operator meets them:
//
//   - the toolkit TYPE catalogue (`/toolkits`, `/toolkit_types`,
//     `/toolkit_available_tools`, `/toolkit_discover_tools`) drops blocked types
//     and blocked tools, so a blocked toolkit cannot be picked;
//   - the toolkit WRITE paths (`Create`, `Update`, `ForkToolkit`) refuse a
//     blocked type with a 403 that names it, so it cannot be created by a client
//     that skipped the form;
//   - the agent tool FREEZE (internal/application/agentexecution/tools.go)
//     strips blocked toolkits and tools out of the execution input. This is the
//     load-bearing one: an agent version saved before a toolkit was blocked
//     still names it, and the freeze is the only point a running agent cannot
//     route around;
//   - `GET /elitea_core/platform_settings/prompt_lib` publishes
//     `blocked_toolkits` so the product UI can mark an existing toolkit blocked.
//
// The sensitive-action fields are stored and served here, and reach the worker's
// SensitiveToolGuardMiddleware through the agent execution input. Note the
// enforcement asymmetry that the descriptions below have to be honest about:
// catalogue and write-path enforcement are unconditional, while the freeze runs
// only where the runtime plane is enabled.
func guardrailsSection() map[string]any {
	return map[string]any{
		"id":          "guardrails",
		"title":       "Guardrails",
		"description": "Control platform-wide security policies, toolkit restrictions, and MCP exposure settings.",
		"order":       1,
		"icon":        "security",
		"fields": []map[string]any{
			{
				"key":         "blocked_toolkits",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "Blocked Toolkits",
				"description": "Toolkit types disabled platform-wide. A blocked type cannot be listed or created, and is stripped out of every agent before it runs. Toolkits of this type that already exist stay visible to administrators so they can be reviewed and deleted \u2014 blocking stops them working, it does not remove them or their stored credentials.",
				"path":        "toolkit_security.blocked_toolkits",
				"section":     "guardrails",
				"default":     []any{},
				"enum_source": "toolkit_names",
			},
			{
				"key":                  "blocked_tools",
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"title":                "Blocked Tools",
				"description":          "Individual tools blocked inside otherwise-allowed toolkits. Map of toolkit type to blocked tool names. Matching ignores case and separators, so 'Create File', 'create_file' and 'create-file' are one entry. Scoped to the named toolkit: blocking 'create_file' under github leaves other toolkits' identically-named tools alone.",
				"path":                 "toolkit_security.blocked_tools",
				"section":              "guardrails",
				"default":              map[string]any{},
				"enum_source_keys":     "toolkit_names",
				"enum_source_values":   "toolkit_tools",
			},
			{
				"key":                  "sensitive_tools",
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"title":                "Sensitive Action Tools",
				"description":          "Tools that require the user to authorize each call before it runs. Map of toolkit type to tool names; the key '*' applies to every toolkit. Enforced by the agent runtime, so it takes effect where agent execution is enabled.",
				"path":                 "toolkit_security.sensitive_tools",
				"section":              "guardrails",
				"default":              map[string]any{},
				"enum_source_keys":     "toolkit_names",
				"enum_source_values":   "toolkit_tools",
			},
			{
				"key":         "sensitive_action_company_name",
				"type":        "string",
				"title":       "Company Name for Policy Message",
				"description": "Company name shown in the authorization dialog. Left blank, the runtime's own default (\"Your organization\") is used.",
				"path":        "toolkit_security.sensitive_action_company_name",
				"section":     "guardrails",
				"default":     "",
			},
			{
				"key":         "sensitive_action_message_template",
				"type":        "string",
				"title":       "Authorization Message Template",
				"description": "Message shown in the authorization dialog. Supports the placeholders {company_name}, {action_name}, {tool_name} and {toolkit_name}. Left blank, the runtime's own default wording is used.",
				"path":        "toolkit_security.sensitive_action_message_template",
				"section":     "guardrails",
				"default":     "",
			},
			// `mcp_exposure.*` and `publishing_guardrail.*` used to be declared
			// here. They are the fields the reference relocates onto the Features
			// page by config-path prefix, and they are the only fields in this
			// section this platform actually reads — so they now live in
			// `mcpConfigurationSection()` and `agentPublishingSection()`, which
			// carry no `unavailable_reason`. Leaving copies here would have been
			// two declarations of one setting, and the unavailable one would have
			// told the operator the opposite of the truth.
		},
	}
}

// mcpConfigurationSection — the Features page's MCP switches. LIVE.
//
// This is the master switch pylon exposes as `mcp_exposure.enabled`, read into
// module state at plugin load and consulted by every MCP surface it has
// (legacy/plugins/elitea_core/utils/mcp_config.py, and its callers in
// mcp_dcr_proxy, mcp_oauth_proxy, mcp_sync_tools and routes/mcp_sse).
//
// It is available here because this platform now reads it in both of the places
// that matter:
//
//   - `GET /elitea_core/platform_settings/prompt_lib` marshals `mcp_enabled` and
//     `mcp_in_menu_enabled` from these rows, and apps/elitea-web's four
//     `useIsMcpVisible` hooks and its `/mcps` route gate on them;
//   - the three MCP proxy routes (`mcp_oauth_proxy`, `mcp_dcr_proxy`,
//     `mcp_sync_tools`) refuse with 403 when the master switch is off, which is
//     what "removes all MCP-related functionality … including API endpoints"
//     has to mean. A switch that only hides the buttons is not a kill switch,
//     and an operator who read that description and turned it off would believe
//     they had closed the API.
func mcpConfigurationSection() map[string]any {
	return map[string]any{
		"id":          "mcp_configuration",
		"page":        configPageFeatures,
		"title":       "MCP Configuration",
		"description": "Control Model Context Protocol exposure across the platform.",
		"order":       10,
		"icon":        "extension",
		"fields": []map[string]any{
			{
				"key":         "mcp_enabled",
				"type":        "boolean",
				"title":       "Enable MCP",
				"description": "Master switch for MCP (Model Context Protocol). When disabled, the MCP proxy and tool-sync endpoints refuse with 403 and every MCP entry point is hidden.",
				"path":        "mcp_exposure.enabled",
				"section":     "mcp_configuration",
				"default":     true,
				// No `requires_restart`. The reference marks both fields as
				// needing one because a pylon reads them into module state at
				// plugin load; here every consumer reads the row per request, so
				// the change is live on the next call. Carrying the flag over
				// would have asked the operator to press a reload button that
				// answers 501.
			},
			{
				"key":         "mcp_in_menu",
				"type":        "boolean",
				"title":       "Show MCPs in UI",
				"description": "When disabled, hides MCP entry points in the UI while leaving the MCP API endpoints working.",
				"path":        "mcp_exposure.in_menu",
				"section":     "mcp_configuration",
				"default":     true,
				// Rendered only while the master switch is on: "hide the menu
				// entry" is not a meaningful choice once the whole subsystem is
				// off, and showing it would imply the two combine in some way
				// they do not.
				"visible_when": map[string]any{"field": "mcp_enabled", "value": true},
			},
		},
	}
}

// agentPublishingSection — the Features page's publishing guardrail. LIVE,
// except for one field that says why it is not.
//
// `is_publish_blocked` and `publish_whitelist_project_ids` are enforced in
// `POST /elitea_core/publish/prompt_lib/{projectID}/{versionID}` — before this
// unit that handler validated the version name, the agent type and the publish
// status and never once asked whether publishing was blocked, so the switch the
// reference page offers had no effect on the only publish path this service has.
//
// `agent_categories` is merged into `GET /elitea_core/agent_categories/…`, which
// apps/elitea-web's `useAgentHubData` reads for the Agents Hub filter bar.
func agentPublishingSection() map[string]any {
	return map[string]any{
		"id":          "agent_publishing",
		"page":        configPageFeatures,
		"title":       "Agent Publishing",
		"description": "Control who may publish agents, and which categories they may publish into.",
		"order":       11,
		"icon":        "publish",
		"fields": []map[string]any{
			{
				"key":         "is_publish_blocked",
				"type":        "boolean",
				"title":       "Block Agent Publishing",
				"description": "When enabled, agent publishing is refused platform-wide except from the projects listed below.",
				"path":        "publishing_guardrail.is_publish_blocked",
				"section":     "agent_publishing",
				"default":     false,
			},
			{
				"key":         "publish_whitelist_project_ids",
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"title":       "Publishing Allowed Projects",
				"description": "Projects where publishing remains allowed while it is blocked globally. If empty, publishing is blocked everywhere.",
				"path":        "publishing_guardrail.whitelist_project_ids",
				"section":     "agent_publishing",
				"default":     []any{},
				"visible_when": map[string]any{
					"field": "is_publish_blocked", "value": true,
				},
			},
			{
				"key":         "agent_categories",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "Agent Categories",
				"description": "Additional agent categories offered alongside the built-in defaults. The built-in defaults cannot be removed here.",
				"path":        "publishing_guardrail.agent_categories",
				"section":     "agent_publishing",
				"default":     []any{},
			},
			{
				"unavailable_reason": publishValidationRulesUnavailable,
				"key":                "publish_validation_rules",
				"type":               "string",
				"format":             "textarea",
				"title":              "Publish Validation Rules",
				"description":        "Custom evaluation criteria for AI validation of agents before publishing.",
				"path":               "publishing_guardrail.publish_validation_rules",
				"section":            "agent_publishing",
				"default":            "",
			},
		},
	}
}

// skillPublishingSection — LIVE, where it used to be withheld.
//
// It was withheld because the subsystem it governs did not exist: there was no
// skill publish endpoint, no public skill catalog and no skill categories
// route, so every field would have been a control over nothing. #267 built all
// three (`internal/api/v2/skillpublish`), which left this section as the last
// missing half — the guardrail was enforced against the AGENT section's switch
// because a skill-specific one had nowhere to be authored, and the catalog's
// category list was the nine hardcoded defaults with no way to add a tenth.
//
// The three fields below are the reference's own
// (`skill_publishing_guardrail.*` in elitea_core's admin_schema.json), keeping
// its field KEYS rather than the agent section's: `is_skill_publish_blocked`
// and `skill_publish_whitelist_project_ids` are the names the reference's
// platform_settings endpoint publishes, and the product UI reads them from
// there, so renaming them here would cost a translation layer on the wire for
// no gain.
//
// The fourth reference field, `skill_publish_validation_rules`, carries its own
// reason instead of being omitted — the same treatment
// `publish_validation_rules` gets on the agent section, for the same cause.
func skillPublishingSection() map[string]any {
	return map[string]any{
		"id":          "skill_publishing",
		"page":        configPageFeatures,
		"title":       "Skill Publishing",
		"description": "Control who may publish skills, and which categories they may publish into.",
		"order":       12,
		"icon":        "bolt",
		"fields": []map[string]any{
			{
				"key":         "is_skill_publish_blocked",
				"type":        "boolean",
				"title":       "Block Skill Publishing",
				"description": "When enabled, skill publishing is refused platform-wide except from the projects listed below. Admin publishes from the public project are always exempt.",
				"path":        "skill_publishing_guardrail.is_publish_blocked",
				"section":     "skill_publishing",
				"default":     false,
			},
			{
				"key":         "skill_publish_whitelist_project_ids",
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"title":       "Skill Publishing Allowed Projects",
				"description": "Projects where skill publishing remains allowed while it is blocked globally. If empty, skill publishing is blocked everywhere.",
				"path":        "skill_publishing_guardrail.whitelist_project_ids",
				"section":     "skill_publishing",
				"default":     []any{},
				"visible_when": map[string]any{
					"field": "is_skill_publish_blocked", "value": true,
				},
			},
			{
				"key":         "skill_categories",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "Skill Categories",
				"description": "Additional skill categories offered in the skill publish dialog and the public catalog filter, alongside the built-in defaults. Managed independently from Agent Categories. The built-in defaults cannot be removed here.",
				"path":        "skill_publishing_guardrail.skill_categories",
				"section":     "skill_publishing",
				"default":     []any{},
			},
			{
				"unavailable_reason": skillPublishValidationRulesUnavailable,
				"key":                "skill_publish_validation_rules",
				"type":               "string",
				"format":             "textarea",
				"title":              "Skill Publish Validation Rules",
				"description":        "Custom evaluation criteria for AI validation of skills before publishing.",
				"path":               "skill_publishing_guardrail.publish_validation_rules",
				"section":            "skill_publishing",
				"default":            "",
			},
		},
	}
}

// mcpServersUnavailable sends this section's caller to the surface that really
// holds the catalogue.
//
// This section used to carry the generic "these settings configure Pylon
// plugin runtimes" reason, and that WAS true of it: the values addressed the
// indexer_worker plugin descriptor, collected over
// the Arbiter bus, and nothing here read them. It is no longer the whole truth,
// because the catalogue itself now exists — shared migration 0094,
// `internal/mcpregistry` and the three routes in mcp_prebuilt.go — so a reader
// told only "Pylon plugin runtimes" would conclude the feature is missing when
// it is merely somewhere else.
//
// The section stays unavailable rather than becoming writable for one reason,
// and it is the same reason rejectCredentialField exists: a catalogue entry
// carries a client secret, and this page's fields are stored as plaintext JSONB
// rows in centry.platform_config that every holder of `runtime.plugins` can
// read. The dedicated surface seals the secret into the platform vault and
// returns a mask. Serving the same data here would mean either storing that
// credential in clear text or accepting a save that silently drops it.
const mcpServersUnavailable = "MCP server definitions are not stored as plugin configuration on this " +
	"platform: an entry carries a client secret, and the values on this page are kept as plaintext " +
	"rows readable by everyone who can open it. The catalogue lives at " +
	"/api/v2/admin/mcp_prebuilt_servers/administration, which seals each client secret into the " +
	"platform vault and never returns it. It takes effect on the next call rather than on the next " +
	"restart, so no field here carries requires_restart."

func mcpServersSection() map[string]any {
	return map[string]any{
		"id": "mcp_servers",
		// managed_surface names the dedicated surface that really holds this
		// section's data, so the client can render the right editor WITHOUT a
		// hardcoded list of section ids.
		//
		// That distinction is the whole reason this key exists on the server.
		// The reference keeps section placement in two client-side lists that
		// have to stay each other's exact complement by hand, and #217 already
		// corrected one instance of it. A client that special-cased
		// `id == "mcp_servers"` would be the same mistake in a new place: the
		// server would move the catalogue and the client would go on rendering
		// an editor for a surface that no longer exists.
		//
		// `unavailable_reason` STAYS. It is still true of the plugin-config
		// value endpoints, which cannot serve this section — a client that
		// cannot render the managed surface must still be told why the ordinary
		// form is missing, and the two value endpoints must keep refusing.
		"managed_surface":    "mcp_prebuilt_servers",
		"unavailable_reason": mcpServersUnavailable,
		"title":              "MCP Servers",
		"description":        "Configure Model Context Protocol server definitions available to the indexer runtime.",
		"order":              2,
		"icon":               "dns",
		// No fields. The one field this section declared was the whole
		// `mcp_servers` object, and declaring it here would describe a control
		// that this page cannot serve — the same "renders as a working control
		// and is not one" failure rejectUnavailableField was written for.
		"fields": []map[string]any{},
	}
}

// analyticsSection — the Features page's Analytics visibility switch. LIVE.
//
// This used to be a field on the withheld `observability` section
// (`analytics.enabled`), which meant it inherited that section's blanket
// "Pylon plugin runtime" refusal even though it was never plugin-shaped: it is
// a `centry.platform_config` row of exactly the same kind as `mcp_in_menu` or
// the voice-feature switches. It moves here because this platform has the
// whole chain for that kind of flag already —
// internal/platformconfig.KeyAnalyticsEnabled, the reader in
// internal/api/v2/eliteacore/platform_flags.go marshalled into
// GET /elitea_core/platform_settings/…, and apps/elitea-web's Settings page
// hides its Analytics tab on the same answer.
//
// It is also ENFORCED, not just hidden: `internal/api/router.go` gates the
// `/analytics*` and `/analytics_costs` routes on the same flag through
// requireAnalyticsEnabled, the same shape as MCP's requireMCPEnabled. A hidden
// tab over an open endpoint would be no gate at all.
func analyticsSection() map[string]any {
	return map[string]any{
		"id":          "analytics",
		"page":        configPageFeatures,
		"title":       "Analytics",
		"description": "Control whether the Analytics tab is visible in project Settings.",
		"order":       16,
		"icon":        "monitoring",
		"fields": []map[string]any{
			{
				"key":         "analytics_enabled",
				"type":        "boolean",
				"title":       "Show Analytics",
				"description": "Controls whether the Analytics tab is visible in project Settings. When disabled, the Analytics page is hidden for all users and the analytics endpoints refuse.",
				"path":        "analytics.enabled",
				"section":     "analytics",
				"default":     true,
			},
		},
	}
}

// llmProxyUnavailable explains why the ordinary plugin-config form cannot
// serve this section.
//
// It is a different sentence from the generic "these settings configure Pylon
// plugin runtimes" reason on purpose. That reason says "these values address a
// Pylon plugin descriptor and nothing here reads them", which was true of the
// LiteLLM section this replaces and is not
// true of anything on this one: every value the LLM Proxy section shows is read,
// and most of it is read on the billed request path. The reason it is not a form
// is narrower — the surface is three unrelated shapes (a live status report, a
// price catalogue keyed on provider+model, and one global alert setting), and a
// flat form over one value document cannot express any of them.
const llmProxyUnavailable = "the LLM proxy is not configured as plugin values on this platform. Its runtime " +
	"status is read from the gateway itself, its model prices are rows in gateway.gateway_models keyed on " +
	"provider and model, and neither is a field in a settings document. The dedicated editor at " +
	"/api/v2/admin/gateway serves all three; this page's value endpoints cannot."

// llmProxySection is the admin surface for Elitea's LLM gateway.
//
// ## What replaced what
//
// This section stands where `litellmSection()` stood. LiteLLM is gone — ADR-0015
// replaced it with `services/elitea-llm-gateway`, a standalone service built on
// maximhq/bifrost's core — and every field the old section declared described
// that removed subsystem: which LiteLLM to talk to, its master key, its database,
// and three action buttons that reconciled teams and keys inside it. None of them
// has a referent any more, so none of them is carried over. A setting whose
// subject no longer exists is not a setting an operator should be shown.
//
// What replaces them is not a translation of those fields. It is the subset of
// Bifrost's own admin UI that this platform can actually back with real data:
//
//   - **Runtime status** — Bifrost's Observability/health view. Ours reports what
//     the gateway HOLDS: the loaded governance snapshot, its age, rows that were
//     rejected or are inert, and whether rate limits are enforceable at all.
//     Nothing else in this platform can tell an operator that a saved rule is not
//     in force.
//   - **Model catalogue and pricing** — Bifrost's Model Catalog and Pricing
//     Overrides. `gateway.gateway_models` is the cost basis for every billed
//     request, and a model missing from it is billed at a rate the gateway
//     invents rather than at a real one, so this is the one screen where a
//     missing row is a silent, ongoing billing fault.
//   - **Budget alerting** — the global soft-alert threshold. The endpoints have
//     existed since #322 with nothing calling them.
//
// Deliberately absent, because this platform cannot back them honestly: Bifrost's
// per-request Logs (no request-log store exists — `llm_usage_events` carries
// billing dimensions only, with no latency, status or payload), Virtual Keys (the
// Bifrost VK slot carries the Elitea project id; there is no key to mint or
// rotate), and provider/key CRUD (provider credentials are per-project
// `ai_credentials` rows in `p_{id}.configuration` sealed in the Fernet vault, not
// global gateway config — they are authored per project, not here).
//
// ## Why it carries no fields
//
// `managed_surface` routes the client to the dedicated editor, exactly as
// mcpServersSection() does. Declaring fields as well would describe controls the
// plugin-config value endpoints cannot serve, which is the failure
// rejectUnavailableField exists to prevent.
func llmProxySection() map[string]any {
	return map[string]any{
		"id": "llm_proxy",
		// The surface name, not the section id, is what the client keys its
		// editor registry on — see mcpServersSection() for why that distinction
		// is load-bearing.
		"managed_surface":    "llm_proxy",
		"unavailable_reason": llmProxyUnavailable,
		"title":              "LLM Proxy",
		"description": "Elitea's LLM gateway: runtime enforcement status, the model price catalogue that " +
			"every billed request is costed against, and the global budget-alert threshold.",
		"order": 4,
		"icon":  "hub",
		// No fields: this section's data is three shapes, none of them a value
		// in a settings document. See the doc comment above.
		"fields": []map[string]any{},
	}
}

// governanceSection describes the LLM-gateway governance authoring surface
// (design-governance-config-authoring §2-3): budgets, rate limits, per-credential
// rate_policy, per-model/provider scopes, the MCP allowlist, and CEL routing
// rules. The renderer maps each field spec to a widget (§2); the routing_rules
// array is detected by SchemaField.jsx and handed to the purpose-built
// RoutingRuleEditor. required_permission hides the section client-side, but the
// authorization boundary is the server-side RequirePermissions wrapper on the
// governance CRUD routes (§4) — this attribute is convenience only.
//
// All values authored here are DEFINITIONS written to the global
// gateway.governance_config table, and the gateway now enforces them.
//
// The history matters, because this comment has been wrong in both directions.
// It first asserted enforcement that did not exist while
// governanceElsewhereUnavailable (this file, ~line 70) correctly denied it;
// #466 removed the false assertion, and an operator had by then authored a rule
// and believed a limit was in force. #218 closed the gap the other way round —
// the gateway reads the table now (elitea-llm-gateway internal/policy), so the
// two statements agree again, on the other value.
//
// Budget limits are USD numbers (§5.1). The gateway scales them to nano-USD at
// the counter boundary and nowhere earlier; the three denominations in §5.1 are
// not interchangeable.
//
// FOUR CONTROLS ARE ENFORCED WITH A CAVEAT, and the caveat belongs with the
// schema rather than in a release note:
//
//   - A token rate limit is applied to the request AFTER the one that crossed
//     it, because a request's token cost is unknown until the provider answers.
//   - Rate limits need the gateway's NATS counter. Without it they load and do
//     nothing; the gateway's GET /governance/status reports that.
//   - `scope.team_ids` is not offered and is rejected on write: this platform
//     has no teams for it to name.
//   - A CEL rule may not reference team_id, tokens_used, complexity_tier or
//     headers. The gateway cannot supply them, and a rule that names one is
//     refused here rather than accepted and never matched — see
//     unevaluableCELVariables in routing_cel.go.
//
// The claim in this comment, in the `description` below and in
// governanceElsewhereUnavailable are pinned together by the guards in
// config_schemas_claims_internal_test.go. Keep all three true at once.
func governanceSection() map[string]any {
	return map[string]any{
		"id":                  "governance",
		"unavailable_reason":  governanceElsewhereUnavailable,
		"title":               "LLM Governance",
		"description":         "Author LLM-gateway governance: budgets, rate limits, credential billing policy, per-model/provider scopes, MCP allowlists, and CEL routing rules. Definitions are enforced by the LLM gateway on every request.",
		"order":               5,
		"icon":                "policy",
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			// --- Budget (project/team/customer/global) ---
			{
				"key":         "budget_is_unlimited",
				"type":        "boolean",
				"title":       "Unlimited Budget",
				"description": "When enabled, no spend limit is enforced and the fields below are ignored.",
				"path":        "budget.is_unlimited",
				"section":     "governance",
				"default":     true,
			},
			{
				"key":          "budget_limit_usd",
				"type":         "number",
				"title":        "Budget Limit (USD)",
				"description":  "Hard spend limit in US dollars per period. Authored in USD; the gateway scales it to nano-USD for counter comparison.",
				"path":         "budget.limit_usd",
				"section":      "governance",
				"default":      nil,
				"minimum":      0,
				"visible_when": map[string]any{"field": "budget_is_unlimited", "value": false},
			},
			{
				"key":          "budget_period",
				"type":         "string",
				"title":        "Budget Period",
				"description":  "The window over which the budget limit accumulates before resetting.",
				"path":         "budget.period",
				"section":      "governance",
				"default":      "monthly",
				"enum":         []string{"daily", "weekly", "monthly", "yearly"},
				"visible_when": map[string]any{"field": "budget_is_unlimited", "value": false},
			},
			{
				"key":          "budget_soft_alert_pct",
				"type":         "integer",
				"title":        "Soft Alert Threshold (%)",
				"description":  "Budget-utilisation percentage (1-100) at which a soft alert is emitted before the hard limit is reached.",
				"path":         "budget.soft_alert_pct",
				"section":      "governance",
				"default":      80,
				"minimum":      1,
				"maximum":      100,
				"visible_when": map[string]any{"field": "budget_is_unlimited", "value": false},
			},
			{
				"key":         "budget_nats_fail_mode",
				"type":        "string",
				"title":       "Budget Fail Mode",
				"description": "How the gateway behaves when the NATS budget counter is unavailable. tiered_hybrid degrades gracefully; fail_open allows traffic; fail_closed blocks it.",
				"path":        "budget.nats_fail_mode",
				"section":     "governance",
				"default":     "tiered_hybrid",
				"enum":        []string{"tiered_hybrid", "fail_open", "fail_closed"},
			},
			// --- Rate limits (token + request buckets) ---
			{
				"key":         "rate_limit_tokens_per_min",
				"type":        "integer",
				"title":       "Token Rate Limit (per minute)",
				"description": "Maximum tokens allowed per minute. Leave empty for no token rate limit.",
				"path":        "rate_limit.tokens_per_min",
				"section":     "governance",
				"default":     nil,
				"minimum":     0,
			},
			{
				"key":         "rate_limit_requests_per_min",
				"type":        "integer",
				"title":       "Request Rate Limit (per minute)",
				"description": "Maximum requests allowed per minute. Leave empty for no request rate limit.",
				"path":        "rate_limit.requests_per_min",
				"section":     "governance",
				"default":     nil,
				"minimum":     0,
			},
			// --- Per-credential billing policy ---
			{
				"key":         "rate_policy",
				"type":        "string",
				"title":       "Credential Rate Policy",
				"description": "Billing treatment for a credential's usage. billed: normal cost accounting. zero-rate-metered: usage recorded at zero cost. excluded: usage recorded as excluded, budget counters untouched.",
				"path":        "credential.rate_policy",
				"section":     "governance",
				"default":     "billed",
				"enum":        []string{"billed", "zero-rate-metered", "excluded"},
			},
			// --- Per-model / per-provider scope ---
			{
				"key":         "scope_providers",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "Scoped Providers",
				"description": "Providers this governance entry applies to. Empty means all providers.",
				"path":        "scope.providers",
				"section":     "governance",
				"default":     []any{},
				"enum_source": "gateway_providers",
			},
			{
				"key":         "scope_models",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "Scoped Models",
				"description": "Models this governance entry applies to. Empty means all models.",
				"path":        "scope.models",
				"section":     "governance",
				"default":     []any{},
				"enum_source": "gateway_models",
			},
			{
				"key":         "scope_project_ids",
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"title":       "Scoped Projects",
				"description": "Projects this governance entry applies to. Empty means all projects.",
				"path":        "scope.project_ids",
				"section":     "governance",
				"default":     []any{},
				"enum_source": "projects",
			},
			// --- MCP allowlist ---
			{
				"key":         "mcp_allowlist",
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"title":       "MCP Server Allowlist",
				"description": "MCP server ids permitted through the gateway. Empty disables the allowlist (all servers permitted).",
				"path":        "mcp.allowlist",
				"section":     "governance",
				"default":     []any{},
			},
			// --- CEL routing rules (routed to RoutingRuleEditor by SchemaField) ---
			{
				"key":         "routing_rules",
				"type":        "array",
				"format":      "routing_rules",
				"title":       "CEL Routing Rules",
				"description": "Weighted routing rules. Each rule is a CEL predicate plus a weighted provider/model target list whose weights sum to 1.0. The server compiles the CEL and re-verifies the weight sum on save.",
				"path":        "routing.rules",
				"section":     "governance",
				"default":     []any{},
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cel":      map[string]any{"type": "string"},
						"scope":    map[string]any{"type": "string"},
						"priority": map[string]any{"type": "integer"},
						"targets": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"provider": map[string]any{"type": "string"},
									"model":    map[string]any{"type": "string"},
									"weight":   map[string]any{"type": "number"},
								},
							},
						},
					},
				},
			},
			{
				"key":         "validate_cel",
				"type":        "action",
				"title":       "Validate CEL",
				"description": "Compile the routing-rule CEL expression against the gateway's governance environment without saving.",
				"section":     "governance",
				"action_task": "validate_cel",
			},
		},
	}
}

func authSection() map[string]any {
	return map[string]any{
		"id": "auth",
		// managed_surface names the dedicated surface that really holds this
		// section's data, so the client renders the right editor WITHOUT a
		// hardcoded list of section ids. See mcpServersSection() for why that
		// distinction is the server's to make.
		"managed_surface":    "identity_providers",
		"unavailable_reason": authProvidersElsewhereUnavailable,
		"title":              "Authentication",
		"description":        "Configure the identity providers this deployment federates logins through.",
		"order":              7,
		"icon":               "lock",
		// No fields, for the same reason mcpServersSection() declares none.
		//
		// The five this section used to declare — `auth_provider`,
		// `form_users`, and the three OIDC values — each addressed a key inside
		// a pylon plugin's YAML, and two of them were `format: password`.
		// Declaring them here would describe controls this page cannot serve,
		// which is the "renders as a working control and is not one" failure
		// rejectUnavailableField exists to stop. The typed editor at
		// `/admin/identity_providers/administration` collects the real
		// equivalents, and it collects more of them than these five could
		// express.
		"fields": []map[string]any{},
	}
}

// emailSection is outbound e-mail (gap G7). It POINTS at its editor.
//
// `managed_surface` names the dedicated surface that really holds this
// section's data, so the client renders the right editor WITHOUT a hardcoded
// list of section ids — see mcpServersSection() for why that distinction is
// the server's to make.
//
// No fields, for the reason authSection() declares none. The eight values this
// section is about include a `format: password`, and a schema that declared it
// would describe a control the plugin-config write path is required to refuse
// (rejectCredentialField). Declaring the other seven and hiding the eighth
// would be worse: the form would save a host and a user name, report success,
// and send nothing, because the relay would refuse the session at AUTH.
func emailSection() map[string]any {
	return map[string]any{
		"id":                  "email",
		"managed_surface":     "email",
		"unavailable_reason":  emailElsewhereUnavailable,
		"title":               "E-mail",
		"description":         "Configure the SMTP relay this deployment sends invitations and notices through.",
		"order":               9,
		"icon":                "mail_outline",
		"required_permission": "runtime.plugins",
		"fields":              []map[string]any{},
	}
}

func resourcesSection() map[string]any {
	type cardDef struct {
		id          string
		title       string
		description string
	}
	cards := []cardDef{
		{"information", "Information", "General platform information and version details"},
		{"documentation", "Documentation", "API reference, guides, and platform concepts"},
		{"release_notes", "Release Notes", "Product updates, improvements, and fixes"},
		{"video_library", "Video Library", "Product walkthroughs and recorded sessions"},
		{"tutorials", "Tutorials", "Step-by-step guides and use cases"},
		{"interactive_tours", "Interactive Tours", "Guided tours to explore key features and workflows"},
	}

	fields := make([]map[string]any, 0, len(cards)*4)
	for _, c := range cards {
		fields = append(fields, map[string]any{
			"key":         "resources_" + c.id + "_enabled",
			"type":        "boolean",
			"title":       c.title + " Card Enabled",
			"description": "Show or hide the " + c.title + " card on the Resources page.",
			"path":        "extra_ui_config.resources." + c.id + ".enabled",
			"section":     "resources",
			"default":     true,
		})
		fields = append(fields, map[string]any{
			"key":         "resources_" + c.id + "_title",
			"type":        "string",
			"title":       c.title + " Card Title",
			"description": "Display title for the " + c.title + " resource card.",
			"path":        "extra_ui_config.resources." + c.id + ".title",
			"section":     "resources",
			"default":     c.title,
		})
		fields = append(fields, map[string]any{
			"key":         "resources_" + c.id + "_description",
			"type":        "string",
			"title":       c.title + " Card Description",
			"description": "Short description displayed on the " + c.title + " resource card.",
			"path":        "extra_ui_config.resources." + c.id + ".description",
			"section":     "resources",
			"default":     c.description,
		})
		fields = append(fields, map[string]any{
			"key":         "resources_" + c.id + "_links",
			"type":        "array",
			"title":       c.title + " Links",
			"description": "Links displayed on the " + c.title + " resource card.",
			"path":        "extra_ui_config.resources." + c.id + ".links",
			"section":     "resources",
			"default":     []any{},
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string"},
					"url":   map[string]any{"type": "string"},
				},
			},
		})
	}

	// Extra fields for the information card
	infoExtra := []map[string]any{
		{
			"key":         "resources_information_version",
			"type":        "string",
			"title":       "Information Card - ELITEA Version Value",
			"description": "Version string displayed on the Information resource card.",
			"path":        "extra_ui_config.resources.information.version",
			"section":     "resources",
			"default":     "",
		},
		{
			"key":         "resources_information_upgrade_date",
			"type":        "string",
			"title":       "Information Card - Last Upgrade Date Value",
			"description": "Last upgrade date displayed on the Information resource card.",
			"path":        "extra_ui_config.resources.information.upgrade_date",
			"section":     "resources",
			"default":     "",
		},
	}
	// Insert after the first 4 (information card fields)
	result := make([]map[string]any, 0, len(fields)+len(infoExtra))
	result = append(result, fields[:4]...)
	result = append(result, infoExtra...)
	result = append(result, fields[4:]...)

	return map[string]any{
		"id":   "resources",
		"page": configPageFeatures,
		// #217 rendered this section on the Configuration page and said in its
		// own report that it belonged here — it put it there because that is
		// where the server's schema had it, and leaving it out would have kept
		// #26 (every Help Center card reading "No links configured") open for
		// another unit. The reference is unambiguous: `ConfigurationPage.jsx`
		// subtracts `resources` via `MOVED_TO_FEATURES` and `FeaturesPage.jsx`
		// renders it as "Help Center". It moves now.
		//
		// Nothing about the Help Center's own read changes: it calls
		// `GET /admin/plugin_config_values/prompt_lib/resources`, which is a
		// separate route with no notion of which admin page authored the row.
		"title":          "Help Center",
		"description":    "Configure the resource cards shown on the environment-wide Help Center page.",
		"order":          13,
		"icon":           "menu_book",
		"always_visible": true,
		"fields":         result,
	}
}

// dedicatedBannerSection — the platform-wide notification banner. LIVE.
//
// It carried extraUIConfigUnavailable ("nothing in this platform reads this
// setting yet") and the reason was accurate: the legacy SPA read the banner from
// a BUILD-TIME environment variable, `VITE_MAINTENANCE_BANNER`, so the rows this
// form wrote were read by nothing anywhere — not here and not in the reference
// deployment either. The admin control and the rendered banner were two
// unconnected things that happened to share a name.
//
// They are connected now. `platformconfig.LoadBanner` resolves these five keys,
// `GET /elitea_core/platform_settings/prompt_lib` marshals the result as
// `dedicated_banner`, and apps/elitea-web's `MaintenanceBanner` renders it in the
// app shell. That also makes it a RUNTIME control rather than a redeploy, which
// is the point of a banner: an operator raising one is telling users about
// something that is happening now.
//
// The `path` values are kept as they were. They are legacy provenance — where
// pylon would have written the key inside `extra_ui_config` — and nothing here
// reads them; the storage key is `key`, in `centry.platform_config` under this
// section id. They stay because a deployment migrating from pylon needs the two
// namings to be traceable to each other.
func dedicatedBannerSection() map[string]any {
	return map[string]any{
		"id":             "dedicated_banner",
		"title":          "Banner",
		"description":    "Enable dedicated banner to communicate important notifications across the platform.",
		"order":          89,
		"icon":           "campaign",
		"always_visible": true,
		"fields": []map[string]any{
			{
				"key":         "banner_enabled",
				"type":        "boolean",
				"title":       "Banner Enabled",
				"description": "When enabled, a notification banner is displayed to all users environment-wide.",
				"path":        "extra_ui_config.vite_maintenance_banner.enabled",
				"section":     "dedicated_banner",
				"default":     false,
			},
			{
				"key":         "banner_dismissible",
				"type":        "boolean",
				"title":       "Dismissible",
				"description": "When enabled, users can close the banner. Dismissed state persists until a new banner is configured or the user logs out.",
				"path":        "extra_ui_config.vite_maintenance_banner.dismissible",
				"section":     "dedicated_banner",
				"default":     false,
			},
			{
				"key":         "banner_icon",
				"type":        "string",
				"title":       "Icon",
				"description": "Icon type displayed alongside the banner message.",
				"path":        "extra_ui_config.vite_maintenance_banner.icon",
				"section":     "dedicated_banner",
				"default":     "info",
				"enum":        []string{"info", "warning"},
			},
			{
				"key":         "banner_style",
				"type":        "string",
				"title":       "Banner Style",
				"description": "Visual style of the banner.",
				"path":        "extra_ui_config.vite_maintenance_banner.style",
				"section":     "dedicated_banner",
				"default":     "info",
				"enum":        []string{"info", "warning"},
			},
			{
				"key":    "banner_message",
				"type":   "string",
				"format": "textarea",
				"title":  "Banner Message",
				"description": "The message content displayed in the banner. Supports Markdown formatting. " +
					"An enabled banner with no message renders nothing.",
				"path":    "extra_ui_config.vite_maintenance_banner.message",
				"section": "dedicated_banner",
				"default": "",
			},
		},
	}
}

// brandingSection is the DATABASE layer of the brand pack (ADR-0024).
//
// LIVE: every field is read by `internal/platformconfig.LoadBranding` through
// `internal/api/v2/branding`'s resolver on the next bootstrap.js request. The
// typed Branding routes (`/admin/branding/administration`, branding.go) edit
// the same rows with the same rules and add the layer/effective-pack view the
// dedicated admin page needs; this section is the same editor without the
// preview, so an operator can rebrand from the Configuration page today.
//
// Every field is OPTIONAL and an empty value means "inherit from the layer
// below" — the mounted BRAND_PACK_PATH file, or the product default. The
// defaults here are therefore the empty string and 0, not the product's
// values: a form that showed "Elitea" as the default would store "Elitea" on
// the first save and pin the name over any file the operator mounts later.
func brandingSection() map[string]any {
	text := func(key, title, description string) map[string]any {
		return map[string]any{
			"key": key, "type": "string", "title": title,
			"description": description + " Leave empty to inherit.",
			"section":     "branding", "default": "",
		}
	}
	number := func(key, title, description string) map[string]any {
		return map[string]any{
			"key": key, "type": "number", "title": title,
			"description": description + " 0 inherits.",
			"section":     "branding", "default": float64(0),
		}
	}
	return map[string]any{
		"id":    "branding",
		"title": "Branding",
		"description": "Restyle the platform in your organisation's name, colours and typography. " +
			"Changes reach every user on their next page load; no rebuild or restart.",
		"order":               88,
		"icon":                "palette",
		"always_visible":      true,
		"required_permission": "configuration.branding",
		"fields": []map[string]any{
			text("product_name", "Product name", "Shown in the document title, the login page and e-mail."),
			text("product_short_name", "Short name", "Used where the full name does not fit."),
			text("product_tagline", "Tagline", "A short line under the name on the login page."),
			text("docs_url", "Documentation URL", "Absolute http(s) URL the help links open."),
			text("support_url", "Support URL", "Absolute http(s) URL the support links open."),
			text("support_email", "Support e-mail", "Address the app's contact-support paths and e-mail footers use."),
			text("sender_name", "E-mail sender name", "Display name outbound e-mail is sent as; the product name when empty."),
			text("brand_hue", "Brand colour", "Six-digit hex colour, e.g. #1A73E8. Every accent and surface tint is derived from it."),
			text("brand_on_brand", "Text on brand colour", "Six-digit hex colour for text placed on the brand colour."),
			text("font_family", "Font family", "CSS font-family list for text, e.g. \"Inter\", Arial, sans-serif. Self-hosted faces arrive with asset upload."),
			text("font_family_mono", "Monospace font family", "CSS font-family list for code."),
			number("base_size", "Base font size", "Pixels, 12 to 18."),
			number("scale", "Type scale", "Ratio between heading steps, 1.05 to 1.5."),
			number("radius_sm", "Small radius", "Pixels."),
			number("radius_md", "Medium radius", "Pixels."),
			number("radius_lg", "Large radius", "Pixels."),
			number("radius_pill", "Pill radius", "Pixels; 9999 for a full pill."),
			text("density", "Density", "comfortable or compact."),
			text("logo_full", "Logo", "Path on this origin, e.g. /branding/assets/logo-full/<digest>.svg."),
			text("logo_mark", "Logo mark", "Path on this origin for the compact mark."),
			text("favicon", "Favicon", "Path on this origin."),
			text("login_art", "Login artwork", "Path on this origin for the login page image."),
			text("logo_email", "E-mail logo", "Path on this origin of an uploaded PNG or WebP logo for e-mail."),
			{
				"key":   "font_faces",
				"type":  "array",
				"title": "Self-hosted font faces",
				"description": "Up to two uploaded WOFF2 faces the app declares as @font-face: " +
					"[{family, url, weight?, style?}]. url is the path an upload to /admin/branding/assets/font returned.",
				"section": "branding",
				"default": []any{},
				"items":   map[string]any{"type": "object"},
			},
			{
				"key":   "scheme_tokens",
				"type":  "object",
				"title": "Hand-tuned scheme tokens",
				"description": "Token id → colour per scheme ({light, dark, hc}), as a branding package carries them. " +
					"Stated tokens win over hue derivation for the ids they name; empty derives everything from the brand colour.",
				"section": "branding",
				"default": map[string]any{},
			},
		},
	}
}

func supportAssistantSection() map[string]any {
	return map[string]any{
		// LIVE. Every field below is read on the next request by
		// `internal/platformconfig.LoadSupportAssistant`, and this page is the
		// ONLY writer of those rows — the reference's
		// `PUT /support_assistant/config` is deliberately not ported, so the
		// form and the server cannot disagree about field names or defaults.
		"id":             "support_assistant",
		"page":           configPageFeatures,
		"title":          "Support Assistant",
		"description":    "Enable the in-app support assistant widget for all users.",
		"order":          14,
		"icon":           "support_agent",
		"always_visible": true,
		"fields": []map[string]any{
			{
				// A BOOLEAN, replacing the reference's `vite_elitea_assistant`
				// string of "0"/"1".
				//
				// That field was a Vite build-time variable injected into the
				// legacy SPA's page, so it could only ever be a string, and the
				// admin form rendered a text box in which "false", "no" and "0"
				// meant three different things to nobody. Nothing here is built
				// at build time, so the switch is the type it always was.
				"key":         "support_assistant_enabled",
				"type":        "boolean",
				"title":       "Assistant Enabled",
				"description": "When enabled, the support assistant widget is available to all users environment-wide. The assistant only appears once an agent is selected below.",
				"path":        "support_assistant_enabled",
				"section":     "support_assistant",
				"default":     false,
			},
			{
				// WRITTEN BACK BY THE SERVER as well as read. Left empty on an
				// enabled deployment, the first support request provisions the
				// hidden project and records its id here, so an operator can see
				// which project the transcripts landed in. Clearing the field
				// does not orphan it: the bootstrap adopts an existing
				// "Support Assistant" project by name before creating one.
				//
				// THE DESCRIPTION NAMES THE CONSEQUENCE, and that is the point
				// of it. Support conversations live in a project shared by
				// everyone, so the assistant enrols each caller into this
				// project as `viewer` on first use
				// (internal/api/v2/supportassistant/store.go's ensureEnrolled).
				// Naming an EXISTING project here therefore grants the entire
				// user base the default-mode viewer rights migration 0068 seeds
				// — conversation and application reads included — and correcting
				// the id afterwards does not withdraw the memberships already
				// written. An operator cannot weigh that from "auto-created if
				// left empty", which is all this field used to say.
				"key":   "support_project_id",
				"type":  "integer",
				"title": "Support Project ID",
				"description": "Project ID used by the support assistant for conversations. Leave empty to have a " +
					"dedicated hidden project created automatically — that is the recommended setting. WARNING: " +
					"naming an existing project here enrols EVERY user on the platform into it as a viewer, " +
					"giving them read access to that project's conversations and agents. Memberships already " +
					"granted are not removed if you change this value.",
				"path":    "support_project_id",
				"section": "support_assistant",
				"default": nil,
			},
			{
				"key":         "support_agent_project_id",
				"type":        "integer",
				"title":       "Agent Project ID",
				"description": "Project ID where the support agent is located. Leave empty to use the support project itself.",
				"path":        "support_agent_project_id",
				"section":     "support_assistant",
				"default":     nil,
			},
			{
				"key":         "support_agent_id",
				"type":        "integer",
				"title":       "Agent ID",
				"description": "Application ID of the support agent. Until this is set the assistant stays hidden, because it has nothing to answer with.",
				"path":        "support_agent_id",
				"section":     "support_assistant",
				"default":     nil,
			},
			{
				"key":         "support_welcome_message",
				"type":        "string",
				"title":       "Welcome Message",
				"description": "Initial greeting message shown in the support widget.",
				"path":        "support_welcome_message",
				"section":     "support_assistant",
				"default":     "Hello! How can I help you today?",
			},
			{
				"key":         "support_assistant_name",
				"type":        "string",
				"title":       "Assistant Name",
				"description": "Display name for the support assistant.",
				"path":        "support_assistant_name",
				"section":     "support_assistant",
				"default":     "ELITEA Support",
			},
			{
				// The reference has no admin field for this — `config.py` reads
				// `config.get('placeholder', ...)` from a key its own
				// admin_schema.json never declares, so no operator could ever
				// set it. Declaring it is the smaller change; leaving the read
				// pointed at a field nobody can write is the defect this page
				// exists to remove.
				"key":         "support_placeholder",
				"type":        "string",
				"title":       "Input Placeholder",
				"description": "Placeholder text shown in the assistant's message box.",
				"path":        "support_placeholder",
				"section":     "support_assistant",
				"default":     "Type a message...",
			},
		},
	}
}

func voiceFeaturesSection() map[string]any {
	return map[string]any{
		"id":   "voice_features",
		"page": configPageFeatures,
		// LIVE. The first pass of this unit marked this section unavailable on
		// the strength of `VoiceControlButton`/`VoiceMiniPlayer` in
		// `features/chat-input` having no render site — which is true, and was
		// the wrong component. `widgets/chat/ui/chat-button/VoiceButton.tsx` is
		// a SECOND voice control, hardcoding the same two flags as module
		// constants, and it IS mounted: `pages/chat` → `ChatBox` →
		// `buildChatBoxInputSlots()` → `<VoiceButton>`. Both flags are now
		// marshalled by `GET /elitea_core/platform_settings/…` and read there.
		"title":          "Voice Features",
		"description":    "Control Voice-to-Voice, Text-to-Voice, and Voice-to-Text features environment-wide.",
		"order":          15,
		"icon":           "record_voice_over",
		"always_visible": true,
		"fields": []map[string]any{
			{
				"key":         "vite_voice_features_enabled",
				"type":        "boolean",
				"title":       "Voice Features Enabled",
				"description": "When disabled, all voice features (V2V, T2V, V2T) are completely hidden for all users.",
				"path":        "extra_ui_config.vite_voice_features_enabled",
				"section":     "voice_features",
				"default":     true,
			},
			{
				"key":         "vite_voice_features_temporarily_disabled",
				"type":        "boolean",
				"title":       "Disable Voice Controls but Keep Them Visible",
				"description": "When enabled, voice buttons remain visible but are non-interactive with an admin tooltip.",
				"path":        "extra_ui_config.vite_voice_features_temporarily_disabled",
				"section":     "voice_features",
				"default":     false,
			},
		},
	}
}

// maintenanceSection — the platform maintenance switch. LIVE, and ENFORCED.
//
// It carried maintenanceSplashUnavailable, which said the switch "would toggle a
// setting nothing enforces". That was true and it is the sentence this section
// had to earn its way out of: something enforces it now.
//
// WHAT REPLACED THE PYLON HOOK. In pylon this was a gevent router hook installed
// on the bootstrap plugin's persisted state, which intercepted every request,
// called `auth_authorize` over RPC, read the caller's administration-mode roles
// and served a 503 splash to anyone who was not an admin
// (legacy/plugins/bootstrap/tools/splash.py). Here it is
// `internal/api/middleware`'s Maintenance middleware on the `/api/v2` group,
// which asks the same question of the same permission model and answers 503 with
// the copy authored below.
//
// The port is deliberately NOT a byte-for-byte one, in two places:
//
//   - The splash is JSON and a client-side page, not `splash_template` HTML. The
//     pylon hook returned a WSGI app serving a stored HTML document because it
//     sat in front of everything, including the SPA's own assets. This
//     middleware sits on the JSON API only — the SPA still loads — so the
//     product can render its own splash in its own theme, and an operator
//     authors words rather than markup. A stored HTML template editable from an
//     admin form is also an XSS surface aimed at every user of the platform, and
//     declining to build one is not a gap.
//   - There is no bypass cookie. `splash_bypass_cookie`/`splash_bypass_token`
//     were a shared static secret in plugin config that granted full access to
//     anyone who had ever seen it. The admin permission is the bypass.
//
// WHO GETS THROUGH. Holders of `runtime.plugins` in administration mode — the
// permission pylon's own maintenance.py declares on this surface — plus the
// routes an admin needs in order to sign in and turn the switch back off. The
// middleware owns that list; see its doc comment for why each entry is on it.
func maintenanceSection() map[string]any {
	return map[string]any{
		"id":    "maintenance",
		"title": "Maintenance",
		"description": "Enable maintenance mode to refuse the API to everyone without administration " +
			"access and show them a splash screen. Administrators keep full access, so this switch can " +
			"always be turned off from here.",
		"order":          91,
		"icon":           "construction",
		"always_visible": true,
		// The same permission the middleware bypasses on. A caller who cannot
		// be admitted during a maintenance window must not be able to start one
		// either.
		"required_permission": "runtime.plugins",
		"fields": []map[string]any{
			{
				"key":   "maintenance_enabled",
				"type":  "boolean",
				"title": "Maintenance Mode",
				"description": "When enabled, every API request from a user without administration access " +
					"is refused with 503 and the splash below.",
				"section": "maintenance",
				"default": false,
			},
			{
				"key":         "maintenance_title",
				"type":        "string",
				"title":       "Splash Title",
				"description": "Heading shown on the splash screen. Left empty, a default is used.",
				"section":     "maintenance",
				"default":     "",
			},
			{
				"key":    "maintenance_message",
				"type":   "string",
				"format": "textarea",
				"title":  "Splash Message",
				"description": "Body text shown on the splash screen — say what is happening and when it " +
					"ends. Supports Markdown formatting. Left empty, a default is used.",
				"section": "maintenance",
				"default": "",
			},
		},
	}
}
