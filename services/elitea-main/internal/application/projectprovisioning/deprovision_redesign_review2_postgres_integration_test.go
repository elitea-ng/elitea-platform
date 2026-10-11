package projectprovisioning_test

// The round-2 review fixes of the row-first delete (#1211, PR #1245).
//
//   TestReusedIdIsNeverCleanedUnderALiveProject / TestProvisionRefusesAnIdTheJournalStillOwns
//       an id that comes back is never destroyed by the cleanup of its earlier life
//   TestDeleteAdoptsTheLeftoversOfAProjectWithNoJournal
//       a project row already gone, with leftovers and no journal row
//   TestQuietDropGivesUpAfterTheLimit
//       a drop for a store nothing recorded is retried a bounded number of times
//   TestBudgetedDeleteContinuesInTheBackgroundWithoutTheReconciler
//       the 202 is true: the detached run finishes by itself
//   TestBlockedCascadeFailsTheDecisionCleanly
//       lock_timeout / statement_timeout on the deciding transaction
//   TestTheRunGetsItsFullLeaseWhenItStarts
//       a decision that waited on a lock does not shorten the run's lease
//   TestShutdownReleasesTheLeaseWithoutGrowingTheBackoff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// hookBuckets is an object store with no storage behind it. Teardown counts its
// calls, runs onTeardown, and then waits for gate (when set) or the context.
type hookBuckets struct {
	teardowns  atomic.Int32
	onTeardown func(ctx context.Context)
	gate       chan struct{}
}

func (*hookBuckets) BootstrapProjectBuckets(context.Context, string) error { return nil }
func (h *hookBuckets) TeardownProjectBuckets(ctx context.Context, _ string) error {
	h.teardowns.Add(1)
	if h.onTeardown != nil {
		h.onTeardown(ctx)
	}
	if h.gate != nil {
		select {
		case <-h.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func newReview2Provisioner(
	t *testing.T, pool *pgxpool.Pool, logger *slog.Logger,
	buckets projectprovisioning.ArtifactBootstrapper, store projectprovisioning.ProjectVectorStore,
	extra ...projectprovisioning.Option,
) *projectprovisioning.Provisioner {
	t.Helper()
	options := []projectprovisioning.Option{projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool))}
	if buckets != nil {
		options = append(options, projectprovisioning.WithArtifactBuckets(buckets))
	}
	if store != nil {
		options = append(options, projectprovisioning.WithVectorStore(store))
	}
	options = append(options, extra...)
	return projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), logger, options...)
}

func schemaExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`,
		fmt.Sprintf("p_%d", projectID)).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func openJournalRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64, cleanup string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO centry.project_deletions (project_id, cleanup) VALUES ($1, $2::jsonb)`, projectID, cleanup); err != nil {
		t.Fatalf("open a journal row for %d: %v", projectID, err)
	}
}

func insertBareProject(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO centry.project (id, name, owner_id, plugins, create_success) VALUES ($1, $2, 1, '{}', true)`,
		id, fmt.Sprintf("Bare %d", id)); err != nil {
		t.Fatalf("insert project %d: %v", id, err)
	}
}

func assertSuperseded(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	entry := readJournal(ctx, t, pool, projectID)
	if !entry.Completed || entry.Leased || entry.Cleanup["superseded"] != true {
		t.Fatalf("journal = %+v, want a completed, unleased row flagged superseded", entry)
	}
}

// An id that comes back is a live project, whatever the journal still owes for
// its earlier life: the cleanup closes the row and touches nothing.
func TestReusedIdIsNeverCleanedUnderALiveProject(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	t.Run("journal open, project live: no step runs", func(t *testing.T) {
		var drops atomic.Int32
		store := &scriptedVectorStore{has: true, dropFn: func(context.Context, int64, bool) (string, error) {
			drops.Add(1)
			return "", nil
		}}
		buckets := &hookBuckets{}
		var logs bytes.Buffer
		provisioner := newReview2Provisioner(t, pool, slog.New(slog.NewTextHandler(&logs, nil)), buckets, store)
		projectID := provisionPlain(ctx, t, provisioner, "Live Under An Old Journal")
		openJournalRow(ctx, t, pool, projectID, `{"had_vector_store":true,"done":{}}`)

		completed, err := provisioner.ResumeDeletion(ctx, projectID)
		if err != nil || !completed {
			t.Fatalf("resume = completed %v, err %v; want the row closed", completed, err)
		}
		if buckets.teardowns.Load() != 0 || drops.Load() != 0 {
			t.Fatalf("a live project was cleaned: teardowns=%d drops=%d", buckets.teardowns.Load(), drops.Load())
		}
		if !schemaExists(ctx, t, pool, projectID) || projectRowCount(ctx, t, pool, projectID) != 1 {
			t.Fatal("the live project lost its schema or its row")
		}
		assertSuperseded(ctx, t, pool, projectID)
		if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "superseded") {
			t.Fatalf("the supersede was not logged at warn:\n%s", logs.String())
		}
	})

	t.Run("a real PgVector database is left alone", func(t *testing.T) {
		skipWithoutVectorExtension(t)
		provisioner, projectID := provisionIndexableProject(ctx, t, pool, "Live Vector Under An Old Journal")
		openJournalRow(ctx, t, pool, projectID, fmt.Sprintf(`{"had_vector_store":true,"vector_database":"project_%d","done":{}}`, projectID))

		if completed, err := provisioner.ResumeDeletion(ctx, projectID); err != nil || !completed {
			t.Fatalf("resume = %v, %v", completed, err)
		}
		if database, role := vectorStoreResidue(ctx, t, pool, projectID); !database || !role {
			t.Fatalf("the live project's vector store was dropped: database=%v role=%v", database, role)
		}
		if !schemaExists(ctx, t, pool, projectID) {
			t.Fatal("the live project's schema was dropped")
		}
		assertSuperseded(ctx, t, pool, projectID)
	})

	t.Run("the project appears between two steps", func(t *testing.T) {
		const id = 9300
		buckets := &hookBuckets{}
		provisioner := newReview2Provisioner(t, pool, nil, buckets, nil)
		if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, fmt.Sprintf("p_%d", id)); err != nil {
			t.Fatal(err)
		}
		openJournalRow(ctx, t, pool, id, `{"done":{}}`)
		// The first step runs while the id is free; the id is taken inside it.
		buckets.onTeardown = func(context.Context) { insertBareProject(ctx, t, pool, id) }

		if completed, err := provisioner.ResumeDeletion(ctx, id); err != nil || !completed {
			t.Fatalf("resume = %v, %v", completed, err)
		}
		if buckets.teardowns.Load() != 1 {
			t.Fatalf("teardowns = %d, want the first step to have run once", buckets.teardowns.Load())
		}
		if !schemaExists(ctx, t, pool, id) {
			t.Fatal("the schema step ran after the id was taken")
		}
		assertSuperseded(ctx, t, pool, id)
	})
}

// The other side of the race: Provision never builds on an id whose cleanup is
// still owed.
func TestProvisionRefusesAnIdTheJournalStillOwns(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	provisioner := newReview2Provisioner(t, pool, nil, nil, nil)

	var sequence string
	if err := pool.QueryRow(ctx, `SELECT pg_get_serial_sequence('centry.project', 'id')`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	var next int64
	if err := pool.QueryRow(ctx, `SELECT nextval($1)`, sequence).Scan(&next); err != nil {
		t.Fatal(err)
	}
	rewind := func() {
		if _, err := pool.Exec(ctx, `SELECT setval($1, $2)`, sequence, next-1); err != nil {
			t.Fatal(err)
		}
	}
	rewind()
	openJournalRow(ctx, t, pool, next, `{"done":{}}`)

	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Reused", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if !errors.Is(err, projectprovisioning.ErrProjectIDInCleanup) {
		t.Fatalf("provision = %v, want ErrProjectIDInCleanup", err)
	}
	if projectRowCount(ctx, t, pool, next) != 0 || schemaExists(ctx, t, pool, next) {
		t.Fatal("a refused provision left a project row or a schema behind")
	}
	if entry := readJournal(ctx, t, pool, next); entry.Completed || entry.Attempts != 0 {
		t.Fatalf("the refused provision changed the journal: %+v (%+v)", entry, result)
	}

	// The id is consumed; the next create draws another and succeeds.
	other := provisionPlain(ctx, t, provisioner, "Next Id")
	if other == next {
		t.Fatalf("the next create drew the refused id %d", next)
	}

	// A COMPLETE row does not block: the earlier life is fully cleaned.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET completed_at = now() WHERE project_id = $1`, next); err != nil {
		t.Fatal(err)
	}
	rewind()
	if got := provisionPlain(ctx, t, provisioner, "Reuse After Clean"); got != next {
		t.Fatalf("provision drew %d, want the reusable id %d", got, next)
	}
}

// vectorProbeStore forwards to the real store, including its database probe,
// with a drop that fails while failing is set.
type vectorProbeStore struct {
	*runtimecomposition.ProjectVectorStore
	failing atomic.Bool
}

func (s *vectorProbeStore) DropProjectVectorStore(ctx context.Context, projectID int64, hadStore bool) (string, error) {
	if s.failing.Load() {
		return fmt.Sprintf("project_%d", projectID), errors.New("pg unreachable")
	}
	return s.ProjectVectorStore.DropProjectVectorStore(ctx, projectID, hadStore)
}

// A DELETE that finds no project row still cleans up what the id left behind,
// when no journal row says it was already handled.
func TestDeleteAdoptsTheLeftoversOfAProjectWithNoJournal(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// gone makes a project whose row and journal are both gone, as an older
	// build leaves it, and returns its id.
	gone := func(t *testing.T, provisioner *projectprovisioning.Provisioner, name string) int64 {
		t.Helper()
		id := provisionPlain(ctx, t, provisioner, name)
		if _, err := provisioner.Deprovision(ctx, id); err != nil {
			t.Fatalf("premise delete: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM centry.project_deletions WHERE project_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("a tenant schema", func(t *testing.T) {
		provisioner := newReview2Provisioner(t, pool, nil, nil, nil)
		id := gone(t, provisioner, "Schema Leftover")
		if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, fmt.Sprintf("p_%d", id)); err != nil {
			t.Fatal(err)
		}

		result, err := provisioner.Deprovision(ctx, id)
		if err != nil || len(result.Pending) != 0 {
			t.Fatalf("delete = %v (pending %v), want the leftovers adopted and cleaned", err, result.Pending)
		}
		if schemaExists(ctx, t, pool, id) {
			t.Fatal("the leftover schema survived")
		}
		entry := readJournal(ctx, t, pool, id)
		if !entry.Completed || entry.Cleanup["had_vector_store"] == true {
			t.Fatalf("journal = %+v, want an adopted, completed row", entry)
		}
		// A second delete is the ordinary 404: the journal row is the record.
		if _, err := provisioner.Deprovision(ctx, id); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
			t.Fatalf("second delete = %v, want ErrProjectNotFound", err)
		}
	})

	t.Run("a live bucket", func(t *testing.T) {
		buckets := &hookBuckets{}
		provisioner := newReview2Provisioner(t, pool, nil, buckets, nil)
		id := gone(t, provisioner, "Bucket Leftover")
		buckets.teardowns.Store(0)
		if _, err := pool.Exec(ctx,
			`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'orphan', 'local')`, id); err != nil {
			t.Skipf("the storage schema refuses a bucket for a missing project: %v", err)
		}
		buckets.onTeardown = func(ctx context.Context) {
			if _, err := pool.Exec(ctx, `DELETE FROM elitea_storage.buckets WHERE project_id = $1`, id); err != nil {
				t.Errorf("purge: %v", err)
			}
		}

		if result, err := provisioner.Deprovision(ctx, id); err != nil || len(result.Pending) != 0 {
			t.Fatalf("delete = %v (pending %v), want the bucket adopted and purged", err, result.Pending)
		}
		if buckets.teardowns.Load() != 1 {
			t.Fatalf("teardowns = %d, want one purge", buckets.teardowns.Load())
		}
		if entry := readJournal(ctx, t, pool, id); !entry.Completed {
			t.Fatalf("journal = %+v, want complete", entry)
		}
	})

	t.Run("a PgVector database", func(t *testing.T) {
		skipWithoutVectorExtension(t)
		seedPublicPgvectorBootstrap(ctx, t, pool)
		store := &vectorProbeStore{ProjectVectorStore: newProjectVectorStoreForTest(t, pool)}
		store.failing.Store(true)
		provisioner := newReview2Provisioner(t, pool, nil, nil, store)
		id := provisionPlain(ctx, t, provisioner, "Database Leftover")
		dropProvisionedVectorStore(t, id)
		if _, err := provisioner.Deprovision(ctx, id); !errors.Is(err, projectprovisioning.ErrVectorStoreNotDropped) {
			t.Fatalf("premise delete = %v, want the drop to fail", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM centry.project_deletions WHERE project_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if database, _ := vectorStoreResidue(ctx, t, pool, id); !database {
			t.Fatal("premise: the database is gone")
		}
		store.failing.Store(false)

		if result, err := provisioner.Deprovision(ctx, id); err != nil || len(result.Pending) != 0 {
			t.Fatalf("delete = %v (pending %v), want the database adopted and dropped", err, result.Pending)
		}
		if database, role := vectorStoreResidue(ctx, t, pool, id); database || role {
			t.Fatalf("database=%v role=%v, want both dropped", database, role)
		}
		entry := readJournal(ctx, t, pool, id)
		if !entry.Completed || entry.Cleanup["had_vector_store"] != true {
			t.Fatalf("journal = %+v, want had_vector_store recorded from the probe", entry)
		}
	})

	t.Run("nothing left: 404 and no journal row", func(t *testing.T) {
		provisioner := newReview2Provisioner(t, pool, nil, nil, nil)
		if _, err := provisioner.Deprovision(ctx, 777001); !errors.Is(err, projectprovisioning.ErrProjectNotFound) {
			t.Fatalf("delete = %v, want ErrProjectNotFound", err)
		}
		if journalRows(ctx, t, pool, 777001) != 0 {
			t.Fatal("a delete of an id that never existed wrote a journal row")
		}
	})
}

// A drop that fails for a store nothing recorded is retried at most the limit
// and then given up, with a note and a warning. The reconciler counts the
// retries as unfinished and only the last run as finished.
func TestQuietDropGivesUpAfterTheLimit(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var drops atomic.Int32
	store := &scriptedVectorStore{has: false, dropFn: func(context.Context, int64, bool) (string, error) {
		drops.Add(1)
		return "project_x", errors.New("pgvector server unreachable")
	}}
	var logs bytes.Buffer
	provisioner := newReview2Provisioner(t, pool, slog.New(slog.NewTextHandler(&logs, nil)), nil, store,
		projectprovisioning.WithQuietDropLimit(3))
	projectID := provisionPlain(ctx, t, provisioner, "Quiet Give Up")

	if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if entry := readJournal(ctx, t, pool, projectID); entry.Completed || entry.Attempts != 1 {
		t.Fatalf("journal after one failure = %+v, want an open row", entry)
	}

	reconciler, err := runtimecomposition.NewProjectDeletionReconciler(provisioner,
		runtimecomposition.ProjectDeletionReconcilerConfig{GracePeriod: time.Millisecond, BatchSize: 5},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	due := func() {
		if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions
SET created_at = now() - interval '1 hour', next_attempt_at = now() - interval '1 second' WHERE project_id = $1`, projectID); err != nil {
			t.Fatal(err)
		}
	}
	due()
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 1 {
		t.Fatalf("second run: worked=%d err=%v", worked, err)
	}
	// Two quiet failures: not finished, counted as unfinished.
	if reconciler.Finished() != 0 || reconciler.Failed() != 1 {
		t.Fatalf("finished=%d failed=%d after a quiet retry, want 0 and 1", reconciler.Finished(), reconciler.Failed())
	}
	due()
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 1 {
		t.Fatalf("third run: worked=%d err=%v", worked, err)
	}
	if got := drops.Load(); got != 3 {
		t.Fatalf("drops = %d, want exactly the limit of 3", got)
	}
	if reconciler.Finished() != 1 || reconciler.Failed() != 1 {
		t.Fatalf("finished=%d failed=%d after the give-up, want 1 and 1", reconciler.Finished(), reconciler.Failed())
	}
	entry := readJournal(ctx, t, pool, projectID)
	if !entry.Completed || !entry.done(projectprovisioning.StepProjectPgvectorDrop) || entry.Attempts != 3 {
		t.Fatalf("journal = %+v, want the drop marked done on the third attempt", entry)
	}
	notes, _ := entry.Cleanup["notes"].(map[string]any)
	if notes[projectprovisioning.StepProjectPgvectorDrop] != "gave up: no vector store recorded" {
		t.Fatalf("notes = %v, want the give-up note", notes)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "gave up") {
		t.Fatalf("the give-up was not logged at warn:\n%s", logs.String())
	}
	// Nothing left to claim, and no further drop.
	due()
	if worked, err := reconciler.RunOnce(ctx); err != nil || worked != 0 || drops.Load() != 3 {
		t.Fatalf("after completion: worked=%d err=%v drops=%d", worked, err, drops.Load())
	}
}

// The budget bounds the WAIT and not the work: a step longer than the budget
// answers with the steps pending, and the run finishes afterwards with no
// reconciler anywhere. HandOffCleanup starts the same detached run.
func TestBudgetedDeleteContinuesInTheBackgroundWithoutTheReconciler(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for name, option := range map[string]projectprovisioning.DeprovisionOption{
		"budget":  projectprovisioning.WithCleanupBudget(200 * time.Millisecond),
		"handoff": projectprovisioning.HandOffCleanup(),
	} {
		t.Run(name, func(t *testing.T) {
			buckets := &hookBuckets{gate: make(chan struct{})}
			provisioner := newReview2Provisioner(t, pool, nil, buckets, nil)
			projectID := provisionPlain(ctx, t, provisioner, "Background "+name)

			started := time.Now()
			result, err := provisioner.Deprovision(ctx, projectID, option)
			if err != nil {
				t.Fatalf("deprovision: %v", err)
			}
			if name == "budget" && time.Since(started) < 150*time.Millisecond {
				t.Fatalf("returned after %v, before the budget", time.Since(started))
			}
			if len(result.Pending) != 3 {
				t.Fatalf("pending = %v, want all three steps (the first is still running)", result.Pending)
			}
			if entry := readJournal(ctx, t, pool, projectID); entry.Completed || !entry.Leased {
				t.Fatalf("journal = %+v, want an open row leased to the background run", entry)
			}

			close(buckets.gate)
			waitForJournalComplete(ctx, t, pool, projectID)
			if err := provisioner.WaitForCleanups(ctx); err != nil {
				t.Fatal(err)
			}
			if schemaExists(ctx, t, pool, projectID) {
				t.Fatal("the schema survived the background run")
			}
			if entry := readJournal(ctx, t, pool, projectID); entry.Attempts != 1 || entry.LastError != nil {
				t.Fatalf("journal = %+v, want one clean run", entry)
			}
		})
	}
}

// A cascade that is blocked behind a tenant row, or slow, fails the DECISION:
// the project is exactly as it was and the delete can be retried.
func TestBlockedCascadeFailsTheDecisionCleanly(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for name, options := range map[string]projectprovisioning.Option{
		"lock timeout":      projectprovisioning.WithDecisionTimeouts(300*time.Millisecond, 30*time.Second),
		"statement timeout": projectprovisioning.WithDecisionTimeouts(30*time.Second, 400*time.Millisecond),
	} {
		t.Run(name, func(t *testing.T) {
			provisioner := newReview2Provisioner(t, pool, nil, nil, nil, options)
			projectID := provisionPlain(ctx, t, provisioner, "Cascade Blocked "+name)
			if _, err := pool.Exec(ctx, fmt.Sprintf(
				`INSERT INTO p_%d.skills (name, description, owner_id, author_id) VALUES ('s', 'd', $1, 1)`, projectID), projectID); err != nil {
				t.Fatalf("seed a tenant row: %v", err)
			}
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
			if _, err := holder.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM p_%d.skills FOR UPDATE`, projectID)); err != nil {
				t.Fatal(err)
			}

			started := time.Now()
			result, err := provisioner.Deprovision(ctx, projectID)
			if !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
				t.Fatalf("delete = %v, want ErrProjectNotRemoved", err)
			}
			if time.Since(started) > 10*time.Second {
				t.Fatalf("the blocked cascade held the decision for %v", time.Since(started))
			}
			if !stepFailed(result.RollbackSteps, projectprovisioning.StepProjectModel) {
				t.Fatalf("steps = %+v, want project_model failed", result.RollbackSteps)
			}
			if projectRowCount(ctx, t, pool, projectID) != 1 || journalRows(ctx, t, pool, projectID) != 0 {
				t.Fatal("a failed decision changed the project or wrote a journal row")
			}
			email := fmt.Sprintf("system_user_%d@centry.user", projectID)
			if got := countRows(ctx, t, pool, `SELECT count(*) FROM public.auth_core__user WHERE email = $1`, email); got != 1 {
				t.Fatalf("the system user count = %d, want the identity intact", got)
			}

			// Retryable: the lock goes and the same delete succeeds.
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := provisioner.Deprovision(ctx, projectID); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if projectRowCount(ctx, t, pool, projectID) != 0 {
				t.Fatal("the retry did not remove the project")
			}
		})
	}
}

// A decision that waited on a lock has used part of the lease it stamped; the run
// restamps it when it starts, so it holds its full bound.
func TestTheRunGetsItsFullLeaseWhenItStarts(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	buckets := &hookBuckets{gate: make(chan struct{})}
	provisioner := newReview2Provisioner(t, pool, nil, buckets, nil)
	projectID := provisionPlain(ctx, t, provisioner, "Lease Restamp")
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO p_%d.skills (name, description, owner_id, author_id) VALUES ('s', 'd', $1, 1)`, projectID), projectID); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := holder.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM p_%d.skills FOR UPDATE`, projectID)); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 1)
	go func() {
		_, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.WithCleanupBudget(200*time.Millisecond))
		errs <- err
	}()
	// The decision has inserted its journal row and is now parked on the tenant
	// row. Let it wait two seconds before the lock goes.
	time.Sleep(2 * time.Second)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("delete: %v", err)
	}

	var remaining float64
	if err := pool.QueryRow(ctx,
		`SELECT extract(epoch FROM claimed_until - clock_timestamp()) FROM centry.project_deletions WHERE project_id = $1`,
		projectID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	lease := projectprovisioning.CleanupLeaseForTest().Seconds()
	if remaining < lease-1.0 {
		t.Fatalf("the run holds %.1fs of a %.0fs lease: the decision's wait was not given back", remaining, lease)
	}
	close(buckets.gate)
	if err := provisioner.WaitForCleanups(ctx); err != nil {
		t.Fatal(err)
	}
}

// A run the process stops releases its lease without counting an attempt or
// growing the backoff, and says why.
func TestShutdownReleasesTheLeaseWithoutGrowingTheBackoff(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	assertInterrupted := func(t *testing.T, projectID int64) {
		t.Helper()
		entry := readJournal(ctx, t, pool, projectID)
		if entry.Completed || entry.Attempts != 0 || entry.Leased || entry.BackedOff ||
			entry.LastError == nil || *entry.LastError != "interrupted" {
			t.Fatalf("journal = %+v, want an open row: no attempt counted, no backoff, lease released, last_error=interrupted", entry)
		}
	}

	t.Run("a delete's own run", func(t *testing.T) {
		lifecycle, shutdown := context.WithCancel(context.Background())
		defer shutdown()
		entered := make(chan struct{}, 1)
		buckets := &hookBuckets{gate: make(chan struct{}), onTeardown: func(context.Context) { entered <- struct{}{} }}
		provisioner := newReview2Provisioner(t, pool, nil, buckets, nil, projectprovisioning.WithLifecycleContext(lifecycle))
		projectID := provisionPlain(ctx, t, provisioner, "Shutdown During A Step")

		result, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.WithCleanupBudget(100*time.Millisecond))
		if err != nil || len(result.Pending) == 0 {
			t.Fatalf("deprovision = %v (pending %v), want a pending answer", err, result.Pending)
		}
		<-entered
		shutdown()
		waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
		defer waitCancel()
		if err := provisioner.WaitForCleanups(waitCtx); err != nil {
			t.Fatalf("the run did not stop with the lifecycle: %v", err)
		}
		assertInterrupted(t, projectID)
	})

	t.Run("a resumed run", func(t *testing.T) {
		const id = 9400
		entered := make(chan struct{}, 1)
		buckets := &hookBuckets{gate: make(chan struct{}), onTeardown: func(context.Context) { entered <- struct{}{} }}
		provisioner := newReview2Provisioner(t, pool, nil, buckets, nil)
		if _, err := pool.Exec(ctx, `SELECT create_tenant_schema($1)`, fmt.Sprintf("p_%d", id)); err != nil {
			t.Fatal(err)
		}
		openJournalRow(ctx, t, pool, id, `{"done":{}}`)

		process, shutdown := context.WithCancel(ctx)
		defer shutdown()
		type outcome struct {
			completed bool
			err       error
		}
		finished := make(chan outcome, 1)
		go func() {
			completed, err := provisioner.ResumeDeletion(process, id)
			finished <- outcome{completed, err}
		}()
		<-entered
		shutdown()
		select {
		case got := <-finished:
			if got.completed || got.err != nil {
				t.Fatalf("resume = %+v, want (false, nil): an interruption is not a failure", got)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the resumed run did not stop with the process context")
		}
		assertInterrupted(t, id)
	})
}
