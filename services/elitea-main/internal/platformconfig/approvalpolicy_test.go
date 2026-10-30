package platformconfig

import (
	"reflect"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/toolcategory"
)

func TestApprovalPolicySettingsDefaults(t *testing.T) {
	t.Parallel()
	settings := ApprovalPolicySettingsFromValues(Values{})
	if settings.Stored {
		t.Fatal("an empty section is stored")
	}
	if got := settings.Policy.CategoriesFor(agentlifecycle.TierHigh); len(got) != 3 {
		t.Fatalf("high = %v", got)
	}
	if len(settings.Policy.RequireHITL) != 0 {
		t.Fatalf("require hitl = %v", settings.Policy.RequireHITL)
	}
}

// A saved section is stored even when every value is the default: only
// then does Main send WorkerCommandV1.approval_policy.
func TestApprovalPolicySettingsStoredOnlyWhenSaved(t *testing.T) {
	t.Parallel()
	saved := ApprovalPolicySettingsFromValues(Values{KeyApprovalRequireHITLTiers: []any{}})
	if !saved.Stored || len(saved.Policy.CategoriesFor(agentlifecycle.TierCritical)) != 3 {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestApprovalPolicySettingsFromValues(t *testing.T) {
	t.Parallel()
	settings := ApprovalPolicySettingsFromValues(Values{
		"low_approval_categories":      []any{"payment"},
		"medium_approval_categories":   []any{"external_send", "admin", "delete", "external_send"},
		"high_approval_categories":     []any{},
		"critical_approval_categories": "everything",
		KeyApprovalRequireHITLTiers:    []any{"critical", "extreme", "high"},
	})
	for tier, want := range map[agentlifecycle.Tier][]toolcategory.Category{
		agentlifecycle.TierLow:      {toolcategory.Payment},
		agentlifecycle.TierMedium:   {toolcategory.Delete, toolcategory.ExternalSend},
		agentlifecycle.TierHigh:     {},
		agentlifecycle.TierCritical: {toolcategory.Delete, toolcategory.ExternalSend, toolcategory.Payment},
	} {
		if got := settings.Policy.CategoriesFor(tier); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", tier, got, want)
		}
	}
	if !reflect.DeepEqual(settings.Policy.RequireHITL, []agentlifecycle.Tier{agentlifecycle.TierCritical, agentlifecycle.TierHigh}) {
		t.Errorf("require hitl = %v", settings.Policy.RequireHITL)
	}
}
