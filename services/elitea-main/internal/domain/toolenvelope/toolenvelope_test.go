package toolenvelope

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseAbsentIsEmpty(t *testing.T) {
	envelope, err := Parse(nil)
	if err != nil || !envelope.Empty() {
		t.Fatalf("Parse(nil) = %+v, %v", envelope, err)
	}
	if err := Check(nil); err != nil {
		t.Fatalf("Check(nil) = %v", err)
	}
}

func TestParseReadsEveryControl(t *testing.T) {
	envelope, err := Parse(map[string]any{
		"tool_limits":      map[string]any{"jira/*": float64(10), "github/create_issue": float64(3)},
		"unattended_tools": []any{"github/get_issue", "github/get_issue", "github/create_comment"},
		"subagent_tools":   []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Envelope{
		ToolLimits:      []Limit{{Tool: "github/create_issue", MaxCalls: 3}, {Tool: "jira/*", MaxCalls: 10}},
		UnattendedTools: []string{"github/create_comment", "github/get_issue"},
		SubagentTools:   []string{},
	}
	if !reflect.DeepEqual(envelope, want) {
		t.Fatalf("Parse = %+v, want %+v", envelope, want)
	}
	if envelope.Empty() {
		t.Fatal("an envelope with an empty subagent list is set: it allows no tool")
	}
}

func TestParseAcceptsJSONNumbers(t *testing.T) {
	decoder := json.NewDecoder(strings.NewReader(`{"tool_limits": {"github/create_issue": 2}}`))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	envelope, err := Parse(raw)
	if err != nil || len(envelope.ToolLimits) != 1 || envelope.ToolLimits[0].MaxCalls != 2 {
		t.Fatalf("Parse = %+v, %v", envelope, err)
	}
}

func TestParseRefusesBadBlocks(t *testing.T) {
	for name, raw := range map[string]any{
		"not an object":    "all",
		"unknown key":      map[string]any{"max_calls": float64(3)},
		"limits list":      map[string]any{"tool_limits": []any{"github/x"}},
		"zero limit":       map[string]any{"tool_limits": map[string]any{"github/x": float64(0)}},
		"fraction":         map[string]any{"tool_limits": map[string]any{"github/x": 1.5}},
		"over the max":     map[string]any{"tool_limits": map[string]any{"github/x": float64(MaxCalls + 1)}},
		"string limit":     map[string]any{"tool_limits": map[string]any{"github/x": "3"}},
		"key without tool": map[string]any{"tool_limits": map[string]any{"github": float64(3)}},
		"empty toolkit":    map[string]any{"unattended_tools": []any{"/get_issue"}},
		"two slashes":      map[string]any{"unattended_tools": []any{"a/b/c"}},
		"partial wildcard": map[string]any{"subagent_tools": []any{"github/get_*"}},
		"not a list":       map[string]any{"subagent_tools": "github/*"},
		"non-string entry": map[string]any{"subagent_tools": []any{float64(3)}},
		"control char":     map[string]any{"subagent_tools": []any{"github/get\nissue"}},
	} {
		if _, err := Parse(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Parse = %v, want ErrInvalid", name, err)
		}
	}
	if err := Check(map[string]any{MetaKey: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Check = %v", err)
	}
}

func TestParseBoundsEntries(t *testing.T) {
	items := make([]any, MaxEntries+1)
	for i := range items {
		items[i] = "toolkit/tool" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
	}
	if _, err := Parse(map[string]any{"unattended_tools": items}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Parse = %v, want the entry bound", err)
	}
}
