package platformconfig

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/approvalpolicy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

// SectionApprovalPolicy is the platform's approval policy (ADR-0041
// decision 5, #1498): per risk tier, the tool categories whose calls need a
// human approval, and the tiers whose pipelines must pass a `hitl` node on
// every path to an external_send or payment tool node. Main reads it
// (through a per-replica cache) when it prepares an agent execution command
// (WorkerCommandV1.approval_policy) and when a pipeline run starts.
//
// A key that is absent keeps the ADR default (approvalpolicy.DefaultPolicy).
const SectionApprovalPolicy = "approval_policy"

// Keys of SectionApprovalPolicy. Each tier has the key
// "<tier>_approval_categories", for example "high_approval_categories".
const (
	KeyApprovalCategoriesSuffix = "approval_categories"
	KeyApprovalRequireHITLTiers = "require_hitl_tiers"
)

// ApprovalPolicySettings is the section.
type ApprovalPolicySettings struct {
	Policy approvalpolicy.Policy
	// Stored reports that the section has a stored value: an administrator
	// saved it. Only then does Main send the policy (WorkerCommandV1 field
	// 68). The chart rolls Main out before the workers, and a worker from an
	// earlier release rejects the field, so a deployment that never saved
	// the section sends none, and no category rule applies until it does.
	// The hitl rule of pipelines is Main's own check and applies from the
	// first save too; its default requires no tier.
	Stored bool
}

// DefaultApprovalPolicySettings is a deployment that never saved the section.
func DefaultApprovalPolicySettings() ApprovalPolicySettings {
	return ApprovalPolicySettings{Policy: approvalpolicy.DefaultPolicy()}
}

// ApprovalCategoriesKey is the key of one tier's categories.
func ApprovalCategoriesKey(tier agentlifecycle.Tier) string {
	return string(tier) + "_" + KeyApprovalCategoriesSuffix
}

// ApprovalPolicySettingsFromValues is the overlay of the section on the ADR
// defaults. An unknown category or tier is skipped; a value that is not a
// list keeps the default.
func ApprovalPolicySettingsFromValues(values Values) ApprovalPolicySettings {
	settings := DefaultApprovalPolicySettings()
	settings.Stored = len(values) > 0
	for _, tier := range agentlifecycle.Tiers {
		key := ApprovalCategoriesKey(tier)
		if _, isList := values[key].([]any); !isList {
			continue
		}
		categories := []toolcategory.Category{}
		for _, raw := range values.Strings(key) {
			if category, ok := toolcategory.Parse(raw); ok {
				categories = append(categories, category)
			}
		}
		settings.Policy.Categories[tier] = approvalpolicy.Normalize(categories)
	}
	if _, isList := values[KeyApprovalRequireHITLTiers].([]any); isList {
		tiers := []agentlifecycle.Tier{}
		for _, raw := range values.Strings(KeyApprovalRequireHITLTiers) {
			if tier := agentlifecycle.Tier(raw); tier.Valid() {
				tiers = append(tiers, tier)
			}
		}
		settings.Policy.RequireHITL = tiers
	}
	return settings
}

// LoadApprovalPolicySettings reads the section. A database without the
// table never saved it and answers the defaults.
func LoadApprovalPolicySettings(ctx context.Context, pool *pgxpool.Pool) (ApprovalPolicySettings, error) {
	values, err := Load(ctx, pool, SectionApprovalPolicy)
	if missingTable(err) {
		return DefaultApprovalPolicySettings(), nil
	}
	if err != nil {
		return DefaultApprovalPolicySettings(), err
	}
	return ApprovalPolicySettingsFromValues(values), nil
}
