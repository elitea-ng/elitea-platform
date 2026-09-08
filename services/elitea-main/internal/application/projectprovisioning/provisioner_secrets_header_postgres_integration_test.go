package projectprovisioning_test

// The project's `X-SECRET` value (#408), at the two levels that own it.
//
// A project vault with no `secrets_header_value` accepts the literal string
// "secret" on the version-details route, because pylon's check_secret_header
// reads `secrets.get("secrets_header_value", "secret")` and the Go port
// reproduces that fallback. Every project the Go provisioner created was in
// that state: EnsureProjectVault creates an EMPTY vault.
//
// These cases pin the two halves that close it — the provisioning step for new
// projects, and the backfill for the projects that already exist — against a
// real database, with the production reader doing the reading.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// theGuessableDefault is what a project with no value accepts today. It is
// spelled out here rather than imported, so a change to the constant in
// api/v2/applications cannot make this test agree with itself.
const theGuessableDefault = "secret"

// TestProvisionSealsARandomSecretsHeaderValue is the acceptance criterion: a
// new project holds a value, the value is not the literal default, and the
// production reader can open it.
func TestProvisionSealsARandomSecretsHeaderValue(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	first, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Header Valued Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, first.Steps)
	}

	secrets := v2secrets.NewHandler(pool)
	value, err := secrets.LookupProjectSecret(ctx,
		fmt.Sprintf("%d", first.ProjectID), v2secrets.SecretsHeaderValueName)
	if err != nil {
		t.Fatalf("the provisioned vault holds no %s: %v", v2secrets.SecretsHeaderValueName, err)
	}
	if value == theGuessableDefault {
		t.Fatalf("the provisioned value is the literal %q, so the X-SECRET check accepts a guess",
			theGuessableDefault)
	}
	if len(value) < 32 {
		t.Errorf("the provisioned value is %d characters; that is short enough to search", len(value))
	}

	// It is random, not derived. Two projects provisioned by the same process
	// must not share a value: one leaked value would otherwise open every
	// project.
	second, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Second Header Valued Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision the second project: %v (steps=%+v)", err, second.Steps)
	}
	other, err := secrets.LookupProjectSecret(ctx,
		fmt.Sprintf("%d", second.ProjectID), v2secrets.SecretsHeaderValueName)
	if err != nil {
		t.Fatalf("the second provisioned vault holds no %s: %v", v2secrets.SecretsHeaderValueName, err)
	}
	if other == value {
		t.Error("two projects were given the SAME X-SECRET value")
	}

	// The step converges. A second pass over a project that holds a value must
	// keep it: the SDK sends this value on every sub-agent call, so a rewrite
	// refuses the calls that are in flight.
	written, err := secrets.EnsureProjectSecretsHeaderValue(ctx, fmt.Sprintf("%d", first.ProjectID))
	if err != nil {
		t.Fatalf("second EnsureProjectSecretsHeaderValue: %v", err)
	}
	if written {
		t.Error("the second pass reported a write over a value that was already there")
	}
	kept, err := secrets.LookupProjectSecret(ctx,
		fmt.Sprintf("%d", first.ProjectID), v2secrets.SecretsHeaderValueName)
	if err != nil || kept != value {
		t.Errorf("the second pass replaced the value: %q -> %q (err=%v)", value, kept, err)
	}
}

// TestProvisionCompensationRemovesTheHeaderValue is the fourth acceptance
// criterion: a later step failure takes the value with the vault.
//
// A surviving value would be worse than none. The rows are keyed by the TEXT
// `project-<id>`, so the next project to draw the same id adopts them, and it
// would inherit an `X-SECRET` value that a failed provisioning run had already
// handed out.
func TestProvisionCompensationRemovesTheHeaderValue(t *testing.T) {
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
		Name: "Doomed After Its Header Value", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err == nil {
		t.Fatal("provision reported success while the bucket step could not run")
	}
	if result.ProjectID == 0 {
		t.Fatal("the failure path reported no project id, so the test cannot check what was left behind")
	}

	// PREMISE. The vault step must have run and succeeded, or "the value is
	// gone" is true of a value that was never written.
	var sealed bool
	for _, status := range result.Steps {
		if status.Step == projectprovisioning.StepProjectSecrets && status.OK != nil && *status.OK {
			sealed = true
		}
	}
	if !sealed {
		t.Fatalf("the vault step did not run before the failure, so this test proves nothing: %+v", result.Steps)
	}

	_, err = v2secrets.NewHandler(pool).LookupProjectSecret(ctx,
		fmt.Sprintf("%d", result.ProjectID), v2secrets.SecretsHeaderValueName)
	if err == nil {
		t.Fatalf("the X-SECRET value survived compensation; project %d would hand it to its successor",
			result.ProjectID)
	}
	if !errors.Is(err, v2secrets.ErrVaultAbsent) {
		t.Errorf("lookup after compensation err = %v, want ErrVaultAbsent", err)
	}
}

// TestBackfillGivesAnExistingProjectAHeaderValue is step 2 of #408: the
// projects that were created before the provisioning step existed.
//
// It also pins the count the pull request has to state. A pass that reports
// nothing is how an operator comes to believe work happened that did not.
func TestBackfillGivesAnExistingProjectAHeaderValue(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Provisioned Before The Fix", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v (steps=%+v)", err, result.Steps)
	}
	projectID := fmt.Sprintf("%d", result.ProjectID)
	secrets := v2secrets.NewHandler(pool)

	// Put the project back in the state every project was in: a readable vault
	// holding no value. StoreProjectSecrets rewrites the vault, so writing an
	// empty value is the shape a pre-#408 project has.
	if err := secrets.StoreProjectSecrets(ctx, projectID,
		map[string]string{v2secrets.SecretsHeaderValueName: ""}); err != nil {
		t.Fatalf("clear the header value: %v", err)
	}

	report, err := secrets.BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if report.Projects == 0 {
		t.Fatal("the backfill examined no project, so it cannot have corrected one")
	}
	if report.Written == 0 {
		t.Fatalf("the backfill wrote nothing while a project had no value: %+v", report)
	}
	if report.Skipped != 0 {
		t.Errorf("%d vaults would not open: %+v", report.Skipped, report)
	}

	value, err := secrets.LookupProjectSecret(ctx, projectID, v2secrets.SecretsHeaderValueName)
	if err != nil {
		t.Fatalf("the backfilled vault holds no %s: %v", v2secrets.SecretsHeaderValueName, err)
	}
	if value == "" || value == theGuessableDefault {
		t.Fatalf("the backfilled value is %q", value)
	}

	// The pass converges. A second run writes nothing and reports the project
	// as already set, so it may run on every start.
	second, err := secrets.BackfillProjectSecretsHeaderValues(ctx)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if second.Written != 0 {
		t.Errorf("the second pass rewrote %d values; the SDK calls holding the old one would refuse",
			second.Written)
	}
	if second.AlreadySet == 0 {
		t.Errorf("the second pass reported no project as already set: %+v", second)
	}
	kept, err := secrets.LookupProjectSecret(ctx, projectID, v2secrets.SecretsHeaderValueName)
	if err != nil || kept != value {
		t.Errorf("the second pass changed the value: %q -> %q (err=%v)", value, kept, err)
	}
}
