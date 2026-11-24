package admin_test

// The legacy names get no new grant (ADR-0059 decision 2, story R0.14a).
//
// runtime.plugins and configuration.governance end on the sunset date. Until
// then the Roles matrix refuses to add either to a role that does not hold it
// (400 permission_deprecated), for a global administrator too, and leaves
// every existing grant alone: a save that keeps one answers 200.
//
// Callers are resolved by the real legacyrbac resolver
// (newEscalationEnvironment), so it runs on PostgreSQL.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// expectDeprecated asserts the 400 and the names it carries.
func expectDeprecated(t *testing.T, recorder *httptest.ResponseRecorder, permission, role string) {
	t.Helper()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 %s (body %s)", recorder.Code,
			adminaccess.CodePermissionDeprecated, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	permissions, _ := body["permissions"].([]any)
	if body["code"] != adminaccess.CodePermissionDeprecated || body["role"] != role ||
		len(permissions) != 1 || permissions[0] != permission {
		t.Fatalf("body = %s, want code %s on %s for %s", recorder.Body.String(),
			adminaccess.CodePermissionDeprecated, role, permission)
	}
	if message, _ := body["error"].(string); message == "" {
		t.Fatalf("the refusal carries no message: %s", recorder.Body.String())
	}
}

func TestAddingALegacyNameToACustomRoleIsRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	for _, legacy := range []string{adminaccess.RuntimePlugins, adminaccess.ConfigurationGovernance} {
		t.Run(legacy, func(t *testing.T) {
			// A global administrator, who passes every other rule.
			expectDeprecated(t, saveCell(t, env.as(escalationSuper), centralMatrix, legacy, "empty_role", true),
				legacy, "empty_role")
			// A narrow matrix editor gets the 400 too, not the 403 of the
			// subset rule: the refusal is decided first.
			expectDeprecated(t, saveCell(t, env.as(escalationMatrix), centralMatrix, legacy, "empty_role", true),
				legacy, "empty_role")
			if contains(grantsSQL(t, env.pool, "administration", "empty_role"), legacy) {
				t.Fatalf("empty_role holds %s after the refusal", legacy)
			}
		})
	}
}

func TestSavingARoleThatAlreadyHoldsALegacyNameAnswers200(t *testing.T) {
	env := newEscalationEnvironment(t)
	if _, err := env.pool.Exec(context.Background(), `INSERT INTO public.auth_core__role_permission (role_id, permission)
		SELECT id, 'runtime.plugins' FROM public.auth_core__role
		WHERE mode = 'administration' AND name = 'empty_role'`); err != nil {
		t.Fatalf("grant the existing legacy name: %v", err)
	}
	router := env.as(escalationSuper)

	// Unchanged: the whole matrix as the page shows it.
	matrix := matrixAt(t, router, centralMatrix)
	if !matrix.granted(t, adminaccess.RuntimePlugins, "empty_role") {
		t.Fatal("premise: the matrix does not show the existing grant")
	}
	expectStatus(t, adminDo(t, router, http.MethodPut, centralMatrix, matrix.Rows), http.StatusOK)

	// Another cell of the same role changes; the legacy grant is kept.
	expectStatus(t, saveCell(t, router, centralMatrix, adminaccess.LLMUsageView, "empty_role", true), http.StatusOK)
	held := grantsSQL(t, env.pool, "administration", "empty_role")
	if !contains(held, adminaccess.RuntimePlugins) || !contains(held, adminaccess.LLMUsageView) {
		t.Fatalf("empty_role holds %v, want runtime.plugins kept and llm.usage.view added", held)
	}

	// Taking the existing grant away is allowed; giving it back is not.
	expectStatus(t, saveCell(t, router, centralMatrix, adminaccess.RuntimePlugins, "empty_role", false), http.StatusOK)
	expectDeprecated(t, saveCell(t, router, centralMatrix, adminaccess.RuntimePlugins, "empty_role", true),
		adminaccess.RuntimePlugins, "empty_role")
}
