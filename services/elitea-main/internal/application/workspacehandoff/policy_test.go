package workspacehandoff

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func boolPtr(value bool) *bool    { return &value }
func intPtr(value int64) *int64   { return &value }
func strPtr(value string) *string { return &value }

func TestNarrowOnlyNarrows(t *testing.T) {
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	ceiling.Enabled, ceiling.ProjectDefaultEnabled = true, true
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

func TestNarrowNamesWhyItIsOff(t *testing.T) {
	on := platformconfig.DefaultCloudExecutorSettings()
	on.Enabled, on.ProjectDefaultEnabled = true, true
	off := on
	off.Enabled = false
	for name, tc := range map[string]struct {
		ceiling           platformconfig.CloudExecutorSettings
		project           ProjectSetting
		device, encrypted bool
		want              string
	}{
		"enabled":                  {on, ProjectSetting{}, true, true, ""},
		"the platform is off":      {off, ProjectSetting{Enabled: boolPtr(true)}, false, false, DisabledPlatformOff},
		"the project turns it off": {on, ProjectSetting{Enabled: boolPtr(false)}, false, false, DisabledProjectOff},
		"the device switch is off": {on, ProjectSetting{}, false, false, DisabledDeviceOff},
		"encryption unconfirmed":   {on, ProjectSetting{Enabled: boolPtr(true)}, true, false, DisabledStorageEncryptionUnconfirmed},
	} {
		got := Narrow(tc.ceiling, tc.project, tc.device, tc.encrypted)
		if got.DisabledReason != tc.want || got.Enabled != (tc.want == "") {
			t.Fatalf("%s: enabled %v reason %q, want %q", name, got.Enabled, got.DisabledReason, tc.want)
		}
	}
}

// The enabled dimension over its three inputs: the platform switch, the
// platform's project default and the project's explicit value (nil = unset).
// The explicit value wins under a platform that is on; unset follows the
// default; the platform's off always wins.
func TestNarrowEnabledMatrix(t *testing.T) {
	for _, tc := range []struct {
		global, projectDefault bool
		explicit               *bool
		want                   bool
		reason                 string
	}{
		{false, false, nil, false, DisabledPlatformOff},
		{false, false, boolPtr(true), false, DisabledPlatformOff},
		{false, false, boolPtr(false), false, DisabledPlatformOff},
		{false, true, nil, false, DisabledPlatformOff},
		{false, true, boolPtr(true), false, DisabledPlatformOff},
		{false, true, boolPtr(false), false, DisabledPlatformOff},
		{true, false, nil, false, DisabledProjectOff},
		{true, false, boolPtr(true), true, ""},
		{true, false, boolPtr(false), false, DisabledProjectOff},
		{true, true, nil, true, ""},
		{true, true, boolPtr(true), true, ""},
		{true, true, boolPtr(false), false, DisabledProjectOff},
	} {
		ceiling := platformconfig.DefaultCloudExecutorSettings()
		ceiling.Enabled, ceiling.ProjectDefaultEnabled = tc.global, tc.projectDefault
		explicit := "unset"
		if tc.explicit != nil {
			explicit = map[bool]string{true: "on", false: "off"}[*tc.explicit]
		}
		got := Narrow(ceiling, ProjectSetting{Enabled: tc.explicit}, true, true)
		if got.Enabled != tc.want || got.DisabledReason != tc.reason {
			t.Errorf("global %v, default %v, project %s: enabled %v reason %q, want %v %q",
				tc.global, tc.projectDefault, explicit, got.Enabled, got.DisabledReason, tc.want, tc.reason)
		}
		// The device switch and the storage check still only turn it off.
		if off := Narrow(ceiling, ProjectSetting{Enabled: tc.explicit}, false, true); off.Enabled {
			t.Errorf("global %v, default %v, project %s: the device switch off left it on", tc.global, tc.projectDefault, explicit)
		}
	}
	// The other switches keep their narrow-only rule whatever the default.
	ceiling := platformconfig.DefaultCloudExecutorSettings()
	ceiling.Enabled, ceiling.ProjectDefaultEnabled, ceiling.Shell = true, true, false
	if got := Narrow(ceiling, ProjectSetting{Shell: boolPtr(true)}, true, true); got.Shell {
		t.Fatalf("a project turned the shell on under a platform that has it off")
	}
}

type ceilingReader platformconfig.CloudExecutorSettings

func (c ceilingReader) CloudExecutorSettings(context.Context) (platformconfig.CloudExecutorSettings, error) {
	return platformconfig.CloudExecutorSettings(c), nil
}

type noProjectSetting struct{}

func (noProjectSetting) CloudExecutorSetting(context.Context, int64) (ProjectSetting, bool, error) {
	return ProjectSetting{}, false, nil
}

type deviceSwitch bool

func (d deviceSwitch) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	var policy platformconfig.NativeClientPolicy
	policy.LocalWork.CloudHandoff = bool(d)
	return policy, nil
}

// no_executor outranks only the storage check, and only when the deployment
// says so: without the option Main claims nothing.
func TestNoExecutorIsReportedOnlyWhenKnown(t *testing.T) {
	on := platformconfig.DefaultCloudExecutorSettings()
	on.Enabled, on.ProjectDefaultEnabled = true, true
	off := on
	off.Enabled = false
	for name, tc := range map[string]struct {
		ceiling           platformconfig.CloudExecutorSettings
		device, encrypted bool
		options           []PolicyOption
		want              string
	}{
		"unknown, all on":         {on, true, true, nil, ""},
		"configured, all on":      {on, true, true, []PolicyOption{WithExecutorConfigured(true)}, ""},
		"none, all on":            {on, true, true, []PolicyOption{WithExecutorConfigured(false)}, DisabledNoExecutor},
		"none, unencrypted":       {on, true, false, []PolicyOption{WithExecutorConfigured(false)}, DisabledNoExecutor},
		"none, the platform off":  {off, true, true, []PolicyOption{WithExecutorConfigured(false)}, DisabledPlatformOff},
		"none, the device off":    {on, false, true, []PolicyOption{WithExecutorConfigured(false)}, DisabledDeviceOff},
		"configured, unencrypted": {on, true, false, []PolicyOption{WithExecutorConfigured(true)}, DisabledStorageEncryptionUnconfirmed},
	} {
		service, err := NewPolicyService(ceilingReader(tc.ceiling), noProjectSetting{}, deviceSwitch(tc.device), tc.encrypted,
			platformconfig.CloudExecutorResources{}, tc.options...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := service.Effective(context.Background(), 1)
		if err != nil || got.DisabledReason != tc.want || got.Enabled != (tc.want == "") {
			t.Fatalf("%s: enabled %v reason %q (%v), want %q", name, got.Enabled, got.DisabledReason, err, tc.want)
		}
	}
	err := disabled(Policy{DisabledReason: DisabledNoExecutor})
	var refusal *DisabledError
	if !errors.Is(err, ErrDisabled) || !errors.As(err, &refusal) || refusal.Reason != DisabledNoExecutor {
		t.Fatalf("disabled() = %v", err)
	}
}
