package projectprovisioning_test

// The vector step's own rules against a real PostgreSQL (ADR-0031 decision 4;
// application_data.go): which replica records it, which closes it, a refused
// identity that must not close it, and the settle after a forced delete.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/vectorclient"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordedByRustReplica decides a delete on a replica that records the vector
// step with a store that answers retry-later, so the row stays open with the
// step pending for another replica to drain.
func recordedByRustReplica(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (int64, *scriptedRemover) {
	t.Helper()
	remover := &scriptedRemover{err: fmt.Errorf("%w: elitea-vector drop_project: connection refused (Unavailable)", platformops.ErrRetryLater)}
	deciding := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData(vectorclient.App, remover))
	created, err := deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Recorded By A Rust Replica", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deciding.Deprovision(ctx, created.ProjectID); !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved", err)
	}
	if apps := journalApps(readJournal(ctx, t, pool, created.ProjectID)); len(apps) != 1 || apps[0] != "vector" {
		t.Fatalf("apps on the row = %v, want [vector]", apps)
	}
	return created.ProjectID, remover
}

func makeDue(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = clock_timestamp() WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
}

func TestADrainOnlyReplicaRunsTheVectorStepButRecordsNone(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// A Python-indexing replica with a client: it records nothing on its own
	// deletes, and its store is not even called for them.
	drainRemover := &scriptedRemover{summary: "dropped the project's vectors in 1 collection(s)"}
	drainOnly := newApplicationProvisioner(pool, projectprovisioning.WithApplicationDataDrain(vectorclient.App, drainRemover))
	if wired := drainOnly.ApplicationsWired(); len(wired) != 0 {
		t.Fatalf("ApplicationsWired = %v on a drain-only replica, want none", wired)
	}
	created, err := drainOnly.Provision(ctx, projectprovisioning.Request{
		Name: "Python Replica", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drainOnly.Deprovision(ctx, created.ProjectID); err != nil {
		t.Fatalf("a drain-only replica's own delete failed: %v", err)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if !entry.Completed || len(journalApps(entry)) != 0 || drainRemover.calls != 0 {
		t.Fatalf("journal=%+v calls=%d; a drain-only replica must record no vector step and call no store for its own delete", entry, drainRemover.calls)
	}

	// A step another replica recorded: the drain-only replica runs it.
	recorded, rustRemover := recordedByRustReplica(ctx, t, pool)
	makeDue(ctx, t, pool, recorded)
	if completed, err := drainOnly.ResumeDeletion(ctx, recorded); err != nil || !completed {
		t.Fatalf("drain: completed=%v err=%v", completed, err)
	}
	if drainRemover.calls != 1 || rustRemover.calls != 1 {
		t.Fatalf("drain calls=%d rust calls=%d, want the drain-only replica to have run the recorded step once", drainRemover.calls, rustRemover.calls)
	}
	if notes := journalNotes(readJournal(ctx, t, pool, recorded)); notes[projectprovisioning.StepVectorData] != "dropped the project's vectors in 1 collection(s)" {
		t.Fatalf("notes = %v", notes)
	}
	// A WithApplicationData for the same name upgrades a drain-only wiring.
	both := newApplicationProvisioner(pool,
		projectprovisioning.WithApplicationDataDrain(vectorclient.App, drainRemover),
		projectprovisioning.WithApplicationData(vectorclient.App, drainRemover))
	if wired := both.ApplicationsWired(); len(wired) != 1 || wired[0] != "vector" {
		t.Fatalf("ApplicationsWired = %v, want [vector]", wired)
	}
}

func TestAReplicaWithTheStoreOffClosesTheStepAndAnUnwiredRustReplicaLeavesItPending(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	recorded, remover := recordedByRustReplica(ctx, t, pool)

	// A Rust-indexing replica without a client (a rollout): pending, as for
	// any unwired application.
	unwired := newApplicationProvisioner(pool)
	makeDue(ctx, t, pool, recorded)
	if completed, err := unwired.ResumeDeletion(ctx, recorded); completed || !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("unwired resume: completed=%v err=%v, want pending", completed, err)
	}
	entry := readJournal(ctx, t, pool, recorded)
	if entry.Completed || journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want the vector step still pending", entry)
	}

	// A replica whose configuration has the store explicitly off closes it
	// with the note, without a store call.
	off := newApplicationProvisioner(pool, projectprovisioning.WithVectorStoreOff())
	makeDue(ctx, t, pool, recorded)
	if completed, err := off.ResumeDeletion(ctx, recorded); err != nil || !completed {
		t.Fatalf("store-off resume: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, recorded)
	if !entry.Completed || !journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want complete", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepVectorData] != projectprovisioning.NoteVectorStoreNotConfigured ||
		projectprovisioning.NoteVectorStoreNotConfigured != "vector store not configured" {
		t.Fatalf("notes = %v, want %q", notes, projectprovisioning.NoteVectorStoreNotConfigured)
	}
	if remover.calls != 1 {
		t.Fatalf("the rust replica's store was called %d times, want only the deciding run's", remover.calls)
	}
	// WithVectorStoreOff beside a wired remover is ignored: the remover runs.
	wiredAndOff := newApplicationProvisioner(pool,
		projectprovisioning.WithVectorStoreOff(),
		projectprovisioning.WithApplicationData(vectorclient.App, &scriptedRemover{summary: "dropped"}))
	if wired := wiredAndOff.ApplicationsWired(); len(wired) != 1 {
		t.Fatalf("ApplicationsWired = %v", wired)
	}
}

func TestARefusedIdentityKeepsTheVectorStepPendingAtTheLongestBackoff(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	remover := &scriptedRemover{err: fmt.Errorf("%w: elitea-vector drop_project 9: as told (PermissionDenied)", platformops.ErrNotAllowed)}
	deciding := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData(vectorclient.App, remover))
	created, err := deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Refused", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := deciding.Deprovision(ctx, created.ProjectID)
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved: a refusal must not close the vector step", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepVectorData) {
		t.Fatalf("result = %+v, want the vector step failed", result.RollbackSteps)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if entry.Completed || journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want the step pending", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepVectorData] != nil {
		t.Fatalf("notes = %v, want no closing note (NoteRefused is for the engines)", notes)
	}
	// The row waits the longest backoff at once, not the first-attempt minute.
	var farOff bool
	if err := pool.QueryRow(ctx, `
SELECT next_attempt_at > clock_timestamp() + interval '50 minutes' FROM centry.project_deletions WHERE project_id = $1`,
		created.ProjectID).Scan(&farOff); err != nil || !farOff {
		t.Fatalf("next_attempt_at is not at the longest backoff (err=%v)", err)
	}
	// An operator fixes the allowlist: the retry drops the project.
	remover.err, remover.summary = nil, "dropped the project's vectors in 2 collection(s)"
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := deciding.ResumeDeletion(ctx, created.ProjectID); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	if remover.calls != 2 {
		t.Fatalf("calls = %d, want 2", remover.calls)
	}
	// The engines keep their rule: a refused DeepWiki closes with NoteRefused.
	engine := &scriptedRemover{err: fmt.Errorf("%w: refused", platformops.ErrNotAllowed)}
	engines := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData(platformops.AppDeepWiki, engine))
	created, err = engines.Provision(ctx, projectprovisioning.Request{
		Name: "Engine Refused", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engines.Deprovision(ctx, created.ProjectID); err != nil {
		t.Fatalf("an engine's refusal closes its step: %v", err)
	}
	if notes := journalNotes(readJournal(ctx, t, pool, created.ProjectID)); notes[projectprovisioning.StepDeepWikiData] != projectprovisioning.NoteRefused {
		t.Fatalf("engine notes = %v", notes)
	}
}

func TestTheVectorStepWaitsOutTheClaimTokenCacheAfterAForcedDelete(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	remover := &scriptedRemover{summary: "dropped the project's vectors in 1 collection(s)"}
	deciding := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData(vectorclient.App, remover))
	created, err := deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Forced While Ingesting", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// An index ingest is running (its job is PENDING).
	if err := admitOne(ctx, pool, created.ProjectID, "ingest"); err != nil {
		t.Fatal(err)
	}
	// An ordinary delete refuses; the forced one cuts the run off and records
	// that it did.
	if _, err := deciding.Deprovision(ctx, created.ProjectID); !errors.Is(err, projectprovisioning.ErrProjectWorkActive) {
		t.Fatalf("err = %v, want ErrProjectWorkActive", err)
	}
	result, err := deciding.Deprovision(ctx, created.ProjectID, projectprovisioning.SkipActiveWorkCheck())
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("forced delete err = %v, want the vector step pending", err)
	}
	// Pending (not initialized, not failed on the store's account), like an
	// unwired application's step.
	pending := false
	for _, step := range result.Pending {
		pending = pending || step == projectprovisioning.StepVectorData
	}
	if !pending || stepFailed(result.RollbackSteps, projectprovisioning.StepVectorData) {
		t.Fatalf("result pending=%v steps=%+v, want the vector step pending", result.Pending, result.RollbackSteps)
	}
	if remover.calls != 0 {
		t.Fatalf("DropProject was called %d time(s) inside the settle window", remover.calls)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if entry.Completed || journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want open with the vector step pending", entry)
	}
	if active, _ := entry.Cleanup["active_runs"].(float64); active != 1 {
		t.Fatalf("active_runs recorded = %v, want 1", entry.Cleanup["active_runs"])
	}
	if _, ok := entry.Cleanup["decided_at"]; !ok {
		t.Fatalf("decided_at not recorded: %v", entry.Cleanup)
	}
	// The project's job rows went with the row: nothing a live count could see.
	var jobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.execution_jobs WHERE resource_project_id = $1`, created.ProjectID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs after the decision = %d err=%v", jobs, err)
	}

	// Inside the settle window a retry still waits.
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := deciding.ResumeDeletion(ctx, created.ProjectID); completed || remover.calls != 0 {
		t.Fatalf("resume inside the settle: completed=%v calls=%d err=%v", completed, remover.calls, err)
	}
	// Once the window has passed (the decision is moved back in time), the
	// retry drops the project.
	if _, err := pool.Exec(ctx, `
UPDATE centry.project_deletions
SET cleanup = jsonb_set(cleanup, '{decided_at}', to_jsonb(clock_timestamp() - interval '1 minute')), next_attempt_at = clock_timestamp()
WHERE project_id = $1`, created.ProjectID); err != nil {
		t.Fatal(err)
	}
	if completed, err := deciding.ResumeDeletion(ctx, created.ProjectID); err != nil || !completed {
		t.Fatalf("resume after the settle: completed=%v err=%v", completed, err)
	}
	if remover.calls != 1 {
		t.Fatalf("calls = %d, want 1", remover.calls)
	}
	// A delete that cut off nothing drops at once.
	created, err = deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Idle", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deciding.Deprovision(ctx, created.ProjectID); err != nil || remover.calls != 2 {
		t.Fatalf("idle delete: err=%v calls=%d", err, remover.calls)
	}
}
