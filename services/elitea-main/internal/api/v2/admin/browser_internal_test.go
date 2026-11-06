package admin

import (
	"strings"
	"testing"
)

func TestBrowserSectionIsGovernanceGated(t *testing.T) {
	section := browserSection()
	if section["id"] != "browser" || section["required_permission"] != "configuration.governance" {
		t.Fatalf("section=%v", section)
	}
	found := false
	for _, candidate := range configSections() {
		if candidate["id"] == "browser" {
			found = true
		}
	}
	if !found {
		t.Fatal("browser is not a configuration section")
	}
	// Open egress must be chosen: the section advertises the allowlist.
	fields, _ := section["fields"].([]map[string]any)
	if len(fields) == 0 || fields[0]["key"] != "browser_egress" || fields[0]["default"] != "allowlist" {
		t.Fatalf("browser_egress default = %v, want allowlist", fields)
	}
}

func TestValidateBrowserValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]any
		want   string
	}{
		{name: "empty", values: map[string]any{}},
		{name: "valid", values: map[string]any{"browser_egress": "allowlist", "max_run_seconds": float64(600),
			"monthly_quota_seconds": float64(36000)}},
		{name: "zero is no cap", values: map[string]any{"max_run_seconds": float64(0), "monthly_quota_seconds": float64(0)}},
		{name: "upper bounds", values: map[string]any{"max_run_seconds": float64(86400),
			"monthly_quota_seconds": float64(100_000_000)}},
		{name: "unknown mode", values: map[string]any{"browser_egress": "closed"}, want: `"browser_egress" must be open or allowlist`},
		{name: "mode not a string", values: map[string]any{"browser_egress": true}, want: `"browser_egress"`},
		{name: "run too long", values: map[string]any{"max_run_seconds": float64(86401)}, want: `"max_run_seconds"`},
		{name: "run negative", values: map[string]any{"max_run_seconds": float64(-1)}, want: `"max_run_seconds"`},
		{name: "run fraction", values: map[string]any{"max_run_seconds": 1.5}, want: `"max_run_seconds"`},
		{name: "run not a number", values: map[string]any{"max_run_seconds": "60"}, want: `"max_run_seconds"`},
		{name: "quota too large", values: map[string]any{"monthly_quota_seconds": float64(100_000_001)},
			want: `"monthly_quota_seconds"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := validateBrowserValues(test.values)
			if test.want == "" && got != "" || test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
