package platformconfig

import (
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

func TestContentControlsSettingsDefaultsAndOverlay(t *testing.T) {
	t.Parallel()
	none := ContentControlsSettingsFromValues(Values{}, Values{}, Values{}, 0)
	if none.Stored || !slices.Equal(none.UntrustedReadThenActTiers, DefaultUntrustedReadThenActTiers()) {
		t.Fatalf("defaults = %+v", none)
	}
	// A saved approval policy without the rule key does not send field 71:
	// it was saved by a release that did not know the key.
	if ContentControlsSettingsFromValues(Values{}, Values{}, Values{"high_approval_categories": []any{}}, 0).Stored {
		t.Fatal("an approval policy without the rule tiers must not store the controls")
	}
	settings := ContentControlsSettingsFromValues(
		Values{KeyAIDisclosureMarkerText: "  ACME AI  ", KeyAIDisclosureOptOutToolkitTypes: []any{"Slack", "unknown", "slack", 7}},
		Values{KeyPlatformPreambleText: " Rules. "},
		Values{KeyApprovalUntrustedReadThenActTiers: []any{"medium", "bogus"}}, 3)
	if !settings.Stored || settings.MarkerText != "ACME AI" || !slices.Equal(settings.OptOutToolkitTypes, []string{"slack"}) ||
		settings.Preamble != "Rules." || settings.PreambleRevision != 3 ||
		!settings.UntrustedReadThenAct(agentlifecycle.TierMedium) || settings.UntrustedReadThenAct(agentlifecycle.TierHigh) {
		t.Fatalf("settings = %+v", settings)
	}
	// A value that is not a list keeps the default tiers.
	kept := ContentControlsSettingsFromValues(Values{}, Values{}, Values{KeyApprovalUntrustedReadThenActTiers: "high"}, 0)
	if !kept.Stored || !kept.UntrustedReadThenAct(agentlifecycle.TierCritical) {
		t.Fatalf("settings = %+v", kept)
	}
}
