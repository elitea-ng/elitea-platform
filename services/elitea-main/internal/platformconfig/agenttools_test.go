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

func TestValidAgentsMaxToolLimit(t *testing.T) {
	for raw, want := range map[any]bool{float64(0): true, float64(10): true, 1.5: false, float64(-1): false, "3": false, float64(MaxAgentsMaxToolLimit + 1): false} {
		if got := ValidAgentsMaxToolLimit(raw); got != want {
			t.Errorf("ValidAgentsMaxToolLimit(%v) = %v", raw, got)
		}
	}
}
