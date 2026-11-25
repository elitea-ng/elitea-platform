package migrations_test

// Shared 0279 (issue 2552, ADR-0059 R0.15): agents.governance.view and
// agents.governance.manage, granted to super_admin and admin. No existing
// grant is removed, a second run writes nothing, and a database without the
// auth_core tables is left alone.
//
// The two databases of the 0261 test (narrow_admin_permissions_..._test.go):
// fresh (001_initial.sql and the corpus before 0279) and legacy (the central
// roles of testdata/postgres/legacy-rbac-matrix.json, upgraded through the
// same corpus).

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

const (
	agentGovernanceFile      = "0279_agent_governance_permissions.sql"
	agentGovernanceMigration = "shared/" + agentGovernanceFile
)

// agentGovernancePermissions are the two names 0279 grants. Written out, not
// read from the registry: the test pins the grant.
var agentGovernancePermissions = []string{"agents.governance.manage", "agents.governance.view"}

func TestAgentGovernancePermissionsKeepEveryGrantAndAddOnlyTheirOwn(t *testing.T) {
	for name, build := range map[string]func(*testing.T) *pgxpool.Pool{
		"fresh":  newPoolBefore0279,
		"legacy": newLegacyPoolBefore0279,
	} {
		t.Run(name, func(t *testing.T) {
			pool := build(t)
			before := allRoleGrants(t, pool)
			if len(before) == 0 {
				t.Fatal("the database holds no grant before 0279; the fixture was not built")
			}
			applySharedCorpus(t, pool)
			after := allRoleGrants(t, pool)

			if lost := grantDifference(before, after); len(lost) > 0 {
				t.Errorf("0279 removed %d grants:\n  %s", len(lost), strings.Join(lost, "\n  "))
			}
			want := map[string]bool{}
			for _, permission := range agentGovernancePermissions {
				for _, role := range []string{"admin", "super_admin"} {
					want[roleGrant{"administration", role, permission}.String()] = true
				}
			}
			for _, row := range grantDifference(after, before) {
				if !want[row] {
					t.Errorf("0279 added an unexpected grant %s", row)
				}
				delete(want, row)
			}
			for row := range want {
				t.Errorf("0279 did not add %s", row)
			}
			for _, permission := range agentGovernancePermissions {
				holders := administrationGrantHolders(t, pool, permission)
				if !slices.Equal(holders, []string{"admin", "super_admin"}) {
					t.Errorf("administration-mode holders of %s = %v, want [admin super_admin]", permission, holders)
				}
			}
		})
	}
}

// Applying the file a second time writes nothing.
func TestAgentGovernancePermissionsReapplyWritesNothing(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	read := func() (count, lastID int) {
		if err := pool.QueryRow(ctx,
			`SELECT count(*), coalesce(max(id), 0) FROM public.auth_core__role_permission`).Scan(&count, &lastID); err != nil {
			t.Fatalf("read the grants: %v", err)
		}
		return count, lastID
	}
	count, lastID := read()
	execMigrationFile(t, pool, agentGovernanceMigration)
	if gotCount, gotID := read(); gotCount != count || gotID != lastID {
		t.Fatalf("re-applying 0279 wrote rows: %d grants (last id %d), was %d (last id %d)",
			gotCount, gotID, count, lastID)
	}
}

// On a database without the auth_core tables the file is a no-op, twice.
func TestAgentGovernancePermissionsIsANoOpWithoutAuthCore(t *testing.T) {
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
		execMigrationFile(t, pool, agentGovernanceMigration)
	}
	if after := relations(); after != before {
		t.Fatalf("0279 created %d relations on a database without auth_core", after-before)
	}
}

// The registry and the migration agree: both names are rows of the Agents and
// governance area, and no built-in narrow role declares either (the agent
// governance role is deferred, owner decision 10).
func TestAgentGovernancePermissionsAreRegistryRows(t *testing.T) {
	t.Parallel()
	for _, name := range agentGovernancePermissions {
		row, ok := adminaccess.Lookup(name)
		if !ok {
			t.Errorf("0279 grants %q, which is not a row of internal/adminaccess", name)
			continue
		}
		if row.Area != adminaccess.AreaAgentsGovernance {
			t.Errorf("%s is in the area %q, want %q", name, row.Area, adminaccess.AreaAgentsGovernance)
		}
		for _, role := range adminaccess.BuiltInRoles() {
			if role.DeclaresSet() && slices.Contains(role.Set(), name) && role.Name != "admin" && role.Name != "super_admin" {
				t.Errorf("the built-in role %s declares %s; no narrow role holds the pair", role.Name, name)
			}
		}
	}
}

/* ── harness ───────────────────────────────────────────────────────────── */

func newPoolBefore0279(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	applyCorpusWithout0279(t, pool)
	return pool
}

func newLegacyPoolBefore0279(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newEmptyDatabase(t)
	applyBootstrapSchema(t, pool)
	loadLegacyRBACMatrix(t, pool)
	applyCorpusWithout0279(t, pool)
	return pool
}

func applyCorpusWithout0279(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	corpus := withoutFile{FS: migrations.Files, hidden: agentGovernanceFile}
	if err := migrate.New(pool, corpus).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus before 0279: %v", err)
	}
	var applied bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM public.auth_core__role_permission WHERE permission = ANY($1))`,
		agentGovernancePermissions).Scan(&applied); err != nil {
		t.Fatalf("read the pre-upgrade grants: %v", err)
	}
	if applied {
		t.Fatal("the pre-upgrade database already holds the agent governance pair: 0279 was not hidden")
	}
}
