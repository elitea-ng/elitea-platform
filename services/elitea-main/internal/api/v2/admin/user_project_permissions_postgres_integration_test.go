package admin_test

// #255 acceptance for `/admin/user_project_permissions/administration`.
//
// This endpoint writes across EVERY personal project (or every shared project)
// at once, so the failure it must not have is a save that lands on the project
// the GET happens to read and on none of the others: the operator re-reads,
// sees their change, and the rest of the estate silently keeps the old matrix.
// Every write below is therefore re-read through the product's own GET AND
// checked with SQL against a project the GET never looks at.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// The roles fixture's personal project is `project_user_9` (id 3). A SECOND one
// is added here: with only one, a write that touched just the project the GET
// reads would pass every assertion.
const secondPersonalProjectID = 6

func userProjectPermissionsRouter(handler *admin.Handler) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(
				auth.ContextWithUser(r.Context(), auth.User{ID: "1", UserID: "1"})))
		})
	})
	router.Get("/admin/user_project_permissions/administration", handler.UserProjectPermissions)
	router.Put("/admin/user_project_permissions/administration", handler.UserProjectPermissionsSave)
	return router
}

func newUserProjectPermissionsEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))

	ctx := context.Background()
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (%d, 'project_user_10', 1, '{}', true)`, secondPersonalProjectID),
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role (project_id, name) VALUES
			(%[1]d, 'admin'), (%[1]d, 'editor'), (%[1]d, 'viewer')`, secondPersonalProjectID),
		// The second personal project starts with its OWN overrides, different
		// from the first, so "the write reached both" cannot be satisfied by
		// two projects that already agreed.
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, 'models.alpha.view'
			FROM public.auth_core__project_role role
			WHERE role.project_id = %d AND role.name = 'editor'`, secondPersonalProjectID),
		// The platform's own role holds something, so a save that touched it
		// would be visible rather than a no-op that looks like success.
		fmt.Sprintf(`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, 'models.alpha.view'
			FROM public.auth_core__project_role role
			WHERE role.project_id = %d AND role.name = 'system'`, personalProjectID),
		// One member of each personal project, for the append_user_role case.
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(9001, 'upp-member@autotest.local', 'UPP Member')`,
		fmt.Sprintf(`INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
			SELECT role.project_id, 9001, role.id
			FROM public.auth_core__project_role role
			WHERE role.project_id IN (%d, %d) AND role.name = 'viewer'`,
			personalProjectID, secondPersonalProjectID),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return pool, userProjectPermissionsRouter(admin.NewHandler(pool))
}

func readRoleMap(t *testing.T, router chi.Router) map[string][]string {
	t.Helper()
	recorder := adminDo(t, router, http.MethodGet, "/admin/user_project_permissions/administration", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	roleMap := map[string][]string{}
	decodeJSONBody(t, recorder.Body.Bytes(), &roleMap)
	return roleMap
}

// storedProjectGrants reads one project's overrides straight from the table.
func storedProjectGrants(t *testing.T, pool *pgxpool.Pool, projectID int, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT override.permission
FROM public.auth_core__project_role_permission override
JOIN public.auth_core__project_role role ON role.id = override.role_id
WHERE role.project_id = $1 AND role.name = $2
ORDER BY 1`, projectID, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	permissions := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatal(err)
		}
		permissions = append(permissions, permission)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(permissions)
	return permissions
}

/* ── read ──────────────────────────────────────────────────────────────── */

func TestUserProjectPermissionsReadsThePersonalProjectMatrix(t *testing.T) {
	_, router := newUserProjectPermissionsEnvironment(t)

	roleMap := readRoleMap(t, router)

	// The fixture grants `models.gamma.delete` to the personal project's admin
	// and viewer roles and nothing to its editor. A read that fell back to the
	// central `default` matrix — as the reference's admin RPC does — would
	// report editor with two permissions and admin with three.
	if got := roleMap["admin"]; !equalStrings(got, []string{"models.gamma.delete"}) {
		t.Fatalf("admin = %v, want [models.gamma.delete]", got)
	}
	if got := roleMap["editor"]; len(got) != 0 {
		t.Fatalf("editor = %v, want no permissions", got)
	}
}

func TestUserProjectPermissionsOldFormatReturnsTheMatrix(t *testing.T) {
	_, router := newUserProjectPermissionsEnvironment(t)

	recorder := adminDo(t, router, http.MethodGet,
		"/admin/user_project_permissions/administration?old_format", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET ?old_format status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var matrix permissionMatrixBody
	decodeJSONBody(t, recorder.Body.Bytes(), &matrix)
	if matrix.Total != len(matrix.Rows) || matrix.Total == 0 {
		t.Fatalf("matrix total = %d with %d rows", matrix.Total, len(matrix.Rows))
	}
	if !matrix.granted(t, "models.gamma.delete", "admin") {
		t.Fatal("the matrix does not report the personal project's admin grant")
	}
	// A permission held by nobody in this project is still a ROW, or it could
	// never be granted through the editor.
	if matrix.granted(t, "models.alpha.view", "admin") {
		t.Fatal("models.alpha.view is reported as granted to the personal project's admin role")
	}
}

/* ── write ─────────────────────────────────────────────────────────────── */

func TestUserProjectPermissionsSaveReachesEveryPersonalProject(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration",
		map[string][]string{
			"admin":  {"models.alpha.view", "models.beta.view"},
			"editor": {"models.alpha.view"},
			"viewer": {},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// RE-READ through the product's own GET (which reads the FIRST personal
	// project)…
	roleMap := readRoleMap(t, router)
	if got := roleMap["admin"]; !equalStrings(got, []string{"models.alpha.view", "models.beta.view"}) {
		t.Fatalf("admin = %v after the save, want [models.alpha.view models.beta.view]", got)
	}
	if got := roleMap["viewer"]; len(got) != 0 {
		t.Fatalf("viewer = %v after being saved with an empty set, want none", got)
	}

	// …and THE assertion the GET cannot make: the SECOND personal project,
	// which the read never looks at, moved too — including losing the
	// `models.alpha.view` its editor started with being replaced by the
	// submitted set rather than merged into it.
	if got := storedProjectGrants(t, pool, secondPersonalProjectID, "admin"); !equalStrings(
		got, []string{"models.alpha.view", "models.beta.view"}) {
		t.Fatalf("second personal project admin = %v, want the saved set", got)
	}
	if got := storedProjectGrants(t, pool, secondPersonalProjectID, "editor"); !equalStrings(
		got, []string{"models.alpha.view"}) {
		t.Fatalf("second personal project editor = %v, want [models.alpha.view]", got)
	}

	// Shared projects are NOT personal projects and must be untouched.
	if got := storedProjectGrants(t, pool, sharedProjectID, "admin"); !equalStrings(
		got, []string{"models.gamma.delete"}) {
		t.Fatalf("shared project admin = %v, want its own untouched override", got)
	}
}

func TestUserProjectPermissionsSaveAcceptsTheMatrixBody(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration",
		[]map[string]any{
			{"name": "models.alpha.view", "admin": true, "editor": false, "viewer": false},
			{"name": "models.gamma.delete", "admin": false, "editor": false, "viewer": false},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if got := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(
		got, []string{"models.alpha.view"}) {
		t.Fatalf("admin = %v after the matrix save, want [models.alpha.view]", got)
	}
	// The row that turned everything off must actually revoke: the fixture gave
	// viewer `models.gamma.delete`.
	if got := storedProjectGrants(t, pool, personalProjectID, "viewer"); len(got) != 0 {
		t.Fatalf("viewer = %v after the matrix save, want none", got)
	}
}

func TestUserProjectPermissionsSaveRejectsUnknownNamesAndWritesNothing(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)
	before := storedProjectGrants(t, pool, personalProjectID, "admin")

	cases := map[string]any{
		"unknown role":       map[string][]string{"wizard": {"models.alpha.view"}},
		"unknown permission": map[string][]string{"admin": {"models.nonexistent.view"}},
		// `system` is DROPPED rather than rejected, so a body naming only it
		// carries no writable role at all. It is still a 400 that writes
		// nothing — for the right reason now.
		"only the system role": map[string][]string{"system": {"models.alpha.view"}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := adminDo(t, router, http.MethodPut,
				"/admin/user_project_permissions/administration", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}

	if after := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(after, before) {
		t.Fatalf("admin grants moved from %v to %v across three rejected saves", before, after)
	}
}

func TestUserProjectPermissionsSaveCreatesRolesOnRequest(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration?create_role_if_not_exist",
		map[string][]string{"wizard": {"models.alpha.view"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	for _, projectID := range []int{personalProjectID, secondPersonalProjectID} {
		if got := storedProjectGrants(t, pool, projectID, "wizard"); !equalStrings(
			got, []string{"models.alpha.view"}) {
			t.Fatalf("project %d wizard = %v, want [models.alpha.view]", projectID, got)
		}
	}
}

func TestUserProjectPermissionsSaveAppendsTheRoleToExistingMembers(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration?append_user_role",
		map[string][]string{"editor": {"models.alpha.view"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	for _, projectID := range []int{personalProjectID, secondPersonalProjectID} {
		var roles int
		if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*)
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = 9001 AND role.name = 'editor'`,
			projectID).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if roles != 1 {
			t.Fatalf("member holds the editor role %d times in project %d, want 1", roles, projectID)
		}
	}

	// It only ADDS: the role the member already held is still there.
	var kept int
	if err := pool.QueryRow(context.Background(), `
SELECT COUNT(*)
FROM public.auth_core__project_user_role assignment
JOIN public.auth_core__project_role role ON role.id = assignment.role_id
WHERE assignment.user_id = 9001 AND role.name = 'viewer'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Fatalf("member holds the viewer role in %d projects after the append, want 2", kept)
	}
}

func TestUserProjectPermissionsSaveTargetsTeamProjectsOnRequest(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)
	personalBefore := storedProjectGrants(t, pool, personalProjectID, "admin")

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration?team_projects&create_role_if_not_exist",
		map[string][]string{"admin": {"models.beta.edit"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The shared projects moved — including project 5, whose name matches the
	// personal-project pattern only if `_` is read as a SQL wildcard.
	for _, projectID := range []int{sharedProjectID, supportProjectID, wildcardNameProjectID} {
		if got := storedProjectGrants(t, pool, projectID, "admin"); !equalStrings(
			got, []string{"models.beta.edit"}) {
			t.Fatalf("shared project %d admin = %v, want [models.beta.edit]", projectID, got)
		}
	}
	// The public project is excluded, and so is every personal one.
	if got := storedProjectGrants(t, pool, publicProjectID, "admin"); len(got) != 0 {
		t.Fatalf("the public project's admin grants = %v, want untouched (none)", got)
	}
	if got := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(got, personalBefore) {
		t.Fatalf("personal project admin = %v after a team-projects save, want %v", got, personalBefore)
	}
}

// TestUserProjectPermissionsRoundTripsItsOwnBody is the case the first
// implementation could not pass: it rejected any submission naming the `system`
// role, and its own GET emits one in BOTH shapes — every project the legacy
// runtime creates has a system role (projects/utils/project_steps.py). So the
// permission editor could load the matrix and never save it.
func TestUserProjectPermissionsRoundTripsItsOwnBody(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	roleMap := readRoleMap(t, router)
	if _, ok := roleMap["system"]; !ok {
		t.Fatalf("the GET body carries no system role, so this test proves nothing: %v", roleMap)
	}
	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration", roleMap)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT of the GET's own role map = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}

	matrixRecorder := adminDo(t, router, http.MethodGet,
		"/admin/user_project_permissions/administration?old_format", nil)
	var matrix permissionMatrixBody
	decodeJSONBody(t, matrixRecorder.Body.Bytes(), &matrix)
	if _, ok := matrix.row("models.gamma.delete"); !ok {
		t.Fatal("the matrix is missing a row the fixture grants")
	}
	if recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration", matrix.Rows); recorder.Code != http.StatusOK {
		t.Fatalf("PUT of the GET's own matrix = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}

	// Round-tripping is not a licence to write the platform's own role: the
	// system grants are exactly as they were.
	if got := storedProjectGrants(t, pool, personalProjectID, "system"); !equalStrings(
		got, []string{"models.alpha.view"}) {
		t.Fatalf("system grants = %v after two round trips, want them untouched", got)
	}
	// …and the roles that WERE submitted still hold what the body said.
	if got := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(
		got, []string{"models.gamma.delete"}) {
		t.Fatalf("admin grants = %v after a round trip, want [models.gamma.delete]", got)
	}
}

// TestUserProjectPermissionsSaveRefusesARolePartOfTheEstateLacks pins the
// per-(project, role) check. "Some project defines it" is not enough: the write
// matches role rows per project, so a role present in one personal project and
// absent from another produced a save that reached half the estate and reported
// success for all of it.
func TestUserProjectPermissionsSaveRefusesARolePartOfTheEstateLacks(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM public.auth_core__project_role WHERE project_id = $1 AND name = 'editor'`,
		secondPersonalProjectID); err != nil {
		t.Fatal(err)
	}
	before := storedProjectGrants(t, pool, personalProjectID, "editor")

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration",
		map[string][]string{"editor": {"models.alpha.view"}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if after := storedProjectGrants(t, pool, personalProjectID, "editor"); !equalStrings(after, before) {
		t.Fatalf("editor grants moved from %v to %v on a refused save", before, after)
	}

	// …and `?create_role_if_not_exist` is the documented way through, so the
	// refusal is a prompt rather than a dead end.
	if allowed := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration?create_role_if_not_exist",
		map[string][]string{"editor": {"models.alpha.view"}}); allowed.Code != http.StatusOK {
		t.Fatalf("PUT with ?create_role_if_not_exist = %d, want 200 (body %s)",
			allowed.Code, allowed.Body.String())
	}
	for _, projectID := range []int{personalProjectID, secondPersonalProjectID} {
		if got := storedProjectGrants(t, pool, projectID, "editor"); !equalStrings(
			got, []string{"models.alpha.view"}) {
			t.Fatalf("project %d editor = %v, want [models.alpha.view]", projectID, got)
		}
	}
}

// TestUserProjectPermissionsPartialMatrixRevokesOnlyItsOwnRows pins the
// revocation bound. A client that submits a filtered or paged view of the
// matrix must not silently revoke every permission it did not mention — the
// same rule roles.go's diffGrants applies to the identically-shaped body.
func TestUserProjectPermissionsPartialMatrixRevokesOnlyItsOwnRows(t *testing.T) {
	pool, router := newUserProjectPermissionsEnvironment(t)

	// The fixture's admin role holds models.gamma.delete, which the submitted
	// row set does not mention.
	recorder := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration",
		[]map[string]any{
			{"name": "models.alpha.view", "admin": true},
		})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if got := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(
		got, []string{"models.alpha.view", "models.gamma.delete"}) {
		t.Fatalf("admin = %v after a one-row matrix, want the new grant AND the unmentioned one", got)
	}

	// The ROLE-MAP shape keeps its replace-the-whole-set meaning: there the
	// submitted list IS the role's permission set, which is what the reference's
	// delete-then-insert means and what the estate-wide editor sends.
	if replace := adminDo(t, router, http.MethodPut,
		"/admin/user_project_permissions/administration",
		map[string][]string{"admin": {"models.alpha.view"}}); replace.Code != http.StatusOK {
		t.Fatalf("role-map PUT status = %d, want 200 (body %s)", replace.Code, replace.Body.String())
	}
	if got := storedProjectGrants(t, pool, personalProjectID, "admin"); !equalStrings(
		got, []string{"models.alpha.view"}) {
		t.Fatalf("admin = %v after a role-map save, want only the submitted set", got)
	}
}
