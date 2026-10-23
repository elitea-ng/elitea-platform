package guardrails_test

// Parity with the SDK, case by case.
//
// Every case below is transcribed from
// `elitea-sdk/tests/runtime/test_blocked_tools.py` — the suite that pins the
// behaviour of `elitea_sdk.runtime.toolkits.security`, which is the code that
// actually refuses a tool call inside the worker. This file exists so the two
// implementations are ASSERTED equal rather than assumed equal: the failure mode
// it guards against is a Go side that matches more loosely than the worker, which
// admits a tool into the catalogue and into a frozen agent input and only then
// has it refused somewhere the operator cannot see.
//
// When the SDK's suite gains a case, this one gains it too.

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

func TestCanonicalKeyCollapsesNamingStyles(t *testing.T) {
	// TestCanonicalMatching::test_blocked_tool_matches_naming_style_variants.
	for _, styled := range []string{"create_file", "CreateFile", "create-file", "Create File", "createfile", "Create-File"} {
		if got := guardrails.CanonicalKey(styled); got != "createfile" {
			t.Fatalf("CanonicalKey(%q) = %q, want %q", styled, got, "createfile")
		}
	}
	// Separator-only values are not keys.
	for _, empty := range []string{"---", "  ", "***", ""} {
		if got := guardrails.CanonicalKey(empty); got != "" {
			t.Fatalf("CanonicalKey(%q) = %q, want empty", empty, got)
		}
	}
}

func TestToolNameAliasesReduceRoutingPrefixes(t *testing.T) {
	// `get_tool_name_aliases` — the base name must always be the last alias.
	cases := map[string][]string{
		"list_branches_in_repo":             {"list_branches_in_repo"},
		"github___list_branches_in_repo":    {"github___list_branches_in_repo", "list_branches_in_repo"},
		"elitea_core:list_branches_in_repo": {"elitea_core:list_branches_in_repo", "list_branches_in_repo"},
		"":                                  nil,
	}
	for input, want := range cases {
		got := guardrails.ToolNameAliases(input)
		if len(got) != len(want) {
			t.Fatalf("ToolNameAliases(%q) = %v, want %v", input, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ToolNameAliases(%q) = %v, want %v", input, got, want)
			}
		}
	}
}

func TestBlockedToolsMatchTheSDK(t *testing.T) {
	// TestBlocklistConfiguration, in order.
	t.Run("configure_blocked_tools", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"github": {"create_issue", "delete_repo"}},
		})
		assertBlocked(t, policy, "github", "create_issue", true)
		assertBlocked(t, policy, "github", "delete_repo", true)
		assertBlocked(t, policy, "github", "get_issue", false)
	})

	t.Run("configure_blocked_toolkits", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"shell"}})
		if !policy.ToolkitBlocked("shell") {
			t.Fatal("shell must be blocked")
		}
		if policy.ToolkitBlocked("github") {
			t.Fatal("github must not be blocked")
		}
	})

	t.Run("blocked_toolkit_blocks_all_tools", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"shell"}})
		assertBlocked(t, policy, "shell", "execute_command", true)
		assertBlocked(t, policy, "shell", "any_other_tool", true)
	})

	t.Run("case_insensitive", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"GitHub": {"Create_Issue"}},
		})
		assertBlocked(t, policy, "github", "create_issue", true)
	})

	t.Run("tool_name_alias_normalization", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"github": {"create_issue"}},
		})
		assertBlocked(t, policy, "github", "github___create_issue", true)
		assertBlocked(t, policy, "github", "github:create_issue", true)
	})

	t.Run("empty_blocklist", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{})
		assertBlocked(t, policy, "github", "create_issue", false)
		if policy.ToolkitBlocked("github") {
			t.Fatal("nothing may be blocked by an empty policy")
		}
		if !policy.Empty() {
			t.Fatal("an empty policy must report Empty")
		}
	})

	// TestCanonicalMatching, in order.
	t.Run("blocked_tool_matches_naming_style_variants", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"GitHub": {"Create-File"}},
		})
		for _, invoked := range []string{"create_file", "CreateFile", "create-file", "Create File", "createfile"} {
			assertBlocked(t, policy, "github", invoked, true)
		}
	})

	t.Run("blocked_tool_matches_prefixed_and_styled", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"github": {"create_file"}},
		})
		assertBlocked(t, policy, "github", "github___CreateFile", true)
		assertBlocked(t, policy, "GITHUB", "github:create-file", true)
	})

	t.Run("blocked_toolkit_matches_naming_style_variants", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{BlockedToolkits: []string{"Data_Analysis"}})
		for _, invoked := range []string{"data_analysis", "data-analysis", "DataAnalysis", "Data Analysis"} {
			if !policy.ToolkitBlocked(invoked) {
				t.Fatalf("%q must be blocked", invoked)
			}
		}
	})

	t.Run("matching_is_toolkit_scoped", func(t *testing.T) {
		// The SDK's AC9: a common verb blocked under one toolkit must not reach
		// another toolkit that happens to name a tool the same way.
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedTools: map[string][]string{"github": {"create_file"}},
		})
		assertBlocked(t, policy, "github", "CreateFile", true)
		assertBlocked(t, policy, "artifacts", "CreateFile", false)
		assertBlocked(t, policy, "filesystem", "create_file", false)
	})

	t.Run("separator_only_entries_are_dropped", func(t *testing.T) {
		policy := guardrails.NewPolicy(guardrails.PolicyInput{
			BlockedToolkits: []string{"---", "  ", "shell"},
			BlockedTools: map[string][]string{
				"***":    {"create_file"},
				"github": {"---", "delete_repo"},
			},
		})
		assertBlocked(t, policy, "github", "delete_repo", true)
		assertBlocked(t, policy, "github", "create_file", false)
		if policy.ToolkitBlocked("") {
			t.Fatal("the empty-canonical toolkit must never be treated as blocked")
		}
		if !policy.ToolkitBlocked("shell") {
			t.Fatal("shell must survive alongside the dropped entries")
		}
	})
}

// TestBlockedToolsHasNoWildcard pins the asymmetry the package comment records.
//
// `is_tool_blocked` looks up only the exact canonical toolkit key, while
// `find_sensitive_tool_match` falls back to `'*'`. Adding a blocked-tools
// wildcard here would block in the catalogue what the worker still executes.
func TestBlockedToolsHasNoWildcard(t *testing.T) {
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedTools: map[string][]string{"*": {"delete_file"}},
	})
	assertBlocked(t, policy, "github", "delete_file", false)
}

func TestSensitiveMatchHonoursTheWildcard(t *testing.T) {
	// The shape the shipped compose files set:
	// ELITEA_SENSITIVE_TOOLS='{"*":["delete_file"]}'.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		SensitiveTools: map[string][]string{"*": {"delete_file"}},
	})
	identity, ok := policy.SensitiveMatch("delete_file", "github")
	if !ok || identity != "*" {
		t.Fatalf("SensitiveMatch = (%q, %v), want (\"*\", true)", identity, ok)
	}
	if _, ok := policy.SensitiveMatch("read_file", "github"); ok {
		t.Fatal("an unlisted tool must not match the wildcard")
	}
}

func TestSensitiveMatchPrefersAConcreteIdentifier(t *testing.T) {
	// A concrete entry decides ahead of the catch-all, so an operator can see
	// WHICH rule fired rather than always being told "*".
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		SensitiveTools: map[string][]string{
			"*":      {"delete_file"},
			"GitHub": {"Delete-File"},
		},
	})
	identity, ok := policy.SensitiveMatch("github___DeleteFile", "github", "my github toolkit")
	if !ok || identity != "github" {
		t.Fatalf("SensitiveMatch = (%q, %v), want (\"github\", true)", identity, ok)
	}
}

func TestSensitiveMatchUsesTheInstanceNameToo(t *testing.T) {
	// The admin form is populated from toolkit TYPES, but a running tool also
	// knows its instance name; both are offered as identifiers.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{
		SensitiveTools: map[string][]string{"prod_repo": {"push"}},
	})
	if _, ok := policy.SensitiveMatch("push", "github"); ok {
		t.Fatal("the type alone must not match an instance-scoped entry")
	}
	if _, ok := policy.SensitiveMatch("push", "github", "prod-repo"); !ok {
		t.Fatal("the instance name must match an instance-scoped entry")
	}
}

func TestDialogCopyFallsBackToTheSDKDefaults(t *testing.T) {
	// An unset field must produce the SDK's own wording, not an empty string
	// interpolated into the authorisation dialog.
	policy := guardrails.NewPolicy(guardrails.PolicyInput{})
	if policy.CompanyName() != guardrails.DefaultSensitiveActionCompanyName {
		t.Fatalf("CompanyName = %q", policy.CompanyName())
	}
	if policy.MessageTemplate() != guardrails.DefaultSensitiveActionMessageTemplate {
		t.Fatalf("MessageTemplate = %q", policy.MessageTemplate())
	}

	configured := guardrails.NewPolicy(guardrails.PolicyInput{
		CompanyName:     "  Acme  ",
		MessageTemplate: " {company_name} says no ",
	})
	if configured.CompanyName() != "Acme" {
		t.Fatalf("CompanyName = %q, want %q", configured.CompanyName(), "Acme")
	}
	if configured.MessageTemplate() != "{company_name} says no" {
		t.Fatalf("MessageTemplate = %q", configured.MessageTemplate())
	}
}

func assertBlocked(t *testing.T, policy guardrails.Policy, toolkitType, toolName string, want bool) {
	t.Helper()
	if got := policy.ToolBlocked(toolkitType, toolName); got != want {
		t.Fatalf("ToolBlocked(%q, %q) = %v, want %v", toolkitType, toolName, got, want)
	}
}
