package admin

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The content control sections (ADR-0043 decisions 3 and 4, #1500): the AI
// disclosure marker, the platform preamble and the "read untrusted, then
// act" tiers of the approval policy. They are LIVE: Main reads them through
// a per-replica cache every time it prepares an agent execution command
// (WorkerCommandV1 field 71), and a save on this replica drops the cache at
// once.

// WithContentControlsCache supplies the cache a save of ai_disclosure,
// platform_preamble or approval_policy drops.
func WithContentControlsCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.contentControls = cache
		}
	}
}

func (h *Handler) invalidateContentControls(section string) {
	if h.contentControls == nil {
		return
	}
	switch section {
	case platformconfig.SectionAIDisclosure, platformconfig.SectionPlatformPreamble, platformconfig.SectionApprovalPolicy:
		h.contentControls.Invalidate()
	}
}

// aiDisclosureSection is the marker text and the toolkit types whose
// projects may opt out. It uses configuration.governance.
func aiDisclosureSection() map[string]any {
	const section = platformconfig.SectionAIDisclosure
	return map[string]any{
		"id":    section,
		"title": "AI Disclosure",
		"description": "Outbound tools that write human-readable text outside the platform (a Jira comment, a " +
			"Slack message, an e-mail, a pull request comment) add this marker to the text they send. A project " +
			"can change the text but cannot remove the marker. Save this section only after every worker runs " +
			"this release. Until the first save, workers add the default text.",
		"order":               8,
		"icon":                "label",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyAIDisclosureMarkerText,
				"type":  "string",
				"title": "Marker Text",
				"description": "The text the tools add at the end of the content they send. Empty is the default, " +
					"\"" + toolcategory.DefaultDisclosureMarkerText + "\". The wording that your jurisdiction " +
					"requires is your decision.",
				"section":   section,
				"default":   "",
				"maxLength": platformconfig.MaxAIDisclosureMarkerTextBytes,
			},
			{
				"key":   platformconfig.KeyAIDisclosureOptOutToolkitTypes,
				"type":  "array",
				"items": map[string]any{"type": "string", "enum": toolcategory.DisclosureToolkitTypes()},
				"title": "Toolkit Types a Project Can Opt Out Of",
				"description": "A project can turn the marker off for these toolkit types only. We recommend " +
					"an empty list.",
				"section":  section,
				"default":  []any{},
				"maxItems": len(toolcategory.DisclosureToolkitTypes()),
			},
		},
	}
}

// platformPreambleSection is the preamble text. Each save that changes the
// text is a new revision with a security audit event (shared 0216).
func platformPreambleSection() map[string]any {
	const section = platformconfig.SectionPlatformPreamble
	return map[string]any{
		"id":    section,
		"title": "Platform Preamble",
		"description": "Instructions that the worker puts before the system prompt of every agent. The agent " +
			"author cannot change or remove them. The worker always puts its built-in rule for untrusted " +
			"content first. Each change makes a new revision and a security audit event. Save this section " +
			"only after every worker runs this release.",
		"order":               9,
		"icon":                "article",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":         platformconfig.KeyPlatformPreambleText,
				"type":        "string",
				"format":      "textarea",
				"title":       "Preamble",
				"description": "Plain text, at most 8 KiB. Keep it short: each model call sends it.",
				"section":     section,
				"default":     "",
				"maxLength":   platformconfig.MaxPlatformPreambleBytes,
			},
		},
	}
}

// untrustedReadThenActField is the approval policy's rule tiers.
func untrustedReadThenActField() map[string]any {
	defaults := []any{}
	for _, tier := range platformconfig.DefaultUntrustedReadThenActTiers() {
		defaults = append(defaults, string(tier))
	}
	return map[string]any{
		"key":   platformconfig.KeyApprovalUntrustedReadThenActTiers,
		"type":  "array",
		"items": map[string]any{"type": "string", "enum": tierStrings()},
		"title": "Tiers With the Read Untrusted, Then Act Rule",
		"description": "For agents of these tiers, a call of a delete, external_send or payment tool asks for " +
			"an approval when it comes after a tool result in the same turn. The default is high and critical.",
		"section":  platformconfig.SectionApprovalPolicy,
		"default":  defaults,
		"maxItems": len(defaults) + 2,
	}
}

// validateContentControlsValues refuses a marker or preamble that is too
// long or holds a control character the worker cannot carry, an unknown
// opt-out toolkit type and an unknown rule tier.
func validateContentControlsValues(section string, values map[string]any) string {
	switch section {
	case platformconfig.SectionAIDisclosure:
		if raw, present := values[platformconfig.KeyAIDisclosureMarkerText]; present {
			text, ok := raw.(string)
			if !ok || len(text) > platformconfig.MaxAIDisclosureMarkerTextBytes || strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
				return fmt.Sprintf("%q must be text of at most %d bytes", platformconfig.KeyAIDisclosureMarkerText, platformconfig.MaxAIDisclosureMarkerTextBytes)
			}
		}
		if raw, present := values[platformconfig.KeyAIDisclosureOptOutToolkitTypes]; present {
			if _, isList := raw.([]any); !isList {
				return fmt.Sprintf("%q must be a list of toolkit types", platformconfig.KeyAIDisclosureOptOutToolkitTypes)
			}
			for _, entry := range approvalStrings(raw) {
				if entry == nil {
					return fmt.Sprintf("%q must be a list of toolkit types", platformconfig.KeyAIDisclosureOptOutToolkitTypes)
				}
				if _, ok := toolcategory.DisclosureToolkitType(*entry); !ok {
					return fmt.Sprintf("%q: %q has no outbound tool that adds the marker", platformconfig.KeyAIDisclosureOptOutToolkitTypes, *entry)
				}
			}
		}
	case platformconfig.SectionPlatformPreamble:
		if raw, present := values[platformconfig.KeyPlatformPreambleText]; present {
			text, ok := raw.(string)
			if !ok || len(text) > platformconfig.MaxPlatformPreambleBytes || strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
				return fmt.Sprintf("%q must be text of at most %d bytes", platformconfig.KeyPlatformPreambleText, platformconfig.MaxPlatformPreambleBytes)
			}
		}
	case platformconfig.SectionApprovalPolicy:
		if raw, present := values[platformconfig.KeyApprovalUntrustedReadThenActTiers]; present {
			if _, isList := raw.([]any); !isList {
				return fmt.Sprintf("%q must be a list of tiers", platformconfig.KeyApprovalUntrustedReadThenActTiers)
			}
			for _, entry := range approvalStrings(raw) {
				if entry == nil || !agentTier(*entry) {
					return fmt.Sprintf("%q must list low, medium, high or critical", platformconfig.KeyApprovalUntrustedReadThenActTiers)
				}
			}
		}
	}
	return ""
}

func agentTier(value string) bool {
	for _, tier := range tierStrings() {
		if tier == value {
			return true
		}
	}
	return false
}

// recordPlatformPreambleTx writes a new preamble revision with its audit
// event in the save's transaction when the save changes the text.
func recordPlatformPreambleTx(ctx context.Context, tx pgx.Tx, section string, values map[string]any, author string) error {
	if section != platformconfig.SectionPlatformPreamble {
		return nil
	}
	raw, present := values[platformconfig.KeyPlatformPreambleText]
	if !present {
		return nil
	}
	text, _ := raw.(string)
	_, err := repos.RecordPlatformPreambleRevisionTx(ctx, tx, strings.TrimSpace(text), author)
	return err
}
