package platformconfig

import (
	"reflect"
	"testing"
)

func TestAgentToolSettingsFromValues(t *testing.T) {
	if got := AgentToolSettingsFromValues(Values{}); !got.Empty() {
		t.Fatalf("no row = %+v, want the defaults", got)
	}
	got := AgentToolSettingsFromValues(Values{
		KeyAgentsMaxToolLimit:             float64(25),
		KeyAgentsUnattendedToolsAllowlist: []any{"github/create_comment", "bad", "github/create_comment", float64(3), "jira/*"},
	})
	want := AgentToolSettings{MaxToolLimit: 25, UnattendedToolsAllowlist: []string{"github/create_comment", "jira/*"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("= %+v, want %+v", got, want)
	}
}

func TestAgentToolSettingsNeverLoosen(t *testing.T) {
	for raw, want := range map[any]uint32{"lots": 1, float64(-3): 1, float64(MaxAgentsMaxToolLimit + 9): MaxAgentsMaxToolLimit, 7.9: 7} {
		if got := AgentToolSettingsFromValues(Values{KeyAgentsMaxToolLimit: raw}).MaxToolLimit; got != want {
			t.Errorf("max_tool_limit %v = %d, want %d", raw, got, want)
		}
	}
}

// Issue 1824: the depth is 1 or 2; anything else is the default, never more.
func TestSubagentsMaxDepth(t *testing.T) {
	if got := AgentToolSettingsFromValues(Values{}).SubagentsDepth(); got != DefaultSubagentsMaxDepth {
		t.Fatalf("no row = %d, want %d", got, DefaultSubagentsMaxDepth)
	}
	for raw, want := range map[any]uint32{float64(1): 1, float64(2): 2, float64(3): 1, float64(0): 1, "2": 1, 1.5: 1} {
		settings := AgentToolSettingsFromValues(Values{KeyAgentsSubagentsMaxDepth: raw})
		if got := settings.SubagentsDepth(); got != want {
			t.Errorf("subagents_max_depth %v = %d, want %d", raw, got, want)
		}
	}
	if !AgentToolSettingsFromValues(Values{KeyAgentsSubagentsMaxDepth: float64(1)}).Empty() {
		t.Error("depth 1 is the default, so the section is empty")
	}
	if AgentToolSettingsFromValues(Values{KeyAgentsSubagentsMaxDepth: float64(2)}).Empty() {
		t.Error("depth 2 is not the default")
	}
	for raw, want := range map[any]bool{float64(1): true, float64(2): true, float64(0): false, float64(3): false, "1": false} {
		if got := ValidSubagentsMaxDepth(raw); got != want {
			t.Errorf("ValidSubagentsMaxDepth(%v) = %v", raw, got)
		}
	}
}

func TestValidAgentsMaxToolLimit(t *testing.T) {
	for raw, want := range map[any]bool{float64(0): true, float64(10): true, 1.5: false, float64(-1): false, "3": false, float64(MaxAgentsMaxToolLimit + 1): false} {
		if got := ValidAgentsMaxToolLimit(raw); got != want {
			t.Errorf("ValidAgentsMaxToolLimit(%v) = %v", raw, got)
		}
	}
}
