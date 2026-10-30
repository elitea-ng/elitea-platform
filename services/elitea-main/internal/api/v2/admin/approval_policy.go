package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The approval policy section (ADR-0041 decision 5, #1498). It is LIVE:
// Main reads it through a per-replica cache (platformconfig.CachedSection)
// every time it prepares an agent execution command and every time a
// pipeline run starts, and a save on this replica drops the cache at once;
// other replicas see a save within platformconfig.DefaultSectionTTL.

// WithApprovalPolicyCache supplies the cache an approval policy save drops.
func WithApprovalPolicyCache(cache SectionInvalidator) Option {
	return func(h *Handler) {
		if cache != nil {
			h.approvalPolicy = cache
		}
	}
}

func (h *Handler) invalidateApprovalPolicy(section string) {
	if section == platformconfig.SectionApprovalPolicy && h.approvalPolicy != nil {
		h.approvalPolicy.Invalidate()
	}
}

func tierStrings() []string {
	out := make([]string, 0, len(agentlifecycle.Tiers))
	for _, tier := range agentlifecycle.Tiers {
		out = append(out, string(tier))
	}
	return out
}

func categoryValues(categories []toolcategory.Category) []any {
	out := make([]any, 0, len(categories))
	for _, category := range categories {
		out = append(out, string(category))
	}
	return out
}

// approvalPolicySection is the tier by category matrix and the pipeline
// hitl rule. It uses configuration.governance, the permission of the other
// runtime governance sections, so no migration seeds a new one.
func approvalPolicySection() map[string]any {
	const section = platformconfig.SectionApprovalPolicy
	defaults := approvalpolicy.DefaultPolicy()
	fields := []map[string]any{}
	for _, tier := range agentlifecycle.Tiers {
		fields = append(fields, map[string]any{
			"key":   platformconfig.ApprovalCategoriesKey(tier),
			"type":  "array",
			"items": map[string]any{"type": "string", "enum": toolcategory.Strings()},
			"title": lifecycleTierTitles[tier] + " Tier: Categories That Need Approval",
			"description": "A call of a tool in one of these categories stops and asks a person to approve it, " +
				"for agents of this tier. Undeclared tools are write. The Sensitive Action Tools list of the " +
				"Guardrails section applies as well, and an agent author cannot remove either rule.",
			"section":  section,
			"default":  categoryValues(defaults.CategoriesFor(tier)),
			"maxItems": len(toolcategory.All),
		})
	}
	fields = append(fields, map[string]any{
		"key":   platformconfig.KeyApprovalRequireHITLTiers,
		"type":  "array",
		"items": map[string]any{"type": "string", "enum": tierStrings()},
		"title": "Tiers That Require a HITL Node in Pipelines",
		"description": "A pipeline of one of these tiers must pass a hitl node on every path to a tool node " +
			"whose category is external_send or payment. A pipeline that breaks the rule does not start " +
			"(PIPELINE_HITL_REQUIRED, naming the node and the path). We recommend high and critical.",
		"section":  section,
		"default":  []any{},
		"maxItems": len(agentlifecycle.Tiers),
	})
	return map[string]any{
		"id":    section,
		"title": "Approval Policy",
		"description": "Which tool calls need a human approval, by the agent's risk tier and the tool's category " +
			"(read, write, delete, external_send, payment, code_execution). The default asks for an approval of " +
			"delete, external_send and payment tools at the high and critical tiers. Save this section only " +
			"after every worker runs this release: until the first save, the category rules are off and only " +
			"the Sensitive Action Tools list applies.",
		"order":               7,
		"icon":                "rule",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields":              fields,
	}
}

// validateApprovalPolicyValues refuses an unknown category or tier, and a
// value that is not a list of strings.
func validateApprovalPolicyValues(section string, values map[string]any) string {
	if section != platformconfig.SectionApprovalPolicy {
		return ""
	}
	for _, tier := range agentlifecycle.Tiers {
		key := platformconfig.ApprovalCategoriesKey(tier)
		raw, present := values[key]
		if !present {
			continue
		}
		for _, entry := range approvalStrings(raw) {
			if entry == nil {
				return fmt.Sprintf("%q must be a list of categories", key)
			}
			if _, ok := toolcategory.Parse(*entry); !ok {
				return fmt.Sprintf("%q: %q is not a tool category (read, write, delete, external_send, payment or code_execution)", key, *entry)
			}
		}
		if _, isList := raw.([]any); !isList {
			return fmt.Sprintf("%q must be a list of categories", key)
		}
	}
	if raw, present := values[platformconfig.KeyApprovalRequireHITLTiers]; present {
		if _, isList := raw.([]any); !isList {
			return fmt.Sprintf("%q must be a list of tiers", platformconfig.KeyApprovalRequireHITLTiers)
		}
		for _, entry := range approvalStrings(raw) {
			if entry == nil || !agentlifecycle.Tier(*entry).Valid() {
				return fmt.Sprintf("%q must list low, medium, high or critical", platformconfig.KeyApprovalRequireHITLTiers)
			}
		}
	}
	return ""
}

// approvalStrings answers each element of a JSON list as a string, nil for one
// that is not a string.
func approvalStrings(raw any) []*string {
	list, _ := raw.([]any)
	out := make([]*string, 0, len(list))
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok {
			out = append(out, nil)
			continue
		}
		out = append(out, &text)
	}
	return out
}
