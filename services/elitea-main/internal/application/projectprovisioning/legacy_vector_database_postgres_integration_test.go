package projectprovisioning_test

// The legacy PgVector drop of a project delete against a real PostgreSQL
// (#1359): a project that still has its project_<id> database and
// project_<id>_user role on the server loses both when an admin URL is
// configured; without one the journal row stays open, failing visibly, until
// the URL or the operator's retired switch is set; a project with no legacy
// database never sees the step.
//
// The legacy "server" is the test server itself, which is also where the
// platform database lives: the shared-server shape, where the deciding
// transaction sees the database in pg_database. Project ids are drawn high and
// random, because database and role names are server-wide and another test's
// scratch database numbers its projects from 1.

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacypgvector"
)

// provisionAtHighID creates a project whose id is drawn from a high random
// range, so its legacy names cannot collide with another test's.
func provisionAtHighID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, provisioner *projectprovisioning.Provisioner) int64 {
	t.Helper()
	var sequence string
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('centry.project', 'id')`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT setval($1, $2)`, sequence, 1_000_000+rand.Int64N(1_000_000_000)); err != nil {
		t.Fatal(err)
	}
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Legacy PgVector " + strconv.FormatInt(time.Now().UnixNano(), 10), OwnerID: 1,
		Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.ProjectID
}

// createLegacyDatabase creates project_<id> and project_<id>_user on the test
// server, the way the old store did (the role owns the database), and removes
// whatever is left when the test ends.
func createLegacyDatabase(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	database := pgx.Identifier{legacypgvector.DatabaseName(projectID)}.Sanitize()
	role := pgx.Identifier{legacypgvector.RoleName(projectID)}.Sanitize()
	for _, statement := range []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD 'legacy'",
		"CREATE DATABASE " + database + " OWNER " + role,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)")
		_, _ = pool.Exec(cleanupCtx, "DROP ROLE IF EXISTS "+role)
	})
}

func legacyNamesPresent(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) (database, role bool) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1),
       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $2)`,
		legacypgvector.DatabaseName(projectID), legacypgvector.RoleName(projectID)).Scan(&database, &role); err != nil {
		t.Fatal(err)
	}
	return database, role
}

func legacyDropper(t *testing.T) *legacypgvector.Dropper {
	t.Helper()
	dropper, err := legacypgvector.NewDropper(provisioningDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	return dropper
}

func TestADeleteDropsTheLegacyPgvectorDatabaseThroughTheAdminURL(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provisioner := newApplicationProvisioner(pool, projectprovisioning.WithLegacyVectorDatabases(legacyDropper(t)))
	projectID := provisionAtHighID(ctx, t, pool, provisioner)
	createLegacyDatabase(ctx, t, pool, projectID)

	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if database, role := legacyNamesPresent(ctx, t, pool, projectID); database || role {
		t.Fatalf("after the delete: database present=%v, role present=%v; want both dropped", database, role)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if !entry.Completed || !journalDone(entry, projectprovisioning.StepProjectPgvectorDrop) {
		t.Fatalf("journal = %+v, want complete with the legacy drop done", entry)
	}
	if entry.Cleanup["had_vector_store"] != true {
		t.Fatalf("journal = %+v, want the decision to have recorded the database it saw", entry)
	}
	want := "dropped legacy PgVector database " + legacypgvector.DatabaseName(projectID) + " and role " + legacypgvector.RoleName(projectID)
	if note := journalNotes(entry)[projectprovisioning.StepProjectPgvectorDrop]; note != want {
		t.Fatalf("note = %v, want %q", note, want)
	}
}

func TestWithoutAnAdminURLTheRowStaysOpenUntilTheServerIsRetired(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	deciding := newApplicationProvisioner(pool)
	projectID := provisionAtHighID(ctx, t, pool, deciding)
	createLegacyDatabase(ctx, t, pool, projectID)

	result, err := deciding.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrLegacyVectorDatabaseNotDropped) {
		t.Fatalf("err = %v, want ErrLegacyVectorDatabaseNotDropped", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectPgvectorDrop) {
		t.Fatalf("result = %+v, want the legacy drop failed", result.RollbackSteps)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Attempts != 1 || entry.LastError == nil {
		t.Fatalf("journal = %+v, want one failed run, last_error set and the row open", entry)
	}
	if journalDone(entry, projectprovisioning.StepProjectPgvectorDrop) {
		t.Fatal("a drop that did not happen is marked done")
	}
	if database, _ := legacyNamesPresent(ctx, t, pool, projectID); !database {
		t.Fatal("the legacy database went without an admin URL")
	}

	// A later run on the same configuration still refuses: the row does not
	// complete over the data by being retried.
	backdate := func() {
		if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = clock_timestamp() WHERE project_id = $1`, projectID); err != nil {
			t.Fatal(err)
		}
	}
	backdate()
	if completed, err := deciding.ResumeDeletion(ctx, projectID); completed || !errors.Is(err, projectprovisioning.ErrLegacyVectorDatabaseNotDropped) {
		t.Fatalf("resume without a URL: completed=%v err=%v, want the row still open", completed, err)
	}

	// The operator retires the server: the step closes, noted, and the
	// database is not touched (in production it went with the server).
	backdate()
	retired := newApplicationProvisioner(pool, projectprovisioning.WithLegacyVectorDatabasesRetired())
	if completed, err := retired.ResumeDeletion(ctx, projectID); err != nil || !completed {
		t.Fatalf("resume retired: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, projectID)
	if note := journalNotes(entry)[projectprovisioning.StepProjectPgvectorDrop]; note != projectprovisioning.NoteLegacyVectorDatabasesRetired {
		t.Fatalf("note = %v, want the retired note", note)
	}
}

// A project the old store never provisioned (a fresh install) shows no
// evidence, and a replica with no admin URL puts no legacy step on its row:
// the delete completes as before.
func TestAProjectWithNoLegacyDatabaseNeverSeesTheStep(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provisioner := newApplicationProvisioner(pool)
	projectID := provisionAtHighID(ctx, t, pool, provisioner)

	result, err := provisioner.Deprovision(ctx, projectID)
	if err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	for _, status := range result.RollbackSteps {
		if status.Step == projectprovisioning.StepProjectPgvectorDrop {
			t.Fatalf("result = %+v, want no legacy step", result.RollbackSteps)
		}
	}
	entry := readJournal(ctx, t, pool, projectID)
	if !entry.Completed || entry.Cleanup["had_vector_store"] != nil {
		t.Fatalf("journal = %+v, want complete with no legacy evidence recorded", entry)
	}
}

// The other evidence: the configuration row the old store wrote into the
// tenant, for a legacy server that is NOT the platform's (so pg_database
// shows nothing here). Recorded by the decision before the tenant rows
// cascade, it keeps the row open without an admin URL.
func TestTheTenantsLegacyConfigurationRowIsEvidence(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provisioner := newApplicationProvisioner(pool)
	projectID := provisionAtHighID(ctx, t, pool, provisioner)
	schema := pgx.Identifier{"p_" + strconv.FormatInt(projectID, 10), "configuration"}.Sanitize()
	if _, err := pool.Exec(ctx, `
INSERT INTO `+schema+` (uuid, project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at)
VALUES (gen_random_uuid(), $1, 'elitea-pgvector', 'PgVector ', 'vectorstorage',
        '{"connection_string":"{{secret.pgvector_project_connstr}}"}'::jsonb, '{}'::jsonb,
        false, true, 'system', now() AT TIME ZONE 'UTC')`, projectID); err != nil {
		t.Fatalf("seed the legacy configuration row: %v", err)
	}

	if _, err := provisioner.Deprovision(ctx, projectID); !errors.Is(err, projectprovisioning.ErrLegacyVectorDatabaseNotDropped) {
		t.Fatalf("err = %v, want ErrLegacyVectorDatabaseNotDropped", err)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Cleanup["had_vector_store"] != true {
		t.Fatalf("journal = %+v, want the evidence recorded and the row open", entry)
	}
}
