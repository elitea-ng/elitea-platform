package admin_test

// The no-escalation rule (issue 2488, ADR-0059 R0.8) on every admin write that
// changes role contents or role assignments.
//
// Every caller here is resolved by the REAL legacyrbac resolver from the
// fixture's auth_core__user_role rows, so a refusal is proved against what the
// route gates resolve in production. Each refusal is followed by a SQL read
// that proves nothing moved, and each legitimate edit by one that proves it
// did.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// The callers. Each holds exactly one administration role.
const (
	escalationAdmin       = "esc-admin@autotest.local"        // admin
	escalationSuper       = "esc-super@autotest.local"        // super_admin
	escalationMatrix      = "esc-matrix@autotest.local"       // matrix_editor
	escalationModes       = "esc-modes@autotest.local"        // modes_operator
	escalationUsersAdmin  = "esc-users@autotest.local"        // users_operator
	escalationTarget      = "esc-target@autotest.local"       // admin, then reassigned
	escalationPlainTarget = "esc-plain-target@autotest.local" // no role
)

type escalationEnvironment struct {
	pool *pgxpool.Pool
	ids  map[string]int
}

func newEscalationEnvironment(t *testing.T) escalationEnvironment {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))
	ctx := context.Background()
	for _, statement := range []string{
		// The fixture's super_admin role gets the permission that makes it one.
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'admin.auth.users.super_admin' FROM public.auth_core__role
			WHERE mode = 'administration' AND name = 'super_admin'`,
		// Three narrow custom roles, the shape both reviews named.
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('matrix_editor', 'administration'), ('modes_operator', 'administration'),
			('users_operator', 'administration'), ('empty_role', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, grant_row.permission FROM public.auth_core__role role
			JOIN (VALUES ('matrix_editor', 'configuration.roles.permissions.edit'),
			             ('matrix_editor', 'configuration.roles.permissions.view'),
			             ('modes_operator', 'modes.users'),
			             ('users_operator', 'admin.auth.users'))
			  AS grant_row(role_name, permission) ON grant_row.role_name = role.name
			WHERE role.mode = 'administration'`,
		`INSERT INTO public.auth_core__user (email, name) VALUES
			('esc-admin@autotest.local', 'Admin'), ('esc-super@autotest.local', 'Super'),
			('esc-matrix@autotest.local', 'Matrix'), ('esc-modes@autotest.local', 'Modes'),
			('esc-users@autotest.local', 'Users'), ('esc-target@autotest.local', 'Target'),
			('esc-plain-target@autotest.local', 'Plain')`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT account.id, role.id FROM public.auth_core__user account
			JOIN (VALUES ('esc-admin@autotest.local', 'admin'), ('esc-super@autotest.local', 'super_admin'),
			             ('esc-matrix@autotest.local', 'matrix_editor'), ('esc-modes@autotest.local', 'modes_operator'),
			             ('esc-users@autotest.local', 'users_operator'), ('esc-target@autotest.local', 'admin'))
			  AS pair(email, role_name) ON pair.email = account.email
			JOIN public.auth_core__role role ON role.name = pair.role_name AND role.mode = 'administration'`,
		// The admin is a `viewer` member of the public project, which has no
		// overrides and so inherits the central default template.
		`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			SELECT role.project_id, account.id, role.id
			FROM public.auth_core__project_role role, public.auth_core__user account
			WHERE role.project_id = 1 AND role.name = 'viewer' AND account.email = 'esc-admin@autotest.local'`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	ids := map[string]int{}
	for _, email := range []string{escalationAdmin, escalationSuper, escalationMatrix, escalationModes,
		escalationUsersAdmin, escalationTarget, escalationPlainTarget} {
		ids[email] = userID(t, pool, email)
	}
	return escalationEnvironment{pool: pool, ids: ids}
}

// as mounts every role write for one caller, with the real resolver.
func (e escalationEnvironment) as(email string) chi.Router {
	return e.asSession(email, 0)
}

// asSession is `as` from the browser session whose reference is sessionRef
// (0: a session without a server-side row).
func (e escalationEnvironment) asSession(email string, sessionRef int64) chi.Router {
	id := fmt.Sprint(e.ids[email])
	principal := &auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession,
		BrowserSessionRef: sessionRef}
	handler := admin.NewHandler(e.pool, admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(e.pool)))
	router := chi.NewRouter()
	router.Mount("/m", rolesRouter(handler, nil, principal))
	router.Mount("/r", roleWriteRouter(handler, nil, principal))
	router.Mount("/o", modesRouter(handler, principal))
	router.Mount("/u", adminUsersRouter(handler, principal))
	router.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), *principal)))
			})
		})
		r.Put("/p/admin/user_project_permissions/administration", handler.UserProjectPermissionsSave)
		r.Post("/b/admin/invites_bulk/administration", handler.BulkInviteMembers)
		r.Put("/r2/admin/roles/{scope}/{mode}", handler.AdminRoleRename)
	})
	return router
}

func expectRefusal(t *testing.T, recorder *httptest.ResponseRecorder, code string) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 %s (body %s)", recorder.Code, code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	if body["code"] != code {
		t.Fatalf("code = %v, want %s (body %s)", body["code"], code, recorder.Body.String())
	}
	if message, _ := body["error"].(string); message == "" {
		t.Fatalf("a refusal must carry a message: %s", recorder.Body.String())
	}
}

func expectStatus(t *testing.T, recorder *httptest.ResponseRecorder, status int) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", recorder.Code, status, recorder.Body.String())
	}
}

const centralMatrix = "/m/admin/permissions/administration/administration"

// matrixAt reads a matrix through a mounted prefix.
func matrixAt(t *testing.T, router chi.Router, path string) permissionMatrixBody {
	t.Helper()
	recorder := adminDo(t, router, http.MethodGet, path, nil)
	expectStatus(t, recorder, http.StatusOK)
	var body permissionMatrixBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	return body
}

func saveCell(t *testing.T, router chi.Router, path, permission, role string, value bool) *httptest.ResponseRecorder {
	t.Helper()
	return adminDo(t, router, http.MethodPut, path, setCell(t, matrixAt(t, router, path), permission, role, value))
}

/* ── the matrix: the administration plane ─────────────────────────────── */

func TestAnAdminCannotMakeTheAdminRoleASuperAdmin(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationAdmin)

	for _, role := range []string{"admin", "editor"} {
		expectRefusal(t, saveCell(t, router, centralMatrix, adminaccess.SuperAdminPermission, role, true),
			adminaccess.CodeReservedPermission)
		if contains(grantsSQL(t, env.pool, "administration", role), adminaccess.SuperAdminPermission) {
			t.Fatalf("%s holds %s after the refusal", role, adminaccess.SuperAdminPermission)
		}
	}
}

func TestAnAdminCannotStripTheSuperAdminPermissionFromSuperAdmin(t *testing.T) {
	env := newEscalationEnvironment(t)
	// Revoking it would leave nobody able to grant it back, short of SQL.
	expectRefusal(t, saveCell(t, env.as(escalationAdmin), centralMatrix, adminaccess.SuperAdminPermission, "super_admin", false),
		adminaccess.CodePermissionNotHeld)
	if !contains(grantsSQL(t, env.pool, "administration", "super_admin"), adminaccess.SuperAdminPermission) {
		t.Fatal("the refused revoke was written")
	}
}

func TestASuperAdminCannotPutTheSuperAdminPermissionOnAnotherRole(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, saveCell(t, env.as(escalationSuper), centralMatrix, adminaccess.SuperAdminPermission, "admin", true),
		adminaccess.CodeReservedPermission)
}

func TestANarrowMatrixEditorCannotWidenItselfOrAnotherRole(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationMatrix)

	// Its own role: locked, even for a permission it would hold.
	expectRefusal(t, saveCell(t, router, centralMatrix, "admin.auth.users", "matrix_editor", true),
		adminaccess.CodeOwnRoleLocked)
	// Another role: only what it holds.
	expectRefusal(t, saveCell(t, router, centralMatrix, "admin.auth.users", "editor", true),
		adminaccess.CodePermissionNotHeld)
	if contains(grantsSQL(t, env.pool, "administration", "editor"), "admin.auth.users") {
		t.Fatal("editor holds admin.auth.users after the refusal")
	}
	// What it holds, it can grant.
	expectStatus(t, saveCell(t, router, centralMatrix, "configuration.roles.permissions.edit", "editor", true), http.StatusOK)
	if !contains(grantsSQL(t, env.pool, "administration", "editor"), "configuration.roles.permissions.edit") {
		t.Fatal("the legitimate grant was not written")
	}
}

func TestAnAdminCannotEditTheAdminRoleItHolds(t *testing.T) {
	env := newEscalationEnvironment(t)
	expectRefusal(t, saveCell(t, env.as(escalationAdmin), centralMatrix, "admin.auth.users", "admin", false),
		adminaccess.CodeOwnRoleLocked)
	if !contains(grantsSQL(t, env.pool, "administration", "admin"), "admin.auth.users") {
		t.Fatal("the refused revoke was written")
	}
}

func TestASuperAdminKeepsFullPowerOverTheMatrix(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationSuper)
	// A permission it does not hold, on a role it does hold.
	expectStatus(t, saveCell(t, router, centralMatrix, "models.alpha.view", "super_admin", true), http.StatusOK)
	// A revoke from another role.
	expectStatus(t, saveCell(t, router, centralMatrix, "models.beta.edit", "admin", false), http.StatusOK)
	if !contains(grantsSQL(t, env.pool, "administration", "super_admin"), "models.alpha.view") {
		t.Fatal("the super_admin grant was not written")
	}
}

/* ── the matrix: the project plane ────────────────────────────────────── */

func TestAMatrixEditCannotWidenTheCallersOwnProjectRole(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationAdmin)
	const template = "/m/admin/permissions/administration/default"

	// The admin is a viewer of the public project, which inherits the
	// template: granting `viewer` more would widen the admin there.
	expectRefusal(t, saveCell(t, router, template, "models.beta.edit", "viewer", true),
		adminaccess.CodeSelfEscalation)
	if contains(grantsSQL(t, env.pool, "default", "viewer"), "models.beta.edit") {
		t.Fatal("the refused template grant was written")
	}
	// A role the admin does not hold anywhere is the admin's to edit.
	expectStatus(t, saveCell(t, router, template, "models.beta.edit", "editor", true), http.StatusOK)
	// The reserved permission goes on no project role.
	expectRefusal(t, saveCell(t, router, template, adminaccess.SuperAdminPermission, "editor", true),
		adminaccess.CodeReservedPermission)
	// Apply to Projects writes the template into the public project too, but
	// the public project is excluded from the sync, so it widens nobody here.
	expectStatus(t, adminDo(t, router, http.MethodPost, template, nil), http.StatusOK)
}

/* ── role definitions ─────────────────────────────────────────────────── */

func TestRoleCreateIgnoresAPermissionListInTheBody(t *testing.T) {
	env := newEscalationEnvironment(t)
	recorder := adminDo(t, env.as(escalationMatrix), http.MethodPost, "/r/admin/roles/administration/administration",
		map[string]any{"name": "fresh_role", "permissions": []string{adminaccess.SuperAdminPermission, "admin.auth.users"}})
	expectStatus(t, recorder, http.StatusCreated)
	if grants := grantsSQL(t, env.pool, "administration", "fresh_role"); len(grants) != 0 {
		t.Fatalf("a new role holds %v, want nothing", grants)
	}
}

/* ── assignment: the modes routes ─────────────────────────────────────── */

func TestTheModesRouteAppliesTheSubsetRule(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationModes)
	plain := env.ids[escalationPlainTarget]

	// Assigning `admin` needs every permission of `admin`.
	expectRefusal(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": plain, "mode": "administration", "role": "admin"}), adminaccess.CodePermissionNotHeld)
	// Assigning itself a wider custom role, too. Since ADR-0059 R0.8a the
	// not-yourself rule decides first, so the code is the more specific one
	// (role_self_assignment_test.go covers the rule itself).
	expectRefusal(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": env.ids[escalationModes], "mode": "administration", "role": "matrix_editor"}),
		adminaccess.CodeSelfAssignment)
	if names := adminRoleNamesSQL(t, env.pool, plain); len(names) != 0 {
		t.Fatalf("the refused assignment was written: %v", names)
	}
	// Removing an admin needs every permission of `admin` as well.
	expectRefusal(t, adminDo(t, router, http.MethodDelete,
		fmt.Sprintf("/o/admin/modes/administration?id=%d:administration:admin", env.ids[escalationTarget]), nil),
		adminaccess.CodePermissionNotHeld)
	// super_admin keeps its own guard and code.
	expectRefusal(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": plain, "mode": "administration", "role": "super_admin"}),
		adminaccess.CodeSuperAdminRequired)
	// A role within its own set is assignable.
	expectStatus(t, adminDo(t, router, http.MethodPost, "/o/admin/modes/administration",
		map[string]any{"user_id": plain, "mode": "administration", "role": "empty_role"}), http.StatusOK)

	// A developer-mode row the legacy runtime left behind grants nothing a
	// route reads, so clearing it needs no subset.
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('legacy_dev', 'developer')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'admin.auth.users' FROM public.auth_core__role WHERE name = 'legacy_dev' AND mode = 'developer'`,
		fmt.Sprintf(`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT %d, id FROM public.auth_core__role WHERE name = 'legacy_dev' AND mode = 'developer'`, plain),
	} {
		if _, err := env.pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	expectStatus(t, adminDo(t, router, http.MethodDelete,
		fmt.Sprintf("/o/admin/modes/administration?id=%d:developer:legacy_dev", plain), nil), http.StatusOK)
}

/* ── assignment: the Users page ───────────────────────────────────────── */

func TestSetAdminRoleAppliesTheSubsetRule(t *testing.T) {
	env := newEscalationEnvironment(t)
	plain := env.ids[escalationPlainTarget]

	expectRefusal(t, adminDo(t, env.as(escalationUsersAdmin), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "admin"}),
		adminaccess.CodePermissionNotHeld)
	// Demoting an admin takes `admin` away, so it needs all of `admin` too.
	expectRefusal(t, adminDo(t, env.as(escalationUsersAdmin), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": env.ids[escalationTarget], "role_name": nil}),
		adminaccess.CodePermissionNotHeld)
	if names := adminRoleNamesSQL(t, env.pool, env.ids[escalationTarget]); fmt.Sprint(names) != "[admin]" {
		t.Fatalf("roles after the refused demotion = %v, want [admin]", names)
	}
	// The admin assigns what it holds.
	expectStatus(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "editor"}), http.StatusOK)
	// And super_admin stays guarded by its own rule.
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "super_admin"}),
		adminaccess.CodeSuperAdminRequired)
	expectStatus(t, adminDo(t, env.as(escalationSuper), http.MethodPost, "/u/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": plain, "role_name": "super_admin"}), http.StatusOK)
}

/* ── the other project-plane writes ───────────────────────────────────── */

func TestThePersonalProjectMatrixCannotWidenTheCaller(t *testing.T) {
	env := newEscalationEnvironment(t)
	// The admin is a viewer of the personal project 3, whose viewer has its
	// own grant (models.gamma.delete).
	if _, err := env.pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 3, $1, id FROM public.auth_core__project_role WHERE project_id = 3 AND name = 'viewer'`,
		env.ids[escalationAdmin]); err != nil {
		t.Fatal(err)
	}
	router := env.as(escalationAdmin)
	const path = "/p/admin/user_project_permissions/administration"

	expectRefusal(t, adminDo(t, router, http.MethodPut, path,
		map[string]any{"viewer": []string{"models.gamma.delete", "models.beta.edit"}}), adminaccess.CodeSelfEscalation)
	if contains(projectGrantsSQL(t, env.pool, 3, "viewer"), "models.beta.edit") {
		t.Fatal("the refused grant was written")
	}
	expectRefusal(t, adminDo(t, router, http.MethodPut, path,
		map[string]any{"editor": []string{adminaccess.SuperAdminPermission}}), adminaccess.CodeReservedPermission)
	expectStatus(t, adminDo(t, router, http.MethodPut, path,
		map[string]any{"editor": []string{"models.beta.edit"}}), http.StatusOK)
	// `append_user_role` gives the roles to every member of every personal
	// project, which no subset of the caller's own set can cover.
	expectRefusal(t, adminDo(t, router, http.MethodPut, path+"?append_user_role",
		map[string]any{"editor": []string{"models.beta.edit"}}), adminaccess.CodeSuperAdminRequired)
}

func TestABulkInviteCannotGiveTheCallerAProjectRole(t *testing.T) {
	env := newEscalationEnvironment(t)
	router := env.as(escalationAdmin)
	const path = "/b/admin/invites_bulk/administration"

	// The admin holds nothing in project 2, so its own account in the batch
	// refuses the whole batch.
	expectRefusal(t, adminDo(t, router, http.MethodPost, path, map[string]any{
		"users": []int{env.ids[escalationAdmin], env.ids[escalationPlainTarget]}, "projects": []int{2}, "role": "admin",
	}), adminaccess.CodeSelfEscalation)
	var members int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM public.auth_core__project_user_role WHERE project_id = 2`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 0 {
		t.Fatalf("a refused batch wrote %d memberships, want none", members)
	}
	// Placing other accounts is the operator authority the central gate grants.
	expectStatus(t, adminDo(t, router, http.MethodPost, path, map[string]any{
		"users": []int{env.ids[escalationPlainTarget]}, "projects": []int{2}, "role": "admin",
	}), http.StatusOK)
}

func TestARenameCannotWidenTheCallerThroughTheTemplateFallback(t *testing.T) {
	env := newEscalationEnvironment(t)
	// The admin holds `helper` in the public project, which has no grants of
	// its own. The central template `power` grants models.beta.edit, and a
	// project role with no grants falls back to the template of the same name.
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('power', 'default')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'models.beta.edit' FROM public.auth_core__role WHERE name = 'power' AND mode = 'default'`,
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES (1, 'helper')`,
		fmt.Sprintf(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			SELECT 1, %d, id FROM public.auth_core__project_role WHERE project_id = 1 AND name = 'helper'`,
			env.ids[escalationAdmin]),
		// The admin's viewer membership would hide the gain: drop it.
		fmt.Sprintf(`DELETE FROM public.auth_core__project_user_role assignment
			USING public.auth_core__project_role role
			WHERE role.id = assignment.role_id AND role.name = 'viewer' AND assignment.user_id = %d`,
			env.ids[escalationAdmin]),
	} {
		if _, err := env.pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	expectRefusal(t, adminDo(t, env.as(escalationAdmin), http.MethodPut, "/r2/admin/roles/public/default",
		map[string]any{"name": "helper", "new_name": "power"}), adminaccess.CodeSelfEscalation)
	if roles := projectRoleNamesSQL(t, env.pool, 1); !contains(roles, "helper") || contains(roles, "power") {
		t.Fatalf("project roles after the refused rename = %v", roles)
	}
}

/* ── SCIM group bindings ──────────────────────────────────────────────── */

func TestAGroupBindingAssignsOnlyARoleTheCallerHolds(t *testing.T) {
	env := newEscalationEnvironment(t)
	ctx := context.Background()
	for _, file := range []string{
		"../../../../migrations/shared/0096_scim_provisioning.sql",
		"../../../../migrations/shared/0098_scim_group_bindings.sql",
	} {
		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	bindingsAs := func(email string) chi.Router {
		id := fmt.Sprint(env.ids[email])
		handler := admin.NewHandler(env.pool,
			admin.WithPermissionResolver(legacyrbac.NewPostgresResolver(env.pool)),
			admin.WithSCIMGroupBindings(scimdirectory.NewStore(env.pool)))
		router := chi.NewRouter()
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession})))
			})
		})
		router.Post("/bindings", handler.SCIMGroupBindingCreate)
		router.Put("/bindings/{id}", handler.SCIMGroupBindingSave)
		router.Delete("/bindings/{id}", handler.SCIMGroupBindingDelete)
		return router
	}
	// The admin is a `viewer` of the public project (project 1).
	router := bindingsAs(escalationAdmin)
	bind := func(role string) map[string]any {
		return map[string]any{"display_name": "Team " + role, "project_id": publicProjectID, "role_name": role}
	}

	// A group the caller is in, or a second account of its own, would carry
	// the role: binding `admin` is assigning it.
	expectRefusal(t, adminDo(t, router, http.MethodPost, "/bindings", bind("admin")), adminaccess.CodePermissionNotHeld)
	// `viewer` is the caller's own role there.
	created := adminDo(t, router, http.MethodPost, "/bindings", bind("viewer"))
	expectStatus(t, created, http.StatusCreated)
	var body struct {
		Binding struct {
			ID string `json:"id"`
		} `json:"binding"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	path := "/bindings/" + body.Binding.ID
	// Moving it to `admin` is assigning `admin`.
	expectRefusal(t, adminDo(t, router, http.MethodPut, path, bind("admin")), adminaccess.CodePermissionNotHeld)

	// A super admin moves it to `admin`. The admin can then neither move nor
	// delete it, because both take `admin` away from its members.
	expectStatus(t, adminDo(t, bindingsAs(escalationSuper), http.MethodPut, path, bind("admin")), http.StatusOK)
	expectRefusal(t, adminDo(t, router, http.MethodPut, path, bind("viewer")), adminaccess.CodePermissionNotHeld)
	expectRefusal(t, adminDo(t, router, http.MethodDelete, path, nil), adminaccess.CodePermissionNotHeld)
	expectStatus(t, adminDo(t, bindingsAs(escalationSuper), http.MethodDelete, path, nil), http.StatusNoContent)
}

/* ── the role-writes lock ─────────────────────────────────────────────── */

// A check made under a hold cannot be outdated before the hold ends: a second
// role write waits for the lock until the hold is released.
func TestARoleWriteWaitsForTheHoldOfAnother(t *testing.T) {
	env := newEscalationEnvironment(t)
	ctx := context.Background()
	_, release, err := adminaccess.HoldRoleWrites(ctx, env.pool)
	if err != nil {
		t.Fatal(err)
	}

	second, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Rollback(ctx) }()
	waiting, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := adminaccess.LockRoleWrites(waiting, second); err == nil {
		t.Fatal("a second role write took the lock while another write held it")
	}
	_ = second.Rollback(ctx)

	release()
	third, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = third.Rollback(ctx) }()
	if err := adminaccess.LockRoleWrites(ctx, third); err != nil {
		t.Fatalf("the lock is still held after release: %v", err)
	}
}

func projectRoleNamesSQL(t *testing.T, pool *pgxpool.Pool, projectID int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT name FROM public.auth_core__project_role WHERE project_id = $1 ORDER BY 1`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	return collectStrings(t, rows)
}
