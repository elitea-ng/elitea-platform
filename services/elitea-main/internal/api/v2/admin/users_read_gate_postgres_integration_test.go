package admin_test

// Acceptance for gating the admin panel's READ surface, and for the migration
// that has to land with it.
//
// `GET /admin/auth_users/{mode}` returns id, name, email, last_login,
// suspended and administration role for every row of auth_core__user. It had
// no permission middleware, so any authenticated session could read the whole
// global user list — ~30k rows on the legacy dev database — while its legacy
// original (legacy/plugins/admin/api/v2/auth_users.py) gates `get()` on
// `admin.auth.users`, the same permission it requires for `post()`.
//
// Unit A14 gated the writes and left this read open on purpose: no deployment
// bootstrapped by elitea-migrate had a single administration-mode role, so
// `legacyrbac.PostgresResolver` resolved every central permission to the empty
// set and a gated read would have answered 403 to everyone. That is why the
// tests here are NOT just "403 without, 200 with". A hand-granted permission
// would prove the middleware works and say nothing about whether any real
// deployment can still reach the page. So the grants under test are the ones
// migrations/shared/0060_admin_central_rbac.sql actually writes, applied from
// the embedded migration corpus, through the real
// `legacyrbac.PostgresResolver` and the real
// `middleware.RequireCentralPermissions` — not a stub resolver.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const adminCentralRBACMigration = "shared/0060_admin_central_rbac.sql"

// gatedAuthUsersRouter mounts the listing exactly as internal/api/router.go
// does — same middleware constructor, same mode, same permission — over the
// REAL database-backed resolver. Nothing here is a test double except the
// principal, which stands in for the authentication middleware that would have
// put it in the context.
func gatedAuthUsersRouter(pool *pgxpool.Pool, principal auth.User) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(middleware.RequireCentralPermissions(
		legacyrbac.NewPostgresResolver(pool),
		auth.PermissionModeAdministration,
		"admin.auth.users",
	)).Get("/admin/auth_users/{mode}", admin.NewHandler(pool).AuthUsers)
	return router
}

func getAuthUsers(t *testing.T, pool *pgxpool.Pool, id int) *httptest.ResponseRecorder {
	t.Helper()
	principal := auth.User{ID: strconv.Itoa(id), UserID: strconv.Itoa(id)}
	recorder := httptest.NewRecorder()
	gatedAuthUsersRouter(pool, principal).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/admin/auth_users/administration?limit=100&offset=0", nil),
	)
	return recorder
}

// applyAdminCentralRBACMigration runs the shipped migration body. It reads the
// EMBEDDED corpus rather than a path on disk, so a migration that is present in
// the working tree but missing from `platformmigrations.Files` fails here.
func applyAdminCentralRBACMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := platformmigrations.Files.ReadFile(adminCentralRBACMigration)
	if err != nil {
		t.Fatalf("read embedded %s: %v", adminCentralRBACMigration, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", adminCentralRBACMigration, err)
	}
}

// grantsFor reads the administration-mode grants of one role straight out of
// the table the resolver reads.
func grantsFor(t *testing.T, pool *pgxpool.Pool, roleName string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT grant_row.permission
FROM public.auth_core__role AS role
JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
WHERE role.name = $1 AND role.mode = 'administration'
ORDER BY grant_row.permission`, roleName)
	if err != nil {
		t.Fatalf("read grants for %s: %v", roleName, err)
	}
	defer rows.Close()
	permissions := []string{}
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		permissions = append(permissions, permission)
	}
	return permissions
}

// prepareReadGateFixture builds the shape of a database bootstrapped by the
// PRE-0060 001_initial.sql: default-mode roles only, a dev bootstrap account at
// id 1 holding the default `admin` role, and no administration mode at all.
func prepareReadGateFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (id, name, mode) VALUES
			(1, 'admin', 'default'),
			(2, 'editor', 'default'),
			(3, 'viewer', 'default')`,
		`INSERT INTO public.auth_core__user (id, email, name) VALUES
			(1, 'dev@elitea.ai', 'Dev User'),
			(2, 'read-gate-member@autotest.local', 'Read Gate Member'),
			(3, 'read-gate-outsider@autotest.local', 'Read Gate Outsider')`,
		// The dev account holds the default-mode admin role, exactly as
		// 001_initial.sql seeds it.
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (1, 1)`,
		// The member holds the default-mode admin role TOO. That is the whole
		// point of the pairing: "admin of a project" must not imply "may read
		// every account on the platform", and before the gate it did.
		`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES (2, 1)`,
		`SELECT setval('auth_core__user_id_seq', (SELECT MAX(id) FROM public.auth_core__user) + 1, false)`,
		`SELECT setval('auth_core__role_id_seq', (SELECT MAX(id) FROM public.auth_core__role) + 1, false)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

/* ── the gate, over what the migration actually grants ─────────────────── */

func TestAuthUsersListingIsGatedOnTheMigrationSeededPermission(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareReadGateFixture(t, pool)

	// Before the migration nobody holds an administration permission, so the
	// gate closes on EVERY principal — including the bootstrap account. This is
	// the exact outcome unit A14 refused to ship without a migration, asserted
	// rather than described.
	for _, id := range []int{1, 2, 3} {
		if recorder := getAuthUsers(t, pool, id); recorder.Code != http.StatusForbidden {
			t.Fatalf("pre-migration GET as user %d status = %d, want 403 (body %s)",
				id, recorder.Code, recorder.Body.String())
		}
	}

	applyAdminCentralRBACMigration(t, pool)

	// What the migration wrote, at the table the resolver reads.
	superAdmin := grantsFor(t, pool, "super_admin")
	for _, permission := range []string{
		"admin.auth.users",
		"admin.auth.users.super_admin",
		"runtime.plugins",
		"projects.projects.projects.view",
		"configuration.roles.permissions.view",
		"admin.moderation",
	} {
		if !contains(superAdmin, permission) {
			t.Fatalf("administration super_admin grants = %v, missing %q", superAdmin, permission)
		}
	}
	// The escalation permission is super_admin-only in the legacy runtime
	// (admin/module.py sets `admin: False`), and users.go gates grant/revoke of
	// the super_admin role on it. If the migration handed it to `admin` the Go
	// guard would be unreachable in practice.
	if adminGrants := grantsFor(t, pool, "admin"); contains(adminGrants, "admin.auth.users.super_admin") {
		t.Fatalf("administration admin must NOT hold admin.auth.users.super_admin, got %v", adminGrants)
	}
	// editor and viewer are declared False everywhere.
	for _, roleName := range []string{"editor", "viewer"} {
		if grants := grantsFor(t, pool, roleName); len(grants) != 0 {
			t.Fatalf("administration %s grants = %v, want none", roleName, grants)
		}
	}

	// WITH the permission: 200, and the rows are really there. A gate that
	// answered 200 with an empty listing would be the #130/#132 failure shape.
	recorder := getAuthUsers(t, pool, 1)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET as the bootstrap account status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var listing adminUsersListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing %q: %v", recorder.Body.String(), err)
	}
	if listing.Total != 3 {
		t.Fatalf("listing total = %d, want 3", listing.Total)
	}
	for _, email := range []string{
		"dev@elitea.ai",
		"read-gate-member@autotest.local",
		"read-gate-outsider@autotest.local",
	} {
		if _, found := userByEmail(t, listing, email); !found {
			t.Fatalf("listing does not contain %s: %+v", email, listing.Rows)
		}
	}

	// WITHOUT it: still 403, and the migration is what makes this a real
	// difference rather than a uniformly-denied endpoint. The member holds the
	// DEFAULT-mode admin role; central resolution ignores it.
	for _, id := range []int{2, 3} {
		recorder := getAuthUsers(t, pool, id)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("post-migration GET as user %d status = %d, want 403 (body %s)",
				id, recorder.Code, recorder.Body.String())
		}
		if body := recorder.Body.String(); strings.Contains(body, "dev@elitea.ai") {
			t.Fatalf("403 body leaked the listing: %s", body)
		}
	}
}

func TestAdminCentralRBACMigrationPromotesOnlyTheBootstrapAccount(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareReadGateFixture(t, pool)
	applyAdminCentralRBACMigration(t, pool)

	// User 2 also holds the default-mode `admin` role. Promoting "everyone who
	// looks like an admin" would be an escalation, and on a stale E2E volume it
	// would hand global administration to `e2e-member@autotest.local` and erase
	// the authorisation difference the admin journeys assert.
	//
	// The bootstrap account gets `admin`, not `super_admin`. Beyond least
	// privilege, eliteacore/handler.go's project-member listing UNIONs in every
	// holder of a role NAMED `super_admin` and never filters on `mode`, so
	// seeding one would put a phantom member on every project's member list —
	// which is exactly how this showed up: as a "N / 2 active" caption reading
	// 3, and a settings/users snapshot with one row too many.
	for id, want := range map[int][]string{
		1: {"admin"},
		2: {},
		3: {},
	} {
		if got := adminRoleNamesSQL(t, pool, id); len(got) != len(want) ||
			(len(want) == 1 && got[0] != want[0]) {
			t.Fatalf("administration roles for user %d = %v, want %v", id, got, want)
		}
	}
}

/* ── the guard ─────────────────────────────────────────────────────────── */

func TestAdminCentralRBACMigrationLeavesAConfiguredDeploymentAlone(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareReadGateFixture(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A deployment whose operator has an administration mode and has REVOKED
	// `admin.auth.users` from `admin` — the legacy-backed shape, and the one a
	// migration must never quietly undo. Absent is not the same as
	// configured-to-empty, which is why the guard keys on "any
	// administration-mode role exists" rather than on the missing rows.
	for _, statement := range []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('super_admin', 'administration'),
			('admin', 'administration')`,
		`INSERT INTO public.auth_core__role_permission (role_id, permission)
			SELECT id, 'runtime.plugins' FROM public.auth_core__role
			WHERE name = 'admin' AND mode = 'administration'`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}

	applyAdminCentralRBACMigration(t, pool)

	if got := grantsFor(t, pool, "admin"); len(got) != 1 || got[0] != "runtime.plugins" {
		t.Fatalf("admin grants after migration = %v, want exactly [runtime.plugins] — the revoke was undone", got)
	}
	if got := grantsFor(t, pool, "super_admin"); len(got) != 0 {
		t.Fatalf("super_admin grants after migration = %v, want none", got)
	}
	// And no surprise promotion either.
	if got := adminRoleNamesSQL(t, pool, 1); len(got) != 0 {
		t.Fatalf("bootstrap account was promoted on a configured deployment: %v", got)
	}
	// The revoked permission really is denied — the guard did not merely skip
	// the INSERT into a table nothing reads.
	if recorder := getAuthUsers(t, pool, 1); recorder.Code != http.StatusForbidden {
		t.Fatalf("GET status = %d, want 403", recorder.Code)
	}
}
