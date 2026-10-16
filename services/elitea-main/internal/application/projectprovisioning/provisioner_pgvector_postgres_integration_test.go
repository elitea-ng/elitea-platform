package projectprovisioning_test

// What these tests exist to catch (#371).
//
// A project created through the Go route had no `vectorstorage` configuration
// row, so an index run inside it had nothing to resolve. The project looked
// complete — schema, corpus, system user, system token — and could not index.
//
// Nothing here asserts a step status or a return value. Every case reads the
// database back, and the resolution case runs the SAME two components an index
// run runs: CurrentConfigurationsRepository.FindByEliteaTitle, which is what
// configuration expansion calls for `pgvector_configuration`, and the vault
// unsecreter that redeems the `{{secret.…}}` reference it stores. A test that
// only asserted the row exists would still pass with an unredeemable
// connection string, because the unsecreter leaves an unresolved placeholder
// verbatim and every validator on the index path accepts it.
//
//   TestProvisionCreatesAVectorStoreAnIndexRunCanResolve
//       the row exists, the reference redeems to a real DSN, and that DSN
//       connects. The connection is what proves the role and database were
//       really created rather than merely named.
//   TestProvisionCompensatesTheVectorStoreWhenALaterStepFails
//       project_admin fails after project_pgvector succeeded. The row and the
//       vault must both be gone.

import (
	"context"
	"errors"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	vectorstoreapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/vectorstore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

const referenceProjectID = 1

func TestProvisionCreatesAVectorStoreAnIndexRunCanResolve(t *testing.T) {
	skipWithoutVectorExtension(t)

	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	vectorStore := newProjectVectorStoreForTest(t, pool)

	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(vectorStore),
		// Required since #373: Provision refuses to run without a vault
		// bootstrapper, so every provisioning test must supply one.
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Indexable Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	projectID := result.ProjectID
	dropProvisionedVectorStore(t, projectID)

	// The step must be in the pipeline, and must have reported success. A step
	// that never ran would leave every assertion below to the reference row.
	assertStepSucceeded(t, result.Steps, projectprovisioning.StepProjectPgvector)

	/* ── the row itself ──────────────────────────────────────────────────
	 *
	 * Read straight out of the tenant, by the identity the upsert writes. The
	 * stored connection string must be the vault REFERENCE, never a redeemed
	 * secret: a tenant row that carries a live DSN puts a credential in a table
	 * every project member can read.
	 */
	var (
		title            string
		configType       string
		section          string
		source           string
		connectionString string
		statusOK         bool
		shared           bool
	)
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
SELECT elitea_title, type, section, source, data->>'connection_string', status_ok, shared
FROM %s.configuration
WHERE project_id = $1 AND type = 'pgvector' AND section = 'vectorstorage' AND source = 'system'`,
		pgx.Identifier{fmt.Sprintf("p_%d", projectID)}.Sanitize()),
		projectID,
	).Scan(&title, &configType, &section, &source, &connectionString, &statusOK, &shared); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("no vectorstorage configuration row for the new project: " +
				"an index run inside it resolves nothing and the project cannot index")
		}
		t.Fatalf("read pgvector configuration row: %v", err)
	}
	if title != vectorstoreapp.DefaultProjectPgvectorTitle {
		t.Errorf("elitea_title = %q, want %q", title, vectorstoreapp.DefaultProjectPgvectorTitle)
	}
	// The three identity columns the toolkit schema and the SDK catalogue match
	// on. A row that carries the right title and the wrong type resolves for
	// nothing.
	if configType != vectorstoreapp.ProjectPgvectorType ||
		section != vectorstoreapp.ProjectPgvectorSection ||
		source != vectorstoreapp.ProjectPgvectorSource {
		t.Errorf("row identity = (%q, %q, %q), want (%q, %q, %q)",
			configType, section, source,
			vectorstoreapp.ProjectPgvectorType,
			vectorstoreapp.ProjectPgvectorSection,
			vectorstoreapp.ProjectPgvectorSource)
	}
	if connectionString != vectorstoreapp.ProjectPgvectorReference {
		t.Errorf("stored connection_string = %q, want the vault reference %q",
			connectionString, vectorstoreapp.ProjectPgvectorReference)
	}
	if !statusOK {
		t.Error("status_ok = false; the configuration picker hides a row that is not ok")
	}
	if shared {
		t.Error("shared = true; one project's vector store must not be visible to another")
	}

	/* ── the resolution an index run performs ────────────────────────────
	 *
	 * This is the discriminating half. configurations.CurrentExpansionService
	 * resolves `pgvector_configuration` by elitea_title in the project's own
	 * tenant (expand.go's expandReference) and then redeems the reference
	 * against that project's vault. Both halves run here, through the
	 * production implementations.
	 */
	finder, err := repos.NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatalf("build configuration finder: %v", err)
	}
	resolved, found, err := finder.FindByEliteaTitle(
		ctx, int32(projectID), vectorstoreapp.DefaultProjectPgvectorTitle, false)
	if err != nil {
		t.Fatalf("resolve pgvector configuration: %v", err)
	}
	if !found {
		t.Fatal("FindByEliteaTitle finds nothing for the new project: " +
			"index start fails admission with configuration_not_found")
	}
	if resolved.Type != vectorstoreapp.ProjectPgvectorType {
		t.Errorf("resolved type = %q, want %q", resolved.Type, vectorstoreapp.ProjectPgvectorType)
	}

	vaults, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatalf("build vault loader: %v", err)
	}
	t.Cleanup(vaults.Destroy)
	unsecreter, err := storage.NewCurrentVaultUnsecreter(vaults)
	if err != nil {
		t.Fatalf("build unsecreter: %v", err)
	}
	redeemed, err := unsecreter.Unsecret(ctx, int32(projectID), resolved.Data)
	if err != nil {
		t.Fatalf("redeem the pgvector connection string: %v", err)
	}
	dsn, _ := redeemed["connection_string"].(string)
	if dsn == "" {
		t.Fatal("the redeemed connection string is empty")
	}
	// The trap this assertion exists for: the unsecreter leaves an unresolved
	// placeholder VERBATIM rather than failing, and a literal
	// "{{secret.pgvector_project_connstr}}" passes every validator on the index
	// path. The run would then fail much later as a connection error against a
	// host of that name, which reads as a network fault rather than as missing
	// provisioning.
	if strings.Contains(dsn, "{{secret.") {
		t.Fatalf("the connection string is still an unredeemed placeholder (%q): "+
			"the configuration row exists but its vault material does not", dsn)
	}

	/* ── and the target really exists ────────────────────────────────────
	 *
	 * Connecting is what separates a well-formed DSN from a provisioned vector
	 * store. It proves the per-project role and database were created, and that
	 * the stored password authenticates.
	 */
	assertVectorStoreAccepts(ctx, t, dsn, projectID)
}

func TestProvisionCompensatesTheVectorStoreWhenALaterStepFails(t *testing.T) {
	skipWithoutVectorExtension(t)

	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	seedPublicPgvectorBootstrap(ctx, t, pool)
	vectorStore := newProjectVectorStoreForTest(t, pool)

	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(vectorStore),
		// Required since #373: Provision refuses to run without a vault
		// bootstrapper, so every provisioning test must supply one.
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	// project_admin runs AFTER project_pgvector and fails on an address that
	// matches no account. No fake is needed to make a later step fail.
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name:        "Rolled Back Project",
		OwnerID:     1,
		AdminEmails: []string{"nobody@example.test"},
		AdminRoles:  []string{"admin"},
		Limits:      projectprovisioning.DefaultLimits(),
	})
	if !errors.Is(err, projectprovisioning.ErrUnknownAdminEmail) {
		t.Fatalf("err = %v, want ErrUnknownAdminEmail", err)
	}
	projectID := result.ProjectID
	if projectID == 0 {
		t.Fatal("the failure path reported no project id, so this test cannot check what was left behind")
	}
	dropProvisionedVectorStore(t, projectID)

	// Premise: the step really ran. Without this the assertions below would pass
	// against a step that never created anything.
	assertStepSucceeded(t, result.Steps, projectprovisioning.StepProjectPgvector)
	assertStepCompensated(t, result.RollbackSteps, projectprovisioning.StepProjectPgvector)

	// The tenant is dropped by a later compensation, so the row is gone with it.
	// The vault is not in the tenant: it is a centry row keyed by project, and
	// nothing else in the pipeline removes it.
	var vaultRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.secrets_key WHERE id = $1`,
		fmt.Sprintf("project-%d", projectID),
	).Scan(&vaultRows); err != nil {
		t.Fatalf("count vault rows: %v", err)
	}
	if vaultRows != 0 {
		t.Errorf("the project vault survived compensation: %d rows left", vaultRows)
	}

	var schemaExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		fmt.Sprintf("p_%d", projectID),
	).Scan(&schemaExists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if schemaExists {
		t.Error("the tenant schema survived compensation, so the configuration row survived with it")
	}

	var projectRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM centry.project WHERE id = $1`, projectID).Scan(&projectRows); err != nil {
		t.Fatalf("count project rows: %v", err)
	}
	if projectRows != 0 {
		t.Error("the failed project row survived compensation")
	}
}

// TestProvisionSkipsTheVectorStoreWithoutABootstrap pins the one rule that
// decides whether the step does anything: the public `elitea-pgvector` row.
//
// Without it the deployment runs no vector store at all and no project — old or
// new — can index, so failing project creation over it would take away a
// capability the deployment never had.
func TestProvisionSkipsTheVectorStoreWithoutABootstrap(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Deliberately no seedPublicPgvectorBootstrap.
	vectorStore := newProjectVectorStoreForTest(t, pool)

	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithVectorStore(vectorStore),
		// Required since #373: Provision refuses to run without a vault
		// bootstrapper, so every provisioning test must supply one.
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "No Vector Store", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision failed with no bootstrap configured: %v (steps=%+v)", err, result.Steps)
	}
	assertStepSucceeded(t, result.Steps, projectprovisioning.StepProjectPgvector)

	var createSuccess bool
	if err := pool.QueryRow(ctx,
		`SELECT create_success FROM centry.project WHERE id = $1`, result.ProjectID,
	).Scan(&createSuccess); err != nil {
		t.Fatalf("read project row: %v", err)
	}
	if !createSuccess {
		t.Error("the project was not marked created")
	}
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// seedPublicPgvectorBootstrap writes the public project's `elitea-pgvector`
// configuration, which is where pylon reads the platform bootstrap from and
// where the composition reads it too.
//
// It points at this test's own PostgreSQL server, so the provisioner creates a
// real role and a real database there. The `postgresql+psycopg://` scheme is
// the stored form: production rows carry SQLAlchemy's scheme, and normalising
// it is part of what the composition must do.
func seedPublicPgvectorBootstrap(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	config := pool.Config().ConnConfig
	dsn := fmt.Sprintf("postgresql+psycopg://%s:%s@%s:%d/%s",
		config.User, config.Password, config.Host, config.Port, config.Database)

	if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.configuration
    (project_id, elitea_title, label, type, section, data, meta, shared, status_ok, source)
VALUES ($1, $2, $2, 'pgvector', 'vectorstorage', jsonb_build_object('connection_string', $3::text),
        '{}'::jsonb, false, true, 'system')
ON CONFLICT (elitea_title) DO UPDATE SET data = EXCLUDED.data`,
		pgx.Identifier{fmt.Sprintf("p_%d", referenceProjectID)}.Sanitize()),
		referenceProjectID, vectorstoreapp.DefaultProjectPgvectorTitle, dsn,
	); err != nil {
		t.Fatalf("seed the public pgvector bootstrap: %v", err)
	}
}

// newProjectVectorStoreForTest composes the production collaborator. Nothing is
// faked: a fake here would prove only that the step calls something.
//
// The secrets handler is built HERE, and it reads SECRETS_MASTER_KEY at
// construction. A caller that sets that variable must set it before this runs.
func newProjectVectorStoreForTest(t *testing.T, pool *pgxpool.Pool) *runtimecomposition.ProjectVectorStore {
	t.Helper()
	return newProjectVectorStoreWithSecretsForTest(t, pool, v2secrets.NewHandler(pool))
}

// newProjectVectorStoreWithSecretsForTest composes the same collaborator with a
// caller-supplied vault port, so a case can prove what happens when the material
// writer holds a DIFFERENT master key from the vault creator (#399).
func newProjectVectorStoreWithSecretsForTest(
	t *testing.T,
	pool *pgxpool.Pool,
	secrets runtimecomposition.ProjectVaultSecrets,
) *runtimecomposition.ProjectVectorStore {
	t.Helper()
	runtime, err := runtimecomposition.NewCurrentConfigurationsRuntime(pool, referenceProjectID, "", nil)
	if err != nil {
		t.Fatalf("compose the Configurations runtime: %v", err)
	}
	t.Cleanup(runtime.Destroy)
	vectorStore, err := runtime.NewProjectVectorStore(pool, secrets, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("compose the project vector store: %v", err)
	}
	return vectorStore
}

// assertVectorStoreAccepts opens the redeemed DSN and writes through it.
//
// A SELECT alone would pass for a role with no rights on its own database. The
// temporary table proves the role can create the objects an index run creates.
func assertVectorStoreAccepts(ctx context.Context, t *testing.T, dsn string, projectID int64) {
	t.Helper()
	normalized := strings.Replace(dsn, "postgresql+psycopg://", "postgresql://", 1)
	connection, err := pgx.Connect(ctx, normalized)
	if err != nil {
		t.Fatalf("the redeemed connection string does not open a session: %v", err)
	}
	defer func() { _ = connection.Close(context.WithoutCancel(ctx)) }()

	var database string
	if err := connection.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatalf("query the project vector store: %v", err)
	}
	if want := fmt.Sprintf("project_%d", projectID); database != want {
		t.Errorf("the connection string points at database %q, want %q", database, want)
	}
	if _, err := connection.Exec(ctx,
		`CREATE TEMPORARY TABLE index_probe (id integer)`); err != nil {
		t.Errorf("the project role cannot create a table in its own vector store: %v", err)
	}
}

func assertStepSucceeded(t *testing.T, steps []projectprovisioning.StepStatus, name string) {
	t.Helper()
	for _, status := range steps {
		if status.Step != name {
			continue
		}
		if status.OK == nil || !*status.OK {
			t.Fatalf("step %q did not report success: %+v", name, status)
		}
		return
	}
	t.Fatalf("step %q was never attempted; the pipeline is %+v", name, steps)
}

func assertStepCompensated(t *testing.T, steps []projectprovisioning.StepStatus, name string) {
	t.Helper()
	for _, status := range steps {
		if status.Step != name {
			continue
		}
		if status.OK == nil || !*status.OK {
			t.Fatalf("compensation for step %q failed: %+v", name, status)
		}
		return
	}
	t.Fatalf("step %q was not compensated; the rollback is %+v", name, steps)
}

// dropProvisionedVectorStore removes the role and database the provisioner
// created on the shared test server. The isolated database the fixture makes is
// dropped by newProvisioningPool; these two live beside it.
func dropProvisionedVectorStore(t *testing.T, projectID int64) {
	t.Helper()
	t.Cleanup(func() {
		databaseURL := testpg.DSN(os.Getenv(databaseURLEnv))
		if databaseURL == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := pgxpool.New(ctx, databaseURL)
		if err != nil {
			return
		}
		defer admin.Close()
		for _, statement := range []string{
			fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`,
				pgx.Identifier{fmt.Sprintf("project_%d", projectID)}.Sanitize()),
			fmt.Sprintf(`DROP ROLE IF EXISTS %s`,
				pgx.Identifier{fmt.Sprintf("project_%d_user", projectID)}.Sanitize()),
		} {
			_, _ = admin.Exec(ctx, statement)
		}
	})
}

// skipWithoutVectorExtension states the missing dependency BEFORE the test
// provisions anything.
//
// WHY THIS GUARD EXISTS (#423)
//
// The four tests that call it run the project_pgvector step, which executes
// CREATE EXTENSION vector inside the per-project database. On a server that
// does not ship pgvector the step fails with "project pgvector provisioning
// unavailable", and the four report a red test — a defect in the code under
// test — for a dependency the developer's PostgreSQL simply does not have.
// internal/infra/pgvector already answers the same condition with the same
// sentence; this says it in the same words.
//
// It is a LOCAL accommodation only. The Test job in .github/workflows/ci-go.yml
// runs a pgvector image and sets ELITEA_REQUIRE_PGVECTOR_POSTGRES_TEST, which
// turns this skip into a failure — the four tests cannot go quiet there, which
// is the whole point of the ledger in scripts/go/declared-skips.txt (#423).
// Same shape as ELITEA_REQUIRE_LEGACY_RBAC_POSTGRES_TEST.
func skipWithoutVectorExtension(t *testing.T) {
	t.Helper()
	const requireEnvironment = "ELITEA_REQUIRE_PGVECTOR_POSTGRES_TEST"
	databaseURL := provisioningDatabaseURL()
	if databaseURL == "" {
		// newProvisioningPool reports the absent DSN, with its own message.
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to %s: %v", databaseURLEnv, err)
	}
	defer func() { _ = connection.Close(context.Background()) }()

	var available bool
	if err := connection.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'vector'
)`).Scan(&available); err != nil {
		t.Fatalf("read the available extensions of the %s server: %v", databaseURLEnv, err)
	}
	if available {
		return
	}
	if os.Getenv(requireEnvironment) == "true" {
		t.Fatalf("%s is set, and the %s server does not provide the vector extension: "+
			"run this job against a pgvector image rather than letting the coverage go silent",
			requireEnvironment, databaseURLEnv)
	}
	t.Skipf("the %s server does not provide the vector extension", databaseURLEnv)
}
