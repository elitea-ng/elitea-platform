package migrations_test

// Shared 0261 (issue 2491, ADR-0059 R0.2, migration A of decision 4): the 25
// narrow administration permissions, granted to super_admin and admin, with
// identity.sso.trust.manage to super_admin alone. No existing grant is
// removed.
//
// Two databases, because they differ in the roles they hold:
//
//   - fresh: 001_initial.sql plus the released corpus (everything before
//     0261). 001_initial.sql alone is not a deployment: names such as
//     configuration.governance exist only in the shared corpus.
//   - legacy: 001_initial.sql whose central roles are replaced by
//     testdata/postgres/legacy-rbac-matrix.json (fifteen roles, `system`
//     among them, and a non-empty editor and viewer), then the released
//     corpus, as a legacy-backed deployment upgrades.
//
// On each, the grants are read before and after 0261: every grant held before
// is held after, and the only rows added are the expected ones.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	narrowPermissionsFile      = "0261_narrow_admin_permissions.sql"
	narrowPermissionsMigration = "shared/" + narrowPermissionsFile
	legacyRBACMatrixFixture    = "../../../testdata/postgres/legacy-rbac-matrix.json"
)

// narrowAdminPermissions are the 24 names 0261 gives to both administrator
// roles. Written out, not read from the registry: the test pins the grant.
var narrowAdminPermissions = []string{
	"access.breakglass.approve",
	"access.breakglass.manage",
	"access.breakglass.request",
	"access.breakglass.review",
	"access.roles.view",
	"identity.sso.settings.manage",
	"identity.sso.view",
	"llm.alerts.manage",
	"llm.catalogue.manage",
	"llm.catalogue.view",
	"llm.guardrails.manage",
	"llm.policy.manage",
	"llm.policy.view",
	"llm.usage.view",
	"mcp.catalogue.manage",
	"platform.config.manage",
	"platform.config.view",
	"platform.email.manage",
	"platform.maintenance.manage",
	"platform.operations.manage",
	"platform.operations.view",
	"projects.members.manage",
	"projects.settings.manage",
	"projects.settings.view",
}

// identityTrustPermission goes to super_admin alone.
const identityTrustPermission = "identity.sso.trust.manage"

// roleGrant is one (mode, role name, permission) row.
type roleGrant struct{ mode, role, permission string }

func (g roleGrant) String() string { return g.mode + "/" + g.role + ": " + g.permission }

func TestNarrowAdminPermissionsKeepEveryGrantAndAddOnlyTheirOwn(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"fresh":  newPoolBefore0261,
		"legacy": newLegacyPoolBefore0261,
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := allRoleGrants(t, pool)
			if len(before) == 0 {
				t.Fatal("the database holds no grant before 0261; the fixture was not built")
			}
			applySharedCorpus(t, pool)
			after := allRoleGrants(t, pool)

			// Nothing is lost.
			if lost := grantDifference(before, after); len(lost) > 0 {
				t.Errorf("0261 removed %d grants:\n  %s", len(lost), strings.Join(lost, "\n  "))
			}

			// Exactly the expected rows are added. A grant the role already
			// held (a legacy dump never holds these names) is no new row.
			want := map[string]bool{}
			for _, permission := range narrowAdminPermissions {
				for _, role := range []string{"admin", "super_admin"} {
					want[roleGrant{"administration", role, permission}.String()] = true
				}
			}
			want[roleGrant{"administration", "super_admin", identityTrustPermission}.String()] = true
			for _, held := range before {
				delete(want, held.String())
			}
			added := grantDifference(after, before)
			for _, row := range added {
				if !want[row] {
					t.Errorf("0261 added an unexpected grant %s", row)
				}
				delete(want, row)
			}
			for row := range want {
				t.Errorf("0261 did not add %s", row)
			}

			// Identity trust: super_admin, never admin.
			holders := administrationGrantHolders(t, pool, identityTrustPermission)
			if !slices.Equal(holders, []string{"super_admin"}) {
				t.Errorf("administration-mode holders of %s = %v, want [super_admin]",
					identityTrustPermission, holders)
			}
		})
	}
}

// Applying the file a second time writes nothing.
func TestNarrowAdminPermissionsReapplyWritesNothing(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	countRows := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.auth_core__role_permission`).Scan(&n); err != nil {
			t.Fatalf("count grants: %v", err)
		}
		return n
	}
	before := countRows()
	maxID := func() int {
		var id int
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(max(id), 0) FROM public.auth_core__role_permission`).Scan(&id); err != nil {
			t.Fatalf("read the last grant id: %v", err)
		}
		return id
	}
	lastID := maxID()
	execMigrationFile(t, pool, narrowPermissionsMigration)
	if after := countRows(); after != before {
		t.Fatalf("re-applying 0261 changed the grant count from %d to %d", before, after)
	}
	if id := maxID(); id != lastID {
		t.Fatalf("re-applying 0261 wrote a row: the last grant id moved from %d to %d", lastID, id)
	}
}

// On a database without the auth_core tables the file is a no-op, twice.
func TestNarrowAdminPermissionsIsANoOpWithoutAuthCore(t *testing.T) {
	pool := newEmptyDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	relations := func() int {
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_class AS c
JOIN pg_namespace AS n ON n.oid = c.relnamespace
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'`).Scan(&n); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return n
	}
	before := relations()
	for range 2 {
		execMigrationFile(t, pool, narrowPermissionsMigration)
	}
	if after := relations(); after != before {
		t.Fatalf("0261 created %d relations on a database without auth_core", after-before)
	}
}

// The upgrade note exists and names the one privilege increase.
func TestNarrowAdminPermissionsUpgradeNoteNamesTheProjectSettingsGrant(t *testing.T) {
	t.Parallel()
	notes, err := filepath.Glob("../../../docs/upgrading/*-2491-narrow-administration-permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("found %d upgrade notes for #2491 under docs/upgrading, want 1: %v", len(notes), notes)
	}
	body, err := os.ReadFile(notes[0]) //nolint:gosec // fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	const sentence = "Both administrator roles gain `projects.settings.view` and `projects.settings.manage`"
	if !strings.Contains(text, sentence) {
		t.Errorf("%s does not say %q", notes[0], sentence)
	}
	if !strings.Contains(text, narrowPermissionsFile) {
		t.Errorf("%s does not name the migration %s", notes[0], narrowPermissionsFile)
	}
}

// The registry and the migration agree: every name 0261 grants is a registry
// row, and identity trust is the registry's seeded_only name.
func TestNarrowAdminPermissionsAreRegistryRows(t *testing.T) {
	t.Parallel()
	for _, name := range append(slices.Clone(narrowAdminPermissions), identityTrustPermission) {
		if _, ok := adminaccess.Lookup(name); !ok {
			t.Errorf("0261 grants %q, which is not a row of internal/adminaccess", name)
		}
	}
	trust, _ := adminaccess.Lookup(identityTrustPermission)
	if trust.Grantable != adminaccess.GrantableSeededOnly || trust.Token {
		t.Errorf("%s = %+v, want seeded_only and never a token scope", identityTrustPermission, trust)
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

// newPoolBefore0261 is 001_initial.sql plus the released corpus: every shared
// migration before 0261.
func newPoolBefore0261(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	applyCorpusWithout0261(t, pool)
	return pool
}

// newLegacyPoolBefore0261 is 001_initial.sql with the central roles of the
// legacy matrix in place of its own, upgraded through the released corpus.
func newLegacyPoolBefore0261(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	loadLegacyRBACMatrix(t, pool)
	applyCorpusWithout0261(t, pool)

	// Premise: the legacy shape, not the fresh one.
	for _, role := range []string{"system", "editor", "viewer"} {
		var n int
		if err := pool.QueryRow(context.Background(), `
SELECT count(*) FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE role.mode = 'administration' AND role.name = $1`, role).Scan(&n); err != nil {
			t.Fatalf("count the grants of %s: %v", role, err)
		}
		if n == 0 {
			t.Fatalf("the legacy database's administration %s holds nothing; the fixture was not loaded", role)
		}
	}
	return pool
}

func applyBootstrapSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	bootstrap, err := os.ReadFile(bootstrapSchemaSQ)
	if err != nil {
		t.Fatalf("read bootstrap schema: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatalf("apply bootstrap schema: %v", err)
	}
}

func applyCorpusWithout0261(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	corpus := withoutFile{FS: migrations.Files, hidden: narrowPermissionsFile}
	if err := migrate.New(pool, corpus).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus before 0261: %v", err)
	}
	var applied bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM public.auth_core__role_permission
    WHERE permission = 'identity.sso.trust.manage')`).Scan(&applied); err != nil {
		t.Fatalf("read the pre-upgrade grants: %v", err)
	}
	if applied {
		t.Fatal("the pre-upgrade database already holds identity.sso.trust.manage: 0261 was not hidden")
	}
}

func applySharedCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply 0261: %v", err)
	}
}

func execMigrationFile(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	body, err := fs.ReadFile(migrations.Files, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, string(body))
		return err
	}); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

// loadLegacyRBACMatrix replaces the central roles 001_initial.sql seeds with
// the fixture's global roles and their grants. The fixture carries no user,
// so no assignment is lost.
func loadLegacyRBACMatrix(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	raw, err := os.ReadFile(legacyRBACMatrixFixture)
	if err != nil {
		t.Fatalf("read %s: %v", legacyRBACMatrixFixture, err)
	}
	var fixture struct {
		GlobalRoles []struct {
			Mode        string   `json:"mode"`
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		} `json:"global_roles"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse %s: %v", legacyRBACMatrixFixture, err)
	}
	if len(fixture.GlobalRoles) == 0 {
		t.Fatalf("%s has no global role", legacyRBACMatrixFixture)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_core__role`); err != nil {
			return fmt.Errorf("clear the seeded roles: %w", err)
		}
		for _, role := range fixture.GlobalRoles {
			var id int
			if err := tx.QueryRow(ctx, `
INSERT INTO public.auth_core__role (name, mode) VALUES ($1, $2) RETURNING id`,
				role.Name, role.Mode).Scan(&id); err != nil {
				return fmt.Errorf("insert role %s/%s: %w", role.Mode, role.Name, err)
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT $1, unnest($2::text[])`, id, role.Permissions); err != nil {
				return fmt.Errorf("insert the grants of %s/%s: %w", role.Mode, role.Name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("load %s: %v", legacyRBACMatrixFixture, err)
	}
}

// allRoleGrants reads every central grant, by role name and mode.
func allRoleGrants(t *testing.T, pool *pgxpool.Pool) []roleGrant {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
SELECT role.mode, role.name, grant_row.permission
FROM public.auth_core__role_permission AS grant_row
JOIN public.auth_core__role AS role ON role.id = grant_row.role_id
WHERE grant_row.permission IS NOT NULL
ORDER BY 1, 2, 3`)
	if err != nil {
		t.Fatalf("read the grants: %v", err)
	}
	defer rows.Close()
	var grants []roleGrant
	for rows.Next() {
		var g roleGrant
		if err := rows.Scan(&g.mode, &g.role, &g.permission); err != nil {
			t.Fatalf("scan a grant: %v", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the grants: %v", err)
	}
	return grants
}

// grantDifference is a minus b, as sorted strings.
func grantDifference(a, b []roleGrant) []string {
	inB := make(map[roleGrant]bool, len(b))
	for _, g := range b {
		inB[g] = true
	}
	var out []string
	for _, g := range a {
		if !inB[g] {
			out = append(out, g.String())
		}
	}
	slices.Sort(out)
	return out
}
