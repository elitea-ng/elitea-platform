package admin

import (
	"strings"
	"testing"
)

func TestWorkerEgressSectionIsGovernanceGated(t *testing.T) {
	section := workerEgressSection()
	if section["id"] != "worker_egress" || section["required_permission"] != "configuration.governance" {
		t.Fatalf("section=%v", section)
	}
	found := false
	for _, candidate := range configSections() {
		if candidate["id"] == "worker_egress" {
			found = true
		}
	}
	if !found {
		t.Fatal("worker_egress is not a configuration section")
	}
}

func TestValidateWorkerEgressValues(t *testing.T) {
	many := make([]any, 513)
	for index := range many {
		many[index] = "example.com"
	}
	for _, test := range []struct {
		name   string
		values map[string]any
		want   string
	}{
		{name: "empty", values: map[string]any{}},
		{name: "valid", values: map[string]any{"mode": "allowlist", "allowlist": []any{"api.github.com", "*.atlassian.net", "example.com:8443", "10.0.0.0/8"}}},
		{name: "open", values: map[string]any{"mode": "open", "allowlist": []any{}}},
		{name: "unknown mode", values: map[string]any{"mode": "closed"}, want: "must be open or allowlist"},
		{name: "mode not a string", values: map[string]any{"mode": true}, want: "must be open or allowlist"},
		{name: "not an array", values: map[string]any{"allowlist": "example.com"}, want: "must be an array"},
		{name: "not a string", values: map[string]any{"allowlist": []any{"example.com", 7}}, want: `"allowlist"[1] must be a string`},
		{name: "a url", values: map[string]any{"allowlist": []any{"https://example.com/path"}}, want: `"allowlist":`},
		{name: "blank", values: map[string]any{"allowlist": []any{" "}}, want: `"allowlist":`},
		{name: "too long", values: map[string]any{"allowlist": []any{strings.Repeat("a", 262)}}, want: "too long"},
		{name: "too many", values: map[string]any{"allowlist": many}, want: "at most 512"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := validateWorkerEgressValues(test.values)
			if test.want == "" && got != "" || test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
