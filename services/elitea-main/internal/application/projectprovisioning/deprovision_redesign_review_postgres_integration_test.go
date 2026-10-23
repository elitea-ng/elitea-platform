package projectprovisioning_test

// The redesign-review fixes of the row-first delete (#1211, PR #1245).
//
//   TestIdentityIsRevokedByTheDecision
//       the system PAT, the system user, the memberships, the roles, the token
//       bindings and the vault are gone when Deprovision returns, before any
//       cleanup step has run.
//   TestBookkeepingSurvivesTheCleanupDeadline
//       a run that hits its deadline still records attempts, last_error, the
//       backoff and the steps it finished.
//   TestTwoReconcilersNeverRunTheSameRow
//       one lease per row: a row waiting in a local queue is not leased.
//   TestAmbiguousCommit
//       a commit that errors after being applied is a delete that happened.
//   TestRedeleteOfAReusedId
//       a complete journal row is reopened; an incomplete one refuses the delete.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// scriptedRemoverFunc is an application engine with nothing behind it: its
// delete runs the function. The tests wire it as the Inventory engine, whose
// step is the LAST of a run, so the base steps before it finish first.
type scriptedRemoverFunc func(ctx context.Context, projectID int64) (string, error)

func (f scriptedRemoverFunc) DeleteProjectData(ctx context.Context, projectID int64) (string, error) {
	return f(ctx, projectID)
}

func newScriptedProvisioner(t *testing.T, pool *pgxpool.Pool, remover projectprovisioning.ApplicationDataRemover, logger *slog.Logger) *projectprovisioning.Provisioner {
	t.Helper()
	options := []projectprovisioning.Option{projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool))}
	if remover != nil {
		options = append(options, projectprovisioning.WithApplicationData("inventory", remover))
	}
	return projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), logger, options...)
}

func provisionPlain(ctx context.Context, t *testing.T, provisioner *projectprovisioning.Provisioner, name string) int64 {
	t.Helper()
	result, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: name, OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision %q: %v (steps=%+v)", name, err, result.Steps)
	}
	return result.ProjectID
}

// The credentials die atomically with the row. HandOffCleanup stops Deprovision
// before any journal step, so what is gone here was removed by the deciding
// transaction alone.
func TestIdentityIsRevokedByTheDecision(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// The first cleanup step (the artifact purge) is held shut, so whatever is
	// gone when Deprovision returns was removed by the deciding transaction.
	buckets := &gatedBuckets{gate: make(chan struct{})}
	provisioner := projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil,
		projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool)),
		projectprovisioning.WithArtifactBuckets(buckets))
	projectID := provisionPlain(ctx, t, provisioner, "Identity Dies With The Row")
	email := fmt.Sprintf("system_user_%d@centry.user", projectID)

	var userID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM public.auth_core__user WHERE email = $1`, email).Scan(&userID); err != nil {
		t.Fatalf("premise: no system user: %v", err)
	}
	// A binding that names the project, so its removal is observable too.
	var tokenID int32
	if err := pool.QueryRow(ctx, `SELECT id FROM public.auth_core__token WHERE user_id = $1 AND name = 'api'`, userID).Scan(&tokenID); err != nil {
		t.Fatalf("premise: no system PAT: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_identity.token_project_binding (token_id, project_id) VALUES ($1, $2)`, tokenID, projectID); err != nil {
		t.Fatalf("seed a token binding: %v", err)
	}
	counts := func() (tokens, users, memberships, roles, bindings, vault int) {
		for dest, query := range map[*int]string{
			&tokens:      fmt.Sprintf(`SELECT count(*) FROM public.auth_core__token WHERE user_id = %d`, userID),
			&users:       fmt.Sprintf(`SELECT count(*) FROM public.auth_core__user WHERE id = %d`, userID),
			&memberships: fmt.Sprintf(`SELECT count(*) FROM public.auth_core__project_user_role WHERE project_id = %d`, projectID),
			&roles:       fmt.Sprintf(`SELECT count(*) FROM public.auth_core__project_role WHERE project_id = %d`, projectID),
			&bindings:    fmt.Sprintf(`SELECT count(*) FROM elitea_identity.token_project_binding WHERE project_id = %d`, projectID),
			&vault:       fmt.Sprintf(`SELECT count(*) FROM centry.secrets_key WHERE id = 'project-%d'`, projectID),
		} {
			if err := pool.QueryRow(ctx, query).Scan(dest); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	if tokens, users, memberships, roles, bindings, vault := counts(); tokens == 0 || users == 0 || memberships == 0 || roles == 0 || bindings == 0 || vault == 0 {
		t.Fatalf("premise: tokens=%d users=%d memberships=%d roles=%d bindings=%d vault=%d, want all present",
			tokens, users, memberships, roles, bindings, vault)
	}

	result, err := provisioner.Deprovision(ctx, projectID, projectprovisioning.HandOffCleanup())
	if err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if tokens, users, memberships, roles, bindings, vault := counts(); tokens+users+memberships+roles+bindings+vault != 0 {
		t.Fatalf("credentials outlived the decision: tokens=%d users=%d memberships=%d roles=%d bindings=%d vault=%d",
			tokens, users, memberships, roles, bindings, vault)
	}
	// The run is in the background and held at its first step: the journal is
	// open, leased to it, no run has closed, and the schema is there.
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || !entry.Leased || entry.Attempts != 0 {
		t.Fatalf("journal = %+v, want an open row leased to the background run, none closed", entry)
	}
	var schema bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, fmt.Sprintf("p_%d", projectID)).Scan(&schema); err != nil || !schema {
		t.Fatalf("the tenant schema is gone before the journal ran: exists=%v err=%v", schema, err)
	}
	if len(result.Pending) != 2 {
		t.Fatalf("pending = %v, want the two journal steps", result.Pending)
	}
	for _, name := range []string{
		projectprovisioning.StepProjectSecrets, projectprovisioning.StepSystemToken,
		projectprovisioning.StepSystemUser, projectprovisioning.StepProjectPermissions,
	} {
		if stepFailed(result.RollbackSteps, name) {
			t.Errorf("identity step %s is reported failed", name)
		}
	}

	// Released, the run finishes by itself: no reconciler is involved.
	close(buckets.gate)
	waitForJournalComplete(ctx, t, pool, projectID)
	if err := provisioner.WaitForCleanups(ctx); err != nil {
		t.Fatal(err)
	}
}

// gatedBuckets is an object store whose purge waits for the gate.
type gatedBuckets struct{ gate chan struct{} }

func (*gatedBuckets) BootstrapProjectBuckets(context.Context, string) error { return nil }
func (g *gatedBuckets) TeardownProjectBuckets(ctx context.Context, _ string) error {
	select {
	case <-g.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForJournalComplete polls until the journal row of the project is complete.
func waitForJournalComplete(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var completed bool
		if err := pool.QueryRow(ctx,
			`SELECT completed_at IS NOT NULL FROM centry.project_deletions WHERE project_id = $1`, projectID,
		).Scan(&completed); err != nil {
			t.Fatalf("read the journal of project %d: %v", projectID, err)
		}
		if completed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the journal of project %d did not complete: %+v", projectID, readJournal(ctx, t, pool, projectID))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A run that hits its deadline still records what it did, on a context of its
// own: attempts, last_error, the backoff, the finished steps, the released lease.
func TestBookkeepingSurvivesTheCleanupDeadline(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	store := scriptedRemoverFunc(func(ctx context.Context, _ int64) (string, error) {
		// Blocks until the run's deadline, and then a little past it.
		<-ctx.Done()
		time.Sleep(300 * time.Millisecond)
		return "", ctx.Err()
	})
	provisioner := newScriptedProvisioner(t, pool, store, nil)
	projectID := provisionPlain(ctx, t, provisioner, "Deadline Bookkeeping")
	projectprovisioning.SetCleanupTimeoutForTest(t, 1500*time.Millisecond)

	result, err := provisioner.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved from the run that hit its deadline", err)
	}
	if len(result.Pending) != 1 || result.Pending[0] != projectprovisioning.StepInventoryData {
		t.Fatalf("pending = %v, want only the engine step", result.Pending)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Attempts != 1 || entry.LastError == nil || !strings.Contains(*entry.LastError, projectprovisioning.StepInventoryData) {
		t.Fatalf("journal = %+v, want one attempt naming the engine step", entry)
	}
	if !entry.BackedOff || entry.Leased || entry.Completed {
		t.Fatalf("journal = %+v, want a backoff, a released lease and an open row", entry)
	}
	for _, step := range []string{projectprovisioning.StepArtifactBuckets, projectprovisioning.StepProjectSchema} {
		if !entry.done(step) {
			t.Errorf("step %s finished before the deadline but is not recorded: %+v", step, entry.Cleanup)
		}
	}
	if entry.done(projectprovisioning.StepInventoryData) {
		t.Error("the step that hit the deadline is recorded done")
	}
}

// Two reconcilers over rows whose step is slow never run the same row, and a
// row is leased only while it is being worked: nothing waits leased in a queue.
func TestTwoReconcilersNeverRunTheSameRow(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var (
		mu       sync.Mutex
		running  = map[int64]int{}
		maxSeen  = map[int64]int{}
		runs     = map[int64]int{}
		entered  = make(chan int64, 8)
		release  = make(chan struct{})
		released atomic.Bool
	)
	store := scriptedRemoverFunc(func(ctx context.Context, projectID int64) (string, error) {
		mu.Lock()
		running[projectID]++
		runs[projectID]++
		maxSeen[projectID] = max(maxSeen[projectID], running[projectID])
		mu.Unlock()
		entered <- projectID
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		running[projectID]--
		mu.Unlock()
		return "deleted", nil
	})
	provisioner := newScriptedProvisioner(t, pool, store, nil)
	for _, id := range []int{9001, 9002, 9003} {
		if _, err := pool.Exec(ctx, `
INSERT INTO centry.project_deletions (project_id, cleanup, created_at)
VALUES ($1, '{"apps":["inventory"],"done":{"artifact_buckets":true,"project_schema":true}}', now() - interval '1 hour')`, id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var reconcilers []*runtimecomposition.ProjectDeletionReconciler
	for range 2 {
		reconciler, err := runtimecomposition.NewProjectDeletionReconciler(provisioner,
			runtimecomposition.ProjectDeletionReconcilerConfig{GracePeriod: time.Millisecond, BatchSize: 10}, logger)
		if err != nil {
			t.Fatal(err)
		}
		reconcilers = append(reconcilers, reconciler)
	}
	var wg sync.WaitGroup
	for _, reconciler := range reconcilers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reconciler.RunOnce(ctx); err != nil {
				t.Errorf("RunOnce: %v", err)
			}
		}()
	}
	// Each reconciler is inside the slow step of its own row.
	first, second := <-entered, <-entered
	if first == second {
		t.Fatalf("both reconcilers entered row %d", first)
	}
	// Only the two rows being worked are leased; the third waits unleased.
	var leased int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM centry.project_deletions WHERE claimed_until > now()`).Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if leased != 2 {
		t.Fatalf("%d rows are leased while two are being worked, want 2 (no row leased in a queue)", leased)
	}
	released.Store(true)
	close(release)
	wg.Wait()

	for _, id := range []int64{9001, 9002, 9003} {
		if runs[id] != 1 || maxSeen[id] != 1 {
			t.Errorf("row %d: runs=%d maxConcurrent=%d, want one run", id, runs[id], maxSeen[id])
		}
		if entry := readJournal(ctx, t, pool, id); !entry.Completed || entry.Attempts != 1 {
			t.Errorf("row %d: journal = %+v, want complete after one run", id, entry)
		}
	}
}

// A commit that returns an error is settled by asking a fresh connection.
func TestAmbiguousCommit(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	t.Run("applied before the error: the delete happened", func(t *testing.T) {
		provisioner := newScriptedProvisioner(t, pool, nil, nil)
		projectID := provisionPlain(ctx, t, provisioner, "Commit Applied")
		projectprovisioning.SetCommitForTest(provisioner, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return errors.New("connection reset after commit")
		})
		result, err := provisioner.Deprovision(ctx, projectID)
		if err != nil {
			t.Fatalf("a commit that was applied was reported as a failure: %v", err)
		}
		if projectRowCount(ctx, t, pool, projectID) != 0 || journalRows(ctx, t, pool, projectID) != 1 {
			t.Fatal("premise: the delete is not in place")
		}
		if len(result.Pending) != 0 {
			t.Fatalf("pending = %v, want the cleanup to have run", result.Pending)
		}
		if entry := readJournal(ctx, t, pool, projectID); !entry.Completed {
			t.Fatalf("journal = %+v, want the cleanup completed", entry)
		}
	})

	t.Run("not applied: the project is unchanged", func(t *testing.T) {
		provisioner := newScriptedProvisioner(t, pool, nil, nil)
		projectID := provisionPlain(ctx, t, provisioner, "Commit Lost")
		projectprovisioning.SetCommitForTest(provisioner, func(context.Context, pgx.Tx) error {
			return errors.New("connection reset before commit")
		})
		_, err := provisioner.Deprovision(ctx, projectID)
		if !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) {
			t.Fatalf("err = %v, want ErrProjectNotRemoved", err)
		}
		if projectRowCount(ctx, t, pool, projectID) != 1 || journalRows(ctx, t, pool, projectID) != 0 {
			t.Fatal("a commit that did not happen changed the project or wrote a journal row")
		}
	})
}

// Re-deleting an id whose journal row is complete reopens the row; one that is
// still incomplete refuses the decision.
func TestRedeleteOfAReusedId(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newScriptedProvisioner(t, pool, nil, nil)
	insertProject := func(id int64) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO centry.project (id, name, owner_id, plugins, create_success) VALUES ($1, $2, 1, '{}', true)`,
			id, fmt.Sprintf("Reused %d", id)); err != nil {
			t.Fatalf("insert project %d: %v", id, err)
		}
	}

	t.Run("a complete row is reopened", func(t *testing.T) {
		const id = 9100
		insertProject(id)
		if _, err := provisioner.Deprovision(ctx, id); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if entry := readJournal(ctx, t, pool, id); !entry.Completed {
			t.Fatalf("premise: journal = %+v, want complete", entry)
		}
		if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET attempts = 7, last_error = 'old', claimed_until = now() + interval '1 hour' WHERE project_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		insertProject(id)
		if _, err := provisioner.Deprovision(ctx, id); err != nil {
			t.Fatalf("delete of the reused id: %v", err)
		}
		// The reopened row was reset (attempts 7 and the old error are gone) and
		// then ran once, from no steps done.
		entry := readJournal(ctx, t, pool, id)
		if !entry.Completed || entry.Attempts != 1 || entry.LastError != nil || entry.Leased {
			t.Fatalf("journal = %+v, want a reopened row that ran once from scratch", entry)
		}
		if projectRowCount(ctx, t, pool, id) != 0 {
			t.Fatal("the project row survived")
		}
	})

	t.Run("an incomplete row refuses the decision", func(t *testing.T) {
		const id = 9101
		insertProject(id)
		if _, err := pool.Exec(ctx, `INSERT INTO centry.project_deletions (project_id, cleanup) VALUES ($1, '{"done":{}}')`, id); err != nil {
			t.Fatal(err)
		}
		_, err := provisioner.Deprovision(ctx, id)
		if !errors.Is(err, projectprovisioning.ErrProjectNotRemoved) || !strings.Contains(err.Error(), "incomplete cleanup journal") {
			t.Fatalf("err = %v, want ErrProjectNotRemoved naming the incomplete journal row", err)
		}
		if projectRowCount(ctx, t, pool, id) != 1 {
			t.Fatal("a refused decision removed the project")
		}
	})
}
