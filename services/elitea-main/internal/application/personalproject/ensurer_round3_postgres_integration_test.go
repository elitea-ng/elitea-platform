package personalproject_test

// Review round 3 of the project delete (#1211), from the personal-project side.
//
//   TestResolverTreatsATombstonedPersonalProjectAsGone
//       the request-time resolvers stop answering with a project being deleted,
//       and the ensurer path then repairs it.
//   TestEnsureRepairSkipsTheActiveWorkCountForAnUnfinishedProject
//       the repair of a project whose creation never finished is not refused by
//       a stray job row; it is still tombstoned first.
//   TestEnsureAsksAgainLaterWhenADeleteIsAlreadyRunning
//       a concurrent delete of the same project is "try again later".

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestResolverTreatsATombstonedPersonalProjectAsGone(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer := newTestEnsurer(t, pool)
	userID := seedUser(t, pool, "resolver-tombstone@autotest.local", "Resolver Tombstone")

	first, err := ensurer.Ensure(ctx, userID)
	if err != nil || first == 0 {
		t.Fatalf("Ensure = %d, %v", first, err)
	}
	resolver := middleware.NewDBPersonalProjectResolver(pool).WithPersonalProjectEnsurer(ensurer)
	uid := strconv.FormatInt(userID, 10)

	if got, err := resolver.PersonalProjectID(ctx, uid); err != nil || int64(got) != first {
		t.Fatalf("premise: resolver = %d, %v; want %d", got, err, first)
	}
	var exists bool
	existsSQL := "SELECT " + projectaccess.ProjectExists(1)
	if err := pool.QueryRow(ctx, existsSQL, first).Scan(&exists); err != nil || !exists {
		t.Fatalf("premise: ProjectExists = %v, %v", exists, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE centry.project SET deleting_at = now() WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}

	// The /llm resolver no longer answers with the doomed project.
	if got, err := resolver.PersonalProjectID(ctx, uid); err != nil || got != 0 {
		t.Fatalf("resolver over a tombstoned project = %d, %v; want 0 (gone)", got, err)
	}
	// The membership guards' existence check says 404 for it.
	if err := pool.QueryRow(ctx, existsSQL, first).Scan(&exists); err != nil || exists {
		t.Fatalf("ProjectExists over a tombstoned project = %v, %v; want false", exists, err)
	}
	// The ensurer path repairs: a new project, which the resolver then answers.
	second, err := ensurer.Ensure(ctx, userID)
	if err != nil || second == 0 || second == first {
		t.Fatalf("Ensure over a tombstoned project = %d, %v; want a new project", second, err)
	}
	if got, err := resolver.PersonalProjectID(ctx, uid); err != nil || int64(got) != second {
		t.Fatalf("resolver after the repair = %d, %v; want %d", got, err, second)
	}
}

// noStoreVectorStore is a vector store that has none to report, so a provisioner
// built with it runs the active-work count (which a deployment WITH a vector
// store does) without any PgVector server.
type noStoreVectorStore struct{}

func (noStoreVectorStore) ProvisionProjectVectorStore(context.Context, int64) error { return nil }
func (noStoreVectorStore) RemoveProjectVectorStore(context.Context, int64) error    { return nil }
func (noStoreVectorStore) ProjectHasVectorStore(context.Context, int64) (bool, error) {
	return false, nil
}
func (noStoreVectorStore) DropProjectVectorStore(context.Context, int64, bool) (string, error) {
	return "", nil
}

func seedStrayJob(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles
    (input_bundle_id, immutable_version, resource_project_id, media_type,
     manifest_digest, manifest_size, manifest_bytes, created_by)
VALUES ('bundle-stray', 'admission:stray', $1, 'application/x-protobuf',
        decode(repeat('61', 32), 'hex'), 1, decode('00', 'hex'), 'actor-1')`, projectID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state
) VALUES (
    'exec-stray', 1, 'cmd-stray', ($1::integer)::text, $1::integer, $1::integer, '7', '7', $2, 'v1',
    'bundle-stray', decode(repeat('61', 32), 'hex'), 'scope-stray', 'key-stray', 'RUNNING', 'RUNNING')`,
		projectID, execution.IndexIngestCapability); err != nil {
		t.Fatalf("seed a stray running job: %v", err)
	}
}

func TestEnsureRepairSkipsTheActiveWorkCountForAnUnfinishedProject(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	provisioner := projectprovisioning.New(
		pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
		projectprovisioning.WithVectorStore(noStoreVectorStore{}),
	)
	ensurer, err := personalproject.NewEnsurer(pool, provisioner)
	if err != nil {
		t.Fatal(err)
	}
	userID := seedUser(t, pool, "stray-job@autotest.local", "Stray Job")
	strandedID := seedUnfinishedProject(ctx, t, pool, userID)
	seedStrayJob(ctx, t, pool, strandedID)

	// Premise: an explicit delete of this project IS refused by the count.
	if _, err := provisioner.Deprovision(ctx, strandedID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("premise: Deprovision err = %v, want ErrProjectWorkActive", err)
	}

	// The repair is not. It removes the unfinished project and recreates.
	repaired, err := ensurer.Ensure(ctx, userID)
	if err != nil {
		t.Fatalf("Ensure over an unfinished project with a stray job: %v", err)
	}
	if repaired == 0 || repaired == strandedID {
		t.Fatalf("Ensure returned %d for stranded project %d", repaired, strandedID)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project WHERE id = $1`, strandedID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the unfinished project survived the repair: %d, %v", left, err)
	}
}

// ErrProjectDeletionInProgress from the delete is "try again later", and the
// error says so.
type inProgressProvisioner struct{}

func (inProgressProvisioner) Provision(context.Context, projectprovisioning.Request) (projectprovisioning.Result, error) {
	panic("provision must not run while another delete holds the project")
}

func (inProgressProvisioner) Deprovision(context.Context, int64, ...projectprovisioning.DeprovisionOption) (projectprovisioning.Result, error) {
	return projectprovisioning.Result{}, projectprovisioning.ErrProjectDeletionInProgress
}

func TestEnsureAsksAgainLaterWhenADeleteIsAlreadyRunning(t *testing.T) {
	ctx := context.Background()
	pool := newPersonalProjectPool(t)
	ensurer, err := personalproject.NewEnsurer(pool, inProgressProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	userID := seedUser(t, pool, "delete-running@autotest.local", "Delete Running")
	strandedID := seedUnfinishedProject(ctx, t, pool, userID)

	_, err = ensurer.Ensure(ctx, userID)
	if !errors.Is(err, projectprovisioning.ErrProjectDeletionInProgress) {
		t.Fatalf("Ensure err = %v, want ErrProjectDeletionInProgress", err)
	}
	if !strings.Contains(err.Error(), "try again later") || !strings.Contains(err.Error(), fmt.Sprint(strandedID)) {
		t.Fatalf("the error does not say what to do: %v", err)
	}
}
