package projectprovisioning_test

// What these tests exist to catch.
//
// A project-create route that answers 201 while provisioning nothing is the
// exact defect this repository keeps re-shipping — issue #128's folders backend
// answered 200 from routes whose behaviour was absent, and its acceptance test
// passed because it asserted the status code. So nothing here asserts a return
// value alone. Every case reads the database back and compares it against the
// only tenant this repository knows to be correct: p_1, which the bootstrap and
// the migration corpus build between them.
//
// The three properties that matter, and the test that pins each:
//
//   TestProvisionBuildsATenantEqualToTheReference
//       the new tenant has exactly p_1's tables, plus the shared rows and RBAC
//       rows a project needs to be usable. Drop any step and this fails.
//   TestProvisionIsIdempotent
//       provisioning twice converges. This is what makes a retry after a
//       partial failure safe.
//   TestProvisionLeavesNothingBehindWhenAStepFails
//       a failure mid-pipeline removes the schema, the ledger rows, the RBAC
//       rows and the project row. A survivor with create_success = TRUE would
//       break elitea-migrate for the WHOLE deployment, not just this project.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const (
	databaseURLEnv  = "ELITEA_TEST_DATABASE_URL"
	bootstrapSchema = "../../infra/db/migrations/001_initial.sql"
)

// provisioningTemplate names the template database that every test in this
// package copies. TestMain sets it.
var provisioningTemplate string

// TestMain builds one template database for the whole package.
//
// Before #425 each of the 23 tests here replayed the ledgered migration corpus
// into its own database. Now the corpus runs one time, into a template, and
// each test gets its database from CREATE DATABASE ... TEMPLATE. The template
// is built by the same migrate.Runner that a deployment uses, and its name
// carries the SHA-256 checksum of every migration and of the bootstrap schema.
func TestMain(m *testing.M) {
	databaseURL := provisioningDatabaseURL()
	if databaseURL == "" {
		os.Exit(m.Run())
	}

	bootstrap, err := os.ReadFile(bootstrapSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bootstrap schema: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := dbtest.BuildContext(context.Background())
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin pool: %v\n", err)
		cancel()
		os.Exit(1)
	}
	templateName, err := dbtest.EnsureTemplate(ctx, adminPool, dbtest.Spec{
		Files:   platformmigrations.Files,
		Seed:    string(bootstrap),
		Tenants: []int64{1},
	})
	adminPool.Close()
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build project provisioning template: %v\n", err)
		os.Exit(1)
	}
	provisioningTemplate = templateName
	os.Exit(m.Run())
}

func provisioningDatabaseURL() string {
	databaseURL := os.Getenv(databaseURLEnv)
	if databaseURL == "" && os.Getenv("ELITEA_TEST_USE_SERVICE_DATABASE_URL") == "1" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	return databaseURL
}

func TestProvisionBuildsATenantEqualToTheReference(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A real account for the project_admin_email step to find.
	var adminUserID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('owner@example.test', 'Owner')
RETURNING id`).Scan(&adminUserID); err != nil {
		t.Fatalf("seed administrator account: %v", err)
	}

	// A DEPLOYMENT-DEFINED central role, of the kind
	// `POST /admin/roles/administration/default` now creates (gap G9). It is
	// seeded before provisioning so the RBAC assertion below can state the
	// second half of that gap's fix: a role added after the platform shipped
	// reaches projects created AFTERWARDS, because createProjectPermissions
	// copies whatever `auth_core__role` holds in the `default` mode rather than
	// a hardcoded list. The first half — reaching projects that already exist —
	// is "Apply to Projects", asserted in
	// internal/api/v2/admin/roles_crud_postgres_integration_test.go.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role (name, mode) VALUES ('security_reviewer', 'default')
ON CONFLICT (name, mode) DO NOTHING`); err != nil {
		t.Fatalf("seed a deployment-defined central role: %v", err)
	}

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name:        "Acceptance Project",
		OwnerID:     1,
		Plugins:     []string{"configuration", "models"},
		AdminEmails: []string{"OWNER@example.test"}, // case-insensitive on purpose
		AdminRoles:  []string{"admin"},
		Limits:      projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	if result.ProjectID <= 1 {
		t.Fatalf("project id = %d, want a newly allocated id greater than the seeded project 1", result.ProjectID)
	}
	projectID := result.ProjectID
	schema := fmt.Sprintf("p_%d", projectID)

	// ── the tenant itself ────────────────────────────────────────────────
	//
	// Compared against p_1 rather than against a hand-written list. A literal
	// list would have to be edited every time the corpus grows, and an
	// out-of-date list silently stops discriminating. p_1 is built by
	// 001_initial.sql's own create_tenant_schema call plus ApplyTenant, which is
	// the composition production runs.
	reference := schemaTables(ctx, t, pool, "p_1")
	provisioned := schemaTables(ctx, t, pool, schema)
	if len(reference) == 0 {
		t.Fatal("reference schema p_1 has no tables; the fixture is broken, not the code")
	}
	if !equalStrings(reference, provisioned) {
		t.Fatalf("tenant schema %s does not match the reference p_1\n missing: %v\n extra:   %v",
			schema, difference(reference, provisioned), difference(provisioned, reference))
	}

	// The tenant migration ledger must be at head for the new project, not just
	// for p_1. Without this, `elitea-migrate` would re-apply the corpus.
	if err := migrate.New(pool, platformmigrations.Files).
		CheckHead(ctx, migrate.ScopeTenant, fmt.Sprintf("%d", projectID)); err != nil {
		t.Fatalf("tenant migration head is not applied for project %d: %v", projectID, err)
	}

	// ── the shared rows ──────────────────────────────────────────────────
	var (
		createSuccess bool
		name          string
		ownerID       int64
		plugins       []string
	)
	if err := pool.QueryRow(ctx,
		`SELECT name, owner_id, plugins, create_success FROM centry.project WHERE id = $1`,
		projectID,
	).Scan(&name, &ownerID, &plugins, &createSuccess); err != nil {
		t.Fatalf("read project row: %v", err)
	}
	if !createSuccess {
		t.Error("create_success = false after a successful provision; every reader that filters on it would ignore this project")
	}
	if name != "Acceptance Project" || ownerID != 1 {
		t.Errorf("project row = (%q, owner %d), want (%q, owner 1)", name, ownerID, "Acceptance Project")
	}
	if !equalStrings([]string{"configuration", "models"}, plugins) {
		t.Errorf("plugins = %v, want [configuration models]", plugins)
	}

	// Quota and statistics: /projects/quota and /projects/statistics answer 404
	// without these rows, so a project provisioned without them reports "no such
	// project" on its own settings page.
	var vcuHard, storageHard int32
	if err := pool.QueryRow(ctx,
		`SELECT vcu_hard_limit, storage_hard_limit FROM centry.project_quota WHERE project_id = $1`,
		projectID,
	).Scan(&vcuHard, &storageHard); err != nil {
		t.Fatalf("read quota row: %v", err)
	}
	// The ProjectCreatePD defaults, asserted by value. A zero here would mean
	// the Go zero value reached the column instead of the reference's default —
	// a project that can spend nothing.
	if vcuHard != 5000 || storageHard != 10 {
		t.Errorf("quota = (vcu %d, storage %d), want (5000, 10)", vcuHard, storageHard)
	}

	var statisticRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.statistic WHERE project_id = $1`, projectID,
	).Scan(&statisticRows); err != nil {
		t.Fatalf("read statistic row: %v", err)
	}
	if statisticRows != 1 {
		t.Errorf("statistic rows = %d, want 1", statisticRows)
	}

	// ── RBAC ─────────────────────────────────────────────────────────────
	//
	// The roles must exist AND carry no per-project permission rows. That
	// emptiness is load-bearing, not an omission: legacyrbac falls back to the
	// central default-mode grants only for a project with no per-project rows,
	// so seeding permissions here would cut the project off from every grant
	// migration 0062-0069 adds.
	roles := projectRoleNames(ctx, t, pool, projectID)
	for _, want := range []string{"admin", "editor", "viewer", "system"} {
		if !contains(roles, want) {
			t.Errorf("project role %q was not created (have %v)", want, roles)
		}
	}
	// The deployment-defined role seeded above. Gap G9's second half: a role an
	// operator adds through the admin surface has to reach the projects created
	// after it, or every new project silently lacks a role the platform lists.
	if !contains(roles, "security_reviewer") {
		t.Errorf("the deployment-defined central default-mode role was not copied into the new project (have %v).\n"+
			"  createProjectPermissions must copy whatever auth_core__role holds in the `default` mode, not a fixed list.", roles)
	}
	var perProjectGrants int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.auth_core__project_role_permission WHERE project_id = $1`,
		projectID,
	).Scan(&perProjectGrants); err != nil {
		t.Fatalf("count per-project grants: %v", err)
	}
	if perProjectGrants != 0 {
		t.Errorf("per-project role_permission rows = %d, want 0 — any row here suppresses the central grant fallback",
			perProjectGrants)
	}

	// The named administrator is a member with the admin role.
	var adminAssigned bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
    WHERE assignment.project_id = $1 AND assignment.user_id = $2 AND role.name = 'admin'
)`, projectID, adminUserID).Scan(&adminAssigned); err != nil {
		t.Fatalf("check administrator assignment: %v", err)
	}
	if !adminAssigned {
		t.Error("project_admin_email did not become a project admin; the caller named an administrator who cannot reach the project")
	}

	// ── the system identity ──────────────────────────────────────────────
	//
	// This is the assertion that proves the project is USABLE rather than merely
	// present. queries/auth_pat.sql's GetActiveProjectSystemPAT is what
	// index_runtime_context, the index-schedule token and authcomposition all
	// resolve; it requires the system user, its project-role assignment, an
	// unexpired token named 'api', and create_success = true. Running the real
	// query is the only way to assert all five at once.
	var (
		patProjectID int64
		patUserID    int64
		patTokenID   int64
		patUUID      *string
		patExpires   *time.Time
		patEmail     string
	)
	if err := pool.QueryRow(ctx, `
SELECT project.id, owner.id, token.id, token.uuid, token.expires, COALESCE(owner.email, '')::text
FROM centry.project AS project
JOIN public.auth_core__user AS owner
  ON owner.email = ('system_user_' || project.id::text || '@centry.user')
JOIN public.auth_core__project_user_role AS assignment
  ON assignment.project_id = project.id AND assignment.user_id = owner.id
JOIN public.auth_core__project_role AS project_role
  ON project_role.id = assignment.role_id AND project_role.project_id = project.id
JOIN public.auth_core__token AS token ON token.user_id = owner.id
WHERE project.id = $1
  AND project.create_success = true
  AND project.suspended = false
  AND owner.suspended = false
  AND token.name = 'api'
  AND token.uuid IS NOT NULL
  AND (token.expires IS NULL OR token.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
ORDER BY token.id
LIMIT 1`, projectID).Scan(
		&patProjectID, &patUserID, &patTokenID, &patUUID, &patExpires, &patEmail,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("GetActiveProjectSystemPAT finds nothing for the new project: " +
				"indexing and scheduled index runs would fail with 'active project-system PAT not found'")
		}
		t.Fatalf("resolve project system PAT: %v", err)
	}
	if patEmail != fmt.Sprintf("system_user_%d@centry.user", projectID) {
		t.Errorf("system user email = %q, want system_user_%d@centry.user", patEmail, projectID)
	}
	if patUUID == nil || *patUUID == "" {
		t.Error("system PAT has no uuid; authsvc cannot sign a bearer form from it")
	}

	// The system user's NAME carries the prefix middleware/project_resolver.go
	// matches when mapping a system principal back to its project.
	var systemName *string
	if err := pool.QueryRow(ctx,
		`SELECT name FROM public.auth_core__user WHERE id = $1`, patUserID,
	).Scan(&systemName); err != nil {
		t.Fatalf("read system user name: %v", err)
	}
	if systemName == nil || *systemName != fmt.Sprintf(":system:project:%d:", projectID) {
		t.Errorf("system user name = %v, want :system:project:%d:", systemName, projectID)
	}

	// Every step reported success, and the successful path rolled nothing back.
	if len(result.RollbackSteps) != 0 {
		t.Errorf("rollback steps on a successful provision: %+v", result.RollbackSteps)
	}
	for _, status := range result.Steps {
		if status.OK == nil || !*status.OK {
			t.Errorf("step %q did not report success: %+v", status.Step, status)
		}
	}
}

// TestProvisionIsIdempotent proves a second provision of the SAME project
// converges instead of erroring or duplicating.
//
// This is the property that makes a retry after a partial failure safe, and it
// is not free: the schema function, the migration ledger, the role inserts, the
// system user and the token insert each had to be written to converge. A
// regression in any one of them shows up here.
func TestProvisionIsIdempotent(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	request := projectprovisioning.Request{
		Name: "Idempotent Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	}
	first, err := provisioner.Provision(ctx, request)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}

	// Re-running the tenant-side steps for the SAME project id is the retry an
	// operator performs after a partial failure. Provision allocates a new row
	// each time by design (the reference has no name uniqueness either), so the
	// convergence that matters is asserted directly against the steps that a
	// retry repeats.
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`,
		fmt.Sprintf("p_%d", first.ProjectID)); err != nil {
		t.Fatalf("re-run create_tenant_schema: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyTenant(ctx, first.ProjectID); err != nil {
		t.Fatalf("re-run ApplyTenant: %v", err)
	}

	tables := schemaTables(ctx, t, pool, fmt.Sprintf("p_%d", first.ProjectID))
	if !equalStrings(schemaTables(ctx, t, pool, "p_1"), tables) {
		t.Error("re-running the tenant steps changed the schema; provisioning is not idempotent")
	}

	// A second Provision with the same request must produce a SEPARATE project,
	// fully provisioned, rather than colliding with the first.
	second, err := provisioner.Provision(ctx, request)
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if second.ProjectID == first.ProjectID {
		t.Fatalf("second provision reused project id %d", second.ProjectID)
	}
	if !equalStrings(schemaTables(ctx, t, pool, "p_1"),
		schemaTables(ctx, t, pool, fmt.Sprintf("p_%d", second.ProjectID))) {
		t.Error("the second project's tenant does not match the reference")
	}
}

// failingMigrator fails ApplyTenant, which is the most dangerous point in the
// pipeline: the project row is already committed, the schema already exists,
// and an implementation that ignored the error would leave a project whose
// tenant is half-built.
type failingMigrator struct{ err error }

func (m failingMigrator) ApplyTenant(context.Context, int64) error { return m.err }

func TestProvisionLeavesNothingBehindWhenAStepFails(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	before := projectIDs(ctx, t, pool)

	provisioner := newTestProvisioner(t, pool, failingMigrator{err: errors.New("corpus unavailable")})
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Doomed Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err == nil {
		t.Fatal("provision reported success while the tenant corpus could not be applied")
	}
	if result.ProjectID == 0 {
		t.Fatal("the failure path reported no project id, so the test cannot check what was left behind")
	}
	projectID := result.ProjectID

	// The project row must be gone. A survivor is not a cosmetic leak: a
	// create_success = TRUE project with no schema makes cmd/elitea-migrate's
	// -all-tenants preflight fail for the ENTIRE deployment.
	var projectRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	if projectRows != 0 {
		t.Error("the failed project row survived compensation")
	}

	// And so must everything the completed steps created.
	for _, check := range []struct {
		what  string
		query string
		arg   any
	}{
		{"quota rows", `SELECT count(*) FROM centry.project_quota WHERE project_id = $1`, projectID},
		{"statistic rows", `SELECT count(*) FROM centry.statistic WHERE project_id = $1`, projectID},
		{"project roles", `SELECT count(*) FROM public.auth_core__project_role WHERE project_id = $1`, projectID},
		{"role assignments", `SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = $1`, projectID},
		// target_id is TEXT in the ledger, so the id is bound as one.
		{"tenant ledger rows", `SELECT count(*) FROM elitea_runtime.schema_migrations
                                WHERE target_kind = 'tenant' AND target_id = $1`, fmt.Sprintf("%d", projectID)},
	} {
		var rows int
		if err := pool.QueryRow(ctx, check.query, check.arg).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", check.what, err)
		}
		if rows != 0 {
			t.Errorf("%s survived compensation: %d left", check.what, rows)
		}
	}

	// The system user is keyed by the project id, so a survivor would collide
	// with the next project that draws the same id from a restored sequence.
	var systemUsers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM public.auth_core__user WHERE email = $1`,
		fmt.Sprintf("system_user_%d@centry.user", projectID),
	).Scan(&systemUsers); err != nil {
		t.Fatalf("count system users: %v", err)
	}
	if systemUsers != 0 {
		t.Errorf("the system user survived compensation: %d left", systemUsers)
	}

	// The schema itself.
	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		fmt.Sprintf("p_%d", projectID),
	).Scan(&schemaExists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaExists {
		t.Error("the tenant schema survived compensation")
	}

	// Nothing else moved.
	if after := projectIDs(ctx, t, pool); !equalStrings(before, after) {
		t.Errorf("the project table changed: before %v, after %v", before, after)
	}

	// The failure is reported per step, and the message carries no raw database
	// error across the boundary.
	var failed bool
	for _, status := range result.Steps {
		if status.OK != nil && !*status.OK {
			failed = true
			if status.Step != "project_schema" {
				t.Errorf("the failing step is reported as %q, want project_schema", status.Step)
			}
			if status.Msg == "corpus unavailable" {
				t.Error("the raw error crossed the trust boundary in the step message")
			}
		}
	}
	if !failed {
		t.Error("no step reported a failure, so the caller cannot tell what went wrong")
	}
	if len(result.RollbackSteps) == 0 {
		t.Error("no compensation was reported")
	}
}

func TestProvisionRejectsAnEmptyNameAndAnUnknownOwner(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))

	if _, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "   ", OwnerID: 1,
	}); !errors.Is(err, projectprovisioning.ErrNameRequired) {
		t.Errorf("blank name err = %v, want ErrNameRequired", err)
	}
	if _, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "No Owner", OwnerID: 0,
	}); !errors.Is(err, projectprovisioning.ErrOwnerRequired) {
		t.Errorf("missing owner err = %v, want ErrOwnerRequired", err)
	}
}

// TestProvisionRefusesAnUnknownAdministrator pins the deliberate narrowing
// documented on createProjectAdmin: pylon CREATES an account for an unknown
// address; this does not, and it must fail loudly rather than answer 201 with an
// administrator who has no access.
func TestProvisionRefusesAnUnknownAdministrator(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name:        "Typo Project",
		OwnerID:     1,
		AdminEmails: []string{"nobody@example.test"},
		AdminRoles:  []string{"admin"},
		Limits:      projectprovisioning.DefaultLimits(),
	})
	if !errors.Is(err, projectprovisioning.ErrUnknownAdminEmail) {
		t.Fatalf("err = %v, want ErrUnknownAdminEmail", err)
	}
	// And the whole project is rolled back rather than left without its admin.
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, result.ProjectID).Scan(&rows); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	if rows != 0 {
		t.Error("a project with an unresolvable administrator survived")
	}
}

// TestDeprovisionRemovesEverythingProvisionCreated is the symmetric half.
//
// It provisions for real and then deletes, asserting the database is back where
// it started. Asserting only the 200 would accept a delete that removed the
// project row and orphaned the schema — which is the state the elitea-migrate
// preflight cannot repair, because the row it would name is gone.
func TestDeprovisionRemovesEverythingProvisionCreated(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Doomed", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID

	// Premise: it really was provisioned. Without this the delete assertions
	// below would pass against a project that never existed.
	if tables := schemaTables(ctx, t, pool, fmt.Sprintf("p_%d", projectID)); len(tables) == 0 {
		t.Fatal("nothing was provisioned, so this test cannot prove anything was deleted")
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}

	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		fmt.Sprintf("p_%d", projectID),
	).Scan(&schemaExists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaExists {
		t.Error("the tenant schema survived deletion")
	}

	for _, check := range []struct {
		what  string
		query string
		arg   any
	}{
		{"project row", `SELECT count(*) FROM centry.project WHERE id = $1`, projectID},
		{"quota rows", `SELECT count(*) FROM centry.project_quota WHERE project_id = $1`, projectID},
		{"statistic rows", `SELECT count(*) FROM centry.statistic WHERE project_id = $1`, projectID},
		{"project roles", `SELECT count(*) FROM public.auth_core__project_role WHERE project_id = $1`, projectID},
		{"role assignments", `SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = $1`, projectID},
		{"system users", `SELECT count(*) FROM public.auth_core__user WHERE email = $1`,
			fmt.Sprintf("system_user_%d@centry.user", projectID)},
		{"tenant ledger rows", `SELECT count(*) FROM elitea_runtime.schema_migrations
                                WHERE target_kind = 'tenant' AND target_id = $1`, fmt.Sprintf("%d", projectID)},
	} {
		var rows int
		if err := pool.QueryRow(ctx, check.query, check.arg).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", check.what, err)
		}
		if rows != 0 {
			t.Errorf("%s survived deletion: %d left", check.what, rows)
		}
	}

	// The reference leaves orphan project_role rows behind for every deleted
	// project because its ProjectPermissions.delete is a no-op. The loop above
	// pins the correction.

	// p_1 must be untouched: the delete names one schema, and a CASCADE against
	// the wrong one would take the whole deployment with it.
	if len(schemaTables(ctx, t, pool, "p_1")) == 0 {
		t.Fatal("deleting one project destroyed the reference tenant p_1")
	}

	// A second delete is a clean not-found rather than a partial re-run.
	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
		t.Errorf("second deprovision err = %v, want ErrProjectNotFound", err)
	}
}

// TestDeprovisionRevokesTokenBindingsForThatProjectOnly pins ADR-0018's §7.3
// invariant at the project level: a token binding must not outlive membership.
//
// Deleting one member from a project revokes that member's bindings for it, in
// eliteacore.UsersDelete. Deleting the whole project takes every membership
// with it — removeProjectPermissions deletes auth_core__project_role, and
// auth_core__project_user_role cascades from the role — so it must take every
// binding too. elitea_identity.token_project_binding.project_id carries no
// foreign key, because centry.project is pylon-owned, so nothing revokes these
// rows for us.
//
// The surviving binding is the half that makes this test discriminate. A
// `DELETE FROM token_project_binding` with no WHERE clause would pass the first
// assertion and fail the second.
func TestDeprovisionRevokesTokenBindingsForThatProjectOnly(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	doomed, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Doomed With A Bound Key", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision doomed: %v", err)
	}
	kept, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Kept With A Bound Key", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision kept: %v", err)
	}

	doomedToken := bindTokenToProject(ctx, t, pool, "deprovision-doomed", doomed.ProjectID)
	keptToken := bindTokenToProject(ctx, t, pool, "deprovision-kept", kept.ProjectID)

	// Premise: both bindings exist. Without this the "gone" assertion below
	// would pass against a binding that was never written.
	if got := countBindings(ctx, t, pool, doomedToken); got != 1 {
		t.Fatalf("doomed binding was not written: got %d rows, want 1", got)
	}

	if _, err := provisioner.Deprovision(ctx, doomed.ProjectID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}

	if got := countBindings(ctx, t, pool, doomedToken); got != 0 {
		t.Errorf("the binding survived the project it names: got %d rows, want 0", got)
	}
	if got := countBindings(ctx, t, pool, keptToken); got != 1 {
		t.Errorf("deleting one project revoked another project's binding: got %d rows, want 1", got)
	}
}

// bindTokenToProject writes one token owned by user 1 and binds it to
// projectID. It returns the token id. It writes the rows directly because the
// token API lives in another package; the columns are the same ones that API
// writes.
func bindTokenToProject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tokenUUID string, projectID int64) int32 {
	t.Helper()
	var tokenID int32
	if err := pool.QueryRow(ctx,
		`INSERT INTO public.auth_core__token (uuid, user_id, name) VALUES ($1, 1, $2) RETURNING id`,
		tokenUUID, tokenUUID,
	).Scan(&tokenID); err != nil {
		t.Fatalf("insert token %s: %v", tokenUUID, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`,
		tokenID, projectID,
	); err != nil {
		t.Fatalf("bind token %s to project %d: %v", tokenUUID, projectID, err)
	}
	return tokenID
}

func countBindings(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tokenID int32) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_identity.token_project_binding WHERE token_id = $1`,
		tokenID,
	).Scan(&rows); err != nil {
		t.Fatalf("count bindings for token %d: %v", tokenID, err)
	}
	return rows
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// newTestProvisioner builds the provisioner with the same vault dependency
// internal/api/router.go wires (#373).
//
// The real secrets handler, not a fake: it is the only code in the tree that
// mints a vault key, so a fake here would prove the step ran and nothing about
// whether what it wrote can be opened.
func newTestProvisioner(
	t *testing.T, pool *pgxpool.Pool, migrator projectprovisioning.TenantMigrator,
) *projectprovisioning.Provisioner {
	t.Helper()
	return projectprovisioning.New(pool, migrator, nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)))
}

// newProvisioningPool builds an isolated database holding exactly what a real
// deployment holds: the bootstrap schema plus the embedded corpus. Nothing is
// hand-created, so a divergence between the fixture and production cannot hide
// a defect. Same shape as migrations/corpus_postgres_integration_test.go.
func newProvisioningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := provisioningDatabaseURL()
	if databaseURL == "" {
		t.Skipf("set %s to run the project provisioning integration test", databaseURLEnv)
	}
	if provisioningTemplate == "" {
		t.Fatalf("TestMain did not build the project provisioning template")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()

	databaseName := fmt.Sprintf("elitea_provision_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, provisioningTemplate, databaseName); err != nil {
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
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
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

	// The template already holds the bootstrap schema and the full ledgered
	// history. Prove that on every copy, rather than trust the copy: CheckHead
	// compares the recorded SHA-256 of every migration against the embedded
	// corpus. A template that drifted from the corpus fails here.
	//
	// p_1 is the reference tenant every assertion compares against, so it must
	// be at head before any test runs.
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.CheckHead(ctx, migrate.ScopeShared, "platform"); err != nil {
		t.Fatalf("verify shared migration head: %v", err)
	}
	if err := runner.CheckHead(ctx, migrate.ScopeTenant, "1"); err != nil {
		t.Fatalf("verify tenant migration head for p_1: %v", err)
	}
	return pool
}

func schemaTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool, schema string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT table_name FROM information_schema.tables
WHERE table_schema = $1 AND table_type = 'BASE TABLE'
ORDER BY table_name`, schema)
	if err != nil {
		t.Fatalf("list tables in %s: %v", schema, err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables in %s: %v", schema, err)
	}
	return names
}

func projectRoleNames(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT name FROM public.auth_core__project_role WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		t.Fatalf("list project roles: %v", err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan role name: %v", err)
		}
		names = append(names, name)
	}
	return names
}

func projectIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text FROM centry.project ORDER BY id`)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan project id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func difference(from, remove []string) []string {
	present := make(map[string]struct{}, len(remove))
	for _, value := range remove {
		present[value] = struct{}{}
	}
	missing := make([]string, 0)
	for _, value := range from {
		if _, ok := present[value]; !ok {
			missing = append(missing, value)
		}
	}
	return missing
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

/* ── issue #374: the delete order ──────────────────────────────────────── */

// TestDeprovisionRemovesAProjectThatRanAnExecution is the case the delete path
// could not do at all.
//
// Seven foreign keys name centry.project(id) with no ON DELETE action. A
// project that ever ran an agent turn or an index ingest holds rows behind
// them, so the project row delete failed — AFTER the schema drop had already
// succeeded. The result was a project row with create_success = TRUE and no
// p_<id> schema, which is the one state cmd/elitea-migrate refuses to run
// against, for every tenant in the deployment.
//
// The seed is a whole execution graph and not one row on purpose. Two of the
// tables that block the delete — index_ingest_results and
// configuration_validation_results — carry no project column at all and reach
// the project only through their execution job, so a seed of the seven obvious
// tables would not exercise the statements that clear them.
func TestDeprovisionRemovesAProjectThatRanAnExecution(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Ran An Execution", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	execution := seedExecutionGraph(ctx, t, pool, projectID)

	// Premise: the rows really are there. Without this the "gone" assertions
	// below would pass against a project that never ran anything, which is what
	// every delete test in this file did before #374.
	for table, rows := range executionGraphRowCounts(ctx, t, pool, projectID, execution) {
		if rows == 0 {
			t.Fatalf("the seed wrote no row in %s, so this test cannot prove the delete clears it", table)
		}
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("deprovision a project that ran an execution: %v (steps=%+v)", err, result.RollbackSteps)
	}

	for table, rows := range executionGraphRowCounts(ctx, t, pool, projectID, execution) {
		if rows != 0 {
			t.Errorf("%s kept %d row(s) that name the deleted project", table, rows)
		}
	}

	var rowSurvived, schemaSurvived bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1),
       EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $2)`,
		projectID, fmt.Sprintf("p_%d", projectID),
	).Scan(&rowSurvived, &schemaSurvived); err != nil {
		t.Fatalf("read the project row and its schema: %v", err)
	}
	if rowSurvived {
		t.Error("the project row survived the delete")
	}
	if schemaSurvived {
		t.Error("the tenant schema survived the delete")
	}

	// The deployment-wide consequence. This is the real preflight, not a copy.
	if _, err := migrate.TenantProjects(ctx, pool); err != nil {
		t.Errorf("elitea-migrate preflight fails after the delete: %v", err)
	}
}

// TestDeprovisionKeepsTheTenantSchemaWhenTheProjectRowCannotGo is the
// acceptance criterion that matters most in #374.
//
// It makes the delete fail on purpose, at the project row, and proves the
// database is NOT left with a project row whose schema is gone. The failure is
// injected with a foreign key this package does not know about, because a
// blocker taken from the covered list would prove only that the list is
// complete today. Any future table that names centry.project behaves the same
// way.
//
// Against the ordering this test replaces — schema first, row second — the
// tenant schema is already gone at this point and the preflight assertion
// below fails for the whole deployment.
func TestDeprovisionKeepsTheTenantSchemaWhenTheProjectRowCannotGo(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Blocked Delete", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	schema := fmt.Sprintf("p_%d", projectID)
	tenantTables := schemaTables(ctx, t, pool, schema)
	if len(tenantTables) == 0 {
		t.Fatal("nothing was provisioned, so this test cannot prove anything was kept")
	}

	// The blocker: one row behind a foreign key with no ON DELETE action, which
	// is the shape of all seven real ones.
	if _, err := pool.Exec(ctx, `
CREATE TABLE centry.delete_blocker (
    project_id INTEGER NOT NULL REFERENCES centry.project(id)
)`); err != nil {
		t.Fatalf("create the blocking table: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO centry.delete_blocker (project_id) VALUES ($1)`, projectID); err != nil {
		t.Fatalf("insert the blocking row: %v", err)
	}

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
		t.Fatalf("deprovision err = %v, want ErrProjectNotRemoved", err)
	}

	// THE ASSERTION THIS TEST EXISTS FOR. The tenant data is the part that
	// cannot be rebuilt, so a delete that cannot remove the project row must
	// keep it.
	if kept := schemaTables(ctx, t, pool, schema); !equalStrings(tenantTables, kept) {
		t.Fatalf("the failed delete changed the tenant schema %s\n missing: %v\n extra:   %v",
			schema, difference(tenantTables, kept), difference(kept, tenantTables))
	}

	var rowSurvived, createSuccess bool
	if err := pool.QueryRow(ctx,
		`SELECT true, create_success FROM centry.project WHERE id = $1`, projectID,
	).Scan(&rowSurvived, &createSuccess); err != nil {
		t.Fatalf("the failed delete removed the project row it could not delete: %v", err)
	}
	if !createSuccess {
		t.Error("the failed delete cleared create_success, which hides the project from every reader that filters on it")
	}

	// The deployment-wide consequence, proved rather than argued: a project row
	// with create_success = TRUE and no schema stops migration for EVERY
	// tenant. Here the schema is there, so the preflight still runs.
	eligible, err := migrate.TenantProjects(ctx, pool)
	if err != nil {
		t.Fatalf("elitea-migrate preflight fails after a failed delete, so no tenant in this deployment can migrate: %v", err)
	}
	if !containsID(eligible, projectID) {
		t.Errorf("the preflight does not list project %d, so its tenant would never migrate again", projectID)
	}

	// The report names the held-back step, and it says the step was not
	// started rather than that it failed halfway.
	var schemaStep projectprovisioning.StepStatus
	for _, status := range result.RollbackSteps {
		if status.Step == "project_schema" {
			schemaStep = status
		}
	}
	if schemaStep.OK == nil || *schemaStep.OK {
		t.Errorf("project_schema is reported as %+v, want a step that did not succeed", schemaStep)
	}
	if schemaStep.Msg != projectprovisioning.HeldBackStepMessage("project_schema") {
		t.Errorf("project_schema msg = %q, want the held-back message", schemaStep.Msg)
	}

	// And the delete converges once the blocker is gone.
	if _, err := pool.Exec(ctx, `DROP TABLE centry.delete_blocker`); err != nil {
		t.Fatalf("drop the blocking table: %v", err)
	}
	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("the retried delete failed: %v", err)
	}
	if tables := schemaTables(ctx, t, pool, schema); len(tables) != 0 {
		t.Error("the retried delete kept the tenant schema")
	}
	if _, err := migrate.TenantProjects(ctx, pool); err != nil {
		t.Errorf("elitea-migrate preflight fails after the retried delete: %v", err)
	}
}

// TestEveryBlockingForeignKeyIsCovered is the guard against the next migration
// reopening #374.
//
// It walks the live catalogue from centry.project through the cascades the
// delete gets for free, and it names any table that could still block the
// project row delete. The answer must be empty. A migration that adds a
// foreign key to centry.project(id), or to any table the delete removes, fails
// this test until referencingDeletes clears it too.
func TestEveryBlockingForeignKeyIsCovered(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	removed := append([]string{"centry.project"}, projectprovisioning.ReferencingTables()...)
	rows, err := pool.Query(ctx, `
WITH RECURSIVE removed AS (
        SELECT class.oid AS rel
        FROM pg_catalog.pg_class AS class
        JOIN pg_catalog.pg_namespace AS space ON space.oid = class.relnamespace
        WHERE space.nspname || '.' || class.relname = ANY($1::text[])
    UNION
        SELECT constraint_.conrelid
        FROM pg_catalog.pg_constraint AS constraint_
        JOIN removed ON constraint_.confrelid = removed.rel
        WHERE constraint_.contype = 'f' AND constraint_.confdeltype = 'c'
)
SELECT DISTINCT space.nspname || '.' || class.relname
FROM pg_catalog.pg_constraint AS constraint_
JOIN removed ON constraint_.confrelid = removed.rel
JOIN pg_catalog.pg_class AS class ON class.oid = constraint_.conrelid
JOIN pg_catalog.pg_namespace AS space ON space.oid = class.relnamespace
WHERE constraint_.contype = 'f'
  AND constraint_.confdeltype NOT IN ('c', 'n', 'd')
  AND constraint_.conrelid NOT IN (SELECT rel FROM removed)
ORDER BY 1`, removed)
	if err != nil {
		t.Fatalf("read the foreign keys that reference the project: %v", err)
	}
	defer rows.Close()

	blocking := make([]string, 0)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan a blocking table: %v", err)
		}
		blocking = append(blocking, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the blocking tables: %v", err)
	}
	if len(blocking) != 0 {
		t.Fatalf("these tables can block a project delete and referencingDeletes does not clear them: %v", blocking)
	}

	// Premise: the walk really does reach the runtime tables. An empty answer
	// from a query that reaches nothing would pass for the wrong reason.
	if !contains(projectprovisioning.ReferencingTables(), "elitea_runtime.execution_jobs") {
		t.Fatal("the covered list does not name execution_jobs, so this test proves nothing")
	}
}

/* ── the execution graph fixture ───────────────────────────────────────── */

// seedExecutionGraph writes one complete execution for projectID: a bundle, a
// job, a claim, two outbox events, an index artifact, an index ingest result, a
// configuration validation result, both replay tables and an index generation
// counter. It returns the execution id.
//
// The rows are written with raw SQL because the repositories that normally
// write them live in other packages, and a fixture that went through them would
// test those packages instead of the delete.
func seedExecutionGraph(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) string {
	t.Helper()
	execution := fmt.Sprintf("execution-delete-%d", projectID)
	bundle := fmt.Sprintf("bundle-delete-%d", projectID)
	claim := fmt.Sprintf("claim-delete-%d", projectID)
	record := fmt.Sprintf("storage-record-%d", projectID)
	digest := make([]byte, 32)
	payload := []byte("payload")

	for _, seed := range []struct {
		what      string
		statement string
		arguments []any
	}{
		{"input bundle", `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, 'v1', 'application/x-protobuf', $2, $3, 7, $4, 'tests')`,
			[]any{bundle, projectID, digest, payload}},

		{"execution job", `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES ($1, 1, $2, $3, $4, $4, '1', 'principal', 'index.ingest.v1', '1',
          $5, $6, 'tests', $2, 'RUNNING', 'RUNNING')`,
			[]any{execution, "command-" + execution, fmt.Sprintf("%d", projectID), projectID, bundle, digest}},

		{"execution claim", `
INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity,
    producer_id, claim_attempt, lease_epoch, fence_token, lease_expires_at
) VALUES ($1, $2, 1, 'session', 'identity', 'producer', 1, 1, $3,
          now() + interval '1 hour')`,
			[]any{claim, execution, digest}},

		{"outbox events", `
INSERT INTO elitea_runtime.output_inbox (
    event_id, logical_output_id, execution_id, generation, claim_id,
    fence_token, workload_identity, workload_session_id, producer_id,
    claim_attempt, lease_epoch, stream_id, sequence, payload_type,
    payload_digest, payload_bytes, settlement_proposal_id, settlement_outcome,
    settlement_proposal_bytes, settlement_proposal_digest,
    settlement_idempotency_key, occurred_at
) VALUES
    ($1 || ':ingest', 'ingest-output', $1, 1, $2, $3, 'identity', 'session',
     'producer', 1, 1, 'stream', 1, 'INDEX_INGEST_RESULT', $3, $4,
     'proposal-ingest', 'SUCCEEDED', $4, $3, 'idempotency-ingest', now()),
    ($1 || ':config', 'config-output', $1, 1, $2, $3, 'identity', 'session',
     'producer', 2, 1, 'stream', 2, 'CONFIGURATION_VALIDATION', $3, $4,
     'proposal-config', 'SUCCEEDED', $4, $3, 'idempotency-config', now())`,
			[]any{execution, claim, digest, payload}},

		{"index result artifact", `
INSERT INTO elitea_runtime.index_result_artifacts (
    artifact_id, immutable_version, execution_id, generation,
    resource_project_id, media_type, byte_length, digest, classification,
    storage_record_id, bytes_verified_at, metadata_created_at
) VALUES ('artifact', 'v1', $1, 1, $2, 'application/octet-stream', 7, $3,
          'internal', $4, now(), now())`,
			[]any{execution, projectID, digest, record}},

		{"index ingest result", `
INSERT INTO elitea_runtime.index_ingest_results (
    logical_output_id, execution_id, generation, input_bundle_id,
    input_bundle_digest, artifact_id, artifact_immutable_version,
    artifact_storage_record_id
) VALUES ('ingest-output', $1, 1, $2, $3, 'artifact', 'v1', $4)`,
			[]any{execution, bundle, digest, record}},

		{"configuration validation result", `
INSERT INTO elitea_runtime.configuration_validation_results (
    logical_output_id, execution_id, generation, configuration_revision_id,
    configuration_type, catalog_revision, catalog_digest, schema_id,
    schema_revision, schema_digest, input_bundle_id, input_bundle_digest,
    settings_entry_id, settings_entry_version, settings_content_digest, valid
) VALUES ('config-output', $1, 1, 'revision', 'application/json', 'catalog',
          $2, 'schema', '1', $2, $3, $2, 'entry', '1', $2, true)`,
			[]any{execution, digest, bundle}},

		{"replay event", `
INSERT INTO elitea_runtime.execution_replay_events (
    event_id, execution_id, generation, projection_project_id, event_type,
    event_bytes, event_digest
) VALUES ($1 || ':replay', $1, 1, $2, 'execution.node_event', $3, $4)`,
			[]any{execution, projectID, payload, digest}},

		{"replay state", `
INSERT INTO elitea_runtime.execution_replay_state (
    execution_id, generation, projection_project_id
) VALUES ($1, 1, $2)`,
			[]any{execution, projectID}},

		{"index generation counter", `
INSERT INTO elitea_runtime.index_generation_counters (
    resource_project_id, toolkit_id, index_name, last_generation
) VALUES ($1, 1, 'index', 1)`,
			[]any{projectID}},
	} {
		if _, err := pool.Exec(ctx, seed.statement, seed.arguments...); err != nil {
			t.Fatalf("seed %s: %v", seed.what, err)
		}
	}
	return execution
}

// executionGraphRowCounts counts what the seed wrote, per table. The two
// tables with no project column are counted by execution, which is the only
// route they have to the project.
func executionGraphRowCounts(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, execution string,
) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, check := range []struct {
		table     string
		statement string
		argument  any
	}{
		{"elitea_runtime.input_bundles",
			`SELECT count(*) FROM elitea_runtime.input_bundles WHERE resource_project_id = $1`, projectID},
		{"elitea_runtime.execution_jobs",
			`SELECT count(*) FROM elitea_runtime.execution_jobs
             WHERE resource_project_id = $1 OR projection_project_id = $1`, projectID},
		{"elitea_runtime.execution_replay_events",
			`SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE projection_project_id = $1`, projectID},
		{"elitea_runtime.execution_replay_state",
			`SELECT count(*) FROM elitea_runtime.execution_replay_state WHERE projection_project_id = $1`, projectID},
		{"elitea_runtime.index_result_artifacts",
			`SELECT count(*) FROM elitea_runtime.index_result_artifacts WHERE resource_project_id = $1`, projectID},
		{"elitea_runtime.index_generation_counters",
			`SELECT count(*) FROM elitea_runtime.index_generation_counters WHERE resource_project_id = $1`, projectID},
		{"elitea_runtime.index_ingest_results",
			`SELECT count(*) FROM elitea_runtime.index_ingest_results WHERE execution_id = $1`, execution},
		{"elitea_runtime.configuration_validation_results",
			`SELECT count(*) FROM elitea_runtime.configuration_validation_results WHERE execution_id = $1`, execution},
		{"elitea_runtime.output_inbox",
			`SELECT count(*) FROM elitea_runtime.output_inbox WHERE execution_id = $1`, execution},
		{"elitea_runtime.execution_claims",
			`SELECT count(*) FROM elitea_runtime.execution_claims WHERE execution_id = $1`, execution},
	} {
		var rows int
		if err := pool.QueryRow(ctx, check.statement, check.argument).Scan(&rows); err != nil {
			t.Fatalf("count rows in %s: %v", check.table, err)
		}
		counts[check.table] = rows
	}
	return counts
}

func containsID(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
