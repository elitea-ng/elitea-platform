package executions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The shared events table of ADR-0061 decision 4b: the desktop host reads its
// desktop rows, Main its cloud rows, so the two producers cannot drift.
type activityFixture struct {
	ToolNamePattern string `json:"tool_name_pattern"`
	Cloud           []struct {
		Name     string          `json:"name"`
		Frame    json.RawMessage `json:"frame"`
		State    string          `json:"state"`
		Activity map[string]any  `json:"activity"`
	} `json:"cloud"`
}

func readActivityFixture(t *testing.T) activityFixture {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "..",
		"libs", "proto", "contracts", "agent-state-activity-v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var fixture activityFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(fixture.Cloud) == 0 {
		t.Fatal("the fixture has no cloud rows")
	}
	return fixture
}

// stateFrameOf stamps one frame on a fresh stream and answers its state frame.
func stateFrameOf(t *testing.T, frame json.RawMessage) map[string]any {
	t.Helper()
	var states participantStates
	_, state := states.stamp(agentEvent(1, string(frame)))
	if state == nil {
		return nil
	}
	if state.Cursor != 0 {
		t.Fatalf("a state frame carries a cursor: %d", state.Cursor)
	}
	return decode(t, state.Data)
}

func TestAgentStateActivityFollowsTheSharedCloudRows(t *testing.T) {
	for _, row := range readActivityFixture(t).Cloud {
		t.Run(row.Name, func(t *testing.T) {
			body := stateFrameOf(t, row.Frame)
			if body == nil {
				t.Fatalf("no state frame for %s", row.Frame)
			}
			if body["state"] != row.State {
				t.Fatalf("state = %v, want %s", body["state"], row.State)
			}
			got, present := body["activity"]
			if row.Activity == nil {
				if present {
					t.Fatalf("activity = %v, want none", got)
				}
				return
			}
			if !reflect.DeepEqual(got, row.Activity) {
				t.Fatalf("activity = %v, want %v", got, row.Activity)
			}
		})
	}
}

func TestAgentStateToolNameIsSanitisedAndCarriesNoArguments(t *testing.T) {
	pattern := regexp.MustCompile(readActivityFixture(t).ToolNamePattern)
	long := "run tests/" + strings.Repeat("x", 100)
	frame, err := json.Marshal(map[string]any{
		"type": "agent_tool_start",
		"response_metadata": map[string]any{
			"tool_name":   long,
			"tool_run_id": "r1",
			"tool_inputs": map[string]any{"command": "cargo test --token=sk-secret-123"},
			"tool_meta":   map[string]any{"name": "shell", "args": "rm -rf /tmp/secret"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var states participantStates
	_, state := states.stamp(agentEvent(1, string(frame)))
	if state == nil {
		t.Fatal("no state frame")
	}
	activity, _ := decode(t, state.Data)["activity"].(map[string]any)
	tool, _ := activity["tool"].(string)
	if !pattern.MatchString(tool) || len(tool) != maxActivityTool || !strings.HasPrefix(tool, "run_tests_x") {
		t.Fatalf("tool = %q", tool)
	}
	for _, secret := range []string{"sk-secret", "cargo", "rm -rf", "shell", "/tmp"} {
		if strings.Contains(string(state.Data), secret) {
			t.Fatalf("the state frame carries %q: %s", secret, state.Data)
		}
	}
	if sanitizeToolName(" /// ") != "" {
		t.Fatal("a name of separators is not empty")
	}
	if got := toolActivity("get_issue", "jira"); got.Tool != "get_issue" {
		t.Fatalf("tool activity = %+v", got)
	}
	if got := toolActivity("add_comments", "jira"); got.ToolKind != "external_send" {
		t.Fatalf("a declared tool has no kind: %+v", got)
	}
	if got := toolActivity("made_up", "jira"); got.ToolKind != "" {
		t.Fatalf("an undeclared tool has a kind: %+v", got)
	}
}

func TestAgentStateActivityChangeWritesAFrame(t *testing.T) {
	var states participantStates
	tool := func(name string) string {
		return `{"type":"agent_tool_start","response_metadata":{"tool_name":"` + name + `","tool_run_id":"r"}}`
	}
	_, first := states.stamp(agentEvent(1, tool("jira_get_issue")))
	_, second := states.stamp(agentEvent(2, tool("jira_search")))
	_, same := states.stamp(agentEvent(3, tool("jira_search")))
	if first == nil || second == nil {
		t.Fatalf("two tools wrote %v and %v", first, second)
	}
	if same != nil {
		t.Fatalf("the same tool wrote a second frame: %s", same.Data)
	}
	activity, _ := decode(t, second.Data)["activity"].(map[string]any)
	if activity["tool"] != "jira_search" {
		t.Fatalf("second frame = %s", second.Data)
	}
	// Back to thinking: the activity is gone from the frame.
	_, ended := states.stamp(agentEvent(4, `{"type":"agent_tool_end","response_metadata":{"tool_name":"jira_search","tool_run_id":"r"}}`))
	if ended == nil {
		t.Fatal("no frame after the tool ended")
	}
	if _, present := decode(t, ended.Data)["activity"]; present {
		t.Fatalf("a thinking frame carries an activity: %s", ended.Data)
	}
}
