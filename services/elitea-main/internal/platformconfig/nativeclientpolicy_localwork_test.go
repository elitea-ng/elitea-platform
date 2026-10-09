package platformconfig

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestLocalWorkPolicyDefaults pins ADR-0029 decision 6: local work is off on a
// deployment that never saved the group.
func TestLocalWorkPolicyDefaults(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{}).LocalWork
	if got.Allowed {
		t.Fatal("local_work.allowed must default to false")
	}
	if got.MaxSandboxMode != SandboxModeWorkspaceWrite || got.Network || got.LocalMCP || got.CloudSync {
		t.Errorf("conservative defaults expected, got %+v", got)
	}
	if got.CommandAllow == nil || got.CommandDeny == nil || got.PathDeny == nil {
		t.Errorf("pattern lists must be empty arrays, never null: %+v", got)
	}
}

func TestLocalWorkPolicyOverlay(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"local_work_allowed": true, "local_work_shell": false,
		"local_work_max_sandbox_mode": "read-only", "local_work_network": true,
		"local_work_command_allow": ["cargo test", "npm test"],
		"local_work_command_deny": ["rm -rf *"],
		"local_work_path_deny": ["**/.env", "~/.ssh/**"],
		"local_work_local_mcp": true, "local_work_local_index": false,
		"local_work_cloud_sync": true, "local_work_memory_write": false}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values).LocalWork
	want := LocalWorkPolicy{
		Allowed: true, Shell: false, MaxSandboxMode: SandboxModeReadOnly, Network: true,
		CommandAllow: []string{"cargo test", "npm test"}, CommandDeny: []string{"rm -rf *"},
		PathDeny: []string{"**/.env", "~/.ssh/**"}, LocalMCP: true, LocalIndex: false,
		CloudSync: true, MemoryWrite: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay = %+v\nwant %+v", got, want)
	}
}

// TestLocalWorkPolicyIgnoresJunk: a hand-written row of the wrong type keeps
// the default; an invalid pattern is dropped, the valid ones are kept; a list
// longer than the bound is cut at the bound.
func TestLocalWorkPolicyIgnoresJunk(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"local_work_allowed": "yes", "local_work_max_sandbox_mode": "root",
		"local_work_command_deny": ["ok", "", "  ", "bad\nline", 3],
		"local_work_path_deny": "not-a-list"}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values).LocalWork
	if got.Allowed || got.MaxSandboxMode != SandboxModeWorkspaceWrite {
		t.Errorf("junk must keep defaults, got %+v", got)
	}
	if !reflect.DeepEqual(got.CommandDeny, []string{"ok"}) {
		t.Errorf("command_deny = %q, want only the valid pattern", got.CommandDeny)
	}
	if got.PathDeny == nil || len(got.PathDeny) != 0 {
		t.Errorf("path_deny = %#v, want an empty array", got.PathDeny)
	}

	many := make([]any, MaxLocalWorkPatterns+5)
	for i := range many {
		many[i] = "pattern"
	}
	got = NativeClientPolicyFromValues(Values{KeyNativeLocalWorkCommandAllow: many}).LocalWork
	if len(got.CommandAllow) != MaxLocalWorkPatterns {
		t.Errorf("command_allow kept %d patterns, want %d", len(got.CommandAllow), MaxLocalWorkPatterns)
	}
}

func TestValidLocalWorkPattern(t *testing.T) {
	for pattern, want := range map[string]bool{
		"cargo test":               true,
		"**/.env":                  true,
		"":                         false,
		"   ":                      false,
		"a\tb":                     false,
		"nul\x00":                  false,
		string([]byte{0xff, 0xfe}): false,
		strings.Repeat("x", MaxLocalWorkPatternBytes):   true,
		strings.Repeat("x", MaxLocalWorkPatternBytes+1): false,
	} {
		if got := ValidLocalWorkPattern(pattern); got != want {
			t.Errorf("ValidLocalWorkPattern(%q) = %v, want %v", pattern, got, want)
		}
	}
	for _, mode := range SandboxModeValues() {
		if !ValidSandboxMode(mode) {
			t.Errorf("ValidSandboxMode(%q) = false", mode)
		}
	}
	if ValidSandboxMode("Read-Only") || ValidSandboxMode("") {
		t.Error("sandbox mode must compare exactly")
	}
}
