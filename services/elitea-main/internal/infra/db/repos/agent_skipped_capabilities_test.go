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
	if got := sanitizeSkippedCapabilities(json.RawMessage(`"nope"`)); got != nil {
		t.Fatalf("non-array kept: %s", got)
	}
	raw := json.RawMessage(`[
		{"kind":"toolkit","name":"wiki","type":"confluence","reason_code":"unsupported_toolkit_family","reason":"not here","extra":"dropped"},
		{"kind":"mystery","name":"x","reason_code":"c","reason":"r"},
		{"kind":"internal_tool","name":"","reason_code":"c","reason":"r"},
		{"kind":"internal_tool","name":"swarm","reason_code":"c","reason":"line\nbreak"}
	]`)
	got := string(sanitizeSkippedCapabilities(raw))
	want := `[{"kind":"toolkit","name":"wiki","type":"confluence","reason_code":"unsupported_toolkit_family","reason":"not here"}]`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestSanitizeSkippedCapabilitiesBoundsTheList(t *testing.T) {
	entry := `{"kind":"application","name":"n","reason_code":"c","reason":"r"}`
	raw := json.RawMessage("[" + strings.Repeat(entry+",", 40) + entry + "]")
	var kept []skippedCapability
	if err := json.Unmarshal(sanitizeSkippedCapabilities(raw), &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != maxSkippedCapabilities {
		t.Fatalf("kept %d", len(kept))
	}
}

func TestDecodeFullMessageCarriesSkippedCapabilities(t *testing.T) {
	meta := json.RawMessage(`{"thread_id":"t","skipped_capabilities":[{"kind":"toolkit","name":"wiki","reason_code":"unsupported_toolkit_family","reason":"r"}]}`)
	message, err := decodeCurrentAgentFullMessage(json.RawMessage(`"answer"`), json.RawMessage(`[]`), meta)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message.SkippedCapabilities), `"wiki"`) {
		t.Fatalf("not carried: %s", message.SkippedCapabilities)
	}
}
