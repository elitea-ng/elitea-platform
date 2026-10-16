package admin_test

// Unit A14 acceptance for the admin Roles surface (the permission matrix).
//
// This is a security-relevant surface: a mistake here grants privilege. So the
// tests below are written against the three ways this endpoint could lie —
//
//  1. Reporting a save that did not happen. Every write is followed by a
//     RE-READ through the product's own `GET /admin/permissions/{scope}/{mode}`,
//     and where the read could paper over a miss (the inherited-matrix case) by
//     SQL as well. A 200 asserts nothing on its own; the pre-A14 build had no
//     route here at all, and #130/#180 show what the easy wrong fix looks like.
//  2. Showing the wrong scope's data. The pre-A14 GET ignored `{scope}`
//     entirely, so `public` and `support` rendered the CENTRAL matrix. That is
//     asserted against directly.
//  3. Letting the wrong caller through. The positive case is not enough:
//     `TestPermissionMatrixRefusesACallerWithoutTheEditPermission` mounts the
//     REAL route middleware and proves a viewer is refused AND that nothing
//     moved.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

/* ── harness ───────────────────────────────────────────────────────────── */

// permissionRow is one row of the matrix. The role columns are dynamic, so the
// decoder keeps them as raw values and `granted` reads them.
type permissionRow map[string]any

type permissionMatrixBody struct {
	Total int             `json:"total"`
	Rows  []permissionRow `json:"rows"`
}

func (m permissionMatrixBody) row(name string) (permissionRow, bool) {
	for _, row := range m.Rows {
		if row["name"] == name {
			return row, true
		}
	}
	return nil, false
}

func (m permissionMatrixBody) granted(t *testing.T, permission, role string) bool {
	t.Helper()
	row, ok := m.row(permission)
	if !ok {
		t.Fatalf("matrix has no row for %q (rows: %d)", permission, len(m.Rows))
	}
	value, ok := row[role]
	if !ok {
		t.Fatalf("matrix row %q has no column for role %q (columns: %v)", permission, role, columnsOf(row))
	}
	flag, ok := value.(bool)
	if !ok {
		t.Fatalf("matrix cell (%q, %q) = %#v, want a boolean", permission, role, value)
	}
	return flag
}

func (m permissionMatrixBody) roles(t *testing.T) []string {
	t.Helper()
	if len(m.Rows) == 0 {
		return nil
	}
	return columnsOf(m.Rows[0])
}

func columnsOf(row permissionRow) []string {
	columns := []string{}
	for key := range row {
		if key != "name" {
			columns = append(columns, key)
		}
	}
	sort.Strings(columns)
	return columns
}

// rolesRouter mounts the three Roles routes exactly as internal/api/router.go
// does. `gate` is the route-level middleware; passing nil mounts them bare, the
// way the users test does, so a case that is not ABOUT authorisation is not
// obscured by it.
func rolesRouter(handler *admin.Handler, gate func(http.Handler) http.Handler, principal *auth.User) chi.Router {
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
	router.With(gate).Get("/admin/permissions/{scope}/{mode}", handler.AdminPermissions)
	router.With(gate).Put("/admin/permissions/{scope}/{mode}", handler.AdminPermissionsSave)
	router.With(gate).Post("/admin/permissions/{scope}/{mode}", handler.AdminPermissionsSync)
	return router
}

// readMatrix re-reads through the SAME GET handler the Roles page calls. This
// is the assertion an unwired or no-op write cannot pass.
func readMatrix(t *testing.T, router chi.Router, scope, mode string) permissionMatrixBody {
	t.Helper()
	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/"+scope+"/"+mode, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s/%s status = %d, want 200 (body %s)", scope, mode, recorder.Code, recorder.Body.String())
	}
	var body permissionMatrixBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode matrix %q: %v", recorder.Body.String(), err)
	}
	if body.Total != len(body.Rows) {
		t.Fatalf("total = %d but %d rows were returned", body.Total, len(body.Rows))
	}
	return body
}

// setCell returns the matrix with one cell flipped, ready to PUT back — the
// exact round-trip the page performs.
func setCell(t *testing.T, matrix permissionMatrixBody, permission, role string, value bool) []permissionRow {
	t.Helper()
	if _, ok := matrix.row(permission); !ok {
		t.Fatalf("cannot set %q: no such row", permission)
	}
	rows := make([]permissionRow, 0, len(matrix.Rows))
	for _, row := range matrix.Rows {
		copied := permissionRow{}
		for key, cell := range row {
			copied[key] = cell
		}
		if copied["name"] == permission {
			copied[role] = value
		}
		rows = append(rows, copied)
	}
	return rows
}

func grantsSQL(t *testing.T, pool *pgxpool.Pool, mode, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT grant_row.permission
FROM public.auth_core__role role
JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = role.id
WHERE role.mode = $1 AND role.name = $2
ORDER BY 1`, mode, role)
	if err != nil {
		t.Fatalf("read central grants: %v", err)
	}
	defer rows.Close()
	return collectStrings(t, rows)
}

func projectGrantsSQL(t *testing.T, pool *pgxpool.Pool, projectID int, role string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT override.permission
FROM public.auth_core__project_role role
JOIN public.auth_core__project_role_permission override ON override.role_id = role.id
WHERE role.project_id = $1 AND role.name = $2
ORDER BY 1`, projectID, role)
	if err != nil {
		t.Fatalf("read project grants: %v", err)
	}
	defer rows.Close()
	return collectStrings(t, rows)
}

func collectStrings(t *testing.T, rows pgx.Rows) []string {
	t.Helper()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return values
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// Fixture project ids. `publicProjectID` is what AI_PROJECT_ID is pointed at.
const (
	publicProjectID   = 1
	sharedProjectID   = 2
	personalProjectID = 3
	supportProjectID  = 4
	// wildcardNameProjectID is a SHARED project whose name matches the personal
	// project pattern only when `_` is read as a wildcard.
	wildcardNameProjectID = 5
)

func newRolesPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_admin_roles_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	for _, projection := range []string{
		dbschema.AuthCoreBaselineSQLCProjection,
		dbschema.CentryProjectsBaselineSQLCProjection,
	} {
		if _, err := pool.Exec(ctx, projection); err != nil {
			t.Fatalf("apply baseline projection: %v", err)
		}
	}
	// centry.platform_config carries the support project id the support scope
	// resolves. No baseline projection holds it, so it is created here the same
	// way internal/api/v2/eliteacore's platform-flags test creates it. Without
	// the table the support scope reads an ERROR rather than an empty store,
	// which is a different answer — and a fixture that cannot tell those two
	// apart cannot test either one.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE centry.platform_config (
			section    text NOT NULL,
			key        text NOT NULL,
			value      jsonb NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now(),
			updated_by text,
			PRIMARY KEY (section, key)
		);`); err != nil {
		t.Fatalf("create centry.platform_config: %v", err)
	}
	return pool
}

// prepareRolesFixture seeds a deployment small enough to reason about exactly.
//
//	central `default`      : system(all 4) admin(3) editor(2) viewer(1)
//	central `administration`: super_admin, admin, editor, viewer with a few grants
//	project 1 (public)     : roles, NO overrides  → inherits central default
//	project 2 (shared)     : roles + its own overrides
//	project 3 (personal)   : named `project_user_9`, must be skipped by sync
//	project 4 (support)    : roles, NO overrides
//
// `models.orphan.permission` is granted to a PROJECT role and to no central role
// at all. It exists to pin the second pre-A14 defect: a permission nobody holds
// centrally must still be a ROW, or it can never be granted.
func prepareRolesFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, statement := range rolesFixtureStatements() {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func rolesFixtureStatements() []string {
	return []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('system', 'default'), ('admin', 'default'),
			('editor', 'default'), ('viewer', 'default'),
			('system', 'administration'), ('super_admin', 'administration'),
			('admin', 'administration'), ('editor', 'administration'),
			('viewer', 'administration')`,

		// Central `default` grants.
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission
			FROM public.auth_core__role role,
			     (VALUES ('models.alpha.view'), ('models.beta.view'),
			             ('models.beta.edit'), ('models.gamma.delete')) AS grants(permission)
			WHERE role.mode = 'default' AND role.name = 'system'`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission
			FROM public.auth_core__role role,
			     (VALUES ('models.alpha.view'), ('models.beta.view'), ('models.beta.edit')) AS grants(permission)
			WHERE role.mode = 'default' AND role.name = 'admin'`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission
			FROM public.auth_core__role role,
			     (VALUES ('models.alpha.view'), ('models.beta.view')) AS grants(permission)
			WHERE role.mode = 'default' AND role.name = 'editor'`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, 'models.alpha.view'
			FROM public.auth_core__role role
			WHERE role.mode = 'default' AND role.name = 'viewer'`,

		// Central `administration` grants.
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, permission
			FROM public.auth_core__role role,
			     (VALUES ('admin.auth.users'), ('configuration.roles.permissions.view'),
			             ('configuration.roles.permissions.edit')) AS grants(permission)
			WHERE role.mode = 'administration' AND role.name IN ('system', 'super_admin', 'admin')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, 'configuration.roles.permissions.view'
			FROM public.auth_core__role role
			WHERE role.mode = 'administration' AND role.name IN ('editor', 'viewer')`,
		// The SAME permission on the SAME role name in BOTH modes. Without an
		// overlap, a write that forgot its `role.mode` predicate would be
		// invisible: it would delete rows that do not exist.
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT role.id, 'models.beta.edit'
			FROM public.auth_core__role role
			WHERE role.mode = 'administration' AND role.name = 'admin'`,

		`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success) VALUES
			(1, 'promptlib_public', 1, '{}', true),
			(2, 'a14-shared',       1, '{}', true),
			(3, 'project_user_9',   1, '{}', true),
			(4, 'a14-support',      1, '{}', true),
			-- A SHARED project whose name matches 'project_user_%' only if the
			-- underscores are treated as SQL wildcards. It must be synced.
			(5, 'projectAuserB-team', 1, '{}', true)`,

		// Project 5 deliberately gets NO roles: the sync has to create them.
		`INSERT INTO public.auth_core__project_role (project_id, name)
			SELECT project.id, role.name
			FROM centry.project project,
			     (VALUES ('system'), ('admin'), ('editor'), ('viewer')) AS role(name)
			WHERE project.id <> 5`,

		// Project 2 has its OWN overrides — deliberately different from central,
		// so a handler that fell back to central would be caught.
		`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, 'models.gamma.delete'
			FROM public.auth_core__project_role role
			WHERE role.project_id = 2 AND role.name = 'admin'`,
		// Granted to a project role and to NO central role. The catalogue must
		// still list it — otherwise it can never be granted centrally again.
		`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, 'models.orphan.permission'
			FROM public.auth_core__project_role role
			WHERE role.project_id = 2 AND role.name = 'editor'`,
		// A stale grant on the personal project, to prove the sync leaves it alone.
		// It is on the SAME role and permission project 2's admin override uses,
		// so a project-scoped write that forgot its `project_id` predicate would
		// take this row with it.
		`INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
			SELECT role.project_id, role.id, permission
			FROM public.auth_core__project_role role,
			     (VALUES ('models.gamma.delete')) AS grants(permission)
			WHERE role.project_id = 3 AND role.name IN ('viewer', 'admin')`,
	}
}

func newRolesEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))
	return pool, rolesRouter(admin.NewHandler(pool), nil, &auth.User{ID: "1", UserID: "1"})
}

/* ── read ──────────────────────────────────────────────────────────────── */

func TestPermissionMatrixListsPermissionsNobodyHoldsCentrally(t *testing.T) {
	_, router := newRolesEnvironment(t)

	matrix := readMatrix(t, router, "administration", "administration")

	// The pre-A14 read built its row set from the grants themselves, so a
	// permission held by no central role simply had no row — and a matrix you
	// cannot see a permission in is a matrix you cannot grant it in.
	if _, ok := matrix.row("models.orphan.permission"); !ok {
		t.Fatalf("catalogue omits a permission held only by a project role; rows = %d", len(matrix.Rows))
	}
	if matrix.granted(t, "models.orphan.permission", "admin") {
		t.Fatalf("models.orphan.permission is reported as granted to the central admin role")
	}
	// …and the row is grantable: the PUT accepts it (covered below), which it
	// would reject as an unknown permission if the catalogue were narrower.
}

func TestPermissionMatrixSeparatesTheFourScopes(t *testing.T) {
	_, router := newRolesEnvironment(t)

	administration := readMatrix(t, router, "administration", "administration")
	standard := readMatrix(t, router, "administration", "default")
	public := readMatrix(t, router, "public", "default")

	// `administration` mode has a super_admin column; `default` does not. The
	// pre-A14 handler ignored neither of these — it ignored SCOPE — but the mode
	// difference is the cheapest proof the two reads are not the same query.
	if !contains(administration.roles(t), "super_admin") {
		t.Fatalf("administration matrix has no super_admin column: %v", administration.roles(t))
	}
	if contains(standard.roles(t), "super_admin") {
		t.Fatalf("default-mode matrix has a super_admin column: %v", standard.roles(t))
	}

	// The public project has no overrides, so it INHERITS central default — the
	// pylon fallback. It must therefore match `standard`, not `administration`.
	if public.granted(t, "admin.auth.users", "admin") {
		t.Fatalf("public scope reported the ADMINISTRATION matrix: admin.auth.users is granted")
	}
	if !public.granted(t, "models.beta.edit", "admin") {
		t.Fatalf("public scope did not inherit the central default matrix")
	}
}

func TestPermissionMatrixReadsAProjectsOwnOverrides(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	// Point the "public" scope at project 2, which HAS overrides of its own.
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(sharedProjectID))

	matrix := readMatrix(t, router, "public", "default")

	if !matrix.granted(t, "models.gamma.delete", "admin") {
		t.Fatalf("project override models.gamma.delete/admin is missing from the matrix")
	}
	// Central default grants admin `models.beta.edit`; the project does not.
	// A handler that fell back to central despite the overrides existing — or
	// that ignored scope altogether — would report true here.
	if matrix.granted(t, "models.beta.edit", "admin") {
		t.Fatalf("matrix leaked the CENTRAL grant models.beta.edit into an overridden project")
	}
	if got := projectGrantsSQL(t, pool, sharedProjectID, "admin"); len(got) != 1 || got[0] != "models.gamma.delete" {
		t.Fatalf("fixture drifted: project %d admin grants = %v", sharedProjectID, got)
	}
}

func TestPermissionMatrixRejectsAnUnknownScope(t *testing.T) {
	_, router := newRolesEnvironment(t)

	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/wat/default", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET unknown scope status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestSupportScopeReportsThatItIsNotConfigured(t *testing.T) {
	_, router := newRolesEnvironment(t)
	t.Setenv("SUPPORT_PROJECT_ID", "")

	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/support/default", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("support GET status = %d, want 404 when SUPPORT_PROJECT_ID is unset", recorder.Code)
	}
	// The reason has to be legible: the page renders the tab unavailable WITH it.
	if body := recorder.Body.String(); !strings.Contains(body, "SUPPORT_PROJECT_ID") {
		t.Fatalf("404 body %q does not name the missing setting", body)
	}
}

func TestSupportScopeReadsItsOwnProjectOnceConfigured(t *testing.T) {
	_, router := newRolesEnvironment(t)
	t.Setenv("SUPPORT_PROJECT_ID", fmt.Sprint(supportProjectID))

	matrix := readMatrix(t, router, "support", "default")
	if !matrix.granted(t, "models.alpha.view", "viewer") {
		t.Fatalf("support scope did not inherit the central default matrix")
	}
	if matrix.granted(t, "admin.auth.users", "admin") {
		t.Fatalf("support scope reported the ADMINISTRATION matrix")
	}
}

// The stored id is the source of truth, not the environment. The hidden support
// project is created by internal/api/v2/supportassistant on the first support
// request after an operator enables the assistant, and its id is written to
// centry.platform_config — nobody types it into a chart. An environment variable
// that disagreed would make this tab edit the permissions of a project the
// assistant is not using, so the stored value has to win.
func TestSupportScopePrefersTheStoredProjectOverTheEnvironment(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	// A DIFFERENT, existing project, so a wrong answer is a wrong matrix rather
	// than a 404 that any bug could produce.
	t.Setenv("SUPPORT_PROJECT_ID", fmt.Sprint(sharedProjectID))
	storeSupportProjectID(t, pool, supportProjectID)

	matrix := readMatrix(t, router, "support", "default")
	if matrix.granted(t, "models.gamma.delete", "admin") {
		t.Fatalf("support scope read the ENVIRONMENT project %d, not the stored one", sharedProjectID)
	}
	if !matrix.granted(t, "models.alpha.view", "viewer") {
		t.Fatalf("support scope did not read the stored project %d", supportProjectID)
	}
}

// With nothing stored, the environment still answers: a deployment may pin the
// project by hand, and that path must not regress into a 404.
func TestSupportScopeFallsBackToTheEnvironmentWhenNothingIsStored(t *testing.T) {
	_, router := newRolesEnvironment(t)
	t.Setenv("SUPPORT_PROJECT_ID", fmt.Sprint(supportProjectID))

	if matrix := readMatrix(t, router, "support", "default"); !matrix.granted(t, "models.alpha.view", "viewer") {
		t.Fatal("support scope ignored SUPPORT_PROJECT_ID with no stored id")
	}
}

// A deployment whose centry.platform_config was never created is a SCHEMA GAP,
// not an outage. It must keep the 404 that names the variable an operator can
// set, rather than an unactionable 503 — the table is created by the bootstrap
// schema and by nothing in the versioned migration history, so its absence is a
// real shape and not a hypothetical one.
func TestSupportScopeStillNamesTheVariableWithNoPlatformConfigTable(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	t.Setenv("SUPPORT_PROJECT_ID", "")
	if _, err := pool.Exec(context.Background(), "DROP TABLE centry.platform_config"); err != nil {
		t.Fatalf("drop centry.platform_config: %v", err)
	}

	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/support/default", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("support GET status = %d, want 404 with no platform_config table (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "SUPPORT_PROJECT_ID") {
		t.Fatalf("404 body %q does not name the setting an operator can act on", body)
	}
}

// The environment fallback still applies with the table gone, so a deployment
// that pins the project by hand keeps working on a partially migrated schema.
func TestSupportScopeUsesTheEnvironmentWithNoPlatformConfigTable(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	t.Setenv("SUPPORT_PROJECT_ID", fmt.Sprint(supportProjectID))
	if _, err := pool.Exec(context.Background(), "DROP TABLE centry.platform_config"); err != nil {
		t.Fatalf("drop centry.platform_config: %v", err)
	}

	if matrix := readMatrix(t, router, "support", "default"); !matrix.granted(t, "models.alpha.view", "viewer") {
		t.Fatal("support scope refused the environment fallback with no platform_config table")
	}
}

// storeSupportProjectID writes the key the admin Features page and the support
// bootstrapper both write.
func storeSupportProjectID(t *testing.T, pool *pgxpool.Pool, projectID int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ($1, $2, $3::text::jsonb, now() AT TIME ZONE 'UTC')
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`,
		platformconfig.SectionSupportAssistant,
		platformconfig.KeySupportProjectID,
		fmt.Sprint(projectID)); err != nil {
		t.Fatalf("store support project id: %v", err)
	}
}

func TestPublicScopeRefusesAProjectThatDoesNotExist(t *testing.T) {
	_, router := newRolesEnvironment(t)
	t.Setenv("AI_PROJECT_ID", "9999")

	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/public/default", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a mis-set AI_PROJECT_ID (body %s)", recorder.Code, recorder.Body.String())
	}
}

/* ── write: PUT, central ───────────────────────────────────────────────── */

func TestPermissionMatrixSaveGrantsAndRevokes(t *testing.T) {
	pool, router := newRolesEnvironment(t)

	t.Run("grants a permission nobody held", func(t *testing.T) {
		before := readMatrix(t, router, "administration", "default")
		if before.granted(t, "models.orphan.permission", "editor") {
			t.Fatalf("fixture already grants models.orphan.permission to editor")
		}

		body := setCell(t, before, "models.orphan.permission", "editor", true)
		if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", body); recorder.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		after := readMatrix(t, router, "administration", "default")
		if !after.granted(t, "models.orphan.permission", "editor") {
			t.Fatalf("re-read does not show the grant")
		}
		if !contains(grantsSQL(t, pool, "default", "editor"), "models.orphan.permission") {
			t.Fatalf("auth_core__role_permission has no row for the grant")
		}
		// Nothing else moved.
		if !after.granted(t, "models.beta.view", "editor") {
			t.Fatalf("an unrelated grant was lost")
		}
	})

	t.Run("revokes a permission that was held", func(t *testing.T) {
		before := readMatrix(t, router, "administration", "default")
		if !before.granted(t, "models.beta.edit", "admin") {
			t.Fatalf("fixture does not grant models.beta.edit to admin")
		}

		body := setCell(t, before, "models.beta.edit", "admin", false)
		if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", body); recorder.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}

		if readMatrix(t, router, "administration", "default").granted(t, "models.beta.edit", "admin") {
			t.Fatalf("re-read still shows the revoked permission")
		}
		if contains(grantsSQL(t, pool, "default", "admin"), "models.beta.edit") {
			t.Fatalf("auth_core__role_permission still has the revoked row")
		}
		// The OTHER mode must be untouched — the write is keyed on mode, and a
		// missing `role.mode = $1` would silently edit administration too. The
		// fixture grants `models.beta.edit` to admin in BOTH modes precisely so
		// that a missing predicate deletes something real here.
		if !contains(grantsSQL(t, pool, "administration", "admin"), "models.beta.edit") {
			t.Fatalf("revoking in default mode also revoked it in administration mode")
		}
		if !contains(grantsSQL(t, pool, "administration", "admin"), "admin.auth.users") {
			t.Fatalf("the administration-mode matrix was collaterally edited")
		}
	})
}

func TestPermissionMatrixSaveNeverWritesTheSystemRole(t *testing.T) {
	pool, router := newRolesEnvironment(t)

	before := readMatrix(t, router, "administration", "default")
	if !before.granted(t, "models.gamma.delete", "system") {
		t.Fatalf("fixture does not grant models.gamma.delete to system")
	}

	// A forged body clearing every system cell. The UI disables that column;
	// the server is the gate, and a caller who bypasses the UI must not be able
	// to strip the platform's own role.
	body := setCell(t, before, "models.gamma.delete", "system", false)
	if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", body); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if !readMatrix(t, router, "administration", "default").granted(t, "models.gamma.delete", "system") {
		t.Fatalf("the system role lost a permission through the matrix")
	}
	if !contains(grantsSQL(t, pool, "default", "system"), "models.gamma.delete") {
		t.Fatalf("auth_core__role_permission lost the system grant")
	}

	// …and the other direction: the body cannot GRANT to system either. Only
	// checking revocation would miss a handler that filtered the delete but not
	// the insert.
	granting := setCell(t, readMatrix(t, router, "administration", "default"),
		"models.orphan.permission", "system", true)
	if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", granting); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if readMatrix(t, router, "administration", "default").granted(t, "models.orphan.permission", "system") {
		t.Fatalf("the system role gained a permission through the matrix")
	}
	if contains(grantsSQL(t, pool, "default", "system"), "models.orphan.permission") {
		t.Fatalf("auth_core__role_permission gained a system grant")
	}
}

// A body carrying only SOME of the matrix must not revoke everything it left
// out. The matrix has ~7 permissions; a filtered client view is a plausible
// caller, and "revoke everything unmentioned" is the destructive reading of a
// diff-based save.
func TestPermissionMatrixSaveOnlyRevokesWhatTheBodyCarried(t *testing.T) {
	pool, router := newRolesEnvironment(t)

	before := readMatrix(t, router, "administration", "default")
	if !before.granted(t, "models.beta.view", "editor") {
		t.Fatalf("fixture does not grant models.beta.view to editor")
	}
	row, _ := before.row("models.alpha.view")
	partial := permissionRow{}
	for key, cell := range row {
		partial[key] = cell
	}
	partial["editor"] = false

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/permissions/administration/default", []permissionRow{partial})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	after := readMatrix(t, router, "administration", "default")
	if after.granted(t, "models.alpha.view", "editor") {
		t.Fatalf("the one submitted cell was not revoked")
	}
	if !after.granted(t, "models.beta.view", "editor") {
		t.Fatalf("a permission the body never mentioned was revoked")
	}
	if !contains(grantsSQL(t, pool, "default", "admin"), "models.beta.edit") {
		t.Fatalf("an unmentioned role's grants were revoked")
	}
}

func TestPermissionMatrixSaveRejectsUnknownNames(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	baseline := grantsSQL(t, pool, "default", "admin")

	cases := map[string][]map[string]any{
		"unknown permission": {{"name": "models.invented.permission", "admin": true}},
		"unknown role":       {{"name": "models.alpha.view", "wizard": true}},
		"missing name":       {{"admin": true}},
		"non-boolean cell":   {{"name": "models.alpha.view", "admin": "yes"}},
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("PUT status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			// Pylon answers {"ok": true} and drops the row. The point of the 400
			// is that nothing was written, so assert that too.
			if got := grantsSQL(t, pool, "default", "admin"); !equalStrings(got, baseline) {
				t.Fatalf("a rejected save still changed the matrix: %v → %v", baseline, got)
			}
		})
	}
}

func TestPermissionMatrixSaveIsIdempotent(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	baseline := grantsSQL(t, pool, "default", "admin")

	matrix := readMatrix(t, router, "administration", "default")
	recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default", matrix.Rows)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", recorder.Code)
	}
	var result struct {
		Granted int `json:"granted"`
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	if result.Granted != 0 || result.Revoked != 0 {
		t.Fatalf("round-tripping an unchanged matrix moved %d/%d cells", result.Granted, result.Revoked)
	}
	if got := grantsSQL(t, pool, "default", "admin"); !equalStrings(got, baseline) {
		t.Fatalf("round-tripping an unchanged matrix changed it: %v → %v", baseline, got)
	}
}

/* ── write: PUT, project scopes ────────────────────────────────────────── */

// The inherited-matrix trap, and the reason `permissionMatrix.inherited` exists.
//
// A project with no overrides DISPLAYS the central matrix. If the save diffed
// against what was displayed, toggling one box would write exactly one override
// row — and the next read, no longer falling back, would show a project with one
// permission and nothing else. Every other permission would be gone, with no
// failed request anywhere.
func TestSavingAnInheritedMatrixMaterialisesAllOfIt(t *testing.T) {
	pool, router := newRolesEnvironment(t)

	before := readMatrix(t, router, "public", "default")
	if got := projectGrantsSQL(t, pool, publicProjectID, "admin"); len(got) != 0 {
		t.Fatalf("fixture project %d already has overrides: %v", publicProjectID, got)
	}
	inheritedAdminGrants := []string{}
	for _, row := range before.Rows {
		if before.granted(t, row["name"].(string), "admin") {
			inheritedAdminGrants = append(inheritedAdminGrants, row["name"].(string))
		}
	}
	if len(inheritedAdminGrants) < 2 {
		t.Fatalf("fixture is too small to detect the trap: %v", inheritedAdminGrants)
	}

	// Toggle ONE unrelated cell and save.
	body := setCell(t, before, "models.gamma.delete", "viewer", true)
	if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/public/default", body); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	after := readMatrix(t, router, "public", "default")
	if !after.granted(t, "models.gamma.delete", "viewer") {
		t.Fatalf("the toggled cell was not saved")
	}
	for _, permission := range inheritedAdminGrants {
		if !after.granted(t, permission, "admin") {
			t.Fatalf("admin lost %q: the inherited matrix was not materialised", permission)
		}
	}
	// Read past the handler: the fallback could hide a miss if the write had
	// stored nothing at all. It must be stored now.
	stored := projectGrantsSQL(t, pool, publicProjectID, "admin")
	if !equalStrings(stored, inheritedAdminGrants) {
		t.Fatalf("stored project grants = %v, want the materialised %v", stored, inheritedAdminGrants)
	}
}

func TestSavingAnOverriddenProjectDiffsAgainstItsOwnRows(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(sharedProjectID))

	before := readMatrix(t, router, "public", "default")
	body := setCell(t, before, "models.gamma.delete", "admin", false)
	if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/public/default", body); recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if readMatrix(t, router, "public", "default").granted(t, "models.gamma.delete", "admin") {
		t.Fatalf("re-read still shows the revoked project override")
	}
	if got := projectGrantsSQL(t, pool, sharedProjectID, "admin"); len(got) != 0 {
		t.Fatalf("project %d admin overrides after revoke = %v, want none", sharedProjectID, got)
	}
	// The OTHER project must not have been touched — the write is keyed on
	// project_id, and a missing predicate would edit every project at once. The
	// fixture puts the SAME role and permission on project 3, so a missing
	// predicate deletes something real here.
	if got := projectGrantsSQL(t, pool, personalProjectID, "admin"); !equalStrings(got, []string{"models.gamma.delete"}) {
		t.Fatalf("revoking on one project also revoked it on another: %v", got)
	}
	if got := projectGrantsSQL(t, pool, personalProjectID, "viewer"); len(got) != 1 {
		t.Fatalf("an unrelated project's overrides changed: %v", got)
	}
}

/* ── write: POST (sync) ────────────────────────────────────────────────── */

func TestSyncPushesTheCentralMatrixOntoSharedProjectsOnly(t *testing.T) {
	pool, router := newRolesEnvironment(t)

	// Revoke a central grant first, so the sync has something to PRUNE as well
	// as something to add. A sync that only ever adds leaves privileges the
	// operator believes they removed.
	central := readMatrix(t, router, "administration", "default")
	if recorder := adminDo(t, router, http.MethodPut, "/admin/permissions/administration/default",
		setCell(t, central, "models.beta.view", "editor", false)); recorder.Code != http.StatusOK {
		t.Fatalf("setup PUT status = %d", recorder.Code)
	}

	recorder := adminDo(t, router, http.MethodPost, "/admin/permissions/administration/default", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST sync status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The shared project now mirrors central `default`.
	wantAdmin := grantsSQL(t, pool, "default", "admin")
	if got := projectGrantsSQL(t, pool, sharedProjectID, "admin"); !equalStrings(got, wantAdmin) {
		t.Fatalf("shared project admin grants = %v, want %v", got, wantAdmin)
	}
	// …including the prune: `models.beta.view` is gone from editor everywhere
	// it was synced.
	if contains(projectGrantsSQL(t, pool, sharedProjectID, "editor"), "models.beta.view") {
		t.Fatalf("sync did not prune a centrally-revoked permission")
	}
	// The project 2 override that central never had must be gone too.
	if contains(projectGrantsSQL(t, pool, sharedProjectID, "editor"), "models.orphan.permission") {
		t.Fatalf("sync left a project-only override in place")
	}

	// The personal project keeps its stale grant: `project_user_%` is skipped,
	// and the underscores in that pattern are escaped so the match is literal.
	if got := projectGrantsSQL(t, pool, personalProjectID, "viewer"); len(got) != 1 || got[0] != "models.gamma.delete" {
		t.Fatalf("the personal project was synced: viewer grants = %v", got)
	}
	// The public project is skipped too — it is edited through its own tab.
	if got := projectGrantsSQL(t, pool, publicProjectID, "admin"); len(got) != 0 {
		t.Fatalf("the public project was synced: admin grants = %v", got)
	}

	// `projectAuserB-team` is a SHARED project. It only looks like a personal
	// one if `_` is read as a SQL wildcard — which is why the skip pattern
	// escapes them. It had no project_role rows either, so this also proves the
	// sync creates them.
	if got := projectGrantsSQL(t, pool, wildcardNameProjectID, "admin"); !equalStrings(got, wantAdmin) {
		t.Fatalf("a shared project matching the UNESCAPED skip pattern was not synced: %v", got)
	}
}

func TestSyncIsRefusedForEveryModeButDefault(t *testing.T) {
	pool, router := newRolesEnvironment(t)
	baseline := projectGrantsSQL(t, pool, sharedProjectID, "admin")

	recorder := adminDo(t, router, http.MethodPost, "/admin/permissions/administration/administration", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST sync in administration mode status = %d, want 400", recorder.Code)
	}
	if got := projectGrantsSQL(t, pool, sharedProjectID, "admin"); !equalStrings(got, baseline) {
		t.Fatalf("a refused sync still wrote: %v → %v", baseline, got)
	}
}

func TestSyncIsRefusedForProjectScopes(t *testing.T) {
	_, router := newRolesEnvironment(t)

	recorder := adminDo(t, router, http.MethodPost, "/admin/permissions/public/default", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("POST sync on the public scope status = %d, want 404", recorder.Code)
	}
}

/* ── authorisation ─────────────────────────────────────────────────────── */

// The negative case. `window.admin_ui_config.permissions` is presentation state;
// the route middleware is the gate, and a caller holding only the VIEW
// permission must be refused the write — and must not have written anything.
func TestPermissionMatrixRefusesACallerWithoutTheEditPermission(t *testing.T) {
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)
	t.Setenv("AI_PROJECT_ID", fmt.Sprint(publicProjectID))

	viewerGate := apimw.RequireCentralPermissions(
		grantingResolver("configuration.roles.permissions.view"),
		auth.PermissionModeAdministration,
		"configuration.roles.permissions.edit",
	)
	guarded := rolesRouter(admin.NewHandler(pool), viewerGate, &auth.User{ID: "1", UserID: "1"})
	// A second, ungated router is used only to READ the matrix back, so the
	// assertion "nothing moved" does not depend on the gate under test.
	open := rolesRouter(admin.NewHandler(pool), nil, &auth.User{ID: "1", UserID: "1"})

	before := readMatrix(t, open, "administration", "default")
	body := setCell(t, before, "models.alpha.view", "viewer", false)

	for _, attempt := range []struct {
		method string
		body   any
	}{
		{http.MethodPut, body},
		{http.MethodPost, nil},
	} {
		recorder := adminDo(t, guarded, attempt.method, "/admin/permissions/administration/default", attempt.body)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s as a viewer status = %d, want 403 (body %s)",
				attempt.method, recorder.Code, recorder.Body.String())
		}
	}

	if !readMatrix(t, open, "administration", "default").granted(t, "models.alpha.view", "viewer") {
		t.Fatalf("a refused write changed the matrix anyway")
	}
	if got := projectGrantsSQL(t, pool, sharedProjectID, "admin"); len(got) != 1 {
		t.Fatalf("a refused sync wrote to the projects: %v", got)
	}
}

// And the read is gated too — the matrix names which role holds which privilege.
func TestPermissionMatrixReadIsGated(t *testing.T) {
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)

	gate := apimw.RequireCentralPermissions(
		grantingResolver("admin.auth.users"),
		auth.PermissionModeAdministration,
		"configuration.roles.permissions.view",
	)
	router := rolesRouter(admin.NewHandler(pool), gate, &auth.User{ID: "1", UserID: "1"})

	recorder := adminDo(t, router, http.MethodGet, "/admin/permissions/administration/default", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("GET without the view permission status = %d, want 403", recorder.Code)
	}
}

/* ── small helpers ─────────────────────────────────────────────────────── */

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
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
