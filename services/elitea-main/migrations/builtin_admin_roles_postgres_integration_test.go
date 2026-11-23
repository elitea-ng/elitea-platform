package migrations_test

// Shared 0263 (issue 2496, ADR-0059 R0.7, migration B of decision 4): the
// built-in administration roles, their grants and the seed record
// elitea_auth.admin_role_seeds.
//
// Two databases, as in the 0261 test: fresh (001_initial.sql plus the corpus
// before 0263) and legacy (001_initial.sql with the central roles of
// testdata/postgres/legacy-rbac-matrix.json, then the corpus before 0263).
//
// ADR-0058 story E5.1 (#2403) creates the same seed table and the
// security_auditor role. Until it merges, e51Fixture stands in for it: the
// seed-table DDL of 0263 and the role insert with its three grants. Replace
// the fixture with the real migration file when #2403 lands.

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	builtInRolesFile      = "0263_builtin_admin_roles.sql"
	builtInRolesMigration = "shared/" + builtInRolesFile
)

// e51Grants are the three security_auditor grants of ADR-0058 E5.1.
var e51Grants = []string{"runs.evidence.read", "security.signals.project.view", "security.signals.view"}

// newRoles are the roles 0263 creates.
var newRoles = []string{
	adminaccess.RoleLLMManager, adminaccess.RoleProjectAdministrator,
	adminaccess.RoleEmergencyAccess, adminaccess.RoleSecurityAuditor,
}

// e51Fixture is ADR-0058 E5.1's part of security_auditor: the seed table if
// absent, and the role with its seed row and three grants under the same
// three cases as 0263.
func e51Fixture(t *testing.T) string {
	t.Helper()
	return `DO $$
DECLARE found_role integer; has_seed boolean;
BEGIN
    ` + seedTableDDL(t) + `;
    SELECT id INTO found_role FROM public.auth_core__role
    WHERE mode = 'administration' AND name = 'security_auditor';
    IF found_role IS NULL THEN
        INSERT INTO public.auth_core__role (name, mode) VALUES ('security_auditor', 'administration')
        RETURNING id INTO found_role;
        INSERT INTO elitea_auth.admin_role_seeds (role_id, name, mode, marker, seeded_by)
        VALUES (found_role, 'security_auditor', 'administration', 'declared', 'e51-fixture');
    ELSE
        SELECT EXISTS (SELECT 1 FROM elitea_auth.admin_role_seeds WHERE role_id = found_role) INTO has_seed;
        IF NOT has_seed THEN
            RETURN;
        END IF;
    END IF;
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT found_role, unnest(ARRAY['security.signals.view', 'security.signals.project.view', 'runs.evidence.read'])
    ON CONFLICT (role_id, permission) DO NOTHING;
END
$$;`
}

// seedTableDDL is the CREATE SCHEMA and CREATE TABLE of 0263, as written there.
func seedTableDDL(t *testing.T) string {
	t.Helper()
	body, err := fs.ReadFile(migrations.Files, builtInRolesMigration)
	if err != nil {
		t.Fatalf("read %s: %v", builtInRolesMigration, err)
	}
	ddl := seedTableDDLPattern.FindString(string(body))
	if ddl == "" {
		t.Fatalf("%s has no seed-table DDL", builtInRolesMigration)
	}
	return strings.TrimSuffix(ddl, ";")
}

// seedTableDDLPattern matches the CREATE SCHEMA and the CREATE TABLE up to the
// first line that closes the column list, whatever its indentation.
var seedTableDDLPattern = regexp.MustCompile(
	`(?s)CREATE SCHEMA IF NOT EXISTS elitea_auth;\s*CREATE TABLE IF NOT EXISTS elitea_auth\.admin_role_seeds \(.*?\n\s*\);`)

/* ── grant parity ─────────────────────────────────────────────────────── */

// Each built-in role holds the registry's set, in either order of 0263 and
// ADR-0058 E5.1, and the two orders give the same grants.
func TestBuiltInAdminRolesHoldTheRegistrySetsInEitherOrder(t *testing.T) {
	orders := map[string]func(*testing.T, *pgxpool.Pool){
		"e51 first": func(t *testing.T, pool *pgxpool.Pool) {
			execSQL(t, pool, e51Fixture(t))
			applySharedCorpus(t, pool)
		},
		"0263 first": func(t *testing.T, pool *pgxpool.Pool) {
			applySharedCorpus(t, pool)
			execSQL(t, pool, e51Fixture(t))
		},
	}
	results := map[string]map[string][]string{}
	for name, apply := range orders {
		t.Run(name, func(t *testing.T) {
			pool := newPoolBefore0263(t)
			apply(t, pool)
			sets := builtInRoleSets(t, pool)
			results[name] = sets
			assertRegistryParity(t, sets)
			assertSeedRows(t, pool, false)
		})
	}
	if a, b := results["e51 first"], results["0263 first"]; a != nil && b != nil {
		for role, set := range a {
			if !slices.Equal(set, b[role]) {
				t.Errorf("%s differs by order:\n  e51 first:  %v\n  0263 first: %v", role, set, b[role])
			}
		}
	}
}

func assertRegistryParity(t *testing.T, sets map[string][]string) {
	t.Helper()
	registry := adminaccess.Names()
	for _, role := range adminaccess.BuiltInRoles() {
		if !role.DeclaresSet() {
			continue
		}
		held, ok := sets[role.Name]
		if !ok {
			t.Errorf("the built-in role %s does not exist", role.Name)
			continue
		}
		want := role.Set()
		switch role.Name {
		case adminaccess.RoleSuperAdmin, adminaccess.RoleAdmin:
			// An administrator role also holds legacy names outside the
			// permission table; over the table it holds exactly its set.
			for _, name := range registry {
				if slices.Contains(held, name) != slices.Contains(want, name) {
					t.Errorf("%s: holds %s = %v, the registry says %v",
						role.Name, name, slices.Contains(held, name), slices.Contains(want, name))
				}
			}
		case adminaccess.RoleSecurityAuditor:
			want = sortedUnion(want, e51Grants)
			fallthrough
		default:
			if !slices.Equal(held, want) {
				t.Errorf("%s holds %v, want %v", role.Name, held, want)
			}
		}
	}
	// The two exceptions to "every new permission goes to both administrator
	// roles".
	if slices.Contains(sets[adminaccess.RoleAdmin], adminaccess.IdentitySSOTrustManage) {
		t.Errorf("admin holds %s", adminaccess.IdentitySSOTrustManage)
	}
	for role, set := range sets {
		if slices.Contains(set, "security.signals.reidentify") {
			t.Errorf("the built-in role %s holds security.signals.reidentify", role)
		}
	}
}

// assertSeedRows checks one seed row per built-in role that exists, with the
// registry's marker.
func assertSeedRows(t *testing.T, pool *pgxpool.Pool, legacy bool) {
	t.Helper()
	rows := seedRows(t, pool)
	for _, role := range adminaccess.BuiltInRoles() {
		marker, ok := rows[role.Name]
		if role.Name == adminaccess.RoleSystem && !legacy {
			if ok {
				t.Errorf("a fresh database has a seed row for system")
			}
			continue
		}
		if !ok {
			t.Errorf("no seed row for %s", role.Name)
			continue
		}
		if marker != string(role.Marker) {
			t.Errorf("%s is marked %q, want %q", role.Name, marker, role.Marker)
		}
	}
}

/* ── custom roles of a built-in name ──────────────────────────────────── */

// A custom llm_manager that predates the upgrade keeps its grants, gets no
// seed row and is reported; the roles API can still rename and delete it.
func TestACustomRoleOfABuiltInNameIsLeftAlone(t *testing.T) {
	for _, name := range []string{adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor} {
		t.Run(name, func(t *testing.T) {
			pool := newPoolBefore0263(t)
			execSQL(t, pool, fmt.Sprintf(`
INSERT INTO public.auth_core__role (name, mode) VALUES ('%s', 'administration');
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, 'llm.usage.view' FROM public.auth_core__role WHERE mode = 'administration' AND name = '%s';`,
				name, name))
			before := roleSet(t, pool, name)

			notices := applyFileCollectingNotices(t, pool, builtInRolesMigration)

			if after := roleSet(t, pool, name); !slices.Equal(after, before) {
				t.Errorf("the custom %s changed from %v to %v", name, before, after)
			}
			if slices.Contains(roleSet(t, pool, name), adminaccess.AccessBreakglassReview) {
				t.Errorf("the custom %s holds %s", name, adminaccess.AccessBreakglassReview)
			}
			if _, seeded := seedRows(t, pool)[name]; seeded {
				t.Errorf("the custom %s got a seed row", name)
			}
			if !slices.ContainsFunc(notices, func(n string) bool { return strings.Contains(n, name) }) {
				t.Errorf("no notice names %s; notices: %v", name, notices)
			}

			// The roles API treats it as any custom role.
			router := rolesAPI(pool)
			renamed := name + "_custom"
			if code := roleWrite(t, router, http.MethodPut,
				fmt.Sprintf(`{"name":%q,"new_name":%q}`, name, renamed)); code != http.StatusOK {
				t.Fatalf("rename of the custom %s answered %d", name, code)
			}
			if code := roleWrite(t, router, http.MethodDelete,
				fmt.Sprintf(`{"name":%q}`, renamed)); code != http.StatusOK {
				t.Fatalf("delete of the custom %s answered %d", name, code)
			}
			// A seeded role on the same database is refused.
			if code := roleWrite(t, router, http.MethodDelete,
				`{"name":"project_administrator"}`); code != http.StatusConflict {
				t.Fatalf("delete of the built-in project_administrator answered %d, want 409", code)
			}
		})
	}
}

/* ── the id sequence ──────────────────────────────────────────────────── */

// 001_initial.sql inserts roles with explicit ids; the serial sequence can sit
// below them. The inserts of 0263 do not collide.
func TestBuiltInAdminRolesDoNotCollideWithExplicitRoleIDs(t *testing.T) {
	pool := newPoolBefore0263(t)
	execSQL(t, pool, `
INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (1, 'r1', 'default'), (2, 'r2', 'default'), (3, 'r3', 'default')
ON CONFLICT DO NOTHING;
SELECT setval(pg_get_serial_sequence('public.auth_core__role', 'id'), 1, false);`)
	var largest int
	queryRow(t, pool, `SELECT max(id) FROM public.auth_core__role`, &largest)
	if largest < 7 {
		t.Fatalf("premise: the largest role id is %d, want at least 7", largest)
	}

	applySharedCorpus(t, pool)
	for _, name := range newRoles {
		var id int
		queryRow(t, pool, `SELECT id FROM public.auth_core__role WHERE mode = 'administration' AND name = '`+name+`'`, &id)
		if id <= largest {
			t.Errorf("%s got id %d, not above the previous largest %d", name, id, largest)
		}
	}
}

/* ── legacy-backed databases and the upgrade ──────────────────────────── */

// No seeded role loses a grant, on either database. On the legacy one,
// system is unassignable and keeps its 195, editor and viewer are as_found.
func TestBuiltInAdminRolesKeepEveryGrant(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"fresh":  newPoolBefore0263,
		"legacy": newLegacyPoolBefore0263,
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := allRoleGrants(t, pool)
			seededBefore := map[string][]string{}
			for _, role := range []string{adminaccess.RoleSuperAdmin, adminaccess.RoleAdmin,
				adminaccess.RoleEditor, adminaccess.RoleViewer, adminaccess.RoleSystem} {
				seededBefore[role] = roleSet(t, pool, role)
			}
			applySharedCorpus(t, pool)
			after := allRoleGrants(t, pool)

			// Each seeded role, its set before and after.
			for role, set := range seededBefore {
				if got := roleSet(t, pool, role); !slices.Equal(got, set) {
					t.Errorf("%s held %d permissions before 0263 and %d after", role, len(set), len(got))
				}
			}

			if lost := grantDifference(before, after); len(lost) > 0 {
				t.Errorf("0263 removed %d grants:\n  %s", len(lost), strings.Join(lost, "\n  "))
			}
			// The only rows added belong to the new roles.
			for _, row := range grantDifference(after, before) {
				if !slices.ContainsFunc(newRoles, func(role string) bool {
					return strings.HasPrefix(row, "administration/"+role+": ")
				}) {
					t.Errorf("0263 added %s", row)
				}
			}
			assertSeedRows(t, pool, name == "legacy")
			if name == "legacy" {
				// The fixture gives system 195; the corpus before 0263
				// removes some on purpose (0136, 0172). 0263 removes none,
				// which the set comparison above proves.
				for _, role := range []string{adminaccess.RoleSystem, adminaccess.RoleEditor, adminaccess.RoleViewer} {
					if len(seededBefore[role]) == 0 {
						t.Errorf("premise: the legacy %s holds nothing", role)
					}
				}
				if !slices.Contains(roleSet(t, pool, adminaccess.RoleSystem), adminaccess.AdminAuthUsersSuperAdmin) {
					t.Errorf("the legacy system role lost %s", adminaccess.AdminAuthUsersSuperAdmin)
				}
			}
		})
	}
}

// Applying the file a second time writes nothing.
func TestBuiltInAdminRolesReapplyWritesNothing(t *testing.T) {
	pool := newMigratedPool(t)
	count := func() string {
		var grants, roles, seeds int
		queryRow(t, pool, `SELECT count(*) FROM public.auth_core__role_permission`, &grants)
		queryRow(t, pool, `SELECT count(*) FROM public.auth_core__role`, &roles)
		queryRow(t, pool, `SELECT count(*) FROM elitea_auth.admin_role_seeds`, &seeds)
		return fmt.Sprintf("%d grants, %d roles, %d seeds", grants, roles, seeds)
	}
	before := count()
	execMigrationFile(t, pool, builtInRolesMigration)
	if after := count(); after != before {
		t.Fatalf("re-applying 0263 changed %s to %s", before, after)
	}
}

// On a database without the auth_core tables the file is a no-op.
func TestBuiltInAdminRolesIsANoOpWithoutAuthCore(t *testing.T) {
	pool := newEmptyDatabase(t)
	execMigrationFile(t, pool, builtInRolesMigration)
	var present bool
	queryRow(t, pool, `SELECT to_regclass('elitea_auth.admin_role_seeds') IS NOT NULL`, &present)
	if present {
		t.Fatal("0263 created the seed table on a database without auth_core")
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

func newPoolBefore0263(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	applyCorpusWithout(t, pool, builtInRolesFile)
	return pool
}

func newLegacyPoolBefore0263(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	loadLegacyRBACMatrix(t, pool)
	applyCorpusWithout(t, pool, builtInRolesFile)
	if n := len(roleSet(t, pool, adminaccess.RoleSystem)); n == 0 {
		t.Fatal("premise: the legacy database has no administration system role")
	}
	return pool
}

func applyCorpusWithout(t *testing.T, pool *pgxpool.Pool, hidden string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migrate.New(pool, withoutFile{FS: migrations.Files, hidden: hidden}).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus without %s: %v", hidden, err)
	}
	var present bool
	queryRow(t, pool, `SELECT to_regclass('elitea_auth.admin_role_seeds') IS NOT NULL`, &present)
	if present {
		t.Fatalf("the corpus without %s already has the seed table", hidden)
	}
}

func execSQL(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

func queryRow(t *testing.T, pool *pgxpool.Pool, sql string, into ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, sql).Scan(into...); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}

// applyFileCollectingNotices runs one migration file on its own connection and
// returns the notices it raised.
func applyFileCollectingNotices(t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	body, err := fs.ReadFile(migrations.Files, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var (
		mu      sync.Mutex
		notices []string
	)
	config := pool.Config().ConnConfig.Copy()
	config.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, notice.Message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(body))
		return err
	}); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(notices)
}

// roleSet is the sorted set of one administration role.
func roleSet(t *testing.T, pool *pgxpool.Pool, name string) []string {
	t.Helper()
	return builtInRoleSetsWhere(t, pool, name)[name]
}

// builtInRoleSets is the set of every administration role.
func builtInRoleSets(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	return builtInRoleSetsWhere(t, pool, "")
}

func builtInRoleSetsWhere(t *testing.T, pool *pgxpool.Pool, only string) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT role.name, grant_row.permission
FROM public.auth_core__role AS role
LEFT JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
WHERE role.mode = 'administration' AND ($1 = '' OR role.name = $1)
ORDER BY 1, 2`, only)
	if err != nil {
		t.Fatalf("read the administration roles: %v", err)
	}
	defer rows.Close()
	sets := map[string][]string{}
	for rows.Next() {
		var role string
		var permission *string
		if err := rows.Scan(&role, &permission); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ok := sets[role]; !ok {
			sets[role] = []string{}
		}
		if permission != nil {
			sets[role] = append(sets[role], *permission)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return sets
}

// seedRows maps an administration role name to its seed marker.
func seedRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT role.name, seed.marker, count(*) OVER (PARTITION BY role.name)
FROM elitea_auth.admin_role_seeds AS seed
JOIN public.auth_core__role AS role ON role.id = seed.role_id
WHERE role.mode = 'administration'`)
	if err != nil {
		t.Fatalf("read the seed rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, marker string
		var n int
		if err := rows.Scan(&name, &marker, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 1 {
			t.Errorf("%s has %d seed rows", name, n)
		}
		out[name] = marker
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func sortedUnion(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

// rolesAPI mounts the role rename and delete of the administration mode.
func rolesAPI(pool *pgxpool.Pool) http.Handler {
	handler := admin.NewHandler(pool)
	router := chi.NewRouter()
	router.Put("/admin/roles/{scope}/{mode}", handler.AdminRoleRename)
	router.Delete("/admin/roles/{scope}/{mode}", handler.AdminRoleDelete)
	return router
}

func roleWrite(t *testing.T, router http.Handler, method, body string) int {
	t.Helper()
	request := httptest.NewRequest(method, "/admin/roles/administration/administration", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code >= 500 {
		t.Fatalf("%s answered %d: %s", method, recorder.Code, recorder.Body.String())
	}
	return recorder.Code
}
