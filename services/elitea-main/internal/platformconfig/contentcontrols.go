package platformconfig

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// The content controls of ADR-0043 decisions 3 and 4 (#1500).
//
//   - SectionAIDisclosure: the AI disclosure marker that the tools which
//     write human-readable text outside the platform append, and the toolkit types whose projects may opt out.
//     A project can change the text (project setting `ai_disclosure`) but
//     cannot remove the marker unless the type is listed here.
//   - SectionPlatformPreamble: instruction text that the worker puts before
//     every agent's system prompt, outside author control. Each save that
//     changes the text is a new revision (shared 0216) with a security audit
//     event. Admission freezes the newest revision's text into each turn's
//     input; the command carries only its revision and digest.
//   - KeyApprovalUntrustedReadThenActTiers in SectionApprovalPolicy: the
//     tiers whose runs have the "read untrusted, then act" rule.
//
// Main sends the controls on each agent command (WorkerCommandV1 field 71)
// only once one of them is stored: a worker of an earlier release rejects
// the field, and the chart rolls Main out first. Until then the worker
// applies its built-in defaults, which are the ADR defaults.
const (
	SectionAIDisclosure     = "ai_disclosure"
	SectionPlatformPreamble = "platform_preamble"

	KeyAIDisclosureMarkerText         = "marker_text"
	KeyAIDisclosureOptOutToolkitTypes = "opt_out_toolkit_types"
	KeyPlatformPreambleText           = "text"

	KeyApprovalUntrustedReadThenActTiers = "untrusted_read_then_act_tiers"

	// MaxAIDisclosureMarkerTextBytes bounds the marker text (the worker
	// holds the same bound).
	MaxAIDisclosureMarkerTextBytes = 200
	// MaxPlatformPreambleBytes bounds the platform preamble (the worker and
	// shared 0216 hold the same bound).
	MaxPlatformPreambleBytes = 8 * 1024
)

// DefaultUntrustedReadThenActTiers is the ADR reading of the rule: the
// tiers where external_send, delete and payment already ask by default.
// It adds approvals only where an administrator removed those categories
// from the tier, and it never adds one at the low and medium tiers, so the
// default changes nothing for a deployment that upgrades.
func DefaultUntrustedReadThenActTiers() []agentlifecycle.Tier {
	return []agentlifecycle.Tier{agentlifecycle.TierHigh, agentlifecycle.TierCritical}
}

// ContentControlsSettings is what Main sends on field 71. The platform
// preamble's text is not part of it: admission freezes the newest revision
// into each turn's input (agentexecution.WithPlatformPreamble).
type ContentControlsSettings struct {
	// MarkerText is the administrator's marker text; empty is the default.
	MarkerText string
	// OptOutToolkitTypes are the toolkit types whose projects may opt out
	// of the marker, by their contract names.
	OptOutToolkitTypes []string
	// UntrustedReadThenActTiers are the tiers whose runs have the rule.
	UntrustedReadThenActTiers []agentlifecycle.Tier

	// Stored reports that an administrator saved the ai_disclosure or
	// platform_preamble section, or the approval policy's rule tiers. Only
	// then does Main send field 71.
	Stored bool
}

// DefaultContentControlsSettings is a deployment that saved none of them.
func DefaultContentControlsSettings() ContentControlsSettings {
	return ContentControlsSettings{UntrustedReadThenActTiers: DefaultUntrustedReadThenActTiers()}
}

// UntrustedReadThenAct reports whether the rule is on for tier.
func (s ContentControlsSettings) UntrustedReadThenAct(tier agentlifecycle.Tier) bool {
	for _, candidate := range s.UntrustedReadThenActTiers {
		if candidate == tier {
			return true
		}
	}
	return false
}

// ContentControlsSettingsFromValues is the overlay of the three stored
// parts on the defaults. A value of the wrong type keeps the default; an
// unknown toolkit type or tier is skipped.
func ContentControlsSettingsFromValues(disclosure, preamble, approval Values) ContentControlsSettings {
	settings := DefaultContentControlsSettings()
	settings.MarkerText = disclosure.trimmed(KeyAIDisclosureMarkerText)
	for _, raw := range disclosure.Strings(KeyAIDisclosureOptOutToolkitTypes) {
		if name, ok := toolcategory.DisclosureToolkitType(raw); ok {
			settings.OptOutToolkitTypes = appendUnique(settings.OptOutToolkitTypes, name)
		}
	}
	_, tiersStored := approval[KeyApprovalUntrustedReadThenActTiers]
	if _, isList := approval[KeyApprovalUntrustedReadThenActTiers].([]any); isList {
		tiers := []agentlifecycle.Tier{}
		for _, raw := range approval.Strings(KeyApprovalUntrustedReadThenActTiers) {
			if tier := agentlifecycle.Tier(raw); tier.Valid() {
				tiers = append(tiers, tier)
			}
		}
		settings.UntrustedReadThenActTiers = tiers
	}
	settings.Stored = len(disclosure) > 0 || len(preamble) > 0 || tiersStored
	return settings
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// LoadContentControlsSettings reads the two sections and the rule tiers of
// the approval policy. A database without
// a table never saved what it holds: that part is the default.
func LoadContentControlsSettings(ctx context.Context, pool *pgxpool.Pool) (ContentControlsSettings, error) {
	if pool == nil {
		return DefaultContentControlsSettings(), nil
	}
	sections := make([]Values, 0, 3)
	for _, section := range []string{SectionAIDisclosure, SectionPlatformPreamble, SectionApprovalPolicy} {
		values, err := Load(ctx, pool, section)
		if UndefinedTable(err) {
			values, err = Values{}, nil
		}
		if err != nil {
			return DefaultContentControlsSettings(), err
		}
		sections = append(sections, values)
	}
	return ContentControlsSettingsFromValues(sections[0], sections[1], sections[2]), nil
}
