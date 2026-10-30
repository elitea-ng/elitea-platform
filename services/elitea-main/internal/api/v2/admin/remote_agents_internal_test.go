package admin

import (
	"strings"
	"testing"
)

func TestRemoteAgentsSectionIsGovernanceGated(t *testing.T) {
	section := remoteAgentsSection()
	if section["id"] != "remote_agents" || section["required_permission"] != "configuration.governance" {
		t.Fatalf("section=%v", section)
	}
	found := false
	for _, candidate := range configSections() {
		if candidate["id"] == "remote_agents" {
			found = true
		}
	}
	if !found {
		t.Fatal("remote_agents is not a configuration section")
	}
}

func TestValidateRemoteAgentsValues(t *testing.T) {
	many := make([]any, 33)
	for index := range many {
		many[index] = "https://issuer" + strings.Repeat("x", index) + ".example/agent-issuer"
	}
	for _, test := range []struct {
		name   string
		values map[string]any
		want   string
	}{
		{name: "empty", values: map[string]any{}},
		{name: "valid", values: map[string]any{"outbound_assertions": "off", "trusted_issuers": []any{
			"https://peer.example/agent-issuer",
			"https://idp.example https://idp.example/keys.json",
		}}},
		{name: "unknown mode", values: map[string]any{"outbound_assertions": "always"}, want: "must be on or off"},
		{name: "not an array", values: map[string]any{"trusted_issuers": "https://a.example"}, want: "must be an array"},
		{name: "not a string", values: map[string]any{"trusted_issuers": []any{7}}, want: `"trusted_issuers"[0] must be a string`},
		{name: "http issuer", values: map[string]any{"trusted_issuers": []any{"http://a.example"}}, want: "https URL"},
		{name: "three fields", values: map[string]any{"trusted_issuers": []any{"https://a.example https://a.example/k x"}}, want: "optionally followed"},
		{name: "duplicate", values: map[string]any{"trusted_issuers": []any{"https://a.example", "https://a.example"}}, want: "listed twice"},
		{name: "too many", values: map[string]any{"trusted_issuers": many}, want: "at most 32"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := validateRemoteAgentsValues(test.values)
			if test.want == "" && got != "" || test.want != "" && !strings.Contains(got, test.want) {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
