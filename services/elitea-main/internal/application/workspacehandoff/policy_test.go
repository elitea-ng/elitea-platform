package workspacehandoff

import (
	"reflect"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

func boolPtr(value bool) *bool    { return &value }
func intPtr(value int64) *int64   { return &value }
func strPtr(value string) *string { return &value }

func TestNarrowOnlyNarrows(t *testing.T) {
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	ceiling.Enabled = true
	full := Narrow(ceiling, ProjectSetting{}, true, true)
	if !full.Enabled || !full.Shell || !full.Network || full.HistoryFullAllowed ||
		full.Limits.MaxSegmentBytes != 200<<20 || full.Limits.MaxProjectWorkspaceBytes != 50<<30 ||
		full.Limits.MaxFiles != 20000 || full.IdleRetention != 30*24*time.Hour || full.ReturnGrace != 7*24*time.Hour {
		t.Fatalf("no project setting = %+v", full)
	}
	for name, tc := range map[string]struct {
		ceiling   func(*platformconfig.CloudExecutorSettings)
		project   ProjectSetting
		device    bool
		encrypted bool
		check     func(Policy) bool
	}{
		"the device switch off":  {nil, ProjectSetting{}, false, true, func(p Policy) bool { return !p.Enabled }},
		"encryption unconfirmed": {nil, ProjectSetting{}, true, false, func(p Policy) bool { return !p.Enabled }},
		"the project turns it off": {nil, ProjectSetting{Enabled: boolPtr(false)}, true, true,
			func(p Policy) bool { return !p.Enabled }},
		"the project cannot turn it on": {func(c *platformconfig.CloudExecutorSettings) { c.Enabled = false },
			ProjectSetting{Enabled: boolPtr(true)}, true, true, func(p Policy) bool { return !p.Enabled }},
		"the project cannot turn the shell on": {func(c *platformconfig.CloudExecutorSettings) { c.Shell = false },
			ProjectSetting{Shell: boolPtr(true)}, true, true, func(p Policy) bool { return !p.Shell }},
		"the project turns the network off": {nil, ProjectSetting{Network: boolPtr(false)}, true, true,
			func(p Policy) bool { return !p.Network }},
		"full history needs the platform": {nil, ProjectSetting{History: strPtr("full")}, true, true,
			func(p Policy) bool { return !p.HistoryFullAllowed }},
		"full history allowed by both": {func(c *platformconfig.CloudExecutorSettings) { c.HistoryFullAllowed = true },
			ProjectSetting{History: strPtr("full")}, true, true, func(p Policy) bool { return p.HistoryFullAllowed }},
		"the project chooses none": {func(c *platformconfig.CloudExecutorSettings) { c.HistoryFullAllowed = true },
			ProjectSetting{History: strPtr("none")}, true, true, func(p Policy) bool { return !p.HistoryFullAllowed }},
		"a lower cap applies": {nil, ProjectSetting{MaxSegmentMB: intPtr(10), MaxProjectWorkspaceGB: intPtr(2)}, true, true,
			func(p Policy) bool {
				return p.Limits.MaxSegmentBytes == 10<<20 && p.Limits.MaxProjectWorkspaceBytes == 2<<30
			}},
		"a higher cap does not": {nil, ProjectSetting{MaxSegmentMB: intPtr(9999), IdleRetentionDays: intPtr(90)}, true, true,
			func(p Policy) bool { return p.Limits.MaxSegmentBytes == 200<<20 && p.IdleRetention == 30*24*time.Hour }},
		"a project allowlist under an open platform": {nil,
			ProjectSetting{NetworkAllowlist: &[]string{"github.com"}}, true, true,
			func(p Policy) bool { return p.Network && reflect.DeepEqual(p.NetworkAllowlist, []string{"github.com"}) }},
		"the allowlist intersects": {func(c *platformconfig.CloudExecutorSettings) { c.NetworkAllowlist = []string{"github.com", "pypi.org"} },
			ProjectSetting{NetworkAllowlist: &[]string{"pypi.org", "evil.example"}}, true, true,
			func(p Policy) bool { return p.Network && reflect.DeepEqual(p.NetworkAllowlist, []string{"pypi.org"}) }},
		"an empty intersection turns the network off, never open": {
			func(c *platformconfig.CloudExecutorSettings) { c.NetworkAllowlist = []string{"github.com"} },
			ProjectSetting{NetworkAllowlist: &[]string{"evil.example"}}, true, true,
			func(p Policy) bool { return !p.Network }},
	} {
		settings := ceiling
		if tc.ceiling != nil {
			tc.ceiling(&settings)
		}
		if got := Narrow(settings, tc.project, tc.device, tc.encrypted); !tc.check(got) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
