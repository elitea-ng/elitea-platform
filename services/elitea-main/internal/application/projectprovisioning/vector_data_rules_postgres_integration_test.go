package projectprovisioning_test

// The vector step's own rules against a real PostgreSQL (ADR-0031 decision 4;
// application_data.go): which replica records it, that no replica closes it on
// its configuration and only the operator's abandon switch does, a refused
// identity that must not close it or delay the other steps, and the settle
// after a forced delete as a planned wait.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// The owner's rule for the vector step (review round 2): a Rust-indexing
// replica records it with or without a client; no replica ever closes it on
// its own configuration; only the operator's abandon switch closes it.
func TestTheVectorStepIsRecordedWithoutAClientAndClosedOnlyByTheAbandonSwitch(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// A Rust-indexing replica WITHOUT a client (a rollout): its own delete
	// records the step and leaves it pending.
	unwiredRust := newApplicationProvisioner(pool, projectprovisioning.WithApplicationDataRecorded(vectorclient.App))
	if wired := unwiredRust.ApplicationsWired(); len(wired) != 1 || wired[0] != "vector" {
		t.Fatalf("ApplicationsWired = %v on a Rust replica without a client, want [vector]", wired)
	}
	created, err := unwiredRust.Provision(ctx, projectprovisioning.Request{
		Name: "Rust Without A Client", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := unwiredRust.Deprovision(ctx, created.ProjectID)
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved: the step is recorded and pending", err)
	}
	if !slices.Contains(result.Pending, projectprovisioning.StepVectorData) {
		t.Fatalf("pending = %v, want the vector step", result.Pending)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if apps := journalApps(entry); len(apps) != 1 || apps[0] != "vector" {
		t.Fatalf("apps on the row = %v, want [vector]", apps)
	}
	if entry.Completed || journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want the vector step pending", entry)
	}

	// A Python-indexing replica without a client NEVER closes it: run after
	// run the step stays pending (attempts grow, the row backs off), and the
	// row carries no note.
	python := newApplicationProvisioner(pool)
	for run := 1; run <= 2; run++ {
		makeDue(ctx, t, pool, created.ProjectID)
		if completed, err := python.ResumeDeletion(ctx, created.ProjectID); completed || !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
			t.Fatalf("python resume %d: completed=%v err=%v, want pending", run, completed, err)
		}
	}
	entry = readJournal(ctx, t, pool, created.ProjectID)
	if entry.Completed || journalDone(entry, projectprovisioning.StepVectorData) || entry.Attempts != 3 || !entry.BackedOff {
		t.Fatalf("journal = %+v, want the vector step still pending after three runs", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepVectorData] != nil {
		t.Fatalf("notes = %v, want none: a replica never closes the step on its configuration", notes)
	}
	// Nor does a Rust replica without a client, on a later run.
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := unwiredRust.ResumeDeletion(ctx, created.ProjectID); completed || err == nil {
		t.Fatalf("unwired rust resume: completed=%v err=%v, want pending", completed, err)
	}

	// The operator's switch, on a replica without a client, closes it with
	// the note and no store call.
	abandoned := newApplicationProvisioner(pool, projectprovisioning.WithVectorCleanupAbandoned())
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := abandoned.ResumeDeletion(ctx, created.ProjectID); err != nil || !completed {
		t.Fatalf("abandon resume: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, created.ProjectID)
	if !entry.Completed || !journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want complete", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepVectorData] != projectprovisioning.NoteVectorCleanupAbandoned ||
		projectprovisioning.NoteVectorCleanupAbandoned != "vector cleanup abandoned by operator" {
		t.Fatalf("notes = %v, want %q", notes, projectprovisioning.NoteVectorCleanupAbandoned)
	}

	// The switch beside a wired remover is ignored: the remover runs.
	remover := &scriptedRemover{summary: "dropped the project's vectors in 1 collection(s)"}
	wiredAndAbandoned := newApplicationProvisioner(pool,
		projectprovisioning.WithVectorCleanupAbandoned(),
		projectprovisioning.WithApplicationData(vectorclient.App, remover))
	created, err = wiredAndAbandoned.Provision(ctx, projectprovisioning.Request{
		Name: "Wired And Abandoned", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wiredAndAbandoned.Deprovision(ctx, created.ProjectID); err != nil || remover.calls != 1 {
		t.Fatalf("wired delete: err=%v calls=%d, want the store called once", err, remover.calls)
	}
	if notes := journalNotes(readJournal(ctx, t, pool, created.ProjectID)); notes[projectprovisioning.StepVectorData] != "dropped the project's vectors in 1 collection(s)" {
		t.Fatalf("notes = %v", notes)
	}
}

// One refused step does not delay the others: a row with a refused vector step
// AND an engine step that fails on its ordinary schedule is retried on the
// engine's schedule (a minute), not parked for the hour the refused step asks
// for — and a row with ONLY the refused step is parked.
func TestARefusedStepDoesNotDelayTheOtherStepsRetry(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	refusing := &scriptedRemover{err: fmt.Errorf("%w: elitea-vector drop_project: refused (PermissionDenied)", platformops.ErrNotAllowed)}
	flaky := &scriptedRemover{err: fmt.Errorf("%w: inventory is still stopping a build", platformops.ErrRetryLater)}
	both := newApplicationProvisioner(pool,
		projectprovisioning.WithApplicationData(vectorclient.App, refusing),
		projectprovisioning.WithApplicationData(platformops.AppInventory, flaky))
	created, err := both.Provision(ctx, projectprovisioning.Request{
		Name: "Refused Beside Flaky", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := both.Deprovision(ctx, created.ProjectID); !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v", err)
	}
	var soon bool
	if err := pool.QueryRow(ctx, `
SELECT next_attempt_at BETWEEN clock_timestamp() + interval '30 seconds' AND clock_timestamp() + interval '2 minutes'
FROM centry.project_deletions WHERE project_id = $1`, created.ProjectID).Scan(&soon); err != nil || !soon {
		t.Fatalf("the row is not on the engine step's first-attempt schedule (err=%v)", err)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if entry.Attempts != 1 || entry.LastError == nil ||
		!strings.Contains(*entry.LastError, projectprovisioning.StepVectorData) || !strings.Contains(*entry.LastError, projectprovisioning.StepInventoryData) {
		t.Fatalf("journal = %+v, want one attempt naming both steps", entry)
	}
	// The engine recovers: its step closes, and now ONLY the refused step is
	// left — the row parks at the longest pause.
	flaky.err, flaky.summary = nil, "deleted 1 graph(s)"
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := both.ResumeDeletion(ctx, created.ProjectID); completed || !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, created.ProjectID)
	if !journalDone(entry, projectprovisioning.StepInventoryData) || journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want the engine step done and the vector step pending", entry)
	}
	var farOff bool
	if err := pool.QueryRow(ctx, `
SELECT next_attempt_at > clock_timestamp() + interval '50 minutes' FROM centry.project_deletions WHERE project_id = $1`,
		created.ProjectID).Scan(&farOff); err != nil || !farOff {
		t.Fatalf("a row with only the refused step left is not parked at the longest pause (err=%v)", err)
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
	if err != nil {
		t.Fatalf("forced delete err = %v, want nil: the settle is a planned wait, not a failure", err)
	}
	// Pending (not initialized, not failed on the store's account), reported
	// in Result.Pending and nowhere else.
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
	// A planned wait: no attempt counted, no last_error, and the next attempt
	// is the END of the settle (within VectorDropSettle of the decision), not
	// the journal's first-attempt minute.
	if entry.Attempts != 0 || entry.LastError != nil {
		t.Fatalf("journal = %+v, want no attempt and no last_error for a planned wait", entry)
	}
	var atSettleEnd bool
	if err := pool.QueryRow(ctx, `
SELECT next_attempt_at BETWEEN clock_timestamp() AND clock_timestamp() + $2 * interval '1 second'
FROM centry.project_deletions WHERE project_id = $1`, created.ProjectID, projectprovisioning.VectorDropSettle.Seconds()).Scan(&atSettleEnd); err != nil || !atSettleEnd {
		t.Fatalf("next_attempt_at is not the end of the settle (err=%v)", err)
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

	// Inside the settle window a retry still waits, still without an error or
	// an attempt.
	makeDue(ctx, t, pool, created.ProjectID)
	if completed, err := deciding.ResumeDeletion(ctx, created.ProjectID); completed || err != nil || remover.calls != 0 {
		t.Fatalf("resume inside the settle: completed=%v calls=%d err=%v, want (false, nil) and no call", completed, remover.calls, err)
	}
	if entry := readJournal(ctx, t, pool, created.ProjectID); entry.Attempts != 0 || entry.LastError != nil {
		t.Fatalf("journal after the second wait = %+v, want still no attempt", entry)
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
