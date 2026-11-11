package repos

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

func laneEntry(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestMergeSubAgentLanesFoldsTheChildLifecycle: the worker's entries fold
// into one lane per child, with its tool rows, text and end, as a reload
// shows it.
func TestMergeSubAgentLanesFoldsTheChildLifecycle(t *testing.T) {
	var lanes []subAgentLane
	var changed bool
	for _, entry := range []map[string]any{
		{"kind": "started", "agent_id": "a_1", "parent_call_id": "call-spawn", "ordinal": 1, "label": "callers", "display_name": "Owl", "avatar": "default:owl"},
		{"kind": "tool_start", "agent_id": "a_1", "call_id": "c1", "tool": "read_file", "args_summary": "src/main.rs"},
		{"kind": "tool_end", "agent_id": "a_1", "call_id": "c1", "ok": true, "summary": "fn main() {}"},
		{"kind": "text", "agent_id": "a_1", "text": "Two callers."},
		{"kind": "finished", "agent_id": "a_1", "status": "completed", "steps": 2, "input_tokens": 100, "output_tokens": 20, "result": "Two callers."},
	} {
		lanes, changed = mergeSubAgentLanes(lanes, laneEntry(t, entry))
		if !changed {
			t.Fatalf("entry %v did not apply", entry)
		}
	}
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d", len(lanes))
	}
	lane := lanes[0]
	if lane.DisplayName != "Owl" || lane.Avatar != "default:owl" || lane.ParentCallID != "call-spawn" || lane.Status != "completed" {
		t.Fatalf("lane = %+v", lane)
	}
	if len(lane.Items) != 2 || lane.Items[0].OK == nil || !*lane.Items[0].OK || lane.Items[1].Text != "Two callers." {
		t.Fatalf("items = %+v", lane.Items)
	}
	if lane.Steps == nil || *lane.Steps != 2 || lane.Result != "Two callers." {
		t.Fatalf("settled = %+v", lane)
	}
	// What a reload decodes is what was stored.
	encoded, _ := json.Marshal(lanes)
	if again := decodeSubAgentLanes(encoded); len(again) != 1 || again[0].AgentID != "a_1" {
		t.Fatalf("decoded = %+v", again)
	}
}

// TestMergeSubAgentLanesIgnoresWhatDoesNotFit: an entry for an unknown
// child, a second start, an unknown status, a malformed entry and an
// over-long id change nothing; text is stripped of control characters and
// bounded.
func TestMergeSubAgentLanesIgnoresWhatDoesNotFit(t *testing.T) {
	lanes, _ := mergeSubAgentLanes(nil, laneEntry(t, map[string]any{"kind": "started", "agent_id": "a_1", "parent_call_id": "p", "ordinal": 1, "display_name": "Owl\x1b[31m"}))
	if lanes[0].DisplayName != "Owl[31m" {
		t.Fatalf("display name = %q", lanes[0].DisplayName)
	}
	for name, entry := range map[string]json.RawMessage{
		"unknown child":  laneEntry(t, map[string]any{"kind": "text", "agent_id": "a_2", "text": "x"}),
		"second start":   laneEntry(t, map[string]any{"kind": "started", "agent_id": "a_1", "parent_call_id": "p", "ordinal": 1}),
		"bad status":     laneEntry(t, map[string]any{"kind": "finished", "agent_id": "a_1", "status": "exploded"}),
		"unknown kind":   laneEntry(t, map[string]any{"kind": "spawned", "agent_id": "a_1"}),
		"end of no call": laneEntry(t, map[string]any{"kind": "tool_end", "agent_id": "a_1", "call_id": "c9", "ok": true}),
		"long id":        laneEntry(t, map[string]any{"kind": "started", "agent_id": strings.Repeat("a", 200), "parent_call_id": "p", "ordinal": 2}),
		"no ordinal":     laneEntry(t, map[string]any{"kind": "started", "agent_id": "a_3", "parent_call_id": "p"}),
		"not an object":  json.RawMessage(`"started"`),
	} {
		if _, changed := mergeSubAgentLanes(lanes, entry); changed {
			t.Errorf("%s applied", name)
		}
	}
	long := strings.Repeat("é", maxSubAgentTextRunes+50)
	next, _ := mergeSubAgentLanes(lanes, laneEntry(t, map[string]any{"kind": "text", "agent_id": "a_1", "text": long}))
	if got := []rune(next[0].Items[0].Text); len(got) != maxSubAgentTextRunes {
		t.Fatalf("text runes = %d", len(got))
	}
	if len(lanes[0].Items) != 0 {
		t.Fatal("a merge changed the lanes it was given")
	}
}

// TestMergeSubAgentLanesResumesAContinuedChild: `send_input` runs a settled
// child again; its result clears until it settles again.
func TestMergeSubAgentLanesResumesAContinuedChild(t *testing.T) {
	lanes, _ := mergeSubAgentLanes(nil, laneEntry(t, map[string]any{"kind": "started", "agent_id": "a_1", "parent_call_id": "p", "ordinal": 1}))
	lanes, _ = mergeSubAgentLanes(lanes, laneEntry(t, map[string]any{"kind": "finished", "agent_id": "a_1", "status": "completed", "result": "first"}))
	lanes, _ = mergeSubAgentLanes(lanes, laneEntry(t, map[string]any{"kind": "resumed", "agent_id": "a_1"}))
	if lanes[0].Status != "running" || lanes[0].Result != "" {
		t.Fatalf("resumed = %+v", lanes[0])
	}
}

// TestProjectSubAgentLaneLeavesOtherFramesAlone: a frame of another type, or
// an agent_subagent frame without its entry, is no business of the lane and
// never reaches the database (a nil executor would panic), so it never fails
// the turn.
func TestProjectSubAgentLaneLeavesOtherFramesAlone(t *testing.T) {
	for name, data := range map[string]string{
		"another type":        `{"type":"agent_llm_chunk","content":"x"}`,
		"names it in content": `{"type":"agent_tool_end","content":"\"agent_subagent\""}`,
		"no entry":            `{"type":"agent_subagent","response_metadata":{}}`,
	} {
		frame := outputapp.NodeEventFrame{BrowserData: json.RawMessage(data)}
		if err := projectSubAgentLane(context.Background(), nil, 1, frame); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestDecodeSubAgentLanesReadsGarbageAsNone(t *testing.T) {
	for _, raw := range []string{"", "{}", `"x"`, "[1]"} {
		if lanes := decodeSubAgentLanes([]byte(raw)); len(lanes) != 0 {
			t.Errorf("%q decoded %v", raw, lanes)
		}
	}
}

// TestDecodeAnswerSubAgentLanesNestsGrandchildren (#1872): a depth-2 lane
// whose parent_agent_id names a depth-1 lane of the same answer is folded
// under that lane's Children, in ordinal order, and dropped from the
// top-level list; a depth-2 lane whose named parent is not among the rows
// (never happens in practice — the worker always starts the parent first —
// but a malformed or partial write must not lose it) stays at the top level
// rather than vanishing.
func TestDecodeAnswerSubAgentLanesNestsGrandchildren(t *testing.T) {
	marshal := func(lane subAgentLane) []byte {
		raw, err := json.Marshal(lane)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rows := [][]byte{
		marshal(subAgentLane{AgentID: "a_1", Ordinal: 1, Depth: 1, Status: "completed"}),
		marshal(subAgentLane{AgentID: "a_3", Ordinal: 3, Depth: 2, ParentAgentID: "a_1", Status: "completed"}),
		marshal(subAgentLane{AgentID: "a_2", Ordinal: 2, Depth: 2, ParentAgentID: "a_1", Status: "running"}),
		// Depth 2, named parent absent among these rows: kept at the top
		// level rather than dropped.
		marshal(subAgentLane{AgentID: "a_orphan", Ordinal: 4, Depth: 2, ParentAgentID: "a_missing", Status: "running"}),
	}
	out := decodeAnswerSubAgentLanes(rows)
	if len(out) != 2 {
		t.Fatalf("top-level lanes = %+v", out)
	}
	if out[0].AgentID != "a_1" || len(out[0].Children) != 2 {
		t.Fatalf("a_1 = %+v", out[0])
	}
	if out[0].Children[0].AgentID != "a_2" || out[0].Children[1].AgentID != "a_3" {
		t.Fatalf("children order = %+v", out[0].Children)
	}
	if out[1].AgentID != "a_orphan" {
		t.Fatalf("orphaned depth-2 lane dropped: %+v", out)
	}
}

// TestDecodeAnswerSubAgentLanesReadsALegacyRow: a row written by the old
// whole-array projection (a value this read path never produces itself, but
// may still meet on a row carried over by a migration or a hand-written
// fixture) decodes as one lane, not none.
func TestDecodeAnswerSubAgentLanesReadsALegacyRow(t *testing.T) {
	out := decodeAnswerSubAgentLanes([][]byte{[]byte(`{"agent_id":"a_1","ordinal":1,"status":"completed","items":[]}`)})
	if len(out) != 1 || out[0].AgentID != "a_1" {
		t.Fatalf("out = %+v", out)
	}
}
