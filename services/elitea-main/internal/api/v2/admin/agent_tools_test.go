package admin

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type invalidatorStub struct{ dropped int }

func (s *invalidatorStub) Invalidate() { s.dropped++ }

// ADR-0050 decision 3: the agents section refuses a ceiling out of range and
// a tool key that is not "toolkit/tool".
func TestValidateAgentToolsValues(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]any{
		"defaults":  {},
		"ceiling":   {platformconfig.KeyAgentsMaxToolLimit: float64(25)},
		"allowlist": {platformconfig.KeyAgentsUnattendedToolsAllowlist: []any{"github/create_comment", "jira/*"}},
	} {
		if reason := validateAgentToolsValues(platformconfig.SectionAgents, values); reason != "" {
			t.Errorf("%s: refused: %s", name, reason)
		}
	}
	for name, values := range map[string]map[string]any{
		"fraction":  {platformconfig.KeyAgentsMaxToolLimit: 2.5},
		"negative":  {platformconfig.KeyAgentsMaxToolLimit: float64(-1)},
		"too high":  {platformconfig.KeyAgentsMaxToolLimit: float64(platformconfig.MaxAgentsMaxToolLimit + 1)},
		"not list":  {platformconfig.KeyAgentsUnattendedToolsAllowlist: "github/*"},
		"bad key":   {platformconfig.KeyAgentsUnattendedToolsAllowlist: []any{"github"}},
		"partial *": {platformconfig.KeyAgentsUnattendedToolsAllowlist: []any{"github/create_*"}},
	} {
		if validateAgentToolsValues(platformconfig.SectionAgents, values) == "" {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if validateAgentToolsValues("run_caps", map[string]any{platformconfig.KeyAgentsMaxToolLimit: "x"}) != "" {
		t.Error("another section is not checked here")
	}
}

func TestAgentToolsSaveDropsTheCache(t *testing.T) {
	t.Parallel()
	cache := &invalidatorStub{}
	h := &Handler{}
	WithAgentToolsCache(cache)(h)
	h.invalidateAgentTools("run_caps")
	h.invalidateAgentTools(platformconfig.SectionAgents)
	if cache.dropped != 1 {
		t.Fatalf("dropped = %d, want 1", cache.dropped)
	}
}
