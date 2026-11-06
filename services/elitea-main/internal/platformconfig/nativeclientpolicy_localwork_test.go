package platformconfig

import (
	"encoding/json"
	"fmt"
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
		"local_work_cloud_sync": true, "local_work_memory_write": false,
		"local_work_cloud_handoff": true, "local_work_index_local_embeddings": false,
		"local_work_index_remote_embeddings": true,
		"local_work_index_model_mirror_url": "https://mirror.example.com/hf"}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values).LocalWork
	want := LocalWorkPolicy{
		Allowed: true, Shell: false, MaxSandboxMode: SandboxModeReadOnly, Network: true,
		CommandAllow: []string{"cargo test", "npm test"}, CommandDeny: []string{"rm -rf *"},
		PathDeny: []string{"**/.env", "~/.ssh/**"}, LocalMCP: true, LocalIndex: false,
		CloudSync: true, MemoryWrite: false, CloudHandoff: true,
		IndexLocalEmbeddings: false, IndexRemoteEmbeddings: true,
		IndexModelMirrorURL:  "https://mirror.example.com/hf",
		ComputerUseAppsAllow: []string{}, ComputerUseAppsDeny: []string{},
		Browser: true, BrowserAgent: true, BrowserAllowDomains: []string{}, BrowserDenyDomains: []string{},
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

// TestLocalWorkComputerUseDefaults pins ADR-0047 decision 3: computer use is
// off on a deployment that never saved it, and both app lists are empty
// arrays, never null.
func TestLocalWorkComputerUseDefaults(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{}).LocalWork
	if got.ComputerUse {
		t.Fatal("local_work.computer_use must default to false")
	}
	if got.ComputerUseAppsAllow == nil || got.ComputerUseAppsDeny == nil ||
		len(got.ComputerUseAppsAllow) != 0 || len(got.ComputerUseAppsDeny) != 0 {
		t.Errorf("app lists must be empty arrays: %#v %#v", got.ComputerUseAppsAllow, got.ComputerUseAppsDeny)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"computer_use":false,"computer_use_apps_allow":[],"computer_use_apps_deny":[]`) {
		t.Errorf("local_work JSON = %s", body)
	}
}

// TestLocalWorkComputerUseOverlay: the switch and both lists are read; an
// invalid entry is dropped and the valid ones kept; a wrong type keeps the
// default; a list is cut at the bound.
func TestLocalWorkComputerUseOverlay(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"local_work_computer_use": true,
		"local_work_computer_use_apps_allow": ["com.apple.mail", "com.example.*", "not an id", 4],
		"local_work_computer_use_apps_deny": ["com.apple.Notes", "*"]}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values).LocalWork
	if !got.ComputerUse {
		t.Error("computer_use = false, want true")
	}
	if !reflect.DeepEqual(got.ComputerUseAppsAllow, []string{"com.apple.mail", "com.example.*"}) {
		t.Errorf("computer_use_apps_allow = %q", got.ComputerUseAppsAllow)
	}
	if !reflect.DeepEqual(got.ComputerUseAppsDeny, []string{"com.apple.Notes"}) {
		t.Errorf("computer_use_apps_deny = %q", got.ComputerUseAppsDeny)
	}

	junk := NativeClientPolicyFromValues(Values{
		KeyNativeLocalWorkComputerUse:          "yes",
		KeyNativeLocalWorkComputerUseAppsAllow: "com.apple.mail",
	}).LocalWork
	if junk.ComputerUse || len(junk.ComputerUseAppsAllow) != 0 {
		t.Errorf("junk must keep defaults, got %+v", junk)
	}

	many := make([]any, MaxBundleIDPatterns+5)
	for i := range many {
		many[i] = "com.example.app"
	}
	cut := NativeClientPolicyFromValues(Values{KeyNativeLocalWorkComputerUseAppsDeny: many}).LocalWork
	if len(cut.ComputerUseAppsDeny) != MaxBundleIDPatterns {
		t.Errorf("computer_use_apps_deny kept %d entries, want %d", len(cut.ComputerUseAppsDeny), MaxBundleIDPatterns)
	}
}

// TestLocalWorkComputerUseAllowListFailsClosed: a stored allow list whose
// every entry is invalid must not load as "no ceiling" (every application).
// It loads as one id no application has. An empty stored list is no ceiling,
// as saved.
func TestLocalWorkComputerUseAllowListFailsClosed(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{
		KeyNativeLocalWorkComputerUseAppsAllow: []any{"bad id", "*"},
	}).LocalWork
	if !reflect.DeepEqual(got.ComputerUseAppsAllow, []string{NoAppBundleID}) {
		t.Fatalf("computer_use_apps_allow = %q, want [%s]", got.ComputerUseAppsAllow, NoAppBundleID)
	}
	if !ValidBundleIDPattern(NoAppBundleID) {
		t.Fatal("NoAppBundleID must itself be a valid bundle id")
	}
	empty := NativeClientPolicyFromValues(Values{KeyNativeLocalWorkComputerUseAppsAllow: []any{}}).LocalWork
	if len(empty.ComputerUseAppsAllow) != 0 {
		t.Errorf("an empty allow list stays empty, got %q", empty.ComputerUseAppsAllow)
	}
	// The deny list needs no sentinel: dropping an entry only drops it, and
	// the desktop's platform list still applies.
	deny := NativeClientPolicyFromValues(Values{
		KeyNativeLocalWorkComputerUseAppsDeny: []any{"bad id"},
	}).LocalWork
	if len(deny.ComputerUseAppsDeny) != 0 {
		t.Errorf("computer_use_apps_deny = %q, want empty", deny.ComputerUseAppsDeny)
	}
}

func TestValidBundleIDPattern(t *testing.T) {
	for pattern, want := range map[string]bool{
		"com.apple.mail":                        true,
		"ai.elitea.desktop":                     true,
		"com.1password.*":                       true,
		"in.sinew.Enpass-Desktop":               true,
		"a":                                     true,
		"":                                      false,
		"*":                                     false,
		".*":                                    false,
		"com..apple":                            false,
		".com":                                  false,
		"com.":                                  false,
		"com.*.mail":                            false,
		"com apple":                             false,
		"com/apple":                             false,
		"com.apple.mail\n":                      false,
		"com.é":                                 false,
		strings.Repeat("a", MaxBundleIDBytes):   true,
		strings.Repeat("a", MaxBundleIDBytes+1): false,
	} {
		if got := ValidBundleIDPattern(pattern); got != want {
			t.Errorf("ValidBundleIDPattern(%q) = %v, want %v", pattern, got, want)
		}
	}
}

// TestLocalWorkBrowserDefaults pins ADR-0048 decision 8 and owner decision 1
// at acceptance: the pane and the agent's toolset are on, plain http and
// private addresses are off, and both domain lists are empty arrays.
func TestLocalWorkBrowserDefaults(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{}).LocalWork
	if !got.Browser || !got.BrowserAgent || got.BrowserHTTP || got.BrowserPrivateNetwork {
		t.Errorf("browser defaults = %+v", got)
	}
	if got.BrowserAllowDomains == nil || got.BrowserDenyDomains == nil ||
		len(got.BrowserAllowDomains) != 0 || len(got.BrowserDenyDomains) != 0 {
		t.Errorf("domain lists must be empty arrays: %#v %#v", got.BrowserAllowDomains, got.BrowserDenyDomains)
	}
}

// TestLocalWorkBrowserOverlay: every key is read; entries are lowercased and
// deduplicated, invalid ones dropped; a wrong type keeps the default; a list
// is cut at the bound.
func TestLocalWorkBrowserOverlay(t *testing.T) {
	var values Values
	if err := json.Unmarshal([]byte(`{
		"local_work_browser": false, "local_work_browser_agent": false,
		"local_work_browser_http": true, "local_work_browser_private_network": true,
		"local_work_browser_allow_domains": ["Example.com", "*.docs.example.org", "example.com",
			"https://x.example", "10.0.0.1", "localhost", "bad host", 7],
		"local_work_browser_deny_domains": ["ads.example.com", "*"]}`), &values); err != nil {
		t.Fatal(err)
	}
	got := NativeClientPolicyFromValues(values).LocalWork
	if got.Browser || got.BrowserAgent || !got.BrowserHTTP || !got.BrowserPrivateNetwork {
		t.Errorf("switches = %+v", got)
	}
	if !reflect.DeepEqual(got.BrowserAllowDomains, []string{"example.com", "*.docs.example.org"}) {
		t.Errorf("browser_allow_domains = %q", got.BrowserAllowDomains)
	}
	if !reflect.DeepEqual(got.BrowserDenyDomains, []string{"ads.example.com"}) {
		t.Errorf("browser_deny_domains = %q", got.BrowserDenyDomains)
	}
	junk := NativeClientPolicyFromValues(Values{
		KeyNativeLocalWorkBrowser:             "no",
		KeyNativeLocalWorkBrowserAllowDomains: "example.com",
	}).LocalWork
	if !junk.Browser || len(junk.BrowserAllowDomains) != 0 {
		t.Errorf("junk must keep defaults, got %+v", junk)
	}
	many := make([]any, MaxBrowserDomains+5)
	for i := range many {
		many[i] = fmt.Sprintf("host%d.example.com", i)
	}
	cut := NativeClientPolicyFromValues(Values{KeyNativeLocalWorkBrowserDenyDomains: many}).LocalWork
	if len(cut.BrowserDenyDomains) != MaxBrowserDomains {
		t.Errorf("browser_deny_domains kept %d entries, want %d", len(cut.BrowserDenyDomains), MaxBrowserDomains)
	}
}

// TestLocalWorkBrowserAllowListFailsClosed: a stored allow list whose every
// entry is invalid loads as one host nobody has, never as "no ceiling".
func TestLocalWorkBrowserAllowListFailsClosed(t *testing.T) {
	got := NativeClientPolicyFromValues(Values{
		KeyNativeLocalWorkBrowserAllowDomains: []any{"bad host", "127.0.0.1"},
	}).LocalWork
	if !reflect.DeepEqual(got.BrowserAllowDomains, []string{NoBrowserHost}) {
		t.Fatalf("browser_allow_domains = %q, want [%s]", got.BrowserAllowDomains, NoBrowserHost)
	}
	if !ValidBrowserDomain(NoBrowserHost) {
		t.Fatal("NoBrowserHost must itself be a valid domain")
	}
	deny := NativeClientPolicyFromValues(Values{KeyNativeLocalWorkBrowserDenyDomains: []any{"bad host"}}).LocalWork
	if len(deny.BrowserDenyDomains) != 0 {
		t.Errorf("browser_deny_domains = %q, want empty", deny.BrowserDenyDomains)
	}
}

func TestValidBrowserDomain(t *testing.T) {
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." +
		strings.Repeat("d", 60) + "." + strings.Repeat("e", 60) + ".com"
	for entry, want := range map[string]bool{
		"example.com":         true,
		"*.example.com":       true,
		"a-b.example.co.uk":   true,
		"example":             false,
		"localhost":           false,
		"*":                   false,
		"*.com.":              false,
		"Example.com":         false,
		"example.com:443":     false,
		"https://example.com": false,
		"10.0.0.1":            false,
		"1.2.3.4.5":           false,
		"exa mple.com":        false,
		"-x.example.com":      false,
		long:                  false,
	} {
		if got := ValidBrowserDomain(entry); got != want {
			t.Errorf("ValidBrowserDomain(%q) = %v, want %v", entry, got, want)
		}
	}
}
