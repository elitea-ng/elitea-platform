package eliteacore

// The READ side of the admin Features page — unit A14, issue #200.
//
// The Features page writes `centry.platform_config`; this file is where those
// rows are consulted. It is a separate file because "what actually reads this
// flag" is the question the whole unit turns on, and the answer should be one
// grep, not an archaeology exercise. Every helper here is called from a handler
// in `handler.go`, and every field the page offers as available is read by one
// of them:
//
//	mcp_enabled                    → mcpFlags, gating PlatformSettings AND the
//	                                 three MCP proxy/sync routes
//	mcp_in_menu                    → mcpFlags, marshalled as mcp_in_menu_enabled
//	is_publish_blocked             → publishGuardrail, enforced in Publish
//	publish_whitelist_project_ids  → publishGuardrail, ditto
//	agent_categories               → extraAgentCategories, merged into
//	                                 AgentCategories
//	vite_voice_features_enabled    → voiceFlags, marshalled as
//	                                 voice_features_enabled and read by
//	                                 widgets/chat's VoiceButton
//	vite_voice_features_temporarily_disabled
//	                               → voiceFlags, ditto
//	blocked_toolkits               → blockedToolkits, marshalled into
//	                                 PlatformSettings so the product UI can mark
//	                                 an existing toolkit blocked
//	analytics_enabled              → analyticsFlags, marshalled as
//	                                 analytics_enabled AND enforced on the
//	                                 `/analytics*` routes by
//	                                 internal/api/router.go's
//	                                 requireAnalyticsEnabled
//
// The other four `guardrails` fields are read outside this package, and this is
// the whole of that list: `blocked_toolkits` and `blocked_tools` in the toolkit
// API surfaces (internal/api/v2/toolkits/guardrails.go) and in the agent tool
// freeze (internal/application/agentexecution/tools.go); `sensitive_tools` and
// the two dialog-copy fields in the agent execution input the worker reads.
//
// A flag with no entry in that list does not belong on the available side of the
// page. `config_schemas.go` states the same fact from the other end, as an
// `unavailable_reason`; if the two ever disagree, the schema is what the
// operator is shown and this file is what the platform does, so the schema is
// the one that is wrong.
//
// ## Why every failure here is permissive
//
// These reads are on the request path of ordinary product traffic — listing
// agent categories, loading the shell's feature flags. A store that cannot be
// read is an operational fault, and resolving it to "MCP is disabled" or
// "publishing is blocked" would turn a database hiccup into a platform-wide
// outage of a subsystem nobody switched off.
//
// Two places are NOT permissive, and both are permissive-would-be-wrong rather
// than exceptions to a style:
//
//   - the admin page's own read (`config_values.go`) reports the failure,
//     because an operator editing configuration must never be shown defaults as
//     if they were the stored state;
//   - the agent tool freeze (internal/application/agentexecution/tools.go) fails
//     the execution, because there the permissive answer is "nothing is
//     blocked", which runs exactly the tools an operator disabled.
//
// `blockedToolkits` below stays permissive because it only decides how the UI
// PAINTS a toolkit; it enforces nothing.

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

// parseProjectID turns the URL segment into the integer the whitelist stores.
//
// A segment that is not an integer yields -1, which no whitelist entry can
// match, so a malformed project id is refused while the guardrail is on rather
// than falling through to project 0. The alternative — treating an unparseable
// id as "not blocked" — would make the guardrail bypassable by sending a project
// id the router accepts and `strconv` does not.
func parseProjectID(raw string) int64 {
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}

// mcpFlagState is the resolved MCP switch pair.
type mcpFlagState struct {
	// enabled is the master switch. False means the MCP API endpoints refuse.
	enabled bool
	// inMenu hides the UI entry points while leaving the API working. It is
	// meaningless while enabled is false and is reported as false there, so a
	// client cannot render an MCP menu entry for a subsystem whose endpoints
	// would 403.
	inMenu bool
}

func (h *Handler) mcpFlags(ctx context.Context) mcpFlagState {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionMCPConfiguration)
	if err != nil {
		return mcpFlagState{enabled: true, inMenu: true}
	}
	enabled := values.Bool(platformconfig.KeyMCPEnabled, true)
	return mcpFlagState{
		enabled: enabled,
		inMenu:  enabled && values.Bool(platformconfig.KeyMCPInMenu, true),
	}
}

// requireMCPEnabled is the gate on the MCP HTTP surface.
//
// It returns true when the request may proceed. The 403 and its wording match
// pylon's (`legacy/plugins/elitea_core/api/v2/mcp_dcr_proxy.py:54`), because the
// existing clients of these routes were written against that answer.
//
// This is the half of the master switch that cannot be done in the client. The
// field's own description promises the switch "removes all MCP-related
// functionality across the entire application including API endpoints"; a
// version that only hid the buttons would leave every one of these routes open
// to anyone who kept a URL, while telling the operator they had closed them.
func (h *Handler) requireMCPEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.mcpFlags(r.Context()).enabled {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "MCP exposure is disabled on this deployment",
	})
	return false
}

// voiceFlagState is the resolved Voice Features pair.
type voiceFlagState struct {
	// enabled hides every voice control when false.
	enabled bool
	// temporarilyDisabled leaves them VISIBLE but non-interactive, with the
	// admin tooltip the button already renders. It is reported as false while
	// `enabled` is false for the same reason `inMenu` is: "visible but
	// disabled" is not a state a hidden control can be in, and a client that
	// combined the two itself would be inventing the rule.
	temporarilyDisabled bool
}

func (h *Handler) voiceFlags(ctx context.Context) voiceFlagState {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionVoiceFeatures)
	if err != nil {
		return voiceFlagState{enabled: true}
	}
	enabled := values.Bool(platformconfig.KeyVoiceEnabled, true)
	return voiceFlagState{
		enabled:             enabled,
		temporarilyDisabled: enabled && values.Bool(platformconfig.KeyVoiceTemporarilyDisabled, false),
	}
}

// analyticsFlagState is the resolved Analytics visibility switch.
type analyticsFlagState struct {
	// enabled hides the Settings > Analytics tab when false, AND — unlike the
	// voice and MCP-menu switches — is also the answer requireAnalyticsEnabled
	// enforces on the `/analytics*` HTTP surface. A client-only switch would
	// leave those routes open to anyone who kept a URL while the tab believed
	// itself hidden.
	enabled bool
}

func (h *Handler) analyticsFlags(ctx context.Context) analyticsFlagState {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionAnalytics)
	if err != nil {
		return analyticsFlagState{enabled: true}
	}
	return analyticsFlagState{enabled: values.Bool(platformconfig.KeyAnalyticsEnabled, true)}
}

// publishGuardrailState is the resolved agent-publishing guardrail.
type publishGuardrailState struct {
	blocked   bool
	whitelist []int64
}

// allows answers whether a given project may publish.
//
// An empty whitelist while blocked means nobody may publish — that is what the
// field's description says and what the reference's own text says
// ("If empty, publishing is blocked for all projects"). The tempting reading,
// that an empty list means "no restrictions", would invert the control at
// exactly the moment an operator first switches it on and has not yet added an
// exemption.
func (g publishGuardrailState) allows(projectID int64) bool {
	if !g.blocked {
		return true
	}
	for _, allowed := range g.whitelist {
		if allowed == projectID {
			return true
		}
	}
	return false
}

func (h *Handler) publishGuardrail(ctx context.Context) publishGuardrailState {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionAgentPublishing)
	if err != nil {
		return publishGuardrailState{}
	}
	return publishGuardrailState{
		blocked:   values.Bool(platformconfig.KeyPublishBlocked, false),
		whitelist: values.Ints(platformconfig.KeyPublishWhitelistProjectIDs),
	}
}

// skillPublishGuardrail is the same resolution over the SKILL section.
//
// Separate from `publishGuardrail` rather than parameterised by section id,
// because the two sections do not share field keys either — the skill switch is
// `is_skill_publish_blocked`, not `is_publish_blocked` — and a single function
// taking three arguments to say "the other one" reads worse than two that each
// say which they are.
func (h *Handler) skillPublishGuardrail(ctx context.Context) publishGuardrailState {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionSkillPublishing)
	if err != nil {
		return publishGuardrailState{}
	}
	return publishGuardrailState{
		blocked:   values.Bool(platformconfig.KeySkillPublishBlocked, false),
		whitelist: values.Ints(platformconfig.KeySkillPublishWhitelistProjectIDs),
	}
}

// projectIDList returns a non-nil slice so an empty whitelist encodes as `[]`
// rather than `null`. A client that has to distinguish those two — and this one
// does, because "blocked with an empty whitelist" means blocked everywhere —
// has been handed the server's problem.
func projectIDList(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// extraAgentCategories returns the operator-configured categories.
func (h *Handler) extraAgentCategories(ctx context.Context) []string {
	values, err := platformconfig.Load(ctx, h.pool, platformconfig.SectionAgentPublishing)
	if err != nil {
		return nil
	}
	return values.Strings(platformconfig.KeyAgentCategories)
}

// blockedToolkits resolves the guardrails blocklist for PlatformSettings.
//
// An unreadable store yields an EMPTY list, and that is the permissive answer on
// purpose: this value only decides whether the UI paints a toolkit as blocked,
// and painting every toolkit blocked because one row would not load would be a
// far louder failure than painting none. The enforcement that matters does not
// live here — the catalogue refuses, and the agent freeze fails the execution
// outright rather than run unguarded.
//
// It returns a non-nil slice so the field encodes as `[]` rather than `null`;
// a client that has to distinguish those two has been handed the server's
// problem.
func (h *Handler) blockedToolkits(ctx context.Context) []string {
	policy, err := platformconfig.LoadGuardrails(ctx, h.pool)
	if err != nil {
		slog.ErrorContext(ctx, "platform_settings: guardrails read failed; reporting no blocked toolkits",
			"err", err)
		return []string{}
	}
	blocked := policy.BlockedToolkits()
	if blocked == nil {
		return []string{}
	}
	sort.Strings(blocked)
	return blocked
}

// announcements resolves the two platform-wide announcements PlatformSettings
// carries — the notification banner and maintenance mode.
//
// Both are read HERE, on the endpoint the SPA already polls, rather than each
// acquiring an endpoint of its own. That is what lets the app shell paint a
// banner and the router paint a maintenance splash from state it is already
// fetching, and it keeps the number of unauthenticated-adjacent reads at one.
//
// The maintenance state published here is the SAME state
// `internal/api/middleware`'s Maintenance gate enforces, resolved from the same
// rows. That matters more than it looks: the SPA must never be able to show
// "everything is fine" while every request it makes is being refused, nor a
// splash over a platform that is up. The two can still disagree for at most the
// middleware's cache TTL, which is the price of not querying on every request
// and is bounded by a compiled constant.
//
// `platform_settings` is on the middleware's allowlist precisely so this stays
// readable during a window — see maintenanceAllowlist.
//
// Both reads are permissive, in the same direction and for the same reason as
// every other read in this file: an unreadable store means no banner and no
// maintenance, never an invented one.
func (h *Handler) announcements(
	ctx context.Context,
) (platformconfig.Banner, platformconfig.Maintenance) {
	banner, err := platformconfig.LoadBanner(ctx, h.pool)
	if err != nil {
		slog.ErrorContext(ctx, "platform_settings: banner read failed; reporting no banner", "err", err)
		banner = platformconfig.Banner{}
	}
	maintenance, err := platformconfig.LoadMaintenance(ctx, h.pool)
	if err != nil {
		slog.ErrorContext(ctx, "platform_settings: maintenance read failed; reporting no maintenance",
			"err", err)
		maintenance = platformconfig.Maintenance{}
	}
	return banner, maintenance
}

// maintenancePayload marshals the maintenance state WITH the calling user's own
// exemption.
//
// `bypass` is the field that keeps the SPA from having to reimplement the
// middleware's rule. Without it a client knows only that the platform is in
// maintenance, not whether ITS requests are being refused, and would have to
// choose between two wrong behaviours: paint the splash for everyone — hiding
// the product from the administrators who are the only people it still works
// for, and who are the ones with a reason to be using it during a window — or
// paint it for nobody and let an ordinary user watch every request fail with no
// explanation.
//
// It is resolved from the SAME permission the middleware admits on, through the
// same resolver, so the two cannot drift into telling a user different things.
// A resolver error yields false, matching the middleware's own refusal
// direction: a caller whose permissions could not be resolved is not admitted,
// so telling them they were would be the client showing a working product over
// an API that is refusing them.
func (h *Handler) maintenancePayload(
	ctx context.Context, state platformconfig.Maintenance,
) map[string]any {
	return map[string]any{
		"enabled": state.Enabled,
		"title":   state.Title,
		"message": state.Message,
		// The operator's own splash body, empty unless one was authored. It is
		// published on this endpoint rather than a new one because the app
		// shell already polls it for the flag, and the standalone splash entry
		// (apps/elitea-web src/entries/maintenance) reads the same document —
		// so the two screens cannot show different words for one window.
		"html":   state.HTML,
		"bypass": state.Enabled && h.holdsMaintenanceBypass(ctx),
	}
}

// holdsMaintenanceBypass asks the permission model the middleware's question.
func (h *Handler) holdsMaintenanceBypass(ctx context.Context) bool {
	if h.permissionResolver == nil {
		return false
	}
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	resolution, err := h.permissionResolver.ResolvePermissions(
		ctx, principal, auth.PermissionModeAdministration, "",
	)
	if err != nil {
		slog.ErrorContext(ctx, "platform_settings: maintenance bypass unresolved; reporting no bypass",
			"err", err)
		return false
	}
	for _, granted := range resolution.Permissions {
		if granted == apimw.MaintenanceAdminPermission {
			return true
		}
	}
	return false
}
