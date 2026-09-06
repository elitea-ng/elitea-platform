package admin_test

// Acceptance for the role-definition writes — create, rename, delete (gap G9).
//
// This surface DEFINES privilege rather than assigning it, so every test below
// asserts a database fact and not only a status code:
//
//  1. A create is re-read through the product's own
//     `GET /admin/permissions/{scope}/{mode}`, because a role that exists but
//     never becomes a matrix column is a role nobody can grant anything to.
//  2. A refusal is followed by "and nothing moved". A 409 that deleted the role
//     anyway is the failure this whole surface has to not have.
//  3. The propagation claim — "a new central default-mode role reaches existing
//     projects through the EXISTING Apply to Projects control" — is measured by
//     running that control, not asserted in a comment.
//
// It shares roles_write_postgres_integration_test.go's fixture and helpers
// (`newRolesPool`, `prepareRolesFixture`, `rolesRouter`, `readMatrix`,
// `adminDo`, `grantingResolver`). Requires a PostgreSQL; skipped otherwise.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// newRole is the role every test that needs a deployment-defined one uses. It
// is deliberately NOT one of the five names the service hardcodes.
const newRole = "security_reviewer"

// roleWriteRouter mounts the three writes exactly as internal/api/router.go
// does. `gate` nil mounts them bare, so a test that is not about authorisation
// is not obscured by one.
func roleWriteRouter(handler *admin.Handler, gate func(http.Handler) http.Handler, principal *auth.User) chi.Router {
	router := chi.NewRouter()
	if principal != nil {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), *principal)))
			})
		})
	}
	if gate == nil {
		gate = func(next http.Handler) http.Handler { return next }
	}
	router.With(gate).Post("/admin/roles/{scope}/{mode}", handler.AdminRoleCreate)
	router.With(gate).Put("/admin/roles/{scope}/{mode}", handler.AdminRoleRename)
	router.With(gate).Delete("/admin/roles/{scope}/{mode}", handler.AdminRoleDelete)
	return router
}

// newRoleWriteEnvironment returns the pool, the three writes, and the MATRIX
// routes. Two routers, because the write surface and the read-back surface must
// not be the same object: a test that re-read through the router it just wrote
// to could not tell a persisted change from a cached one.
func newRoleWriteEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router, chi.Router) {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))
	principal := &auth.User{ID: "1", UserID: "1"}
	return pool,
		roleWriteRouter(admin.NewHandler(pool), nil, principal),
		rolesRouter(admin.NewHandler(pool), nil, principal)
}

func rolesURL(scope, mode string) string { return "/admin/roles/" + scope + "/" + mode }

// centralRoleExists and projectRoleExists read the tables directly. The matrix
// re-read is the primary assertion; these two are what distinguish "the role is
// gone" from "the matrix stopped listing it".
func centralRoleExists(t *testing.T, pool *pgxpool.Pool, mode, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM public.auth_core__role WHERE mode = $1 AND name = $2)`,
		mode, name).Scan(&exists)
	if err != nil {
		t.Fatalf("read central role: %v", err)
	}
	return exists
}

func projectsWithRole(t *testing.T, pool *pgxpool.Pool, name string) []int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT project_id FROM public.auth_core__project_role WHERE name = $1 ORDER BY project_id`, name)
	if err != nil {
		t.Fatalf("read project roles: %v", err)
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return ids
}

// seedMember creates a user and assigns it one project role, so the delete
// guard has something real to count.
func seedMember(t *testing.T, pool *pgxpool.Pool, email string, projectID int, role string) int {
	t.Helper()
	ctx := context.Background()
	var userID int
	if err := pool.QueryRow(ctx,
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	tag, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT $1, $2, role.id FROM public.auth_core__project_role role
WHERE role.project_id = $1 AND role.name = $3`, projectID, userID, role)
	if err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("seed membership assigned %d rows: project %d has no role %q",
			tag.RowsAffected(), projectID, role)
	}
	return userID
}

/* ── create ────────────────────────────────────────────────────────────── */

// The whole point of the surface: a role that did not exist becomes a real
// column of the matrix the operator then grants on.
func TestAdminRoleCreateAddsACentralRoleTheMatrixThenShows(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	before := readMatrix(t, matrix, "administration", "default")
	if contains(before.roles(t), newRole) {
		t.Fatalf("the fixture already defines %q", newRole)
	}

	recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}

	if !centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("the create answered 201 and wrote no row")
	}
	after := readMatrix(t, matrix, "administration", "default")
	if !contains(after.roles(t), newRole) {
		t.Fatalf("the new role is not a matrix column: %v", after.roles(t))
	}
	// It starts with nothing granted. A role that arrives pre-granted would be
	// a privilege nobody chose.
	if after.granted(t, "models.alpha.view", newRole) {
		t.Fatal("a new role arrived holding a permission")
	}
	if got := grantsSQL(t, pool, "default", newRole); len(got) != 0 {
		t.Fatalf("a new role arrived with grants: %v", got)
	}
	// The OTHER mode is untouched: auth_core__role is UNIQUE (name, mode), and
	// a write that forgot its mode predicate would be invisible without this.
	if centralRoleExists(t, pool, "administration", newRole) {
		t.Fatal("creating a default-mode role also created an administration-mode one")
	}
}

// The public/support tabs address a PROJECT's roles, and their target-mode
// segment is ignored — the same rule readMatrix already follows.
func TestAdminRoleCreateAddsAProjectRole(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	recorder := adminDo(t, writes, http.MethodPost, rolesURL("public", "default"),
		map[string]any{"name": newRole})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}

	if got := projectsWithRole(t, pool, newRole); len(got) != 1 || got[0] != publicProjectID {
		t.Fatalf("project roles named %q = %v, want only the public project", newRole, got)
	}
	if centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("a project-scoped create wrote a CENTRAL role")
	}
	if !contains(readMatrix(t, matrix, "public", "default").roles(t), newRole) {
		t.Fatal("the new project role is not a column of the public matrix")
	}
}

func TestAdminRoleCreateRefusals(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)

	for name, probe := range map[string]struct {
		scope, mode string
		body        any
		want        int
	}{
		// The name already exists in this mode.
		"duplicate":       {"administration", "default", map[string]any{"name": "admin"}, http.StatusConflict},
		"empty name":      {"administration", "default", map[string]any{"name": ""}, http.StatusBadRequest},
		"matrix row key":  {"administration", "default", map[string]any{"name": "name"}, http.StatusBadRequest},
		"illegal chars":   {"administration", "default", map[string]any{"name": "a role"}, http.StatusBadRequest},
		"unknown mode":    {"administration", "prompt_lib", map[string]any{"name": newRole}, http.StatusBadRequest},
		"unknown scope":   {"tenant", "default", map[string]any{"name": newRole}, http.StatusNotFound},
		"no body at all":  {"administration", "default", nil, http.StatusBadRequest},
		"duplicate again": {"public", "default", map[string]any{"name": "admin"}, http.StatusConflict},
	} {
		recorder := adminDo(t, writes, http.MethodPost, rolesURL(probe.scope, probe.mode), probe.body)
		if recorder.Code != probe.want {
			t.Errorf("%s: status = %d, want %d (body %s)",
				name, recorder.Code, probe.want, recorder.Body.String())
		}
	}

	// Nothing was created by any of them.
	if len(projectsWithRole(t, pool, "name")) != 0 || centralRoleExists(t, pool, "default", "name") {
		t.Fatal("a refused create wrote a role anyway")
	}
	if centralRoleExists(t, pool, "prompt_lib", newRole) {
		t.Fatal("a role was written in a mode no resolver reads")
	}
}

/* ── propagation ───────────────────────────────────────────────────────── */

// The propagation claim, measured.
//
// A new central `default`-mode role reaches EXISTING projects only through the
// "Apply to Projects" control the page already has
// (`POST /admin/permissions/administration/default`). This unit added no second
// mechanism, so if that control did not pick a new role up, a created role
// would be invisible to every project that existed before it — which is the
// half of gap G9 that a create-only fix would have left in place.
func TestANewCentralRoleReachesExistingProjectsThroughApplyToProjects(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	// Before the sync it exists centrally and in NO project. That is the state
	// gap G9 describes, and asserting it is what makes the next step meaningful.
	if got := projectsWithRole(t, pool, newRole); len(got) != 0 {
		t.Fatalf("a create reached projects %v on its own", got)
	}

	if recorder := adminDo(t, matrix, http.MethodPost,
		"/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("Apply to Projects status = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	// Every SHARED project, and not the personal one. `publicProjectID` is the
	// sync's own exclusion (it is the matrix the central default one already
	// is), and project 3 is `project_user_9`.
	got := projectsWithRole(t, pool, newRole)
	want := []int{sharedProjectID, supportProjectID, wildcardNameProjectID}
	if !equalInts(got, want) {
		t.Fatalf("after Apply to Projects the role is in projects %v, want %v", got, want)
	}
}

/* ── rename ────────────────────────────────────────────────────────────── */

// A rename keeps the grants and the members, and follows the role into the
// per-project copies the platform joins BY NAME.
func TestAdminRoleRenameCarriesGrantsMembersAndProjectCopies(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	// Grant it something through the matrix, and push it to the projects.
	before := readMatrix(t, matrix, "administration", "default")
	if recorder := adminDo(t, matrix, http.MethodPut, "/admin/permissions/administration/default",
		setCell(t, before, "models.alpha.view", newRole, true)); recorder.Code != http.StatusOK {
		t.Fatalf("grant status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	if recorder := adminDo(t, matrix, http.MethodPost,
		"/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d", recorder.Code)
	}
	memberID := seedMember(t, pool, "reviewer@example.com", sharedProjectID, newRole)

	const renamed = "risk_reviewer"
	recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "default"),
		map[string]any{"name": newRole, "new_name": renamed})
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	if centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("the old central name survived the rename")
	}
	if !centralRoleExists(t, pool, "default", renamed) {
		t.Fatal("the new central name does not exist")
	}
	// The grant followed. It is keyed on role_id, so this asserts the rename was
	// an UPDATE and not a delete-and-recreate — which would have dropped it.
	if got := grantsSQL(t, pool, "default", renamed); !equalStrings(got, []string{"models.alpha.view"}) {
		t.Fatalf("grants after rename = %v, want the one that was granted", got)
	}
	// The per-project copies followed, so the projects are not left holding a
	// role the platform no longer defines.
	if got := projectsWithRole(t, pool, newRole); len(got) != 0 {
		t.Fatalf("projects %v still carry the old name", got)
	}
	if got := projectsWithRole(t, pool, renamed); len(got) == 0 {
		t.Fatal("no project copy was renamed")
	}
	// And the member kept the role: project_user_role references role_id.
	var stillAssigned bool
	if err := pool.QueryRow(context.Background(), `
SELECT EXISTS (
  SELECT 1 FROM public.auth_core__project_user_role assignment
  JOIN public.auth_core__project_role role ON role.id = assignment.role_id
  WHERE assignment.user_id = $1 AND role.name = $2)`, memberID, renamed).Scan(&stillAssigned); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if !stillAssigned {
		t.Fatal("the rename dropped the member's assignment")
	}
}

// A project that ALREADY has a role under the new name is left alone rather
// than merged: merging would move members between two roles a deployment
// deliberately kept apart. The response reports the shortfall.
func TestAdminRoleRenameSkipsAProjectThatAlreadyHasTheNewName(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	if recorder := adminDo(t, matrix, http.MethodPost,
		"/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d", recorder.Code)
	}
	const renamed = "risk_reviewer"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, $2)`,
		sharedProjectID, renamed); err != nil {
		t.Fatalf("seed the colliding project role: %v", err)
	}

	recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "default"),
		map[string]any{"name": newRole, "new_name": renamed})
	if recorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d (body %s)", recorder.Code, recorder.Body.String())
	}

	// The colliding project keeps BOTH rows; every other project was renamed.
	if got := projectsWithRole(t, pool, newRole); !equalInts(got, []int{sharedProjectID}) {
		t.Fatalf("the old name survives in projects %v, want only the colliding one", got)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode rename response: %v", err)
	}
	// Reported, not silent: the operator can see that one project was skipped.
	if renamedCount, ok := body["projects_renamed"].(float64); !ok || int(renamedCount) != 2 {
		t.Fatalf("projects_renamed = %v, want 2 (the two projects without a collision)", body["projects_renamed"])
	}
}

func TestAdminRoleRenameRefusals(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)

	for name, probe := range map[string]struct {
		body any
		want int
	}{
		// A built-in role is never a rename target: the service names it in its
		// own SQL and Go, which a rename would not follow.
		"built-in source": {map[string]any{"name": "admin", "new_name": "administrator"}, http.StatusConflict},
		"built-in target": {map[string]any{"name": "editor", "new_name": "viewer"}, http.StatusConflict},
		"unknown role":    {map[string]any{"name": "nobody", "new_name": newRole}, http.StatusNotFound},
		"no new name":     {map[string]any{"name": "nobody"}, http.StatusBadRequest},
		"reserved target": {map[string]any{"name": "nobody", "new_name": "name"}, http.StatusBadRequest},
		// A rename to the same name is not short-circuited: an unknown role
		// still answers 404 rather than a 200 that claims work was done.
		"same name, unknown role": {
			map[string]any{"name": newRole, "new_name": newRole}, http.StatusNotFound,
		},
	} {
		recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "default"), probe.body)
		if recorder.Code != probe.want {
			t.Errorf("%s: status = %d, want %d (body %s)",
				name, recorder.Code, probe.want, recorder.Body.String())
		}
	}

	// AND NOTHING MOVED. A refusal that renamed the role anyway is the failure
	// this surface must not have.
	for _, name := range []string{"admin", "editor", "viewer", "system"} {
		if !centralRoleExists(t, pool, "default", name) {
			t.Errorf("a refused rename removed the built-in role %q", name)
		}
	}
	if centralRoleExists(t, pool, "default", "administrator") {
		t.Fatal("a refused rename created the new name anyway")
	}
}

// Renaming a role to a name another role in the SAME mode already holds is a
// 409, not a 500 from the unique constraint.
func TestAdminRoleRenameReportsACollisionAsAConflict(t *testing.T) {
	_, writes, _ := newRoleWriteEnvironment(t)

	for _, name := range []string{newRole, "risk_reviewer"} {
		if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
			map[string]any{"name": name}); recorder.Code != http.StatusCreated {
			t.Fatalf("create %s status = %d", name, recorder.Code)
		}
	}

	recorder := adminDo(t, writes, http.MethodPut, rolesURL("administration", "default"),
		map[string]any{"name": newRole, "new_name": "risk_reviewer"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("colliding rename status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
}

/* ── delete ────────────────────────────────────────────────────────────── */

// A role that still holds members is refused, WITH the count. The count is the
// fact the operator acts on; "no" on its own tells them nothing.
func TestAdminRoleDeleteRefusesARoleWithMembersAndReportsHowMany(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	if recorder := adminDo(t, matrix, http.MethodPost,
		"/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d", recorder.Code)
	}
	seedMember(t, pool, "one@example.com", sharedProjectID, newRole)
	seedMember(t, pool, "two@example.com", supportProjectID, newRole)

	recorder := adminDo(t, writes, http.MethodDelete, rolesURL("administration", "default"),
		map[string]any{"name": newRole})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("delete with members status = %d, want 409 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if members, ok := body["members"].(float64); !ok || int(members) != 2 {
		t.Fatalf("members = %v, want 2 — the count is the part the operator acts on", body["members"])
	}

	// AND NOTHING WAS DELETED. auth_core__project_user_role cascades from the
	// role, so a delete that got through would have stripped both users of it
	// silently.
	if !centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("a refused delete removed the central role anyway")
	}
	if got := projectsWithRole(t, pool, newRole); len(got) == 0 {
		t.Fatal("a refused delete removed the project copies")
	}
	var assignments int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*) FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE role.name = $1`, newRole).Scan(&assignments); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if assignments != 2 {
		t.Fatalf("assignments after a refused delete = %d, want 2", assignments)
	}
}

// The project-scope half of the same guard: a PROJECT role with members is
// refused on the public/support tabs too.
func TestAdminRoleDeleteRefusesAProjectRoleWithMembers(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("public", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	seedMember(t, pool, "public-member@example.com", publicProjectID, newRole)

	recorder := adminDo(t, writes, http.MethodDelete, rolesURL("public", "default"),
		map[string]any{"name": newRole})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
	if got := projectsWithRole(t, pool, newRole); len(got) != 1 {
		t.Fatalf("a refused project delete changed the role rows: %v", got)
	}
}

// The successful delete: the central row, its grants and its per-project copies
// all go, and nothing else does.
func TestAdminRoleDeleteRemovesTheRoleItsGrantsAndItsProjectCopies(t *testing.T) {
	pool, writes, matrix := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	before := readMatrix(t, matrix, "administration", "default")
	if recorder := adminDo(t, matrix, http.MethodPut, "/admin/permissions/administration/default",
		setCell(t, before, "models.alpha.view", newRole, true)); recorder.Code != http.StatusOK {
		t.Fatalf("grant status = %d", recorder.Code)
	}
	if recorder := adminDo(t, matrix, http.MethodPost,
		"/admin/permissions/administration/default", nil); recorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d", recorder.Code)
	}

	recorder := adminDo(t, writes, http.MethodDelete, rolesURL("administration", "default"),
		map[string]any{"name": newRole})
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("the delete answered 200 and left the role")
	}
	if got := grantsSQL(t, pool, "default", newRole); len(got) != 0 {
		t.Fatalf("the role's grants survived it: %v", got)
	}
	if got := projectsWithRole(t, pool, newRole); len(got) != 0 {
		t.Fatalf("the per-project copies survived in %v, so every project still offers a role the platform no longer defines", got)
	}
	// The rest of the matrix is untouched — the delete is scoped to one role.
	after := readMatrix(t, matrix, "administration", "default")
	for _, role := range []string{"system", "admin", "editor", "viewer"} {
		if !contains(after.roles(t), role) {
			t.Errorf("deleting one role removed %q from the matrix", role)
		}
	}
	if !after.granted(t, "models.alpha.view", "admin") {
		t.Fatal("deleting one role revoked another role's grant")
	}
}

func TestAdminRoleDeleteRefusesEveryBuiltInRole(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)

	for _, name := range []string{"system", "admin", "editor", "viewer"} {
		recorder := adminDo(t, writes, http.MethodDelete, rolesURL("administration", "default"),
			map[string]any{"name": name})
		if recorder.Code != http.StatusConflict {
			t.Errorf("delete %q status = %d, want 409 (body %s)", name, recorder.Code, recorder.Body.String())
		}
		if !centralRoleExists(t, pool, "default", name) {
			t.Fatalf("the built-in role %q was deleted anyway", name)
		}
	}
	// `super_admin` lives in the administration mode in this fixture.
	recorder := adminDo(t, writes, http.MethodDelete, rolesURL("administration", "administration"),
		map[string]any{"name": "super_admin"})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("delete super_admin status = %d, want 409", recorder.Code)
	}
	if !centralRoleExists(t, pool, "administration", "super_admin") {
		t.Fatal("super_admin was deleted")
	}
}

// An intermediary that drops the DELETE body must not turn a delete into "no
// name given". The query parameter is the documented fallback.
func TestAdminRoleDeleteAcceptsTheNameFromTheQuery(t *testing.T) {
	pool, writes, _ := newRoleWriteEnvironment(t)

	if recorder := adminDo(t, writes, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}

	recorder := adminDo(t, writes, http.MethodDelete,
		rolesURL("administration", "default")+"?name="+newRole, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete via the query status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("the query-parameter delete answered 200 and removed nothing")
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// Each write has its OWN permission, and holding one does not confer the
// others. A single combined check would let a caller trusted to add a role
// delete one.
func TestRoleWritesAreGatedPerPermissionAndNothingMoves(t *testing.T) {
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))
	principal := &auth.User{ID: "1", UserID: "1"}

	// Give the caller the CREATE permission and nothing else, then try all
	// three writes with the gate each route really carries.
	holder := grantingResolver(admin.RolesCreatePermission)
	gateFor := func(permission string) func(http.Handler) http.Handler {
		return apimw.RequireCentralPermissions(holder, auth.PermissionModeAdministration, permission)
	}
	handler := admin.NewHandler(pool)

	create := roleWriteRouter(handler, gateFor(admin.RolesCreatePermission), principal)
	if recorder := adminDo(t, create, http.MethodPost, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusCreated {
		t.Fatalf("the caller HOLDS create but was refused: %d (body %s)",
			recorder.Code, recorder.Body.String())
	}

	rename := roleWriteRouter(handler, gateFor(admin.RolesEditPermission), principal)
	if recorder := adminDo(t, rename, http.MethodPut, rolesURL("administration", "default"),
		map[string]any{"name": newRole, "new_name": "risk_reviewer"}); recorder.Code != http.StatusForbidden {
		t.Fatalf("rename without %s = %d, want 403", admin.RolesEditPermission, recorder.Code)
	}
	remove := roleWriteRouter(handler, gateFor(admin.RolesDeletePermission), principal)
	if recorder := adminDo(t, remove, http.MethodDelete, rolesURL("administration", "default"),
		map[string]any{"name": newRole}); recorder.Code != http.StatusForbidden {
		t.Fatalf("delete without %s = %d, want 403", admin.RolesDeletePermission, recorder.Code)
	}

	// The two refusals changed nothing.
	if centralRoleExists(t, pool, "default", "risk_reviewer") {
		t.Fatal("a refused rename renamed the role anyway")
	}
	if !centralRoleExists(t, pool, "default", newRole) {
		t.Fatal("a refused delete removed the role anyway")
	}
}

/* ── small helpers ─────────────────────────────────────────────────────── */

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
