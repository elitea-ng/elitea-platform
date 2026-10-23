package projectprovisioning_test

// What this test exists to catch (#399).
//
// Two code paths created a project vault, and they read DIFFERENT master keys.
// A third path — the material WRITER — held a third. Both creators are
// create-if-absent and idempotent, so they compose without an error and leave a
// vault that one path cannot decrypt. Nothing reports the fault. Only a later
// read fails, and by then the vault exists and looks healthy.
//
// WHY A TEST WITH NO MASTER KEY COULD NOT CATCH IT. With no master key at
// all every path agrees by accident: an absent
// SECRETS_MASTER_KEY and an absent ELITEA_VAULT_MASTER_KEY_FILE both mean
// "unwrapped". That is the standalone stack. It is not the staging deployment,
// which sets SECRETS_MASTER_KEY, and which is precisely where the two key
// sources diverge.
//
// So this file pins the DEPLOYMENT SHAPE the divergence needs:
// SECRETS_MASTER_KEY set, ELITEA_VAULT_MASTER_KEY_FILE unset. Under that shape
// the vault key row is master-key-wrapped, and a reader or writer built from
// the other source cannot open it. The final case proves that, so a future
// change that moved the writer back to the other key source would fail here
// rather than in production.
//
// The per-project PgVector material that was the first writer this pinned went
// with the per-project store (#1359); the round trip below writes through the
// same handler every later material write uses.

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// referenceMasterKey is a Fernet key: the URL-safe base64 encoding of 32 bytes,
// which is the form cryptography.Fernet.generate_key produces and the form
// SECRETS_MASTER_KEY must carry. It is a test constant and secures nothing.
const referenceMasterKey = "dGVzdC1tYXN0ZXIta2V5LTMyLWJ5dGVzLXNlZWQtMDE="

// unwrappedStoredKeyBytes is the length of a stored project key that NO master
// key wraps: the 44-byte base64 encoding of the raw Fernet key. A wrapped key
// is a Fernet token over those 44 bytes, so it is much longer. The difference
// is how this test proves the deployment shape really is the wrapped one.
const unwrappedStoredKeyBytes = 44

func TestProvisionWritesVaultMaterialTheDeploymentMasterKeyCanOpen(t *testing.T) {
	// The deployment shape. Five files under deploy/ set SECRETS_MASTER_KEY; no
	// file under deploy/ sets ELITEA_VAULT_MASTER_KEY_FILE. Set the first
	// BEFORE anything builds a secrets handler, which reads it at construction.
	t.Setenv("SECRETS_MASTER_KEY", referenceMasterKey)
	assertVaultMasterKeyFileIsUnset(t)

	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Wrapped Vault Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	projectID := result.ProjectID

	// Premise check. The step must have RUN, or every assertion below would be
	// measuring an empty result.
	assertStepSucceeded(t, result.Steps, projectprovisioning.StepProjectSecrets)

	/* ── the vault really is master-key-wrapped ──────────────────────────
	 *
	 * Without this the rest of the test could pass on an unwrapped vault, which
	 * is the shape that already worked. The divergence only exists when the
	 * stored key is wrapped, so the test must prove it got there.
	 */
	vaultID := "project-" + strconv.FormatInt(projectID, 10)
	var storedKey []byte
	if err := pool.QueryRow(ctx,
		`SELECT data FROM centry.secrets_key WHERE id = $1`, vaultID,
	).Scan(&storedKey); err != nil {
		t.Fatalf("read the stored vault key for %s: %v", vaultID, err)
	}
	if len(storedKey) == unwrappedStoredKeyBytes {
		t.Fatalf("the stored key for %s is %d bytes, which is the UNWRAPPED form: "+
			"SECRETS_MASTER_KEY did not reach the vault creator, so this test "+
			"cannot detect a key mismatch", vaultID, len(storedKey))
	}

	/* ── OPEN the vault and read a value back ────────────────────────────
	 *
	 * This is the assertion the issue turns on. A create-if-absent that never
	 * opens the vault cannot detect a key mismatch, and an existence check on
	 * the two rows passes for a vault nothing can decrypt.
	 */
	handler := v2secrets.NewHandler(pool)
	projectVaultID := strconv.FormatInt(projectID, 10)

	/* ── a full write-then-read round trip through the same key ──────────
	 *
	 * A create-if-absent that never opens the vault cannot detect a key
	 * mismatch, and an existence check on the two rows passes for a vault
	 * nothing can decrypt. This writes through the deployment's handler and
	 * reads the value back, which opens the vault both ways.
	 */
	if err := handler.StoreSecret(ctx, nil, projectVaultID, "issue_399_round_trip", "round-trip-value"); err != nil {
		t.Fatalf("write vault material through the deployment's key: %v", err)
	}
	readBack, err := handler.LookupProjectSecret(ctx, projectVaultID, "issue_399_round_trip")
	if err != nil || readBack != "round-trip-value" {
		t.Fatalf("round trip = %q error=%v, want \"round-trip-value\"", readBack, err)
	}

	/* ── the negative control ────────────────────────────────────────────
	 *
	 * The other key source is ELITEA_VAULT_MASTER_KEY_FILE, which is unset here
	 * and therefore means "no master key". A reader and a writer built that way
	 * must BOTH fail on this vault. If either succeeded, the two key sources
	 * would be interchangeable, and this whole test would prove nothing.
	 */
	otherLoader, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatalf("build the other-key vault loader: %v", err)
	}
	t.Cleanup(otherLoader.Destroy)
	if _, err := otherLoader.LoadProjectVault(ctx, projectID); err == nil {
		t.Fatal("a reader keyed off the OTHER source opened a master-key-wrapped vault: " +
			"the two key sources are not distinguishable, so this test cannot fail " +
			"when the writer moves back to the wrong one")
	}

	otherWriter, err := repos.NewCurrentSecretVaultRepository(pool, nil)
	if err != nil {
		t.Fatalf("build the other-key vault writer: %v", err)
	}
	t.Cleanup(otherWriter.Destroy)
	err = otherWriter.MutateProject(ctx, projectID, []centrysecrets.Mutation{{
		Collection: centrysecrets.RegularSecrets,
		Name:       "issue_399_round_trip",
		Value:      "written-with-the-wrong-key",
	}})
	if err == nil {
		t.Fatal("a writer keyed off the OTHER source rewrote a master-key-wrapped vault: " +
			"the negative control does not discriminate")
	}
	if !errors.Is(err, repos.ErrCurrentVaultUnavailable) {
		t.Fatalf("the other-key write failed with %v, want ErrCurrentVaultUnavailable", err)
	}

	// And the vault the wrong key could not open is still intact.
	stillThere, err := handler.LookupProjectSecret(
		ctx, projectVaultID, "issue_399_round_trip")
	if err != nil || stillThere == "written-with-the-wrong-key" {
		t.Fatalf("the failed other-key write damaged the vault: %v", err)
	}
}

// TestProvisionDeprovisionRemovesTheWrappedVault proves the delete half still
// works when only ONE owner remains.
//
// The vector-store step used to delete the vault too (#399), and that step is
// gone (#1359), so this pins that removeProjectSecrets alone is enough. A vault left
// behind is adopted by the next project that draws the same id, and its rows
// carry no foreign key to centry.project, so nothing else would ever sweep it.
func TestProvisionDeprovisionRemovesTheWrappedVault(t *testing.T) {
	t.Setenv("SECRETS_MASTER_KEY", referenceMasterKey)
	assertVaultMasterKeyFileIsUnset(t)

	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	provisioner := projectprovisioning.New(
		pool,
		migrate.New(pool, platformmigrations.Files),
		nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
	)
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Deleted Vault Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	projectID := result.ProjectID

	// Premise: the vault is there before the delete. Without this the assertion
	// below passes for a project whose vault was never created.
	if rows := countVaultRows(ctx, t, pool, projectID); rows != 2 {
		t.Fatalf("centry vault rows before the delete = %d, want 2", rows)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if rows := countVaultRows(ctx, t, pool, projectID); rows != 0 {
		t.Fatalf("centry vault rows after the delete = %d, want 0: "+
			"the vault outlived its project and the next project with this id adopts it", rows)
	}
}

// assertVaultMasterKeyFileIsUnset guards the premise of both cases above. The
// divergence needs one key source present and the other absent.
func assertVaultMasterKeyFileIsUnset(t *testing.T) {
	t.Helper()
	if value, present := os.LookupEnv("ELITEA_VAULT_MASTER_KEY_FILE"); present && value != "" {
		t.Fatalf("ELITEA_VAULT_MASTER_KEY_FILE is set to %q: this case pins the deployment "+
			"shape where it is UNSET, so it cannot run here", value)
	}
}

// countVaultRows lives in provisioner_secrets_postgres_integration_test.go.

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
