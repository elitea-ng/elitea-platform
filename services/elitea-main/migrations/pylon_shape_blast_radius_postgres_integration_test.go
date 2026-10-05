package migrations_test

// The blast radius of this corpus on a PYLON-MANAGED auth_core.
//
// # The defect
//
// Nine grant files in this corpus carry the same risk assessment, and
// shared/0083_viewer_secret_list_and_own_avatar.sql is the one that acts on it:
//
//	"That shape is the fresh Go database. It is never the shape of a
//	 pylon-backed database, of a legacy dump, or of the end-to-end stack: each
//	 one seeds per-project rows, and those rows suppress the central fallback
//	 completely. So no existing deployment's members gain anything here."
//
// A pylon-backed database seeds NO per-project permission row.
// legacy/plugins/auth_core/db/migrations/202602261000_permission_consolidation.py
// TRUNCATEs auth_core__project_role_permission, pylon project creation writes
// auth_core__project_role rows only, and testdata/postgres/legacy-rbac-matrix.json
// — exported from pylon at migration head 202604161400 — reports
// project_role_permission_rows: 0 with an empty permission list on all ten of
// its project roles.
//
// So the central fallback in internal/infra/legacyrbac/postgres.go is LIVE on
// that database, and 0083's deviating grant reached every project viewer on it.
// The string also gates pylon's own listing route, which declares
// `DEFAULT_MODE: {"admin": True, "viewer": False, "editor": True}`
// (legacy/plugins/secrets/api/v2/secrets.py), so the row widened a second
// product's surface as well. shared/0090 withdraws it there.
//
// # Why no existing test could see this
//
// Every permission test in this package builds its database with
// newMigratedPool, which applies internal/infra/db/migrations/001_initial.sql
// and then the corpus. That is the fresh GO shape — the one shape the false
// paragraph describes correctly. Nothing in the repository ever applied the
// corpus to a pylon-shaped auth_core, so the unmeasured shape read as safe.
//
// This file builds that shape from the exported matrix and applies the real
// corpus to it. Revert shared/0090 and TestAPylonBackedViewerKeepsTheMatrixSecretSplit
// fails.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
)

// legacyMatrixPath is the pylon RBAC export this corpus transcribes. It is the
// same file every grant test in this package names in prose.
const legacyMatrixPath = "../../../testdata/postgres/legacy-rbac-matrix.json"

// viewerSecretListing is the one string shared/0083 adds to the default-mode
// viewer and shared/0090 withdraws again on a pylon-managed database.
const viewerSecretListing = "configuration.secrets.secret.list"

/* ── the blast radius ──────────────────────────────────────────────────── */

// A viewer on a pylon-backed database keeps the matrix split for the secrets
// surface, and an editor keeps the listing the matrix gives them.
//
// The second half is what makes the first half mean anything. A pylon shape
// that resolved NOTHING would pass a bare "the viewer is refused" assertion
// while proving that the fallback never ran.
func TestAPylonBackedViewerKeepsTheMatrixSecretSplit(t *testing.T) {
	pool := newPylonShapedPool(t)

	seedRoleMembership(t, pool, 9001, "viewer", 1)
	seedPylonMember(t, pool, 9002)
	seedRoleMembership(t, pool, 9003, "editor", 9002)

	viewer := resolveDefaultModeFor(t, pool, "1", "1")
	editor := resolveDefaultModeFor(t, pool, "9002", "1")

	if !slices.Contains(editor.Permissions, viewerSecretListing) {
		t.Fatalf("the editor does not resolve %s on a pylon-shaped database, so this "+
			"test cannot discriminate a refusal from a dead fallback. Resolved %d "+
			"permissions.", viewerSecretListing, len(editor.Permissions))
	}
	if slices.Contains(viewer.Permissions, viewerSecretListing) {
		t.Errorf("a project viewer resolves %s on a pylon-backed database.\n"+
			"  The legacy matrix withholds it from that role, and pylon's own listing\n"+
			"  route declares viewer: False for the default mode. shared/0083 added the\n"+
			"  row and stated that no existing deployment's members gain anything.",
			viewerSecretListing)
	}
}

// The withdrawal takes one row and no more.
//
// A DELETE that matched on the permission alone would strip the string from the
// admin and the editor too, and every secrets route would answer 403 to the
// people who administer the project.
func TestThePylonWithdrawalTakesOnlyTheViewerRow(t *testing.T) {
	pool := newPylonShapedPool(t)

	seedRoleMembership(t, pool, 9011, "admin", 1)
	admin := resolveDefaultModeFor(t, pool, "1", "1")

	for _, permission := range slices.Concat([]string{viewerSecretListing}, secretValueStrings) {
		if !slices.Contains(admin.Permissions, permission) {
			t.Errorf("a project admin does not resolve %s on a pylon-backed database. "+
				"shared/0090 must withdraw the viewer's row and nothing else.", permission)
		}
	}
}

// The viewer keeps everything the matrix gives it.
//
// The matrix gives the default-mode viewer 84 permissions. A withdrawal that
// removed more than the one deviating row would take the project away from a
// read-only member without any migration saying so.
func TestAPylonBackedViewerKeepsTheMatrixGrants(t *testing.T) {
	pool := newPylonShapedPool(t)

	seedRoleMembership(t, pool, 9021, "viewer", 1)
	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	// shared/0136 removes, on purpose, the strings no code checks (#6874). The
	// matrix still lists some of them for the viewer; losing them grants and
	// refuses nothing, because no gate reads them.
	retired := retiredBy0136(t)
	for _, permission := range matrixPermissionsFor(t, "default", "viewer") {
		if permission == viewerSecretListing || retired[permission] {
			continue
		}
		if !slices.Contains(resolution.Permissions, permission) {
			t.Errorf("a project viewer does not resolve %s, which the legacy matrix "+
				"gives the default-mode viewer", permission)
		}
	}
}

// The two avatar strings stay, and shared/0090 says why.
//
// `models.social.avatar.update` is not in pylon's catalogue: shared/0080 mapped
// it onto the legacy name `models.social.avatar.post`. `models.social.avatar.get`
// gates legacy/plugins/social/api/v2/avatar.py, which lists the plugin's STOCK
// avatar image files and reads no project data. So neither row widens a pylon
// route, and withdrawing either one would only stop an editor and a viewer from
// seeing their own picture in the Go product.
func TestAPylonBackedEditorAndViewerKeepTheOwnAvatarRoutes(t *testing.T) {
	pool := newPylonShapedPool(t)

	seedRoleMembership(t, pool, 9031, "viewer", 1)
	seedPylonMember(t, pool, 9032)
	seedRoleMembership(t, pool, 9033, "editor", 9032)

	for userID, roleName := range map[string]string{"1": "viewer", "9032": "editor"} {
		resolution := resolveDefaultModeFor(t, pool, userID, "1")
		for _, permission := range avatarStrings {
			if !slices.Contains(resolution.Permissions, permission) {
				t.Errorf("a %s does not resolve %s on a pylon-backed database. "+
					"shared/0090 withdraws the secret listing and leaves the avatar "+
					"routes alone.", roleName, permission)
			}
		}
	}
}

// The Go-managed database keeps the #402 decision.
//
// shared/0090 acts on ownership of auth_core, not on a row count. A guard that
// read "auth_core__project_role_permission is empty" would fire on the fresh Go
// database as well and would undo #402 everywhere.
func TestAGoManagedViewerStillResolvesTheSecretListing(t *testing.T) {
	pool := newMigratedPool(t)

	seedRoleMembership(t, pool, 9041, "viewer", 1)
	resolution := resolveDefaultModeFor(t, pool, "1", "1")

	if !slices.Contains(resolution.Permissions, viewerSecretListing) {
		t.Errorf("a viewer on a Go-managed database does not resolve %s. "+
			"shared/0090 must act only where pylon owns auth_core.", viewerSecretListing)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// legacyMatrix is the shape of testdata/postgres/legacy-rbac-matrix.json that
// this file reads. The export carries more, and none of the rest is needed.
type legacyMatrix struct {
	GlobalRoles []struct {
		Name        string   `json:"name"`
		Mode        string   `json:"mode"`
		Permissions []string `json:"permissions"`
	} `json:"global_roles"`
}

func readLegacyMatrix(t *testing.T) legacyMatrix {
	t.Helper()
	body, err := os.ReadFile(legacyMatrixPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyMatrixPath, err)
	}
	var matrix legacyMatrix
	if err := json.Unmarshal(body, &matrix); err != nil {
		t.Fatalf("parse %s: %v", legacyMatrixPath, err)
	}
	if len(matrix.GlobalRoles) == 0 {
		t.Fatalf("%s carries no central role, so it cannot build the pylon shape",
			legacyMatrixPath)
	}
	return matrix
}

func matrixPermissionsFor(t *testing.T, mode, name string) []string {
	t.Helper()
	for _, role := range readLegacyMatrix(t).GlobalRoles {
		if role.Mode == mode && role.Name == name {
			return role.Permissions
		}
	}
	t.Fatalf("%s has no %s-mode role named %q", legacyMatrixPath, mode, name)
	return nil
}

// newPylonShapedPool builds a database whose auth_core is PYLON's, and then
// applies this repository's shared corpus to it.
//
// The steps match a cutover. 001_initial.sql supplies the table shapes and the
// product tables the corpus needs; its Go role seeds are then replaced by the
// exported pylon matrix, central grants included and per-project grants
// deliberately absent; the ledgered corpus runs last, exactly as elitea-migrate
// runs it in deploy/centry-hybrid/pov-compose.yml.
func newPylonShapedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newIsolatedDatabase(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	bootstrap, err := os.ReadFile(bootstrapSchemaSQ)
	if err != nil {
		t.Fatalf("read bootstrap schema: %v", err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply bootstrap schema: %v", err)
	}

	loadPylonAuthCore(t, pool)

	runner := migrate.New(pool, migrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}
	return pool
}

// loadPylonAuthCore replaces the Go role seeds with the exported pylon matrix.
//
// The DELETE cascades to auth_core__role_permission and auth_core__user_role,
// so nothing 001_initial.sql seeded survives. No auth_core__project_role_permission
// row is written, because pylon's own consolidation migration truncated that
// table and nothing repopulates it.
func loadPylonAuthCore(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	matrix := readLegacyMatrix(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__role`); err != nil {
		t.Fatalf("clear the Go role seeds: %v", err)
	}

	for _, role := range matrix.GlobalRoles {
		var roleID int
		if err := pool.QueryRow(ctx,
			`INSERT INTO public.auth_core__role (name, mode) VALUES ($1, $2) RETURNING id`,
			role.Name, role.Mode).Scan(&roleID); err != nil {
			t.Fatalf("seed the %s-mode %q role: %v", role.Mode, role.Name, err)
		}
		for _, permission := range role.Permissions {
			if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
VALUES ($1, $2) ON CONFLICT (role_id, permission) DO NOTHING`,
				roleID, permission); err != nil {
				t.Fatalf("seed the %q grant on the %s-mode %q role: %v",
					permission, role.Mode, role.Name, err)
			}
		}
	}

	// The project roles pylon creates, with no permission row of their own.
	// project 1 is the one 001_initial.sql creates, and the one the tests above
	// resolve in. seedRoleMembership supplies the assignment.
	var overrideRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.auth_core__project_role_permission`).Scan(&overrideRows); err != nil {
		t.Fatalf("count the per-project grants: %v", err)
	}
	if overrideRows != 0 {
		t.Fatalf("the pylon shape carries %d per-project permission rows, so the "+
			"central fallback would be suppressed and this harness would measure "+
			"nothing", overrideRows)
	}
}

// seedPylonMember creates a second user, so that two roles can be resolved in
// the same database. requireUser() refuses a caller with no auth_core__user row.
func seedPylonMember(t *testing.T, pool *pgxpool.Pool, userID int) {
	t.Helper()
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, $2, 'Pylon member')
ON CONFLICT (id) DO NOTHING`, userID, fmt.Sprintf("pylon-%d@example.com", userID)); err != nil {
		t.Fatalf("seed the pylon member: %v", err)
	}
}

// newIsolatedDatabase creates a throwaway database and returns a pool on it.
//
// newMigratedPool does the same and then applies the corpus in one step, which
// leaves no point at which auth_core can be given pylon's shape. The creation
// half is repeated here rather than extracted, so that this file owns its own
// harness.
func newIsolatedDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv(databaseURLEnv)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		t.Skipf("set %s to run the pylon-shape blast-radius test", databaseURLEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_pylon_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx,
		"CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURLEnv, err)
	}
	config.ConnConfig.Database = databaseName
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}
