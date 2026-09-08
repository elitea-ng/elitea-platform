package admin

// The parts of the role-definition surface that need no database (gap G9).
//
// The three writes are covered end to end by
// roles_crud_postgres_integration_test.go, which needs a PostgreSQL and skips
// without one. The rules below are the ones a skipped suite would leave
// unmeasured on a developer machine, and they are the rules that decide whether
// a destructive write happens at all.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateRoleNameRefusesTheMatrixRowKey(t *testing.T) {
	// `permissionMatrix.response` writes the PERMISSION under the key "name"
	// and each role under its own key. A role called "name" would occupy the
	// same key and replace the permission name in every row of the matrix, so
	// the whole page would render one column of role flags and no permissions.
	err := validateRoleName(matrixRowNameKey)
	if err == nil {
		t.Fatal(`a role named "name" was accepted; it collides with the matrix row key`)
	}
	assertStatus(t, err, http.StatusBadRequest)
}

func TestValidateRoleNameBounds(t *testing.T) {
	for name, accepted := range map[string]bool{
		"security_reviewer":           true,
		"Auditor-2":                   true,
		"a":                           true,
		strings.Repeat("r", 64):       true,
		"":                            false,
		strings.Repeat("r", 65):       false,
		"admin ":                      false, // a trailing space reads as `admin` and grants nothing
		" admin":                      false,
		"_leading":                    false,
		"role.with.dots":              false,
		"drop table auth_core__role;": false,
		"role\nname":                  false,
		`{"name":"x"}`:                false,
		"тест":                        false,
	} {
		err := validateRoleName(name)
		if accepted && err != nil {
			t.Errorf("validateRoleName(%q) = %v, want accepted", name, err)
		}
		if !accepted && err == nil {
			t.Errorf("validateRoleName(%q) was accepted", name)
		}
	}
}

// Every built-in name is refused for the two DESTRUCTIVE writes, and creation
// is deliberately not guarded by the same list — a deployment missing `editor`
// must be able to put it back, and the unique constraint refuses a duplicate.
func TestRefuseBuiltInCoversEveryNameTheServiceHardcodes(t *testing.T) {
	for _, name := range []string{"super_admin", "admin", "editor", "viewer", roleSystem} {
		if err := refuseBuiltIn(name, "deleted"); err == nil {
			t.Errorf("%q is a built-in role but was accepted as a delete target", name)
		} else {
			assertStatus(t, err, http.StatusConflict)
		}
	}
	if err := refuseBuiltIn("security_reviewer", "deleted"); err != nil {
		t.Fatalf("a deployment-defined role was refused: %v", err)
	}
}

// `roleSystem` is the constant roles.go already uses for the matrix column it
// refuses to write. The two lists must not drift apart: a `system` column that
// cannot be edited but CAN be deleted is the worse of the two failures.
func TestBuiltInRolesContainsTheMatrixSystemRole(t *testing.T) {
	if _, ok := builtInRoles[roleSystem]; !ok {
		t.Fatalf("builtInRoles does not carry %q, the role roles.go refuses to grant to", roleSystem)
	}
}

// The body wins; the query is the fallback for an intermediary that drops a
// DELETE body. A request with neither yields an empty name, which the handlers
// answer 400 for.
func TestDecodeRoleRequestFallsBackToTheQuery(t *testing.T) {
	fromBody := httptest.NewRequest(http.MethodDelete, "/admin/roles/administration/default?name=from_query",
		strings.NewReader(`{"name":"from_body"}`))
	body, err := decodeRoleRequest(fromBody)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Name != "from_body" {
		t.Fatalf("name = %q, want the body to win over the query", body.Name)
	}

	fromQuery := httptest.NewRequest(http.MethodDelete,
		"/admin/roles/administration/default?name=from_query&new_name=renamed", nil)
	body, err = decodeRoleRequest(fromQuery)
	if err != nil {
		t.Fatalf("decode empty body: %v", err)
	}
	if body.Name != "from_query" || body.NewName != "renamed" {
		t.Fatalf("query fallback = %+v, want from_query/renamed", body)
	}

	malformed := httptest.NewRequest(http.MethodPost, "/admin/roles/administration/default",
		strings.NewReader(`{`))
	if _, err := decodeRoleRequest(malformed); err == nil {
		t.Fatal("a malformed body was accepted")
	}
}

// An unknown scope is 404 and an unknown MODE is 400, and the difference is not
// cosmetic: a scope names a matrix that does not exist, while a mode names a
// role list no resolver would ever read. Neither reaches a database, so a nil
// pool is enough to measure both.
func TestResolveRoleTargetRefusesUnknownScopesAndModes(t *testing.T) {
	handler := NewHandler(nil)

	if _, err := handler.resolveRoleTarget(t.Context(), "not_a_scope", "default"); err == nil {
		t.Fatal("an unknown scope was accepted")
	} else {
		assertStatus(t, err, http.StatusNotFound)
	}

	if _, err := handler.resolveRoleTarget(t.Context(), scopeAdministration, "prompt_lib"); err == nil {
		t.Fatal("a mode no resolver reads was accepted")
	} else {
		assertStatus(t, err, http.StatusBadRequest)
	}

	for _, mode := range []string{"default", "administration", "developer"} {
		target, err := handler.resolveRoleTarget(t.Context(), scopeAdministration, mode)
		if err != nil {
			t.Fatalf("resolve %s: %v", mode, err)
		}
		if !target.central() || target.mode != mode {
			t.Fatalf("resolve %s = %+v", mode, target)
		}
	}
}

// Without a pool every write answers 503 rather than panicking on a nil pool —
// the same fail-closed answer the matrix handlers give.
func TestRoleWritesAnswer503WithoutAPool(t *testing.T) {
	handler := NewHandler(nil)
	for name, serve := range map[string]http.HandlerFunc{
		"create": handler.AdminRoleCreate,
		"rename": handler.AdminRoleRename,
		"delete": handler.AdminRoleDelete,
	} {
		recorder := httptest.NewRecorder()
		serve(recorder, httptest.NewRequest(http.MethodPost, "/admin/roles/administration/default", nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without a pool = %d, want 503", name, recorder.Code)
		}
	}
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	typed, ok := err.(matrixError) //nolint:errorlint // the helpers return the value type directly
	if !ok {
		t.Fatalf("error %v is not a matrixError, so it would be reported as a 500", err)
	}
	if typed.status != want {
		t.Fatalf("status = %d, want %d (%s)", typed.status, want, typed.message)
	}
}
