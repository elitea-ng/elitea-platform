package projectprovisioning_test

// The project_secrets step (#373), at the level the pipeline owns it.
//
// provisioner_postgres_integration_test.go proves the pipeline builds a tenant
// equal to the reference and leaves nothing behind when a step fails. These
// cases pin the same two properties for the vault specifically, because the
// vault is the one resource in the pipeline that no foreign key removes: its
// rows are keyed by the TEXT `project-<id>` and not by centry.project.id, so a
// vault that outlives its compensation would be ADOPTED by the next project
// that draws the same id — it would open, and it would hold another project's
// secrets.
//
// The route-level half of this issue is in
// internal/api/v2/projects/create_route_postgres_integration_test.go, which
// asserts the customer-visible effect: the model picker of a new project
// resolves.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// TestProvisionCreatesAVaultTheReaderCanOpen pins the step's success path.
//
// Counting the two rows is not enough. A key row that does not decrypt its data
// row is WORSE than an absent pair: the secrets handler treats an unreadable
// vault as a hard error and never overwrites it, so the project would answer
// 500 for ever with no way back. The assertion is therefore that the production
// reader opens what provisioning wrote.
func TestProvisionCreatesAVaultTheReaderCanOpen(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Vaulted Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	projectID := result.ProjectID

	// The step is reported under a stable name. A caller reads the step list to
	// see how far provisioning got, so a renamed step is a broken report.
	var reported bool
	for _, status := range result.Steps {
		if status.Step == projectprovisioning.StepProjectSecrets {
			reported = true
			if status.OK == nil || !*status.OK {
				t.Errorf("the %s step did not report success: %+v",
					projectprovisioning.StepProjectSecrets, status)
			}
		}
	}
	if !reported {
		t.Errorf("the step list does not contain %q: %+v",
			projectprovisioning.StepProjectSecrets, result.Steps)
	}

	if got := countVaultRows(ctx, t, pool, projectID); got != 2 {
		t.Fatalf("the new project has %d vault rows, want 2 (one key, one data)", got)
	}

	loader, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatalf("compose vault loader: %v", err)
	}
	defer loader.Destroy()
	vault, err := loader.LoadProjectVault(ctx, projectID)
	if err != nil {
		t.Fatalf("the provisioned vault does not open: %v", err)
	}
	// Empty, not broken: an absent name reads as "no such secret".
	if _, err := vault.LookupRegular("default_llm_model_name"); !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		t.Errorf("lookup in the provisioned vault err = %v, want ErrSecretNotFound", err)
	}

	// The step converges. Running it again against a project that already has a
	// readable vault must not mint a second key over the first, which would
	// orphan the data row.
	secrets := v2secrets.NewHandler(pool)
	before := vaultKeyBytes(ctx, t, pool, projectID)
	if err := secrets.EnsureProjectVault(ctx, fmt.Sprintf("%d", projectID)); err != nil {
		t.Fatalf("second EnsureProjectVault: %v", err)
	}
	if after := vaultKeyBytes(ctx, t, pool, projectID); string(after) != string(before) {
		t.Error("a second EnsureProjectVault replaced the stored key, orphaning the data row")
	}
}

// failingBucketBootstrapper fails the step that runs AFTER project_secrets.
//
// That ordering is the whole point of this fixture. Compensation walks the step
// list in reverse, so a failure in a LATER step is the only way to prove the
// vault's own compensation runs — a failure in project_secrets itself would
// leave nothing to undo.
type failingBucketBootstrapper struct{ err error }

func (b failingBucketBootstrapper) BootstrapProjectBuckets(context.Context, string) error {
	return b.err
}

func (b failingBucketBootstrapper) TeardownProjectBuckets(context.Context, string) error {
	return nil
}

// TestProvisionCompensationRemovesTheVault makes a later step fail on purpose
// and proves the vault rows go with the rest of the project.
func TestProvisionCompensationRemovesTheVault(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := projectprovisioning.New(pool,
		migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
		projectprovisioning.WithArtifactBuckets(
			failingBucketBootstrapper{err: errors.New("object store unavailable")}),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Doomed After Its Vault", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err == nil {
		t.Fatal("provision reported success while the bucket step could not run")
	}
	if result.ProjectID == 0 {
		t.Fatal("the failure path reported no project id, so the test cannot check what was left behind")
	}
	projectID := result.ProjectID

	// PREMISE. The vault step must have RUN and succeeded before the failure,
	// or "the rows are gone" would be true of a vault that was never written.
	var vaultCreated bool
	for _, status := range result.Steps {
		if status.Step == projectprovisioning.StepProjectSecrets && status.OK != nil && *status.OK {
			vaultCreated = true
		}
	}
	if !vaultCreated {
		t.Fatalf("the vault step did not run before the failure, so this test proves nothing: %+v", result.Steps)
	}

	if got := countVaultRows(ctx, t, pool, projectID); got != 0 {
		t.Errorf("%d vault rows survived compensation; the next project to draw id %d would adopt them",
			got, projectID)
	}
	// And the compensation is reported, so an operator can see it ran.
	var compensated bool
	for _, status := range result.RollbackSteps {
		if status.Step == projectprovisioning.StepProjectSecrets {
			compensated = true
			if status.OK == nil || !*status.OK {
				t.Errorf("vault compensation did not report success: %+v", status)
			}
		}
	}
	if !compensated {
		t.Errorf("the rollback list does not contain %q: %+v",
			projectprovisioning.StepProjectSecrets, result.RollbackSteps)
	}
}

// failingVaultBootstrapper fails the vault step itself.
type failingVaultBootstrapper struct{ err error }

func (v failingVaultBootstrapper) EnsureProjectVault(context.Context, string) error { return v.err }

func (v failingVaultBootstrapper) EnsureProjectSecretsHeaderValue(
	context.Context, string,
) (bool, error) {
	return false, v.err
}

func (v failingVaultBootstrapper) RemoveProjectVault(context.Context, string) error { return nil }

func (v failingVaultBootstrapper) RemoveProjectVaultTx(context.Context, pgx.Tx, string) error {
	return nil
}

// TestProvisionRollsBackWhenTheVaultStepFails is the other direction: a failure
// IN the new step must undo every step before it.
//
// The tenant schema is the dangerous one. project_schema creates the schema and
// THEN applies the corpus, so a schema exists by the time a later step fails. A
// surviving p_<id> beside a surviving project row with create_success = TRUE is
// the state cmd/elitea-migrate refuses to run against, for the whole
// deployment rather than for the one project.
func TestProvisionRollsBackWhenTheVaultStepFails(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	before := projectIDs(ctx, t, pool)
	provisioner := projectprovisioning.New(pool,
		migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(
			failingVaultBootstrapper{err: errors.New("vault unavailable")}),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Doomed At Its Vault", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err == nil {
		t.Fatal("provision reported success while the vault step could not run")
	}
	if result.ProjectID == 0 {
		t.Fatal("the failure path reported no project id, so the test cannot check what was left behind")
	}
	projectID := result.ProjectID

	// The failing step is named, and the raw cause does not cross the boundary.
	var failed bool
	for _, status := range result.Steps {
		if status.OK != nil && !*status.OK {
			failed = true
			if status.Step != projectprovisioning.StepProjectSecrets {
				t.Errorf("the failing step is reported as %q, want %q",
					status.Step, projectprovisioning.StepProjectSecrets)
			}
			if status.Msg == "vault unavailable" {
				t.Error("the raw error crossed the trust boundary in the step message")
			}
		}
	}
	if !failed {
		t.Error("no step reported a failure, so the caller cannot tell what went wrong")
	}

	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		fmt.Sprintf("p_%d", projectID),
	).Scan(&schemaExists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaExists {
		t.Error("the tenant schema survived a failure of the vault step")
	}
	for _, check := range []struct {
		what  string
		query string
		arg   any
	}{
		{"project rows", `SELECT count(*) FROM centry.project WHERE id = $1`, projectID},
		{"quota rows", `SELECT count(*) FROM centry.project_quota WHERE project_id = $1`, projectID},
		{"project roles", `SELECT count(*) FROM public.auth_core__project_role WHERE project_id = $1`, projectID},
		{"system users", `SELECT count(*) FROM public.auth_core__user WHERE email = $1`,
			fmt.Sprintf("system_user_%d@centry.user", projectID)},
	} {
		var rows int
		if err := pool.QueryRow(ctx, check.query, check.arg).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", check.what, err)
		}
		if rows != 0 {
			t.Errorf("%s survived a failure of the vault step: %d left", check.what, rows)
		}
	}
	if after := projectIDs(ctx, t, pool); !equalStrings(before, after) {
		t.Errorf("the project table changed: before %v, after %v", before, after)
	}
}

// TestDeprovisionRemovesTheVault is the delete half.
//
// Nothing cascades these rows: centry.secrets_key.id is the TEXT
// `project-<id>`, so it carries no foreign key to centry.project.
func TestDeprovisionRemovesTheVault(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Deleted With Its Vault", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID

	// PREMISE.
	if got := countVaultRows(ctx, t, pool, projectID); got != 2 {
		t.Fatalf("the project was provisioned with %d vault rows, so this test cannot prove a delete", got)
	}
	// A second project's vault, to prove the delete names one project only.
	kept, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Kept With Its Vault", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision the kept project: %v", err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}

	if got := countVaultRows(ctx, t, pool, projectID); got != 0 {
		t.Errorf("%d vault rows survived the delete", got)
	}
	if got := countVaultRows(ctx, t, pool, kept.ProjectID); got != 2 {
		t.Errorf("deleting one project removed another project's vault: %d rows left, want 2", got)
	}
}

// TestProvisionRefusesWithoutAVaultBootstrapper pins the fail-closed rule.
//
// A provisioner with no vault dependency must refuse BEFORE its first insert.
// Answering 201 for a project with no vault is exactly the defect #373 reports,
// so a silently inert step would reintroduce it in a form no test could see.
func TestProvisionRefusesWithoutAVaultBootstrapper(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	before := projectIDs(ctx, t, pool)
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil)
	if _, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "No Vault Configured", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	}); err == nil {
		t.Fatal("provisioning ran without a vault bootstrapper")
	}
	if after := projectIDs(ctx, t, pool); !equalStrings(before, after) {
		t.Errorf("the refused create still wrote a project row: before %v, after %v", before, after)
	}
}

/* ── #375, at the pipeline level ───────────────────────────────────────── */

// TestProvisionWithoutAdministratorEmailsMakesTheOwnerAnAdministrator pins the
// rule createProjectAdmin records for #375.
func TestProvisionWithoutAdministratorEmailsMakesTheOwnerAnAdministrator(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A real account for the owner to resolve to, distinct from the bootstrap
	// user so the assertion cannot pass on a seeded row.
	var ownerID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('maker@example.test', 'Maker')
RETURNING id`).Scan(&ownerID); err != nil {
		t.Fatalf("seed owner account: %v", err)
	}

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Maker Owned", OwnerID: ownerID, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, created.Steps)
	}

	var role string
	if err := pool.QueryRow(ctx, `
SELECT role.name
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2`,
		created.ProjectID, ownerID,
	).Scan(&role); err != nil {
		t.Fatalf("the owner of the new project has no role assignment: %v", err)
	}
	if role != "admin" {
		t.Errorf("the owner holds the %q role, want admin — it could not add the first member", role)
	}
}

// TestProvisionRefusesAnOwnerWithNoAccount proves the owner fallback fails
// loudly rather than answering 201 for a project with no member.
func TestProvisionRefusesAnOwnerWithNoAccount(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	before := projectIDs(ctx, t, pool)
	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		// No auth_core__user carries this id.
		Name: "Ownerless", OwnerID: 987654, Limits: projectprovisioning.DefaultLimits(),
	})
	if !errors.Is(err, projectprovisioning.ErrUnknownOwner) {
		t.Fatalf("err = %v, want ErrUnknownOwner", err)
	}
	// And the whole project is rolled back rather than left with no member.
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, result.ProjectID).Scan(&rows); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	if rows != 0 {
		t.Error("a project whose owner has no account survived")
	}
	if after := projectIDs(ctx, t, pool); !equalStrings(before, after) {
		t.Errorf("the project table changed: before %v, after %v", before, after)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func countVaultRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) int {
	t.Helper()
	vaultID := fmt.Sprintf("project-%d", projectID)
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT
    (SELECT count(*) FROM centry.secrets_key WHERE id = $1)
  + (SELECT count(*) FROM centry.secrets_data WHERE id = $1)`,
		vaultID,
	).Scan(&rows); err != nil {
		t.Fatalf("count vault rows for %s: %v", vaultID, err)
	}
	return rows
}

func vaultKeyBytes(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) []byte {
	t.Helper()
	var stored []byte
	if err := pool.QueryRow(ctx,
		`SELECT data FROM centry.secrets_key WHERE id = $1`,
		fmt.Sprintf("project-%d", projectID),
	).Scan(&stored); err != nil {
		t.Fatalf("read vault key for project %d: %v", projectID, err)
	}
	return stored
}
