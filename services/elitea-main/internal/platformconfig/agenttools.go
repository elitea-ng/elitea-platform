package platformconfig

import (
	"context"
	"math"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolenvelope"
)

// SectionAgents is the platform's ceilings on the per-agent tool envelope
// (ADR-0050 decision 3, issue 1613). An agent version's
// meta.tool_envelope can only narrow the tools a run has; these ceilings
// narrow the envelope again:
//
//   - max_tool_limit is the most calls of any one toolkit tool in a turn (0 is none);
//   - unattended_tools_allowlist names the tools that are not read-only and
//     that an agent may still use in a turn nobody waits on. A tool that
//     writes and is not named stays out of unattended turns whatever the
//     agent's envelope says.
//   - subagents_max_depth (ADR-0050 decision 4, issue 1824) is how deep
//     sub-agents may nest: 1 (the default) or 2.
//
// Main reads it (through a per-replica cache) at every agent admission and
// freezes it into the run input (AgentExecutionInputV1.tool_envelope).
const SectionAgents = "agents"

// Field keys of SectionAgents.
const (
	KeyAgentsMaxToolLimit              = "max_tool_limit"
	KeyAgentsUnattendedToolsAllowlist  = "unattended_tools_allowlist"
	MaxAgentsMaxToolLimit              = 10_000
	MaxAgentsUnattendedAllowlistLength = 256
	KeyAgentsSubagentsMaxDepth         = "subagents_max_depth"
	// DefaultSubagentsMaxDepth and MaxSubagentsMaxDepth bound
	// subagents_max_depth (ADR-0050 decision 4, issue 1824): 1 lets an
	// agent start sub-agents that start none; 2 lets each of them start
	// sub-agents of its own, which start none.
	DefaultSubagentsMaxDepth = 1
	MaxSubagentsMaxDepth     = 2
)

// AgentToolSettings is the section.
type AgentToolSettings struct {
	MaxToolLimit             uint32   `json:"max_tool_limit"`
	UnattendedToolsAllowlist []string `json:"unattended_tools_allowlist"`
	// SubagentsMaxDepth is 1 or 2; 0 (a section never saved) is the
	// default, 1. Read it through SubagentsDepth.
	SubagentsMaxDepth uint32 `json:"subagents_max_depth"`
}

// Empty reports no tool ceilings: no per-tool limit and no tool that
// writes allowed unattended. The sub-agent depth is not a tool ceiling: it
// travels in the run's sub-agent policy, not in the tool envelope, so a
// depth of 2 alone leaves a run's envelope absent.
func (s AgentToolSettings) Empty() bool {
	return s.MaxToolLimit == 0 && len(s.UnattendedToolsAllowlist) == 0
}

// AgentToolSettingsFromValues is the overlay. A limit out of range is
// clamped and an unreadable one is 1; a malformed allowlist entry is
// dropped. A hand-written row can tighten the ceilings and never silently
// loosen them.
func AgentToolSettingsFromValues(values Values) AgentToolSettings {
	var settings AgentToolSettings
	if raw, present := values[KeyAgentsMaxToolLimit]; present && raw != nil {
		number, ok := raw.(float64)
		switch {
		case !ok || math.IsNaN(number) || number < 0:
			settings.MaxToolLimit = 1
		case number > MaxAgentsMaxToolLimit:
			settings.MaxToolLimit = MaxAgentsMaxToolLimit
		default:
			settings.MaxToolLimit = uint32(math.Trunc(number))
		}
	}
	if raw, ok := values[KeyAgentsUnattendedToolsAllowlist].([]any); ok {
		for _, item := range raw {
			if key, ok := item.(string); ok && ValidAgentToolKey(key) {
				settings.UnattendedToolsAllowlist = append(settings.UnattendedToolsAllowlist, key)
			}
		}
		slices.Sort(settings.UnattendedToolsAllowlist)
		settings.UnattendedToolsAllowlist = slices.Compact(settings.UnattendedToolsAllowlist)
		if len(settings.UnattendedToolsAllowlist) > MaxAgentsUnattendedAllowlistLength {
			settings.UnattendedToolsAllowlist = settings.UnattendedToolsAllowlist[:MaxAgentsUnattendedAllowlistLength]
		}
	}
	// A depth out of range or unreadable is the default: a hand-written row
	// never lets sub-agents nest deeper than the maximum.
	if raw, present := values[KeyAgentsSubagentsMaxDepth]; present && raw != nil {
		if ValidSubagentsMaxDepth(raw) {
			settings.SubagentsMaxDepth = uint32(raw.(float64))
		} else {
			settings.SubagentsMaxDepth = DefaultSubagentsMaxDepth
		}
	}
	return settings
}

// SubagentsDepth is the effective depth: 1 or 2.
func (s AgentToolSettings) SubagentsDepth() uint32 {
	if s.SubagentsMaxDepth >= MaxSubagentsMaxDepth {
		return MaxSubagentsMaxDepth
	}
	return DefaultSubagentsMaxDepth
}

// ValidSubagentsMaxDepth reports 1 or 2. JSON numbers arrive as float64.
func ValidSubagentsMaxDepth(raw any) bool {
	number, ok := raw.(float64)
	return ok && (number == DefaultSubagentsMaxDepth || number == MaxSubagentsMaxDepth)
}

// ValidAgentToolKey reports a "<toolkit>/<tool>" key, the tool part "*" for
// every tool of the toolkit (the toolenvelope key rule). The worker matches
// an allowlist key by the toolkit's type.
func ValidAgentToolKey(key string) bool {
	return toolenvelope.ValidKey(key)
}

// ValidAgentsMaxToolLimit reports a whole number from 0 to
// MaxAgentsMaxToolLimit. JSON numbers arrive as float64.
func ValidAgentsMaxToolLimit(raw any) bool {
	number, ok := raw.(float64)
	return ok && !math.IsNaN(number) && number == math.Trunc(number) &&
		number >= 0 && number <= MaxAgentsMaxToolLimit
}

// LoadAgentToolSettings reads the section. A database without the table
// never saved it and answers the defaults.
func LoadAgentToolSettings(ctx context.Context, pool *pgxpool.Pool) (AgentToolSettings, error) {
	values, err := Load(ctx, pool, SectionAgents)
	if UndefinedTable(err) {
		return AgentToolSettings{}, nil
	}
	if err != nil {
		return AgentToolSettings{}, err
	}
	return AgentToolSettingsFromValues(values), nil
}
