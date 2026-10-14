package repos

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeSkippedCapabilities(t *testing.T) {
	if got := sanitizeSkippedCapabilities(nil); got != nil {
		t.Fatalf("absent list kept: %s", got)
	}
	if got := sanitizeSkippedCapabilities(json.RawMessage(`[]`)); got != nil {
		t.Fatalf("a bare array kept: %s", got)
	}
	raw := json.RawMessage(`{"more":3,"items":[
		{"kind":"toolkit","name":"wiki","type":"confluence","reason_code":"unsupported_toolkit_family","reason":"not here","extra":"dropped"},
		{"kind":"mystery","name":"x","reason_code":"c","reason":"r"},
		{"kind":"internal_tool","name":"","reason_code":"c","reason":"r"},
		{"kind":"internal_tool","name":"swarm","reason_code":"c","reason":"line\nbreak"}
	]}`)
	got := string(sanitizeSkippedCapabilities(raw))
	want := `{"items":[{"kind":"toolkit","name":"wiki","type":"confluence","reason_code":"unsupported_toolkit_family","reason":"not here"}],"more":3}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestSanitizeSkippedCapabilitiesBoundsTheList(t *testing.T) {
	entry := `{"kind":"application","name":"n","reason_code":"c","reason":"r"}`
	raw := json.RawMessage(`{"items":[` + strings.Repeat(entry+",", 40) + entry + `]}`)
	var kept struct {
		Items []skippedCapability `json:"items"`
	}
	if err := json.Unmarshal(sanitizeSkippedCapabilities(raw), &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept.Items) != maxSkippedCapabilities {
		t.Fatalf("kept %d", len(kept.Items))
	}
}

// Every terminal that ends a turn carries the list: the final message and both
// pauses (the pause event is what a paused reply is persisted from).
func TestEveryTerminalEventCarriesSkippedCapabilities(t *testing.T) {
	list := `"skipped_capabilities":{"items":[{"kind":"toolkit","name":"wiki","reason_code":"c","reason":"r"}]}`
	for name, meta := range map[string]string{
		"full message":  `{"thread_id":"t",` + list + `}`,
		"hitl pause":    `{"thread_id":"t","hitl_interrupt":{},` + list + `}`,
		"authorization": `{"authorization_requests":[],` + list + `}`,
	} {
		got := skippedCapabilitiesFromMetadata(json.RawMessage(meta))
		if !strings.Contains(string(got), `"wiki"`) {
			t.Fatalf("%s: not carried: %s", name, got)
		}
	}
	if skippedCapabilitiesFromMetadata(json.RawMessage(`{"thread_id":"t"}`)) != nil {
		t.Fatal("absent list invented")
	}
}
