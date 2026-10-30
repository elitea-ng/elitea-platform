package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The two run-safety Configuration sections: the administrator's session
// policy (ADR-0042 decision 5) and the platform's run caps (ADR-0041
// decision 3). Both are LIVE and both are read through a per-replica cache
// (platformconfig.CachedSection) that a save on this replica drops at once;
// other replicas see a save within platformconfig.DefaultSectionTTL.

// SectionInvalidator drops a cached section after a save.
type SectionInvalidator interface {
	Invalidate()
}

// WithSessionPolicyCache supplies the cache a session_policy save drops.
func WithSessionPolicyCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.sessionPolicy = cache
		}
	}
}

// WithRunCapsCache supplies the cache a run_caps save drops.
func WithRunCapsCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.runCaps = cache
		}
	}
}

func (h *Handler) invalidateRunSafety(section string) {
	switch section {
	case platformconfig.SectionSessionPolicy:
		if h.sessionPolicy != nil {
			h.sessionPolicy.Invalidate()
		}
	case platformconfig.SectionRunCaps:
		if h.runCaps != nil {
			h.runCaps.Invalidate()
		}
	}
}

// sessionPolicySection bounds every browser session. It shares the
// permission of the identity provider editor (runtime.plugins), the other
// half of how users sign in, so no migration seeds anything.
func sessionPolicySection() map[string]any {
	const section = platformconfig.SectionSessionPolicy
	return map[string]any{
		"id":    section,
		"title": "Session Policy",
		"description": "The longest a browser session may last. Each value is a maximum in seconds; 0 sets no " +
			"maximum and keeps the deployment's own session lifetimes. A maximum shortens new sessions and " +
			"also ends existing sessions that pass it. When a maximum idle timeout is set, a session with no " +
			"idle limit gets that maximum.",
		"order":               7,
		"icon":                "timer",
		"always_visible":      true,
		"required_permission": "runtime.plugins",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeySessionMaxIdleTimeoutSeconds,
				"type":  "integer",
				"title": "Maximum Idle Timeout (seconds)",
				"description": fmt.Sprintf("Sign a browser out after this long without a request. 0 sets no "+
					"maximum. From %d to %d.", platformconfig.MinSessionBoundSeconds,
					platformconfig.MaxSessionIdleTimeoutSeconds),
				"section": section,
				"default": 0,
				"minimum": 0,
				"maximum": platformconfig.MaxSessionIdleTimeoutSeconds,
			},
			{
				"key":   platformconfig.KeySessionMaxAbsoluteLifetimeSeconds,
				"type":  "integer",
				"title": "Maximum Session Lifetime (seconds)",
				"description": fmt.Sprintf("Sign a browser out this long after sign-in, whatever its activity. "+
					"0 sets no maximum. From %d to %d.", platformconfig.MinSessionBoundSeconds,
					platformconfig.MaxSessionAbsoluteLifetimeSeconds),
				"section": section,
				"default": 0,
				"minimum": 0,
				"maximum": platformconfig.MaxSessionAbsoluteLifetimeSeconds,
			},
		},
	}
}

// runCapsSection is the platform's run-level caps. It uses the permission
// of the other runtime governance sections (worker egress,
// configuration.governance), so no migration seeds a new one.
func runCapsSection() map[string]any {
	const section = platformconfig.SectionRunCaps
	fields := []map[string]any{
		{
			"key":   platformconfig.KeyRunMaxWallSeconds,
			"type":  "integer",
			"title": "Maximum Run Time (seconds)",
			"description": fmt.Sprintf("Stop an agent run this long after it started. A resumed run counts "+
				"from the start of the run it continues, so time waiting for an approval counts. 0 is off. "+
				"From %d to %d.", platformconfig.MinRunMaxWallSeconds, platformconfig.MaxRunMaxWallSeconds),
			"section": section,
			"default": 0,
			"minimum": 0,
			"maximum": platformconfig.MaxRunMaxWallSeconds,
		},
	}
	defaults := platformconfig.DefaultTierRunCaps()
	for _, tier := range agentlifecycle.Tiers {
		for _, name := range platformconfig.RunCapNames {
			field := map[string]any{
				"key":     platformconfig.RunCapKey(tier, name),
				"type":    "integer",
				"title":   lifecycleTierTitles[tier] + " Tier: " + runCapTitles[name],
				"section": section,
				"minimum": 0,
				"maximum": platformconfig.RunCapLimit(name),
				"description": runCapDescriptions[name] + " 0 is no cap. An agent author can set a lower " +
					"value for one agent, never a higher one.",
			}
			switch name {
			case platformconfig.RunCapMaxSpend:
				field["type"] = "number"
				field["default"] = defaults.MaxSpend
			case platformconfig.RunCapMaxToolCalls:
				field["default"] = defaults.MaxToolCalls
			default:
				field["default"] = 0
			}
			fields = append(fields, field)
		}
	}
	return map[string]any{
		"id":    section,
		"title": "Run Caps",
		"description": "Limits on one agent run, by the agent's risk tier. A run that reaches a cap stops with " +
			"CAP_REACHED and the cap's name, and the conversation shows why. The tier caps apply only after " +
			"the first save of a tier value: save them after every worker runs this release. Until then, " +
			"workers of this release stop a run at 200 tool calls.",
		"order":               6,
		"icon":                "hourglass_bottom",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields":              fields,
	}
}

var runCapTitles = map[string]string{
	platformconfig.RunCapMaxToolCalls:   "Maximum Tool Calls",
	platformconfig.RunCapMaxModelTurns:  "Maximum Model Turns",
	platformconfig.RunCapMaxSpend:       "Maximum Spend (USD)",
	platformconfig.RunCapMaxRecordsRead: "Maximum Records Read",
}

var runCapDescriptions = map[string]string{
	platformconfig.RunCapMaxToolCalls: "The most toolkit tool calls (MCP tools included) one run may make, " +
		"nested agents and pipeline nodes included. The default is 200.",
	platformconfig.RunCapMaxModelTurns: "The most model calls one run may make, nested agents included. An " +
		"agent's own step limit still applies: the lower of the two stops first.",
	platformconfig.RunCapMaxSpend: "The most one run may spend on model calls and metered tools, in USD at " +
		"the price catalogue rate. Checked every 30 seconds against the gateway's request log, so a run can " +
		"pass it by the spend of that interval.",
	platformconfig.RunCapMaxRecordsRead: "The most records (search results, rows, issues) one run may read " +
		"from the toolkits that count records. Other tools count 0.",
}

// validateRunSafetyValues applies the ranges the schema cannot enforce
// (JSON numbers arrive as float64, so integrality too). 0 is accepted: it
// is "no maximum" or "off", never "no idle limit" on a session.
func validateRunSafetyValues(section string, values map[string]any) string {
	type bound struct {
		key            string
		minimum, limit int64
	}
	var bounds []bound
	switch section {
	case platformconfig.SectionSessionPolicy:
		bounds = []bound{
			{platformconfig.KeySessionMaxIdleTimeoutSeconds, platformconfig.MinSessionBoundSeconds,
				platformconfig.MaxSessionIdleTimeoutSeconds},
			{platformconfig.KeySessionMaxAbsoluteLifetimeSeconds, platformconfig.MinSessionBoundSeconds,
				platformconfig.MaxSessionAbsoluteLifetimeSeconds},
		}
	case platformconfig.SectionRunCaps:
		bounds = []bound{
			{platformconfig.KeyRunMaxWallSeconds, platformconfig.MinRunMaxWallSeconds,
				platformconfig.MaxRunMaxWallSeconds},
		}
		if message := validateTierRunCaps(values); message != "" {
			return message
		}
	default:
		return ""
	}
	for _, b := range bounds {
		raw, present := values[b.key]
		if !present {
			continue
		}
		if !platformconfig.ValidBoundSeconds(raw, b.minimum, b.limit) {
			return fmt.Sprintf("%q must be 0 or a whole number of seconds from %d to %d", b.key, b.minimum, b.limit)
		}
	}
	return ""
}

// validateTierRunCaps refuses a per-tier cap that is not 0 or within its
// range: a whole number for a count, at most four decimals for spend.
func validateTierRunCaps(values map[string]any) string {
	for _, tier := range agentlifecycle.Tiers {
		for _, name := range platformconfig.RunCapNames {
			key := platformconfig.RunCapKey(tier, name)
			raw, present := values[key]
			if !present {
				continue
			}
			if !platformconfig.ValidRunCap(name, raw) {
				if name == platformconfig.RunCapMaxSpend {
					return fmt.Sprintf("%q must be 0 or an amount in USD up to %d, with at most four decimals",
						key, platformconfig.MaxRunCapSpend)
				}
				return fmt.Sprintf("%q must be 0 or a whole number up to %.0f", key, platformconfig.RunCapLimit(name))
			}
		}
	}
	return ""
}
