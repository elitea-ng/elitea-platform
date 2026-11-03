package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The unattended grant fields of the approvals section (ADR-0045 decisions
// 2 and 4, #1435, #1436). They are LIVE: a start reads them when it admits
// a grant, and a chain check reads the limits the start stored.

// unattendedGrantFields are the grant switch, the withheld categories and
// the chain defaults and maxima.
func unattendedGrantFields(section string) []map[string]any {
	defaults := platformconfig.DefaultUnattendedSettings()
	whole := func(key, title, description string, fallback, low, high int) map[string]any {
		return map[string]any{
			"key": key, "type": "integer", "title": title, "description": description,
			"section": section, "default": fallback, "minimum": low, "maximum": high,
		}
	}
	return []map[string]any{
		{
			"key":   platformconfig.KeyApprovalsUnattendedGrants,
			"type":  "boolean",
			"title": "Unattended Grants",
			"description": "A person who starts a run can give an unattended grant: the answers to the step " +
				"limit, the output limit and named sensitive tools, before the run starts. Each use of a named " +
				"tool is recorded in the audit trail as that person's decision (reason unattended_grant). " +
				"Turn this off to refuse every grant (422 unattended_grant_refused). Schedules and triggers " +
				"never carry a grant.",
			"section": section,
			"default": defaults.Enabled,
		},
		{
			"key":   platformconfig.KeyApprovalsWithheldCategories,
			"type":  "array",
			"items": map[string]any{"type": "string", "enum": toolcategory.Strings()},
			"title": "Categories a Grant Cannot Name",
			"description": "A grant that names a tool in one of these categories is refused. The default " +
				"withholds payment.",
			"section":  section,
			"default":  categoryValues(defaults.Withheld),
			"maxItems": len(toolcategory.All),
		},
		whole(platformconfig.KeyApprovalsChainDefaultContinuations, "Chain: Default Continuations",
			"How many automatic continuations a chain gets when its grant names no number. Default 20.",
			platformconfig.DefaultChainContinuations, 0, platformconfig.MaxChainContinuationsBound),
		whole(platformconfig.KeyApprovalsChainMaxContinuations, "Chain: Maximum Continuations",
			"The largest number of continuations a grant can name. A larger number is refused. Default 200.",
			platformconfig.DefaultChainMaxContinuations, 0, platformconfig.MaxChainContinuationsBound),
		whole(platformconfig.KeyApprovalsChainSpendMultiplier, "Chain: Default Spend Multiplier",
			"When a grant names no spend limit, the chain's limit is the per-run spend cap of the agent's "+
				"tier (Run Caps section) times this number. Default 10. No per-run cap means no default limit.",
			platformconfig.DefaultChainSpendMultiplier, 1, platformconfig.MaxChainSpendMultiplier),
		{
			"key":   platformconfig.KeyApprovalsChainMaxSpendUSD,
			"type":  "number",
			"title": "Chain: Maximum Spend (USD)",
			"description": "The largest chain spend a grant can name, in USD. 0 (the default) sets no platform " +
				"maximum. The gateway's period budgets apply to every link of a chain in all cases.",
			"section": section,
			"default": 0,
			"minimum": 0,
			"maximum": platformconfig.MaxChainSpendUSDBound,
		},
		whole(platformconfig.KeyApprovalsChainDefaultDeadlineHours, "Chain: Default Deadline (hours)",
			"How long a chain can run when its grant names no deadline. Default 12.",
			platformconfig.DefaultChainDeadlineHours, platformconfig.MinChainDeadlineHours,
			platformconfig.MaxChainDeadlineHoursBound),
		whole(platformconfig.KeyApprovalsChainMaxDeadlineHours, "Chain: Maximum Deadline (hours)",
			"The longest deadline a grant can name. A longer one is refused. Default 168 (7 days).",
			platformconfig.DefaultChainMaxDeadlineHours, platformconfig.MinChainDeadlineHours,
			platformconfig.MaxChainDeadlineHoursBound),
	}
}

// validateUnattendedGrantValues checks the grant fields of a save.
func validateUnattendedGrantValues(values map[string]any) string {
	if raw, present := values[platformconfig.KeyApprovalsUnattendedGrants]; present {
		if _, ok := raw.(bool); !ok {
			return fmt.Sprintf("%q must be true or false", platformconfig.KeyApprovalsUnattendedGrants)
		}
	}
	if raw, present := values[platformconfig.KeyApprovalsWithheldCategories]; present {
		if _, isList := raw.([]any); !isList {
			return fmt.Sprintf("%q must be a list of categories", platformconfig.KeyApprovalsWithheldCategories)
		}
		for _, entry := range approvalStrings(raw) {
			if entry == nil {
				return fmt.Sprintf("%q must be a list of categories", platformconfig.KeyApprovalsWithheldCategories)
			}
			if _, ok := toolcategory.Parse(*entry); !ok {
				return fmt.Sprintf("%q: %q is not a tool category", platformconfig.KeyApprovalsWithheldCategories, *entry)
			}
		}
	}
	type bound struct {
		key       string
		low, high float64
	}
	for _, check := range []bound{
		{platformconfig.KeyApprovalsChainDefaultContinuations, 0, platformconfig.MaxChainContinuationsBound},
		{platformconfig.KeyApprovalsChainMaxContinuations, 0, platformconfig.MaxChainContinuationsBound},
		{platformconfig.KeyApprovalsChainSpendMultiplier, 1, platformconfig.MaxChainSpendMultiplier},
		{platformconfig.KeyApprovalsChainDefaultDeadlineHours, platformconfig.MinChainDeadlineHours,
			platformconfig.MaxChainDeadlineHoursBound},
		{platformconfig.KeyApprovalsChainMaxDeadlineHours, platformconfig.MinChainDeadlineHours,
			platformconfig.MaxChainDeadlineHoursBound},
	} {
		if raw, present := values[check.key]; present && !platformconfig.ValidWholeInRange(raw, check.low, check.high) {
			return fmt.Sprintf("%q must be a whole number from %.0f to %.0f", check.key, check.low, check.high)
		}
	}
	if raw, present := values[platformconfig.KeyApprovalsChainMaxSpendUSD]; present {
		usd, ok := raw.(float64)
		if !ok || usd < 0 || usd > platformconfig.MaxChainSpendUSDBound {
			return fmt.Sprintf("%q must be a USD amount from 0 to %d", platformconfig.KeyApprovalsChainMaxSpendUSD,
				platformconfig.MaxChainSpendUSDBound)
		}
	}
	return ""
}
