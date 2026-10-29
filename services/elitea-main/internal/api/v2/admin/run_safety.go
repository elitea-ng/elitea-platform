package admin

import (
	"fmt"

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
	return map[string]any{
		"id":    section,
		"title": "Run Caps",
		"description": "Limits on one agent run. A run that reaches a cap stops with CAP_REACHED and the " +
			"conversation shows why.",
		"order":               6,
		"icon":                "hourglass_bottom",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
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
		},
	}
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
