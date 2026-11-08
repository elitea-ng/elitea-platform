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
//   - max_tool_limit is the most calls of any one tool in a run (0 is none);
//   - unattended_tools_allowlist names the tools that are not read-only and
//     that an agent may still use in a turn nobody waits on. A tool that
//     writes and is not named stays out of unattended turns whatever the
//     agent's envelope says.
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
)

// AgentToolSettings is the section.
type AgentToolSettings struct {
	MaxToolLimit             uint32   `json:"max_tool_limit"`
	UnattendedToolsAllowlist []string `json:"unattended_tools_allowlist"`
}

// Empty reports the defaults: no ceiling and no tool that writes allowed
// unattended.
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
	return settings
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
