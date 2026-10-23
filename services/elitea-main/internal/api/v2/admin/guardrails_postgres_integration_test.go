package admin_test

// Guardrails, end to end through the store.
//
// The unit test beside this one asserts that the SECTION is available and that
// the validator accepts the right shapes. This asserts the thing that actually
// matters: a value saved through the admin page is the value the enforcement
// path reads back. Both halves are checked separately — through the product's
// own GET, and by reading the row with SQL — so a handler that echoed the
// request could not pass both.
//
// It is the same test the `resources` section gets, and for the same reason:
// #130/#180 shipped a write that reported success and stored nothing, twice.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

func saveGuardrails(t *testing.T, router chi.Router, values map[string]any) {
	t.Helper()
	recorder := configDo(t, router, http.MethodPut,
		"/admin/plugin_config_values/administration/guardrails",
		map[string]any{"values": values})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT guardrails status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if !decodeConfigBody(t, recorder).Saved {
		t.Fatal("the save did not report saved")
	}
}

// TestGuardrailsSaveIsReadableByItsEnforcementPath is the whole point of the
// unit. It writes through the page and reads through `LoadGuardrails` — the
// function the toolkit surfaces and the agent freeze both call — rather than
// through the page's own GET, because the page agreeing with itself is not
// evidence that anything enforces the value.
func TestGuardrailsSaveIsReadableByItsEnforcementPath(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	saveGuardrails(t, router, map[string]any{
		"blocked_toolkits":                  []any{"Shell", "Data-Source"},
		"blocked_tools":                     map[string]any{"GitHub": []any{"Create-Issue"}},
		"sensitive_tools":                   map[string]any{"*": []any{"delete_file"}},
		"sensitive_action_company_name":     "Acme",
		"sensitive_action_message_template": "{company_name} requires approval for {action_name}.",
	})

	policy, err := platformconfig.LoadGuardrails(context.Background(), pool)
	if err != nil {
		t.Fatalf("the enforcement path could not read what the page saved: %v", err)
	}

	// The operator typed one naming style; the enforcement matches another.
	if !policy.ToolkitBlocked("shell") || !policy.ToolkitBlocked("datasource") {
		t.Fatal("a saved blocked toolkit is not enforced")
	}
	if !policy.ToolBlocked("github", "github___CreateFile") && !policy.ToolBlocked("github", "create_issue") {
		t.Fatal("a saved blocked tool is not enforced")
	}
	if !policy.ToolBlocked("github", "create_issue") {
		t.Fatal("a saved blocked tool is not enforced under its canonical name")
	}
	if identity, ok := policy.SensitiveMatch("delete_file", "github"); !ok || identity != "*" {
		t.Fatalf("the sensitive wildcard did not survive the round trip: %q %v", identity, ok)
	}
	if policy.CompanyName() != "Acme" {
		t.Fatalf("company name = %q", policy.CompanyName())
	}

	// And the rows exist, independently of any handler.
	for _, key := range []string{
		"blocked_toolkits", "blocked_tools", "sensitive_tools",
		"sensitive_action_company_name", "sensitive_action_message_template",
	} {
		if _, ok := storedValueSQL(t, pool, "guardrails", key); !ok {
			t.Errorf("no row was written for %q", key)
		}
	}
}

// TestGuardrailsMapsSurviveTheRoundTripUnmangled guards the JSONB shape rather
// than the matching. A map stored as an array of pairs, or with its values
// flattened, would still let some of the assertions above pass while breaking
// the admin form that has to render it back.
func TestGuardrailsMapsSurviveTheRoundTripUnmangled(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	saveGuardrails(t, router, map[string]any{
		"blocked_tools": map[string]any{"github": []any{"create_issue", "delete_repo"}},
	})

	raw, ok := storedValueSQL(t, pool, "guardrails", "blocked_tools")
	if !ok {
		t.Fatal("no row was written")
	}
	var stored map[string][]string
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("the stored value is not a map of string lists: %s", raw)
	}
	if len(stored["github"]) != 2 || stored["github"][0] != "create_issue" {
		t.Fatalf("stored = %v", stored)
	}

	// And the page reads its own write back in the same shape.
	recorder := configDo(t, router, http.MethodGet,
		"/admin/plugin_config_values/administration/guardrails", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET guardrails status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	values := decodeConfigBody(t, recorder).Values
	tools, ok := values["blocked_tools"].(map[string]any)
	if !ok {
		t.Fatalf("blocked_tools came back as %T", values["blocked_tools"])
	}
	if entries, ok := tools["github"].([]any); !ok || len(entries) != 2 {
		t.Fatalf("blocked_tools[github] = %v", tools["github"])
	}
}

// TestGuardrailsRefusesAValueNoConsumerCouldRead is the 400 the validator
// produces, asserted through the ENDPOINT: a rule that only holds in a unit test
// is a rule the route can still be wired to skip.
func TestGuardrailsRefusesAValueNoConsumerCouldRead(t *testing.T) {
	pool, router := newConfigEnvironment(t)

	recorder := configDo(t, router, http.MethodPut,
		"/admin/plugin_config_values/administration/guardrails",
		map[string]any{"values": map[string]any{
			"blocked_tools": map[string]any{"github": "create_issue"},
		}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if _, ok := storedValueSQL(t, pool, "guardrails", "blocked_tools"); ok {
		t.Fatal("a refused write left a row behind")
	}
}

// TestAnUnconfiguredGuardrailsSectionBlocksNothing. A fresh install must not
// have an implicit policy, and `LoadGuardrails` on an empty section must be
// distinguishable from a failed read — it returns no error.
func TestAnUnconfiguredGuardrailsSectionBlocksNothing(t *testing.T) {
	pool, _ := newConfigEnvironment(t)

	policy, err := platformconfig.LoadGuardrails(context.Background(), pool)
	if err != nil {
		t.Fatalf("an unconfigured section must not read as an error: %v", err)
	}
	if !policy.Empty() {
		t.Fatal("a fresh install has an implicit policy")
	}
}
