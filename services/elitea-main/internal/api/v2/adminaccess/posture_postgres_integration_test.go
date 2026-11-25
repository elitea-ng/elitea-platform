package adminaccess_test

// The checks of the access posture on PostgreSQL (ADR-0059 R2.14a, issue
// 2528): each one passing and failing from fixtures, through the route, on a
// fresh database (001_initial.sql and the shared corpus) and on the legacy
// matrix fixture (testdata/postgres/legacy-rbac-matrix.json, then the
// corpus).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

var postureClock = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

const legacyRBACMatrix = "../../../../../../testdata/postgres/legacy-rbac-matrix.json"

func postureDatabaseURL(t *testing.T) string {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	return databaseURL
}

// postureOf reads the list through the route as a security auditor.
func postureOf(t *testing.T, pool *pgxpool.Pool, initialAdmins ...string) postureResponse {
	t.Helper()
	handler := console.NewPostureHandler(registry.PostureSources{
		DB:                  pool,
		InitialGlobalAdmins: initialAdmins,
		GlobalAdministratorExists: func(ctx context.Context) (bool, error) {
			return identityrepo.GlobalAdministratorExists(ctx, pool)
		},
		Now: func() time.Time { return postureClock },
	})
	return decodePosture(t, servePosture(t, handler, postureDirectory(), auth.User{ID: "1", UserID: "1"}))
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
	}
}

func seedPostureUser(t *testing.T, pool *pgxpool.Pool, name string, roles ...string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2) RETURNING id`,
		name+"-"+uuid.NewString()[:8]+"@autotest.local", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		exec(t, pool, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE mode = 'administration' AND name = $2`, id, role)
	}
	return id
}

func customRole(t *testing.T, pool *pgxpool.Pool, name string, permissions ...string) {
	t.Helper()
	var id int
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration') RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if len(permissions) > 0 {
		exec(t, pool, `INSERT INTO public.auth_core__role_permission (role_id, permission) SELECT $1, unnest($2::text[])`,
			id, permissions)
	}
}

// expect asserts one check's severity and state.
func expect(t *testing.T, body postureResponse, id, severity, state string) (string, int, []string) {
	t.Helper()
	gotSeverity, gotState, text, count, roles := body.check(t, id)
	if gotSeverity != severity || gotState != state {
		t.Fatalf("%s: %s %s, want %s %s (%s)", id, gotSeverity, gotState, severity, state, text)
	}
	return text, count, roles
}

// On a fresh database every check of the story passes, and nothing counts
// as a failure.
func TestThePostureOfAFreshDatabasePasses(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_fresh")
	body := postureOf(t, pool)
	for _, c := range body.Checks {
		if c.State != registry.PostureStatePass {
			t.Errorf("%s (%s) does not pass on a fresh database: %s", c.ID, c.Severity, c.Text)
		}
	}
	if body.Failures != 0 || body.Warnings != 0 || body.Notices != 0 {
		t.Fatalf("a fresh database: %d failures, %d warnings, %d notices", body.Failures, body.Warnings, body.Notices)
	}
}

// fail: a role of a built-in name without a seed row. security_auditor
// with its seed row passes; without it, it fails.
func TestThePostureFailsARoleOfABuiltInNameWithoutASeedRow(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_collision")
	expect(t, postureOf(t, pool), registry.PostureBuiltInNameCollision, "fail", registry.PostureStatePass)
	exec(t, pool, `DELETE FROM elitea_auth.admin_role_seeds WHERE name = $1`, registry.RoleSecurityAuditor)
	body := postureOf(t, pool)
	text, count, roles := expect(t, body, registry.PostureBuiltInNameCollision, "fail", registry.PostureStateAttention)
	if count != 1 || !slices.Equal(roles, []string{registry.RoleSecurityAuditor}) || !strings.Contains(text, registry.RoleSecurityAuditor) {
		t.Fatalf("the collision names %v (%d): %s", roles, count, text)
	}
	if body.Failures < 1 {
		t.Fatalf("a failing fail check is not counted: %d", body.Failures)
	}
}

// fail: a seeded role whose set differs from the registry, with the reset
// command in the text; put back, it passes.
func TestThePostureFailsABuiltInRoleWhoseSetDrifted(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_drift")
	expect(t, postureOf(t, pool), registry.PostureBuiltInSetDrift, "fail", registry.PostureStatePass)
	exec(t, pool, `
DELETE FROM public.auth_core__role_permission
WHERE permission = $1 AND role_id = (SELECT id FROM public.auth_core__role WHERE mode = 'administration' AND name = $2)`,
		registry.AccessRolesView, registry.RoleAdmin)
	exec(t, pool, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT id, $1 FROM public.auth_core__role WHERE mode = 'administration' AND name = $2`,
		registry.RuntimePlugins, registry.RoleLLMManager)
	text, count, roles := expect(t, postureOf(t, pool), registry.PostureBuiltInSetDrift, "fail", registry.PostureStateAttention)
	if count != 2 || !slices.Equal(roles, []string{registry.RoleAdmin, registry.RoleLLMManager}) {
		t.Fatalf("the drift names %v (%d)", roles, count)
	}
	for _, command := range []string{"elitea-main admin-role reset admin", "elitea-main admin-role reset llm_manager"} {
		if !strings.Contains(text, command) {
			t.Errorf("the text does not carry %q: %s", command, text)
		}
	}
}

// warn: tokens with inherit. An expired one, a scoped one and a native
// session anchor (no uuid) do not count.
func TestThePostureWarnsOfInheritTokens(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_tokens")
	owner := seedPostureUser(t, pool, "owner")
	token := func(id any, access string, expires any) {
		exec(t, pool, `
INSERT INTO public.auth_core__token (uuid, expires, user_id, name, admin_access) VALUES ($1, $2, $3, 'test', $4)`,
			id, expires, owner, access)
	}
	token(uuid.NewString(), registry.TokenAccessInherit, postureClock.Add(-time.Hour))
	token(uuid.NewString(), registry.TokenAccessScoped, nil)
	token(nil, registry.TokenAccessInherit, nil)
	expect(t, postureOf(t, pool), registry.PostureInheritTokens, "warn", registry.PostureStatePass)
	token(uuid.NewString(), registry.TokenAccessInherit, nil)
	token(uuid.NewString(), registry.TokenAccessInherit, postureClock.Add(time.Hour))
	body := postureOf(t, pool)
	if _, count, _ := expect(t, body, registry.PostureInheritTokens, "warn", registry.PostureStateAttention); count != 2 {
		t.Fatalf("%d inherit tokens, want 2", count)
	}
	if body.Failures != 0 || body.Warnings != 1 {
		t.Fatalf("a warning is counted as %d failures, %d warnings", body.Failures, body.Warnings)
	}
}

// warn: a custom role that holds a legacy permission. A role NAMED like a
// legacy permission without holding one is not reported.
func TestThePostureWarnsOfCustomRolesWithALegacyPermission(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_legacy")
	customRole(t, pool, "runtime_plugins", registry.LLMUsageView)
	expect(t, postureOf(t, pool), registry.PostureCustomRoleLegacyPermission, "warn", registry.PostureStatePass)
	customRole(t, pool, "platform_ops", registry.RuntimePlugins, registry.LLMUsageView)
	customRole(t, pool, "governance_ops", registry.ConfigurationGovernance)
	text, count, roles := expect(t, postureOf(t, pool), registry.PostureCustomRoleLegacyPermission, "warn",
		registry.PostureStateAttention)
	if count != 2 || !slices.Equal(roles, []string{"governance_ops", "platform_ops"}) {
		t.Fatalf("the check names %v (%d)", roles, count)
	}
	if !strings.Contains(text, "not its name") {
		t.Errorf("the text does not say it is about the permissions: %s", text)
	}
}

// warn: identity.initial_global_admins configured while a global
// administrator exists; passes with no list, and with a list and no
// administrator.
func TestThePostureWarnsOfAnInitialAdminListThatOutlivedTheBootstrap(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_bootstrap")
	expect(t, postureOf(t, pool), registry.PostureInitialGlobalAdminsConfigured, "warn", registry.PostureStatePass)
	expect(t, postureOf(t, pool, "email:first@example.com"), registry.PostureInitialGlobalAdminsConfigured, "warn",
		registry.PostureStatePass)
	seedPostureUser(t, pool, "root", registry.RoleSuperAdmin)
	expect(t, postureOf(t, pool), registry.PostureInitialGlobalAdminsConfigured, "warn", registry.PostureStatePass)
	if _, count, _ := expect(t, postureOf(t, pool, "email:first@example.com", "oidc:subject"),
		registry.PostureInitialGlobalAdminsConfigured, "warn", registry.PostureStateAttention); count != 2 {
		t.Fatalf("%d entries, want 2", count)
	}
}

// info: the standing super_admin holders are counted, named and never a
// failure; a suspended holder is not one.
func TestThePostureCountsSuperAdminHoldersAsInformation(t *testing.T) {
	pool := isolatedDatabase(t, postureDatabaseURL(t), "elitea_posture_super")
	expect(t, postureOf(t, pool), registry.PostureSuperAdminHolders, "info", registry.PostureStatePass)
	root := seedPostureUser(t, pool, "root", registry.RoleSuperAdmin)
	suspended := seedPostureUser(t, pool, "gone", registry.RoleSuperAdmin)
	exec(t, pool, `UPDATE public.auth_core__user SET suspended = true WHERE id = $1`, suspended)
	body := postureOf(t, pool)
	if _, count, _ := expect(t, body, registry.PostureSuperAdminHolders, "info", registry.PostureStateAttention); count != 1 {
		t.Fatalf("%d holders, want 1", count)
	}
	for _, c := range body.Checks {
		if c.ID == registry.PostureSuperAdminHolders && (len(c.Facts.People) != 1 || c.Facts.People[0].ID != root) {
			t.Fatalf("the holders are %+v, want only %d", c.Facts.People, root)
		}
	}
	if body.Failures != 0 || body.Notices != 1 {
		t.Fatalf("the super_admin count is %d failures, %d notices; want 0, 1", body.Failures, body.Notices)
	}
}

// On the legacy matrix fixture: editor and viewer (as_found) do not trip the
// drift check; `system` exists (info) and, once held, warns.
func TestThePostureOfALegacyBackedDatabase(t *testing.T) {
	pool := legacyPostureDatabase(t)
	body := postureOf(t, pool)
	_, _, drifted := expect(t, body, registry.PostureBuiltInSetDrift, "fail", body.state(t, registry.PostureBuiltInSetDrift))
	for _, role := range []string{registry.RoleEditor, registry.RoleViewer, registry.RoleSystem} {
		if slices.Contains(drifted, role) {
			t.Errorf("%s (no declared set) trips the drift check: %v", role, drifted)
		}
	}
	expect(t, body, registry.PostureLegacySystemRoleExists, "info", registry.PostureStateAttention)
	expect(t, body, registry.PostureLegacySystemRoleHeld, "warn", registry.PostureStatePass)

	holder := seedPostureUser(t, pool, "legacy-system", registry.RoleSystem)
	body = postureOf(t, pool)
	_, count, _ := expect(t, body, registry.PostureLegacySystemRoleHeld, "warn", registry.PostureStateAttention)
	if count != 1 {
		t.Fatalf("%d holders of system, want 1 (%d)", count, holder)
	}
	expect(t, body, registry.PostureLegacySystemRoleExists, "info", registry.PostureStateAttention)
}

func (p postureResponse) state(t *testing.T, id string) string {
	t.Helper()
	_, state, _, _, _ := p.check(t, id)
	return state
}

// legacyPostureDatabase is 001_initial.sql with the central roles of the
// legacy matrix fixture, then the shared corpus.
func legacyPostureDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := postureDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	name := fmt.Sprintf("elitea_posture_legacy_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		dropPool, err := pgxpool.New(dropCtx, databaseURL)
		if err != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+quoted+" WITH (FORCE)")
	})
	bootstrap, err := os.ReadFile("../../../infra/db/migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(legacyRBACMatrix)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		GlobalRoles []struct {
			Mode        string   `json:"mode"`
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		} `json:"global_roles"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_core__role`); err != nil {
			return err
		}
		for _, role := range fixture.GlobalRoles {
			var id int
			if err := tx.QueryRow(ctx, `INSERT INTO public.auth_core__role (name, mode) VALUES ($1, $2) RETURNING id`,
				role.Name, role.Mode).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT $1, unnest($2::text[])`, id, role.Permissions); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("load %s: %v", legacyRBACMatrix, err)
	}
	if err := migrate.New(pool, migrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared corpus: %v", err)
	}
	return pool
}
