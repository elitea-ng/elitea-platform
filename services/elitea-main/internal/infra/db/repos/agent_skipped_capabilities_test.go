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
	want := `{"items":[{"kind":"toolkit","name":"wiki","type":"confluence","reason_code":"unsupported_toolkit_family","reason":"not here"}],"more":6}`
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

// The longest reason the worker sends (a SharePoint app-only credential) and
// the limits the worker applies must survive Main's sanitizer untouched.
func TestSanitizerKeepsWhatTheWorkerCanSend(t *testing.T) {
	reason := "a SharePoint credential without an OAuth discovery endpoint (app-only access with a client secret) is not supported by the Rust worker: its SharePoint tools act as the signed-in person with a delegated token, so set the credential's OAuth discovery endpoint. Microsoft retired app-only access to the OneNote Graph API (MC1011142), so the OneNote tools cannot run on an app-only credential at all"
	name := strings.Repeat("ж", 64) // 128 bytes, the worker's label bound
	entry := skippedCapability{Kind: "toolkit", Name: name, Type: name, ReasonCode: "unsupported_setting", Reason: reason}
	raw, _ := json.Marshal(map[string]any{"items": []skippedCapability{entry}})
	var got struct {
		Items []skippedCapability `json:"items"`
		More  int                 `json:"more"`
	}
	if err := json.Unmarshal(sanitizeSkippedCapabilities(raw), &got); err != nil || len(got.Items) != 1 || got.More != 0 || got.Items[0] != entry {
		t.Fatalf("worker entry altered: %+v err=%v", got, err)
	}
	long := entry
	long.Reason = strings.Repeat("x", 513)
	raw, _ = json.Marshal(map[string]any{"items": []skippedCapability{entry, long}})
	if err := json.Unmarshal(sanitizeSkippedCapabilities(raw), &got); err != nil || len(got.Items) != 1 || got.More != 1 {
		t.Fatalf("over-long reason not counted into more: %+v", got)
	}
}

// A resumed turn finalizes the SAME message group a pause already stamped. A
// finalize that carries no list must keep the stored one, so no query may strip
// the key; they only replace it when the new terminal carries one.
func TestFinalizeQueriesNeverStripTheStoredList(t *testing.T) {
	for _, query := range queryTextsThatFinalizeATurn() {
		if strings.Contains(query, "- 'skipped_capabilities'") {
			t.Fatalf("a finalize query deletes the stored list:\n%s", query)
		}
		if !strings.Contains(query, "'skipped_capabilities', ") {
			t.Fatalf("a finalize query does not write the list:\n%s", query)
		}
	}
}
