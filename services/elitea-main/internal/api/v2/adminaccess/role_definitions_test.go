package adminaccess_test

// The role definitions and their comparison (ADR-0059 R3.9a, issue 2540),
// on the fake directory of console_test.go: every built-in role with the set
// the registry declares, the seed record present.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// RoleDefinitions implements console.RoleDefinitionReader.
func (d *directory) RoleDefinitions(context.Context) ([]registry.StoredRole, bool, error) {
	if d.err != nil {
		return nil, false, d.err
	}
	var out []registry.StoredRole
	for name, set := range d.roles {
		out = append(out, registry.StoredRole{
			Name: name, Marker: registry.SeedMarker(d.builtIn[name]), Permissions: slices.Clone(set),
		})
	}
	return out, true, nil
}

func serveRoles(t *testing.T, d *directory, user *auth.User, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	console.NewRoleDefinitionsHandler(d, d).Register(r)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != nil {
		req = req.WithContext(auth.ContextWithUser(req.Context(), *user))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

type roleDefinitions struct {
	Roles []struct {
		Name            string `json:"name"`
		Status          string `json:"status"`
		Locked          bool   `json:"locked"`
		PermissionCount int    `json:"permission_count"`
		HeldByCaller    bool   `json:"held_by_caller"`
		Assignable      bool   `json:"assignable_by_caller"`
		RefusalCode     string `json:"refusal_code"`
		SelfRefusalCode string `json:"self_refusal_code"`
	} `json:"roles"`
}

// adminCaller is a directory with one user, 7, who holds admin.
func adminCaller() *directory {
	d := newDirectory()
	d.users[7] = []string{registry.RoleAdmin}
	return d
}

// TestRoleDefinitionsGolden: each built-in role as areas, for a caller who
// holds admin.
func TestRoleDefinitionsGolden(t *testing.T) {
	t.Parallel()
	rec := serveRoles(t, adminCaller(), session(7), console.RoleDefinitionsPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	goldenIn(t, "role-definitions", "builtin-roles", rec.Body.Bytes())
}

// TestRoleComparisonGolden: admin beside llm_manager.
func TestRoleComparisonGolden(t *testing.T) {
	t.Parallel()
	rec := serveRoles(t, adminCaller(), session(7), console.RoleComparePath+"?a=admin&b=llm_manager")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	goldenIn(t, "role-definitions", "compare-admin-llm_manager", rec.Body.Bytes())
}

// TestRoleDefinitionsWhoMayAssign: for a holder of admin, super_admin is not
// assignable (role_super_admin_required) and the caller's own role carries
// role_self_assignment; system is never assignable; a narrow role is.
func TestRoleDefinitionsWhoMayAssign(t *testing.T) {
	t.Parallel()
	rec := serveRoles(t, adminCaller(), session(7), console.RoleDefinitionsPath)
	var body roleDefinitions
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	byName := map[string]int{}
	for i, role := range body.Roles {
		byName[role.Name] = i
	}
	check := func(name string, assignable bool, refusal, self string, held bool) {
		t.Helper()
		i, ok := byName[name]
		if !ok {
			t.Fatalf("no role %s in %s", name, rec.Body)
		}
		got := body.Roles[i]
		if got.Assignable != assignable || got.RefusalCode != refusal || got.SelfRefusalCode != self ||
			got.HeldByCaller != held {
			t.Errorf("%s: assignable %v code %q self %q held %v; want %v %q %q %v",
				name, got.Assignable, got.RefusalCode, got.SelfRefusalCode, got.HeldByCaller,
				assignable, refusal, self, held)
		}
	}
	check(registry.RoleSuperAdmin, false, registry.CodeSuperAdminRequired, registry.CodeSuperAdminRequired, false)
	check(registry.RoleAdmin, true, "", registry.CodeSelfAssignment, true)
	check(registry.RoleLLMManager, true, "", registry.CodeSelfAssignment, false)
	check(registry.RoleSystem, false, registry.CodeSetLocked, registry.CodeSetLocked, false)

	// A narrow caller may not hand out a role wider than its own set.
	d := newDirectory()
	d.users[8] = []string{registry.RoleLLMManager}
	rec = serveRoles(t, d, session(8), console.RoleDefinitionsPath)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, role := range body.Roles {
		if role.Name == registry.RoleProjectAdministrator &&
			(role.Assignable || role.RefusalCode != registry.CodePermissionNotHeld) {
			t.Errorf("llm_manager assigning project_administrator: %+v", role)
		}
	}
}

// TestRoleDefinitionsSuperAdminMayAssignAllButSystem: a standing global
// administrator is exempt from the subset and not-yourself rules, never from
// an unassignable role.
func TestRoleDefinitionsSuperAdminMayAssignAllButSystem(t *testing.T) {
	t.Parallel()
	d := newDirectory()
	d.users[9] = []string{registry.RoleSuperAdmin}
	rec := serveRoles(t, d, session(9), console.RoleDefinitionsPath)
	var body roleDefinitions
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, role := range body.Roles {
		if role.Name == registry.RoleSystem {
			if role.Assignable || role.Status != string(registry.RoleStatusUnassignable) {
				t.Errorf("system: %+v", role)
			}
			continue
		}
		if !role.Assignable || role.SelfRefusalCode != "" {
			t.Errorf("super_admin assigning %s: %+v", role.Name, role)
		}
	}
}

func TestRoleComparisonRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		console.RoleComparePath + "?a=admin":               http.StatusBadRequest,
		console.RoleComparePath + "?a=admin&b=no_such_one": http.StatusNotFound,
	}
	for path, want := range cases {
		if rec := serveRoles(t, adminCaller(), session(7), path); rec.Code != want {
			t.Errorf("%s: status %d, want %d (%s)", path, rec.Code, want, rec.Body)
		}
	}
	if rec := serveRoles(t, adminCaller(), nil, console.RoleDefinitionsPath); rec.Code != http.StatusUnauthorized {
		t.Errorf("no user: status %d", rec.Code)
	}
	r := chi.NewRouter()
	console.NewRoleDefinitionsHandler(nil, nil).Register(r)
	req := httptest.NewRequest(http.MethodGet, console.RoleDefinitionsPath, nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), *session(7)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured: status %d", rec.Code)
	}
}
