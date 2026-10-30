package platformconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// The content controls of ADR-0043 decisions 3 and 4 (#1500).
//
//   - SectionAIDisclosure: the AI disclosure marker that outbound tools
//     (external_send tools that write human-readable text outside the
//     platform) append, and the toolkit types whose projects may opt out.
//     A project can change the text (project setting `ai_disclosure`) but
//     cannot remove the marker unless the type is listed here.
//   - SectionPlatformPreamble: instruction text that the worker puts before
//     every agent's system prompt, outside author control. Each save that
//     changes the text is a new revision (shared 0216) with a security audit
//     event.
//   - KeyApprovalUntrustedReadThenActTiers in SectionApprovalPolicy: the
//     tiers whose runs have the "read untrusted, then act" rule.
//
// Main sends the three on each agent command (WorkerCommandV1 field 71)
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

// ContentControlsSettings is what Main sends on field 71.
type ContentControlsSettings struct {
	// MarkerText is the administrator's marker text; empty is the default.
	MarkerText string
	// OptOutToolkitTypes are the toolkit types whose projects may opt out
	// of the marker, by their contract names.
	OptOutToolkitTypes []string
	// Preamble is the administrator's preamble text; empty is none.
	Preamble string
	// PreambleRevision is the preamble's newest revision (shared 0216); 0
	// when none was saved.
	PreambleRevision uint32
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
func ContentControlsSettingsFromValues(disclosure, preamble, approval Values, revision uint32) ContentControlsSettings {
	settings := DefaultContentControlsSettings()
	settings.MarkerText = disclosure.trimmed(KeyAIDisclosureMarkerText)
	for _, raw := range disclosure.Strings(KeyAIDisclosureOptOutToolkitTypes) {
		if name, ok := toolcategory.DisclosureToolkitType(raw); ok {
			settings.OptOutToolkitTypes = appendUnique(settings.OptOutToolkitTypes, name)
		}
	}
	settings.Preamble = preamble.trimmed(KeyPlatformPreambleText)
	settings.PreambleRevision = revision
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

// LoadContentControlsSettings reads the two sections, the rule tiers of the
// approval policy and the preamble's newest revision. A database without
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
	revision, err := LatestPlatformPreambleRevision(ctx, pool)
	if err != nil {
		return DefaultContentControlsSettings(), err
	}
	return ContentControlsSettingsFromValues(sections[0], sections[1], sections[2], revision), nil
}

// LatestPlatformPreambleRevision reads the newest preamble revision; 0
// when there is none or shared 0216 has not run.
func LatestPlatformPreambleRevision(ctx context.Context, pool *pgxpool.Pool) (uint32, error) {
	var revision int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(revision), 0) FROM elitea_runtime.platform_preamble_revisions`).Scan(&revision)
	if UndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("platform preamble: read the newest revision: %w", err)
	}
	if revision < 0 || revision > int64(^uint32(0)) {
		return 0, fmt.Errorf("platform preamble: revision %d is out of range", revision)
	}
	return uint32(revision), nil
}
