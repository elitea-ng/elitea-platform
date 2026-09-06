// Package platformconfig is the READ side of `centry.platform_config` — the
// table the admin Features and Configuration pages write (unit A14, issue #200).
//
// It exists as its own package for one reason: the writer lives in
// `internal/api/v2/admin` and every consumer lives somewhere else
// (`internal/api/v2/eliteacore` today), and a flag whose only reader is the page
// that wrote it is the defect this unit was opened to remove. Putting the read
// in the writer's package would have made `eliteacore` import `admin`, which is
// backwards; putting a copy of the query in each consumer would have made
// "which flags are actually read" un-greppable.
//
// The contract is deliberately narrow. There is no cache, no invalidation and no
// process-level state: each call is one indexed query on a table with a handful
// of rows, and the alternative — the reference's design, where a pylon reads the
// value into module state at plugin load and needs a restart signal to pick up a
// change — is exactly why every field on that page carried `requires_restart`
// and why this platform has no restart signal to offer. Reading per request is
// what lets the admin page's save take effect on the next call instead of on the
// next deployment.
package platformconfig

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Section ids. These are the section ids `internal/api/v2/admin/config_schemas.go`
// declares; they are named here so a rename breaks the build rather than
// quietly returning an empty map to every consumer.
const (
	SectionMCPConfiguration = "mcp_configuration"
	SectionAgentPublishing  = "agent_publishing"
	SectionSkillPublishing  = "skill_publishing"
	SectionVoiceFeatures    = "voice_features"
	SectionGuardrails       = "guardrails"
	SectionDedicatedBanner  = "dedicated_banner"
	SectionMaintenance      = "maintenance"
	SectionSupportAssistant = "support_assistant"
	SectionAnalytics        = "analytics"
	// SectionBranding is the DATABASE layer of the brand pack (ADR-0024
	// decision 1). Its keys are declared in branding.go beside the resolver
	// that reads them.
	SectionBranding = "branding"
	// The `email` section is NOT declared here. It is `emailsettings.Section`,
	// declared beside the resolver that reads it and the store that writes it
	// (gap G7) — the same placement SectionBranding's keys use, and for the
	// same reason: a section whose reader and writer are one package has no
	// second consumer for this file to serve.
)

// Field keys, for the same reason.
const (
	KeyMCPEnabled                 = "mcp_enabled"
	KeyMCPInMenu                  = "mcp_in_menu"
	KeyPublishBlocked             = "is_publish_blocked"
	KeyPublishWhitelistProjectIDs = "publish_whitelist_project_ids"
	KeyAgentCategories            = "agent_categories"

	// The skill-publishing guardrail is a SEPARATE section with its own three
	// keys, exactly as the reference keeps it
	// (`skill_publishing_guardrail` in elitea_core's admin_schema.json). It is
	// not an alias of the agent trio: an operator who freezes agent publishing
	// during an incident has not thereby frozen skill publishing, and the
	// reference's own platform_settings endpoint publishes the two pairs
	// independently for exactly that reason.
	KeySkillPublishBlocked             = "is_skill_publish_blocked"
	KeySkillPublishWhitelistProjectIDs = "skill_publish_whitelist_project_ids"
	KeySkillCategories                 = "skill_categories"

	KeyVoiceEnabled             = "vite_voice_features_enabled"
	KeyVoiceTemporarilyDisabled = "vite_voice_features_temporarily_disabled"

	KeyBlockedToolkits                = "blocked_toolkits"
	KeyBlockedTools                   = "blocked_tools"
	KeySensitiveTools                 = "sensitive_tools"
	KeySensitiveActionCompanyName     = "sensitive_action_company_name"
	KeySensitiveActionMessageTemplate = "sensitive_action_message_template"

	KeyBannerEnabled     = "banner_enabled"
	KeyBannerDismissible = "banner_dismissible"
	KeyBannerIcon        = "banner_icon"
	KeyBannerStyle       = "banner_style"
	KeyBannerMessage     = "banner_message"

	KeyMaintenanceEnabled = "maintenance_enabled"
	KeyMaintenanceTitle   = "maintenance_title"
	KeyMaintenanceMessage = "maintenance_message"

	KeySupportAssistantEnabled = "support_assistant_enabled"
	KeySupportProjectID        = "support_project_id"
	KeySupportAgentProjectID   = "support_agent_project_id"
	KeySupportAgentID          = "support_agent_id"
	KeySupportWelcomeMessage   = "support_welcome_message"
	KeySupportAssistantName    = "support_assistant_name"
	KeySupportPlaceholder      = "support_placeholder"

	// KeyAnalyticsEnabled ported from the withheld `observability` section's
	// `analytics_enabled` field (config_schemas.go's "Observability, Runtime and
	// Admin Panel are GONE too" note) — the one field on that section that was
	// never Pylon-plugin-shaped. It gates both the Settings > Analytics tab
	// (via PlatformSettings, in eliteacore/platform_flags.go) and the
	// `/analytics*` HTTP surface (internal/api/router.go's
	// requireAnalyticsEnabled).
	KeyAnalyticsEnabled = "analytics_enabled"
)

// Values is one section's stored rows, decoded. A key ABSENT from the map has
// never been written, which is distinct from a key written as its default — the
// callers below take a fallback for exactly that reason, so "the operator has
// not configured this" and "the operator turned this on" do not collapse.
type Values map[string]any

// Load reads one section. A nil pool or a query error yields no values and the
// error; callers decide what an unreadable store means for them, and every one
// of them below chooses the permissive answer, because a database hiccup must
// not silently switch a platform's MCP subsystem off.
func Load(ctx context.Context, pool *pgxpool.Pool, section string) (Values, error) {
	if pool == nil {
		return Values{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT key, value FROM centry.platform_config WHERE section = $1`, section)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := make(Values)
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		values[key] = decoded
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// Bool reads a boolean, falling back when the key is absent or holds something
// else. A stored value of the wrong type is treated as absent rather than as
// false: the write path type-checks every field against its schema, so a
// mistyped row can only have arrived by hand, and reading it as "off" would let
// a stray `UPDATE` disable a subsystem.
func (v Values) Bool(key string, fallback bool) bool {
	got, ok := v[key].(bool)
	if !ok {
		return fallback
	}
	return got
}

// Strings reads an array of strings, skipping any element that is not one.
func (v Values) Strings(key string) []string {
	entries, ok := v[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if text, ok := entry.(string); ok && text != "" {
			out = append(out, text)
		}
	}
	return out
}

// Ints reads an array of JSON numbers as int64s.
//
// JSON has no integer type, so these arrive as float64 whatever the schema says.
// A non-integral element is skipped rather than truncated — a project id of 1.5
// is not project 1, and rounding it would grant the whitelist to a project the
// operator did not name.
func (v Values) Ints(key string) []int64 {
	entries, ok := v[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(entries))
	for _, entry := range entries {
		number, ok := entry.(float64)
		if !ok || number != float64(int64(number)) {
			continue
		}
		out = append(out, int64(number))
	}
	return out
}

// Int reads a single JSON number as an int64, reporting whether the key held
// one. It is a two-value read rather than a fallback read because the callers
// that need it — the support assistant's project and agent ids — have to tell
// "the operator has not chosen a project yet" from "the operator chose project
// 0", and a fallback collapses those into one answer.
//
// A non-integral number is ABSENT, not truncated, for the same reason Ints
// skips one: agent 1.5 is not agent 1, and resolving it to a neighbouring row
// would run somebody else's agent against every support conversation on the
// platform.
func (v Values) Int(key string) (int64, bool) {
	number, ok := v[key].(float64)
	if !ok || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

// String reads a string, falling back when the key is absent or holds something
// else — the same wrong-type-is-absent rule Bool applies, and for the same
// reason.
func (v Values) String(key string, fallback string) string {
	got, ok := v[key].(string)
	if !ok {
		return fallback
	}
	return got
}

// Float reads a single JSON number, yielding 0 when the key is absent or holds
// something else — the wrong-type-is-absent rule Bool applies. Callers that use
// it treat 0 as "not set", which is why it has no fallback parameter: the only
// consumer so far (the brand overlay) has no field for which 0 is a value.
func (v Values) Float(key string) float64 {
	number, ok := v[key].(float64)
	if !ok {
		return 0
	}
	return number
}

// trimmed reads a string with surrounding whitespace removed, "" when absent.
func (v Values) trimmed(key string) string {
	return strings.TrimSpace(v.String(key, ""))
}

// StringLists reads a `map[string][]string` — the shape both guardrail tool maps
// use (`{"github": ["create_issue"]}`).
//
// Elements that are not strings are skipped, and a key whose value is not an
// array at all is skipped entirely rather than recorded as an empty list. An
// empty list and a malformed value are different facts: the first is an operator
// who selected a toolkit and no tools, the second is a row that should never have
// been written, and recording the second as the first would make a corrupt row
// look like a deliberate choice in the admin form.
func (v Values) StringLists(key string) map[string][]string {
	entries, ok := v[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for name, raw := range entries {
		items, ok := raw.([]any)
		if !ok {
			continue
		}
		values := make([]string, 0, len(items))
		for _, item := range items {
			if text, ok := item.(string); ok && text != "" {
				values = append(values, text)
			}
		}
		out[name] = values
	}
	return out
}
