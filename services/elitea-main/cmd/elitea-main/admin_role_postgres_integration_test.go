package main

// `elitea-main admin-role` against the full ledgered corpus (the dbtest
// template of newMigrateTestDatabase, copied per test). CI-only: skipped
// without ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

type adminRoleRun struct {
	code           int
	stdout, stderr string
}

func runAdminRoleAgainst(t *testing.T, dsn string, args ...string) adminRoleRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runAdminRoleWith(context.Background(), args,
		func(key string) (string, bool) { return dsn, key == "DATABASE_URL" }, &stdout, &stderr, pgxpool.New)
	return adminRoleRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (r adminRoleRun) expect(t *testing.T, code int, text string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", r.code, code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout+r.stderr, text) {
		t.Fatalf("output does not contain %q\nstdout: %s\nstderr: %s", text, r.stdout, r.stderr)
	}
}

func adminRoleAccount(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("create %s: %v", email, err)
	}
	return id
}

func heldAdministrationRoles(t *testing.T, pool *pgxpool.Pool, userID int64) []string {
	t.Helper()
	roles, err := adminaccess.UserCentralRoles(t.Context(), pool, int(userID), adminaccess.ModeAdministration)
	if err != nil {
		t.Fatal(err)
	}
	return roles
}

func operatorHistory(t *testing.T, pool *pgxpool.Pool, userID int64) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT action || ':' || role_name FROM elitea_auth.admin_role_grants
WHERE user_id = $1 AND source = 'operator' AND actor_user_id IS NULL ORDER BY id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	history, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// accessEvents are the chained events of one action: the reason codes of
// each, after checking it has a chain position and a hash.
func accessEvents(t *testing.T, pool *pgxpool.Pool, action string) [][]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT partition_seq, octet_length(hash), event->'data'->'reasons'
FROM elitea_runtime.security_audit_outbox
WHERE event->'data'->>'action' = $1 ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events [][]string
	for rows.Next() {
		var (
			seq     int64
			hashLen int
			reasons []string
		)
		if err := rows.Scan(&seq, &hashLen, &reasons); err != nil {
			t.Fatal(err)
		}
		if seq <= 0 || hashLen != 32 {
			t.Fatalf("event of %s is not chained: seq %d, hash %d bytes", action, seq, hashLen)
		}
		events = append(events, reasons)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// Acceptance (1): on a database with no administrator, the command grants a
// role, writes one history row with source operator and one chained event.
func TestAdminRoleGrantsOnADatabaseWithNoAdministrator(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	if counts, err := adminaccess.CountAdministrators(t.Context(), pool); err != nil || counts.Protected() {
		t.Fatalf("a fresh database has an administrator: %+v (err %v)", counts, err)
	}
	id := adminRoleAccount(t, pool, "first-admin@corp.example")

	runAdminRoleAgainst(t, dsn, "grant", "First-Admin@corp.example", "super_admin", "--reason", "first administrator").
		expect(t, adminRoleExitOK, "granted super_admin to user")
	if got := heldAdministrationRoles(t, pool, id); !slices.Equal(got, []string{"super_admin"}) {
		t.Fatalf("holds %v, want super_admin", got)
	}
	if history := operatorHistory(t, pool, id); !slices.Equal(history, []string{"granted:super_admin"}) {
		t.Fatalf("history %v, want one operator grant", history)
	}
	events := accessEvents(t, pool, adminaccess.AccessActionRoleAssigned)
	if len(events) != 1 || !slices.Contains(events[0], "source:operator") {
		t.Fatalf("events %v, want one with source operator", events)
	}

	// Again: nothing to change, nothing written.
	runAdminRoleAgainst(t, dsn, "grant", strconv.FormatInt(id, 10), "super_admin").
		expect(t, adminRoleExitOK, "nothing to change")
	if history := operatorHistory(t, pool, id); len(history) != 1 {
		t.Fatalf("a no-op grant wrote history: %v", history)
	}

	// list names the holder and the marker.
	runAdminRoleAgainst(t, dsn, "list").expect(t, adminRoleExitOK, "super_admin [declared]: 1 holder(s)")
}

// Acceptance (3): a revoke of the last administrator needs
// --override-last-administrator and is refused without it.
func TestAdminRoleRevokeOfTheLastAdministratorNeedsTheOverride(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	id := adminRoleAccount(t, pool, "last@corp.example")
	runAdminRoleAgainst(t, dsn, "grant", "last@corp.example", "super_admin").expect(t, adminRoleExitOK, "granted")

	runAdminRoleAgainst(t, dsn, "revoke", "last@corp.example", "super_admin").
		expect(t, adminRoleExitRefused, "--override-last-administrator")
	if got := heldAdministrationRoles(t, pool, id); !slices.Equal(got, []string{"super_admin"}) {
		t.Fatalf("a refused revoke changed the roles: %v", got)
	}
	if history := operatorHistory(t, pool, id); len(history) != 1 {
		t.Fatalf("a refused revoke wrote history: %v", history)
	}

	runAdminRoleAgainst(t, dsn, "revoke", "last@corp.example", "super_admin", "--override-last-administrator").
		expect(t, adminRoleExitOK, "no administrator")
	if got := heldAdministrationRoles(t, pool, id); len(got) != 0 {
		t.Fatalf("still holds %v", got)
	}
	events := accessEvents(t, pool, adminaccess.AccessActionRoleRemoved)
	if len(events) != 1 || !slices.Contains(events[0], "severity:critical") ||
		!slices.Contains(events[0], "override:last_administrator") {
		t.Fatalf("events %v, want one critical override", events)
	}

	// Without the rule in play (a second holder), no flag is needed.
	second := adminRoleAccount(t, pool, "second@corp.example")
	runAdminRoleAgainst(t, dsn, "grant", "last@corp.example", "super_admin").expect(t, adminRoleExitOK, "granted")
	runAdminRoleAgainst(t, dsn, "grant", strconv.FormatInt(second, 10), "super_admin").expect(t, adminRoleExitOK, "granted")
	runAdminRoleAgainst(t, dsn, "revoke", "last@corp.example", "super_admin").expect(t, adminRoleExitOK, "revoked")
}

// Acceptance (4): `reset admin` on a database where admin lost one registry
// permission and gained one extra restores exactly the registry set and
// writes one critical event; `reset editor` is refused.
func TestAdminRoleResetRestoresTheDeclaredSet(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	ctx := t.Context()
	declared, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleAdmin)
	lost := adminaccess.LLMCatalogueManage
	extra := adminaccess.ConfigurationServiceDescriptors
	if !slices.Contains(declared.Set(), lost) || slices.Contains(declared.Set(), extra) {
		t.Fatalf("fixture: %s must be in the admin set and %s outside it", lost, extra)
	}
	if _, err := pool.Exec(ctx, `
DELETE FROM public.auth_core__role_permission grant_row USING public.auth_core__role role
WHERE grant_row.role_id = role.id AND role.mode = 'administration' AND role.name = 'admin'
  AND grant_row.permission = $1`, lost); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, $1 FROM public.auth_core__role WHERE mode = 'administration' AND name = 'admin'`, extra); err != nil {
		t.Fatal(err)
	}

	run := runAdminRoleAgainst(t, dsn, "reset", "admin")
	run.expect(t, adminRoleExitOK, "admin: added "+lost)
	run.expect(t, adminRoleExitOK, "admin: removed "+extra)
	got, err := adminaccess.CentralRolePermissions(ctx, pool, adminaccess.ModeAdministration, []string{"admin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, declared.Set()) {
		t.Fatalf("after reset admin holds %d permissions, want exactly the %d of the registry", len(got), len(declared.Set()))
	}
	events := accessEvents(t, pool, adminaccess.AccessActionRoleSetReset)
	if len(events) != 1 || !slices.Contains(events[0], "severity:critical") ||
		!slices.Contains(events[0], "added:"+lost) || !slices.Contains(events[0], "removed:"+extra) {
		t.Fatalf("events %v, want one critical reset naming the change", events)
	}

	// Nothing left to reset: no second event.
	runAdminRoleAgainst(t, dsn, "reset", "admin").expect(t, adminRoleExitOK, "nothing to change")
	if events := accessEvents(t, pool, adminaccess.AccessActionRoleSetReset); len(events) != 1 {
		t.Fatalf("a no-op reset wrote an event: %v", events)
	}

	runAdminRoleAgainst(t, dsn, "reset", "editor").expect(t, adminRoleExitRefused, "marked as_found")
	runAdminRoleAgainst(t, dsn, "reset", "no_such_role").expect(t, adminRoleExitUsage, `no administration role "no_such_role"`)
}

// A binary older than the database refuses a reset: its registry could lack
// a permission that the newer release granted.
func TestAdminRoleResetRefusesABinaryOlderThanTheDatabase(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	manifest, err := migrate.LoadManifest(platformmigrations.Files, migrate.ScopeShared)
	if err != nil {
		t.Fatal(err)
	}
	newer := migrate.Head(manifest) + 1
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.schema_migrations (target_kind, target_id, version, name, checksum)
SELECT target_kind, target_id, $1, 'from_a_newer_release', checksum
FROM elitea_runtime.schema_migrations WHERE target_kind = 'shared' ORDER BY version DESC LIMIT 1`, newer); err != nil {
		t.Fatal(err)
	}
	runAdminRoleAgainst(t, dsn, "reset", "admin").expect(t, adminRoleExitRefused, "this binary knows")
}

// Unknown and ambiguous users, unknown and never-assigned roles: refused with
// no write.
func TestAdminRoleRefusesUnknownUsersAndRoles(t *testing.T) {
	pool, dsn := newMigrateTestDatabase(t)
	adminRoleAccount(t, pool, "Twin@corp.example")
	adminRoleAccount(t, pool, "twin@corp.example")
	plain := adminRoleAccount(t, pool, "plain@corp.example")

	runAdminRoleAgainst(t, dsn, "grant", "nobody@corp.example", "admin").expect(t, adminRoleExitUsage, `no user "nobody@corp.example"`)
	runAdminRoleAgainst(t, dsn, "grant", "999999", "admin").expect(t, adminRoleExitUsage, `no user "999999"`)
	runAdminRoleAgainst(t, dsn, "grant", "TWIN@corp.example", "admin").expect(t, adminRoleExitUsage, "more than one account")
	runAdminRoleAgainst(t, dsn, "grant", "plain@corp.example", "no_such_role").expect(t, adminRoleExitUsage, `no administration role "no_such_role"`)
	runAdminRoleAgainst(t, dsn, "revoke", "plain@corp.example", "admin").expect(t, adminRoleExitOK, "does not hold admin")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO public.auth_core__role (name, mode) VALUES ('system', 'administration')`); err != nil {
		t.Fatal(err)
	}
	runAdminRoleAgainst(t, dsn, "grant", "plain@corp.example", "system").expect(t, adminRoleExitRefused, "never assigned")
	if history := operatorHistory(t, pool, plain); len(history) != 0 {
		t.Fatalf("a refused invocation wrote history: %v", history)
	}
}
