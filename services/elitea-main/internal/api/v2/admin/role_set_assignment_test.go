package admin_test

// Several administration roles per person: the assignment contract (ADR-0059
// R0.8b, issue 2574).
//
//   - The Users route takes a SET (`role_names`) and applies the difference
//     between the roles the person holds and the set; the single `role_name`
//     form still replaces the set with one role. Any role of the mode is
//     accepted, not four fixed names.
//   - Every rule of #2555 and R0.8a applies to each role of the difference:
//     one role that fails refuses the whole request, the refusal names it,
//     and nothing is written.
//   - POST /admin/modes/administration adds one role and keeps the others.
//   - The listing returns every administration role (`admin_roles`, by name
//     since R0.13) beside the kept single `admin_role`.
//
// Callers are resolved by the real legacyrbac resolver (newEscalationEnvironment);
// the built-in roles carry their registry sets (seedBuiltInRoles).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

const roleSetProjectAdministrator = "r08b-project-admin@autotest.local"

// seedBuiltInRoles makes each named built-in administration role exist with
// its registry set, as shared 0263 seeds it, and makes a project
// administrator caller.
func seedBuiltInRoles(t *testing.T, env escalationEnvironment, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		role, ok := adminaccess.LookupBuiltInRole(name)
		if !ok || !role.DeclaresSet() {
			t.Fatalf("%s is not a built-in role with a declared set", name)
		}
		if _, err := env.pool.Exec(ctx, `
INSERT INTO public.auth_core__role (name, mode)
SELECT $1::text, 'administration'
WHERE NOT EXISTS (SELECT 1 FROM public.auth_core__role WHERE name = $1::text AND mode = 'administration')`,
			name); err != nil {
			t.Fatalf("create the %s role: %v", name, err)
		}
		if _, err := env.pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, granted.permission
FROM public.auth_core__role role, unnest($2::text[]) AS granted(permission)
WHERE role.name = $1 AND role.mode = 'administration'
  AND NOT EXISTS (
      SELECT 1 FROM public.auth_core__role_permission existing
      WHERE existing.role_id = role.id AND existing.permission = granted.permission)`,
			name, role.Set()); err != nil {
			t.Fatalf("grant the %s set: %v", name, err)
		}
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Project admin')
		ON CONFLICT DO NOTHING`, roleSetProjectAdministrator); err != nil {
		t.Fatalf("create the project administrator: %v", err)
	}
	env.ids[roleSetProjectAdministrator] = userID(t, env.pool, roleSetProjectAdministrator)
}

func setRoles(roleNames ...string) map[string]any {
	return map[string]any{"action": "set_admin_role", "role_names": roleNames}
}

func forUser(body map[string]any, id int) map[string]any {
	body["user_id"] = id
	return body
}

// expectNothingWritten proves a refused request left the target's roles and
// the history as they were.
func expectNothingWritten(t *testing.T, env escalationEnvironment, email string, roles ...string) {
	t.Helper()
	expectRoles(t, env, email, roles...)
	if history := grantHistory(t, env.pool, env.ids[email]); len(history) != 0 {
		t.Fatalf("a refused request wrote history: %+v", history)
	}
}

func expectRefusedRole(t *testing.T, body []byte, role string) {
	t.Helper()
	var refusal map[string]any
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if refusal["role"] != role {
		t.Fatalf("the refusal names %v, want %s (body %s)", refusal["role"], role, body)
	}
}

/* ── the subset rule on each role ─────────────────────────────────────── */

// Acceptance (2): a project administrator does not hold every permission of
// admin, so the Users route refuses it, in the single form and when admin is
// one role of two in the set.
func TestAProjectAdministratorCannotAssignAdminOnTheUsersRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleProjectAdministrator, adminaccess.RoleAdmin)
	giveRole(t, env, roleSetProjectAdministrator, adminaccess.RoleProjectAdministrator)
	caller := env.as(roleSetProjectAdministrator)
	plain := env.ids[escalationPlainTarget]

	for name, body := range map[string]map[string]any{
		"single form": {"action": "set_admin_role", "user_id": plain, "role_name": "admin"},
		// empty_role holds nothing, so the caller may give it; admin fails.
		"one of two in the set": forUser(setRoles("empty_role", "admin"), plain),
	} {
		t.Run(name, func(t *testing.T) {
			recorder := adminDo(t, caller, http.MethodPost, usersAction, body)
			expectRefusal(t, recorder, adminaccess.CodePermissionNotHeld)
			expectRefusedRole(t, recorder.Body.Bytes(), "admin")
			expectNothingWritten(t, env, escalationPlainTarget)
		})
	}
}

// Acceptance (2), the modes route.
func TestAProjectAdministratorCannotAssignAdminOnTheModesRoute(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleProjectAdministrator, adminaccess.RoleAdmin)
	giveRole(t, env, roleSetProjectAdministrator, adminaccess.RoleProjectAdministrator)

	recorder := adminDo(t, env.as(roleSetProjectAdministrator), http.MethodPost, modesAssign,
		map[string]any{"user_id": env.ids[escalationPlainTarget], "mode": "administration", "role": "admin"})
	expectRefusal(t, recorder, adminaccess.CodePermissionNotHeld)
	expectRefusedRole(t, recorder.Body.Bytes(), "admin")
	expectNothingWritten(t, env, escalationPlainTarget)
}

// Taking a role counts too: a role the caller cannot give, it cannot take,
// and a set that keeps the other role but drops admin is refused.
func TestASetThatTakesARoleTheCallerCannotGiveIsRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleProjectAdministrator, adminaccess.RoleAdmin)
	giveRole(t, env, roleSetProjectAdministrator, adminaccess.RoleProjectAdministrator)
	giveRole(t, env, escalationTarget, "empty_role") // the target holds admin and empty_role

	recorder := adminDo(t, env.as(roleSetProjectAdministrator), http.MethodPost, usersAction,
		forUser(setRoles("empty_role"), env.ids[escalationTarget]))
	expectRefusal(t, recorder, adminaccess.CodePermissionNotHeld)
	expectRefusedRole(t, recorder.Body.Bytes(), "admin")
	expectNothingWritten(t, env, escalationTarget, "admin", "empty_role")
}

// The not-yourself rule on the set form: a caller that keeps its own role and
// adds one changes its own roles.
func TestASetThatChangesTheCallersOwnRolesIsRefused(t *testing.T) {
	env := newEscalationEnvironment(t)
	recorder := adminDo(t, env.as(escalationAdmin), http.MethodPost, usersAction,
		forUser(setRoles("admin", "empty_role"), env.ids[escalationAdmin]))
	expectRefusal(t, recorder, adminaccess.CodeSelfAssignment)
	expectNothingWritten(t, env, escalationAdmin, "admin")
}

/* ── the difference ───────────────────────────────────────────────────── */

// Acceptance (3): removing one of two roles leaves the other, and writes one
// history row.
func TestRemovingOneOfTwoRolesLeavesTheOtherAndWritesOneRow(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleSecurityAuditor)
	plain := env.ids[escalationPlainTarget]
	kept := assignmentIdentity(t, env.pool, plain, adminaccess.RoleLLMManager)

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		forUser(setRoles(adminaccess.RoleLLMManager), plain)), http.StatusOK)

	expectRoles(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)
	if after := assignmentIdentity(t, env.pool, plain, adminaccess.RoleLLMManager); after != kept {
		t.Fatalf("the kept role was rewritten: %s became %s", kept, after)
	}
	want := []grantRow{{
		user: plain, actor: env.ids[escalationSuper], role: adminaccess.RoleSecurityAuditor,
		mode: "administration", action: "revoked", source: "console",
	}}
	if got := grantHistory(t, env.pool, plain); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// The set form gives several roles at once, one history row each, and the
// listing reports every role beside the kept single field.
func TestTheSetFormGivesSeveralRolesAndTheListingReportsThem(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor)
	plain := env.ids[escalationPlainTarget]
	router := env.as(escalationSuper)

	recorder := adminDo(t, router, http.MethodPost, usersAction,
		forUser(setRoles(adminaccess.RoleSecurityAuditor, adminaccess.RoleLLMManager, adminaccess.RoleLLMManager), plain))
	expectStatus(t, recorder, http.StatusOK)
	var result struct {
		RoleName  *string  `json:"role_name"`
		RoleNames []string `json:"role_names"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if result.RoleName != nil || !slices.Equal(result.RoleNames, []string{"llm_manager", "security_auditor"}) {
		t.Fatalf("result = %+v, want role_name null and both roles", result)
	}
	expectRoles(t, env, escalationPlainTarget, adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor)
	if history := grantHistory(t, env.pool, plain); len(history) != 2 {
		t.Fatalf("history = %+v, want two granted rows", history)
	}

	// The listing: both roles, and no single `admin_role` field (R0.8c).
	row := listedUser(t, router, plain)
	if !slices.Equal(row.AdminRoles, []string{"llm_manager", "security_auditor"}) || row.AdminRole != nil || !row.IsAdmin {
		t.Fatalf("listing = %+v, want admin_roles [llm_manager security_auditor], no admin_role and is_admin true", row)
	}

	// The same set again changes nothing.
	expectStatus(t, adminDo(t, router, http.MethodPost, usersAction,
		forUser(setRoles(adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor), plain)), http.StatusOK)
	if history := grantHistory(t, env.pool, plain); len(history) != 2 {
		t.Fatalf("an unchanged set wrote history: %+v", history)
	}
}

// Acceptance (5): the single role_name form still replaces the whole set with
// one role, and the listing returns the set alone.
func TestTheSingleFormStillReplacesTheSet(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltInRoles(t, env, adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleSecurityAuditor)
	plain := env.ids[escalationPlainTarget]
	router := env.as(escalationSuper)

	row := listedUser(t, router, plain)
	if !slices.Equal(row.AdminRoles, []string{"llm_manager", "security_auditor"}) {
		t.Fatalf("listing before = %+v, want both roles", row)
	}

	recorder := adminDo(t, router, http.MethodPost, usersAction,
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"})
	expectStatus(t, recorder, http.StatusOK)
	expectRoles(t, env, escalationPlainTarget, "editor")

	row = listedUser(t, router, plain)
	if row.AdminRole != nil || !slices.Equal(row.AdminRoles, []string{"editor"}) {
		t.Fatalf("listing after = %+v, want admin_roles [editor] and no admin_role", row)
	}
	// admin_roles is sorted by name, no name above another (ADR-0059 R0.13).
	giveRole(t, env, escalationPlainTarget, adminaccess.RoleLLMManager)
	giveRole(t, env, escalationPlainTarget, "super_admin")
	row = listedUser(t, router, plain)
	if !slices.Equal(row.AdminRoles, []string{"editor", "llm_manager", "super_admin"}) || row.AdminRole != nil {
		t.Fatalf("listing = %+v, want [editor llm_manager super_admin] and no admin_role", row)
	}
}

/* ── the request shape ────────────────────────────────────────────────── */

func TestTheUsersRouteRefusesAMalformedRoleSet(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget]
	for name, body := range map[string]map[string]any{
		"both forms": {"action": "set_admin_role", "user_id": target, "role_name": "admin", "role_names": []string{"admin"}},
		"both forms, null single": {
			"action": "set_admin_role", "user_id": target, "role_name": nil, "role_names": []string{"admin"},
		},
		"an empty name":     forUser(setRoles("admin", " "), target),
		"not an array":      {"action": "set_admin_role", "user_id": target, "role_names": "admin"},
		"an undefined role": forUser(setRoles("admin", "no_such_role"), target),
	} {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction, body),
				http.StatusBadRequest)
			expectNothingWritten(t, env, escalationTarget, "admin")
		})
	}
}

// An empty set takes every role.
func TestAnEmptySetTakesEveryRole(t *testing.T) {
	env := newEscalationEnvironment(t)
	giveRole(t, env, escalationTarget, "empty_role")
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, usersAction,
		forUser(setRoles(), env.ids[escalationTarget])), http.StatusOK)
	expectRoles(t, env, escalationTarget)
}

/* ── the modes route adds ─────────────────────────────────────────────── */

func TestTheModesRouteAddsARoleBesideTheOthers(t *testing.T) {
	env := newEscalationEnvironment(t)
	target := env.ids[escalationTarget] // holds admin
	before := assignmentIdentity(t, env.pool, target, "admin")

	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, modesAssign,
		map[string]any{"user_id": target, "mode": "administration", "role": "empty_role"}), http.StatusOK)

	expectRoles(t, env, escalationTarget, "admin", "empty_role")
	if after := assignmentIdentity(t, env.pool, target, "admin"); after != before {
		t.Fatalf("the held role was rewritten: %s became %s", before, after)
	}
	history := grantHistory(t, env.pool, target)
	if len(history) != 1 || history[0].role != "empty_role" || history[0].action != "granted" {
		t.Fatalf("history = %+v, want one granted empty_role row", history)
	}
}

/* ── helpers ──────────────────────────────────────────────────────────── */

type listedAdminUser struct {
	ID         int      `json:"id"`
	// AdminRole catches the single field R0.8c removed: it must be absent.
	AdminRole  json.RawMessage `json:"admin_role"`
	AdminRoles []string        `json:"admin_roles"`
	IsAdmin    bool     `json:"is_admin"`
}

func listedUser(t *testing.T, router chi.Router, id int) listedAdminUser {
	t.Helper()
	recorder := adminDo(t, router, http.MethodGet, "/u/admin/auth_users/administration?limit=100&offset=0", nil)
	expectStatus(t, recorder, http.StatusOK)
	var listing struct {
		Rows []listedAdminUser `json:"rows"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode the listing: %v", err)
	}
	for _, row := range listing.Rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("user %d is not in the listing", id)
	return listedAdminUser{}
}
