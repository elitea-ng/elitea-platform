package admin_test

// Unit A14 acceptance for the admin Users write surface.
//
// The defect class these tests exist for is NOT "the endpoint answers the wrong
// status". The pre-A14 build had NO route for any of these writes, and the
// #130/#180 pattern shows what the easy wrong fix looks like: a handler that
// answers 200 and touches nothing. Asserting on the status code therefore
// proves nothing. EVERY case below performs the write and then RE-READS —
// through the product's own `GET /admin/auth_users/{mode}` handler, and through
// SQL where the read path could conceivably paper over a miss.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	dbschema "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/schema"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type adminUserRow struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	LastLogin *string `json:"last_login"`
	Suspended bool    `json:"suspended"`
	IsAdmin   bool    `json:"is_admin"`
	AdminRole *string `json:"admin_role"`
}

type adminUsersListing struct {
	Rows   []adminUserRow `json:"rows"`
	Total  int            `json:"total"`
	Counts struct {
		Platform int `json:"platform"`
		System   int `json:"system"`
	} `json:"counts"`
}

// permissionResolverFunc lets a test state exactly which administration
// permissions the CALLER holds.
type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context, principal auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

func grantingResolver(permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
	}
}

// adminUsersRouter mounts the three routes exactly as internal/api/router.go
// does, minus the route-level permission middleware (which needs the whole auth
// stack; `TestRequireCentralPermissions*` in internal/api/middleware covers that
// layer, and the handler's OWN super_admin guard is exercised here).
func adminUsersRouter(handler *admin.Handler, principal *auth.User) chi.Router {
	router := chi.NewRouter()
	if principal != nil {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), *principal)))
			})
		})
	}
	router.Get("/admin/auth_users/{mode}", handler.AuthUsers)
	router.Post("/admin/auth_users/{mode}", handler.AuthUsersAction)
	router.Put("/admin/user_suspend/{mode}/{userID}", handler.UserSuspend)
	return router
}

func adminDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// readUsers re-reads the listing through the SAME GET handler the admin Users
// page calls. This is the assertion an unwired or no-op write cannot pass.
func readUsers(t *testing.T, router chi.Router, query string) adminUsersListing {
	t.Helper()
	target := "/admin/auth_users/administration?limit=100&offset=0"
	if query != "" {
		target += "&" + query
	}
	recorder := adminDo(t, router, http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-read GET status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing adminUsersListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode re-read body %q: %v", recorder.Body.String(), err)
	}
	return listing
}

func userByEmail(t *testing.T, listing adminUsersListing, email string) (adminUserRow, bool) {
	t.Helper()
	for _, row := range listing.Rows {
		if row.Email == email {
			return row, true
		}
	}
	return adminUserRow{}, false
}

func userID(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM public.auth_core__user WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	return id
}

// adminRoleNamesSQL bypasses the read handler entirely: the single-role rule
// lives in the user_role table, and a listing that reports only the HIGHEST
// role would hide a leftover second assignment.
func adminRoleNamesSQL(t *testing.T, pool *pgxpool.Pool, id int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT role.name
FROM public.auth_core__user_role assignment
JOIN public.auth_core__role role ON role.id = assignment.role_id
WHERE assignment.user_id = $1 AND role.mode = 'administration'
ORDER BY role.name`, id)
	if err != nil {
		t.Fatalf("read administration roles: %v", err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan role: %v", err)
		}
		names = append(names, name)
	}
	return names
}

/* ── delete ────────────────────────────────────────────────────────────── */

func TestAuthUsersDeleteRemovesTheUser(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	const victim = "a14-editor@autotest.local"
	id := userID(t, pool, victim)

	if _, found := userByEmail(t, readUsers(t, router, ""), victim); !found {
		t.Fatalf("fixture does not contain %s", victim)
	}

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "delete", "users": []map[string]any{{"id": id}}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST delete status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if row, found := userByEmail(t, readUsers(t, router, ""), victim); found {
		t.Fatalf("re-read still lists %s after delete: %+v", victim, row)
	}

	var remaining int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM public.auth_core__user WHERE email = $1`, victim).Scan(&remaining); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("auth_core__user still has %d row(s) for %s", remaining, victim)
	}
	// The role assignment must go with it — a dangling grant would resurrect
	// with the id if it were ever reused.
	if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
		t.Fatalf("administration role assignments survived the delete: %v", names)
	}
}

func TestAuthUsersDeleteRejectsAnEmptySelection(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	before := readUsers(t, router, "")
	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "delete", "users": []map[string]any{}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST delete with no users status = %d, want 400", recorder.Code)
	}
	if after := readUsers(t, router, ""); after.Total != before.Total {
		t.Fatalf("user count changed from %d to %d on a rejected delete", before.Total, after.Total)
	}
}

/* ── set_admin_role ────────────────────────────────────────────────────── */

func TestSetAdminRoleAssignsReplacesAndClears(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	const target = "a14-plain@autotest.local"
	id := userID(t, pool, target)

	setRole := func(t *testing.T, role any) {
		t.Helper()
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": role})
		if recorder.Code != http.StatusOK {
			t.Fatalf("set_admin_role(%v) status = %d, want 200 (body %s)", role, recorder.Code, recorder.Body.String())
		}
	}

	t.Run("assigns a role that was absent", func(t *testing.T) {
		before, _ := userByEmail(t, readUsers(t, router, ""), target)
		if before.AdminRole != nil {
			t.Fatalf("fixture user already has role %v", *before.AdminRole)
		}
		setRole(t, "editor")
		after, found := userByEmail(t, readUsers(t, router, ""), target)
		if !found {
			t.Fatalf("%s vanished from the listing", target)
		}
		if after.AdminRole == nil || *after.AdminRole != "editor" {
			t.Fatalf("admin_role after assign = %v, want \"editor\"", after.AdminRole)
		}
		if !after.IsAdmin {
			t.Fatalf("is_admin = false after assigning editor")
		}
	})

	t.Run("replaces rather than accumulates", func(t *testing.T) {
		setRole(t, "admin")
		after, _ := userByEmail(t, readUsers(t, router, ""), target)
		if after.AdminRole == nil || *after.AdminRole != "admin" {
			t.Fatalf("admin_role after replace = %v, want \"admin\"", after.AdminRole)
		}
		// The listing reports only the highest role, so it alone cannot tell an
		// exclusive assignment from an accumulating one. SQL can.
		if names := adminRoleNamesSQL(t, pool, id); len(names) != 1 || names[0] != "admin" {
			t.Fatalf("administration role rows = %v, want exactly [admin]", names)
		}
	})

	t.Run("a null role_name clears every administration role", func(t *testing.T) {
		setRole(t, nil)
		after, _ := userByEmail(t, readUsers(t, router, ""), target)
		if after.AdminRole != nil {
			t.Fatalf("admin_role after clear = %v, want null", *after.AdminRole)
		}
		if after.IsAdmin {
			t.Fatalf("is_admin = true after clearing every role")
		}
		if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
			t.Fatalf("administration role rows after clear = %v, want none", names)
		}
	})
}

func TestSetAdminRoleRejectsAnUnknownRoleWithoutClearingTheCurrentOne(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	const target = "a14-editor@autotest.local"
	id := userID(t, pool, target)

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "root"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("set_admin_role(root) status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	// The validation must reject BEFORE the delete-then-insert runs; otherwise a
	// typo silently strips the user's real role.
	if names := adminRoleNamesSQL(t, pool, id); len(names) != 1 || names[0] != "editor" {
		t.Fatalf("administration role rows = %v, want [editor] untouched", names)
	}
}

func TestSetAdminRoleRollsBackWhenTheRoleIsNotDefinedInThisDeployment(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	// `viewer` is a valid role NAME but this fixture never creates the row, so
	// the INSERT ... SELECT matches nothing. Reporting success there would clear
	// the user's real role and grant nothing.
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM public.auth_core__role WHERE name = 'viewer' AND mode = 'administration'`); err != nil {
		t.Fatalf("drop viewer role: %v", err)
	}
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	const target = "a14-editor@autotest.local"
	id := userID(t, pool, target)

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "viewer"})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if names := adminRoleNamesSQL(t, pool, id); len(names) != 1 || names[0] != "editor" {
		t.Fatalf("administration role rows = %v, want [editor] (transaction must roll back)", names)
	}
}

func TestSetAdminRoleGuardsSuperAdminEscalation(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)

	const target = "a14-plain@autotest.local"
	id := userID(t, pool, target)

	t.Run("granting super_admin is refused without the super_admin permission", func(t *testing.T) {
		router := adminUsersRouter(
			admin.NewHandler(pool, admin.WithPermissionResolver(grantingResolver("admin.auth.users"))),
			&auth.User{ID: "1", UserID: "1"},
		)
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "super_admin"})
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
		}
		if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
			t.Fatalf("roles = %v after a refused escalation, want none", names)
		}
	})

	t.Run("granting super_admin is refused when no resolver is configured", func(t *testing.T) {
		// Fail closed: an unconfigured resolver must not read as "allowed".
		router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "super_admin"})
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
		}
		if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
			t.Fatalf("roles = %v after a refused escalation, want none", names)
		}
	})

	t.Run("granting super_admin succeeds with the permission", func(t *testing.T) {
		router := adminUsersRouter(
			admin.NewHandler(pool, admin.WithPermissionResolver(
				grantingResolver("admin.auth.users", "admin.auth.users.super_admin"))),
			&auth.User{ID: "1", UserID: "1"},
		)
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "super_admin"})
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
		}
		row, _ := userByEmail(t, readUsers(t, router, ""), target)
		if row.AdminRole == nil || *row.AdminRole != "super_admin" {
			t.Fatalf("admin_role = %v, want \"super_admin\"", row.AdminRole)
		}
	})

	t.Run("revoking super_admin is refused without the super_admin permission", func(t *testing.T) {
		// Runs after the grant above: the target now holds super_admin.
		router := adminUsersRouter(
			admin.NewHandler(pool, admin.WithPermissionResolver(grantingResolver("admin.auth.users"))),
			&auth.User{ID: "1", UserID: "1"},
		)
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": "viewer"})
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
		}
		if names := adminRoleNamesSQL(t, pool, id); len(names) != 1 || names[0] != "super_admin" {
			t.Fatalf("roles = %v after a refused revoke, want [super_admin]", names)
		}
	})
}

/* ── suspend ───────────────────────────────────────────────────────────── */

func TestUserSuspendTogglesAndIsVisibleOnReRead(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	const target = "a14-plain@autotest.local"
	id := userID(t, pool, target)

	before, _ := userByEmail(t, readUsers(t, router, ""), target)
	if before.Suspended {
		t.Fatalf("fixture user is already suspended")
	}

	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/user_suspend/administration/%d", id), map[string]any{"suspended": true})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT suspend status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	after, _ := userByEmail(t, readUsers(t, router, ""), target)
	if !after.Suspended {
		t.Fatalf("suspended = false on re-read after suspending")
	}

	recorder = adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/user_suspend/administration/%d", id), map[string]any{"suspended": false})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT unsuspend status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	restored, _ := userByEmail(t, readUsers(t, router, ""), target)
	if restored.Suspended {
		t.Fatalf("suspended = true on re-read after unsuspending")
	}
}

// ADR-0025 WP3: an admin suspension revokes the account's native devices
// (reason user_deactivated, revoked_by the operator) and its browser sessions
// in the SAME transaction; unsuspending resurrects neither.
func TestUserSuspendRevokesDevicesAndBrowserSessions(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})
	id := userID(t, pool, "a14-plain@autotest.local")
	ctx := context.Background()

	var device string
	if err := pool.QueryRow(ctx, `
		INSERT INTO elitea_auth.native_sessions (user_id, client_id, device_name, platform, idle_timeout_seconds)
		VALUES ($1, 'dev.elitea.test', 'Phone', 'ios', 2592000) RETURNING id::text`, id).Scan(&device); err != nil {
		t.Fatal(err)
	}
	browser := "browser-session-for-the-admin-suspension-hook-test"
	if _, err := pool.Exec(ctx, `
		INSERT INTO elitea_auth.browser_sessions
		    (id, user_id, email, provider, provider_session_index, created_at, last_seen_at, expires_at, idle_timeout_seconds)
		VALUES ($1, $2, 'x@autotest.local', 'oidc', '', now(), now(), now() + interval '1 hour', 3600)`, browser, id); err != nil {
		t.Fatal(err)
	}

	if recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/user_suspend/administration/%d", id), map[string]any{"suspended": true}); recorder.Code != http.StatusOK {
		t.Fatalf("suspend = %d %s", recorder.Code, recorder.Body.String())
	}
	var reason *string
	var revokedBy *int64
	var browserRevoked bool
	if err := pool.QueryRow(ctx, `SELECT revoke_reason, revoked_by FROM elitea_auth.native_sessions WHERE id = $1`,
		device).Scan(&reason, &revokedBy); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM elitea_auth.browser_sessions WHERE id = $1`,
		browser).Scan(&browserRevoked); err != nil {
		t.Fatal(err)
	}
	if reason == nil || *reason != "user_deactivated" || revokedBy == nil || *revokedBy != 1 || !browserRevoked {
		t.Fatalf("after suspend: reason=%v revoked_by=%v browser revoked=%v", reason, revokedBy, browserRevoked)
	}

	if recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/user_suspend/administration/%d", id), map[string]any{"suspended": false}); recorder.Code != http.StatusOK {
		t.Fatalf("unsuspend = %d", recorder.Code)
	}
	var stillRevoked bool
	_ = pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM elitea_auth.native_sessions WHERE id = $1`, device).Scan(&stillRevoked)
	if !stillRevoked {
		t.Fatal("unsuspending must not resurrect a revoked device")
	}
}

func TestUserSuspendRejectsMissingFieldAndUnknownUser(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	id := userID(t, pool, "a14-plain@autotest.local")

	// An absent `suspended` must not be read as `false`: that would silently
	// UNSUSPEND on a malformed request.
	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/user_suspend/administration/%d", id), map[string]any{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT without `suspended` status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}

	recorder = adminDo(t, router, http.MethodPut,
		"/admin/user_suspend/administration/99999999", map[string]any{"suspended": true})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT for an unknown user status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

/* ── listing ───────────────────────────────────────────────────────────── */

func TestAuthUsersListingSearchSortCountsAndRowUniqueness(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	t.Run("counts split platform from system users", func(t *testing.T) {
		listing := readUsers(t, router, "")
		if listing.Counts.System != 1 {
			t.Fatalf("counts.system = %d, want 1", listing.Counts.System)
		}
		if listing.Counts.Platform != 3 {
			t.Fatalf("counts.platform = %d, want 3", listing.Counts.Platform)
		}
	})

	t.Run("user_type filters the rows and total", func(t *testing.T) {
		listing := readUsers(t, router, "user_type=platform")
		if listing.Total != 3 {
			t.Fatalf("platform total = %d, want 3", listing.Total)
		}
		for _, row := range listing.Rows {
			if row.Email == "system@centry.user" {
				t.Fatalf("system user leaked into the platform tab")
			}
		}
		system := readUsers(t, router, "user_type=system")
		if system.Total != 1 {
			t.Fatalf("system total = %d, want 1", system.Total)
		}
	})

	t.Run("search matches name and email, case-insensitively", func(t *testing.T) {
		listing := readUsers(t, router, "search=EDITOR")
		if listing.Total != 1 {
			t.Fatalf("search total = %d, want 1 (rows %+v)", listing.Total, listing.Rows)
		}
		if listing.Rows[0].Email != "a14-editor@autotest.local" {
			t.Fatalf("search returned %s", listing.Rows[0].Email)
		}
		// `counts` labels the tabs and must NOT shrink with the search.
		if listing.Counts.Platform != 3 {
			t.Fatalf("counts.platform = %d under search, want 3 (counts are unfiltered)", listing.Counts.Platform)
		}
	})

	t.Run("sort_order reverses the listing", func(t *testing.T) {
		ascending := readUsers(t, router, "user_type=platform&sort_by=email&sort_order=asc")
		descending := readUsers(t, router, "user_type=platform&sort_by=email&sort_order=desc")
		if len(ascending.Rows) != 3 || len(descending.Rows) != 3 {
			t.Fatalf("expected 3 rows each, got %d and %d", len(ascending.Rows), len(descending.Rows))
		}
		if ascending.Rows[0].Email != descending.Rows[2].Email {
			t.Fatalf("desc is not the reverse of asc: %s vs %s",
				ascending.Rows[0].Email, descending.Rows[2].Email)
		}
		if ascending.Rows[0].Email >= ascending.Rows[2].Email {
			t.Fatalf("asc is not ascending: %v", ascending.Rows)
		}
	})

	t.Run("an unknown sort_by falls back instead of failing", func(t *testing.T) {
		// The column name is interpolated into SQL; the allow-list is what keeps
		// that safe, so a rejected value must not reach the query.
		listing := readUsers(t, router, "sort_by="+url.QueryEscape("email; DROP TABLE public.auth_core__user"))
		if listing.Total != 4 {
			t.Fatalf("total = %d, want 4", listing.Total)
		}
	})

	t.Run("a user holding two administration roles appears exactly once", func(t *testing.T) {
		// The pre-A14 listing LEFT JOINed the role table, so this user was
		// emitted twice while `total` (a separate COUNT) still said 4.
		id := userID(t, pool, "a14-editor@autotest.local")
		if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = 'viewer' AND mode = 'administration'`, id); err != nil {
			t.Fatalf("add second role: %v", err)
		}

		listing := readUsers(t, router, "")
		occurrences := 0
		var row adminUserRow
		for _, candidate := range listing.Rows {
			if candidate.Email == "a14-editor@autotest.local" {
				occurrences++
				row = candidate
			}
		}
		if occurrences != 1 {
			t.Fatalf("user appears %d times in the listing, want 1", occurrences)
		}
		if len(listing.Rows) != listing.Total {
			t.Fatalf("len(rows) = %d but total = %d", len(listing.Rows), listing.Total)
		}
		// Highest-priority role wins, matching list_users_paginated.
		if row.AdminRole == nil || *row.AdminRole != "editor" {
			t.Fatalf("admin_role = %v, want \"editor\" (editor outranks viewer)", row.AdminRole)
		}
	})
}

/* ── mode gating (no database needed) ──────────────────────────────────── */

func TestAdminUserWritesAreAdministrationModeOnly(t *testing.T) {
	// Mirrors pylon's `mode_handlers = {'administration': AdminAPI}`: no other
	// mode has a handler at all. Checked before the pool is touched, so this
	// case needs no database.
	router := adminUsersRouter(admin.NewHandler(nil), &auth.User{ID: "1", UserID: "1"})

	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/default",
		map[string]any{"action": "delete", "users": []map[string]any{{"id": 1}}})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("POST in default mode status = %d, want 404", recorder.Code)
	}

	recorder = adminDo(t, router, http.MethodPut, "/admin/user_suspend/default/1",
		map[string]any{"suspended": true})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT in default mode status = %d, want 404", recorder.Code)
	}
}

func TestAuthUsersActionRejectsUnknownActions(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool), &auth.User{ID: "1", UserID: "1"})

	id := userID(t, pool, "a14-plain@autotest.local")

	// pylon answers {"ok": true} to `toggle_admin` while doing nothing — the
	// exact "green toast that lies" shape. This port refuses it instead.
	recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"action": "toggle_admin", "user_id": id, "is_admin": true})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("toggle_admin status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
		t.Fatalf("roles = %v after a rejected action, want none", names)
	}

	recorder = adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
		map[string]any{"users": []map[string]any{{"id": id}}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing action status = %d, want 400", recorder.Code)
	}
}

/* ── fixture + pool ────────────────────────────────────────────────────── */

func prepareAdminUsersFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	statements := []string{
		`INSERT INTO public.auth_core__role (name, mode) VALUES
			('super_admin', 'administration'),
			('admin', 'administration'),
			('editor', 'administration'),
			('viewer', 'administration'),
			('admin', 'default')`,
		`INSERT INTO public.auth_core__user (email, name, last_login) VALUES
			('a14-admin@autotest.local',  'A14 Admin',  '2026-08-01 10:00:00'),
			('a14-editor@autotest.local', 'A14 Editor', NULL),
			('a14-plain@autotest.local',  'A14 Plain',  '2026-08-02 11:30:00'),
			('system@centry.user',        'System',     NULL)`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT u.id, r.id
			FROM public.auth_core__user u, public.auth_core__role r
			WHERE u.email = 'a14-admin@autotest.local'
			  AND r.name = 'admin' AND r.mode = 'administration'`,
		`INSERT INTO public.auth_core__user_role (user_id, role_id)
			SELECT u.id, r.id
			FROM public.auth_core__user u, public.auth_core__role r
			WHERE u.email = 'a14-editor@autotest.local'
			  AND r.name = 'editor' AND r.mode = 'administration'`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
}

func newAdminUsersPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
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

	databaseName := fmt.Sprintf("elitea_admin_users_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	if _, err := pool.Exec(ctx, dbschema.AuthCoreBaselineSQLCProjection); err != nil {
		t.Fatalf("apply auth_core baseline projection: %v", err)
	}
	// A suspension revokes the account's browser sessions and native device
	// sessions in the same transaction (ADR-0025 WP3): the real migrations,
	// not a copy of them.
	for _, migration := range []string{"0117_browser_sessions.sql", "0141_native_auth.sql"} {
		file := filepath.Join("..", "..", "..", "..", "migrations", "shared", migration)
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	return pool
}
