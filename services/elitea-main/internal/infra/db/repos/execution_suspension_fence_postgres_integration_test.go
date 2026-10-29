package repos

// The kill switch's admission fence (safety/fence.go) against a real
// PostgreSQL: no run admitted while a suspension is being written escapes
// both the refusal and the cancel. The admission transaction here runs the
// fence and then writes the job, exactly the shape of the four admission
// stores (the fence is their first statement, the job insert follows).
// Requires ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

// beginFencedAdmission opens an admission transaction for project 1, runs the
// fence and writes the run's job; the caller commits or rolls back.
func beginFencedAdmission(ctx context.Context, pool *pgxpool.Pool, executionID string) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return nil, err
	}
	fenced := safety.WithAdmissionSubject(ctx, safety.Subject{ProjectID: 1})
	if err := fenceSuspendedAdmission(fenced, pgxExecutor{queryer: tx}); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	bundleID := "bundle-" + executionID
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, '1', 'application/x-protobuf', 1, sha256('m'::bytea), 1, '\x00'::bytea, 'test')`, bundleID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("insert bundle: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES ($1, 1, 'cmd-' || $1, '1', 1, 1, '42', 'user:42', 'index.ingest.v1',
    '1', $2, sha256('r'::bytea), 'scope', 'key-' || $1, 'PENDING', 'RUNNING', now())`, executionID, bundleID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("insert job: %w", err)
	}
	return tx, nil
}

func waitForAdvisoryWaiter(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM pg_locks
			  WHERE locktype = 'advisory' AND NOT granted
			    AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing ever waited on the admission fence")
}

func projectSuspension(reason string) safety.NewSuspension {
	return safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeProject, ProjectID: 1},
		Reason: reason, ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	}
}

// The admission passed its check and has not committed when the suspension
// is created: the suspension waits for it, and its cancel sees the job.
func TestAdmissionFence_InFlightAdmissionIsCancelled(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	ctx := t.Context()

	tx, err := beginFencedAdmission(ctx, pool, "fence-inflight")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		s   safety.Suspension
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := repo.CreateSuspension(ctx, projectSuspension("incident"))
		done <- result{s, err}
	}()
	waitForAdvisoryWaiter(t, pool)
	select {
	case r := <-done:
		t.Fatalf("the suspension committed while an admission held the fence: %+v", r)
	default:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.s.CancelledRuns != 1 || desiredState(t, pool, "fence-inflight") != "CANCELLED" {
		t.Fatalf("cancelled %d, desired %s: the run escaped", r.s.CancelledRuns, desiredState(t, pool, "fence-inflight"))
	}
}

// The suspension is being written when the admission starts: the admission
// waits for it, and its re-read refuses the run, whatever the cache said.
func TestAdmissionFence_AdmissionAfterSuspensionIsRefused(t *testing.T) {
	_, pool := newSuspensionRepo(t)
	ctx := t.Context()

	suspension, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = suspension.Rollback(ctx) }()
	if _, err := suspension.Exec(ctx, suspensionFenceLockSQL, safety.AdmissionFenceLockName); err != nil {
		t.Fatal(err)
	}
	if _, err := suspension.Exec(ctx, `INSERT INTO elitea_runtime.execution_suspensions
		(scope, project_id, scope_key, reason, actor_id) VALUES ('project', 1, '1', 'incident', 7)`); err != nil {
		t.Fatal(err)
	}

	admitted := make(chan error, 1)
	go func() {
		tx, err := beginFencedAdmission(ctx, pool, "fence-late")
		if err == nil {
			err = tx.Commit(ctx)
		}
		admitted <- err
	}()
	waitForAdvisoryWaiter(t, pool)
	if err := suspension.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-admitted
	var refused *safety.SuspendedError
	if !errors.As(err, &refused) || refused.Scope != safety.ScopeProject {
		t.Fatalf("admission after the suspension = %v, want a project refusal", err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE execution_id = 'fence-late'`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("a refused admission wrote %d jobs (%v)", jobs, err)
	}
}

// Many admissions race one suspension: every run either was refused or
// ends cancelled. None is left running.
func TestAdmissionFence_NoRunEscapesUnderContention(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	ctx := t.Context()

	const runs = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := map[string]bool{}
	start := make(chan struct{})
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id := "fence-race-" + strconv.Itoa(i)
			tx, err := beginFencedAdmission(ctx, pool, id)
			if err != nil {
				if !errors.Is(err, safety.ErrExecutionSuspended) {
					t.Errorf("%s: %v", id, err)
				}
				return
			}
			time.Sleep(time.Duration(i%5) * time.Millisecond)
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("%s commit: %v", id, err)
				return
			}
			mu.Lock()
			admitted[id] = true
			mu.Unlock()
		}()
	}
	close(start)
	time.Sleep(5 * time.Millisecond)
	if _, err := repo.CreateSuspension(ctx, projectSuspension("race")); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	for id := range admitted {
		if got := desiredState(t, pool, id); got != "CANCELLED" {
			t.Errorf("%s was admitted and left %s", id, got)
		}
	}
}
