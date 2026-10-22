package projectprovisioning_test

// The vector_data step of a project delete against a real PostgreSQL (ADR-0031
// decision 4): the deciding replica records elitea-vector as the application
// "vector" on the journal row, the base steps finish beside it, the store's
// summary is the step's note, and a
// store that is unreachable keeps the row open until a later run reaches it.
// Uses the provisioning template TestMain builds (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/vectorclient"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

func TestTheVectorStepIsRecordedRunAndRetriedUntilReached(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The store is unreachable on the delete's own run (the client classifies
	// UNAVAILABLE as retry-later), then answers on the resumed run.
	remover := &scriptedRemover{err: fmt.Errorf("%w: elitea-vector drop_project: connection refused (Unavailable)", platformops.ErrRetryLater)}
	deciding := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData(vectorclient.App, remover))
	if wired := deciding.ApplicationsWired(); len(wired) != 1 || wired[0] != "vector" {
		t.Fatalf("ApplicationsWired = %v, want [vector]", wired)
	}
	created, err := deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Vectors Go With The Project", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := created.ProjectID

	result, err := deciding.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepVectorData) {
		t.Fatalf("result = %+v, want the vector step failed", result.RollbackSteps)
	}
	if remover.calls != 1 {
		t.Fatalf("the store was called %d times by the delete's run", remover.calls)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Attempts != 1 {
		t.Fatalf("journal = %+v, want one failed run and the row open", entry)
	}
	if apps := journalApps(entry); len(apps) != 1 || apps[0] != "vector" {
		t.Fatalf("apps on the row = %v, want [vector]", apps)
	}
	if journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatal("a retried step is marked done")
	}
	// The base steps ran beside it and are done; no PgVector drop exists any
	// more (#1359).
	for _, step := range []string{projectprovisioning.StepArtifactBuckets, projectprovisioning.StepProjectSchema} {
		if !journalDone(entry, step) {
			t.Fatalf("journal = %+v, want %s done beside the vector step", entry, step)
		}
	}
	if journalDone(entry, "project_pgvector_drop") {
		t.Fatal("the retired PgVector drop ran")
	}

	// The store is reachable on the next run: the summary is the step's note
	// and the row completes.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = clock_timestamp() WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	remover.summary, remover.err = "dropped the project's vectors in 2 collection(s)", nil
	if completed, err := deciding.ResumeDeletion(ctx, projectID); err != nil || !completed {
		t.Fatalf("resume: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, projectID)
	if !entry.Completed || !journalDone(entry, projectprovisioning.StepVectorData) {
		t.Fatalf("journal = %+v, want complete with the vector step done", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepVectorData] != "dropped the project's vectors in 2 collection(s)" {
		t.Fatalf("notes = %v, want the store's summary", notes)
	}
	if remover.calls != 2 {
		t.Fatalf("the store was called %d times in all, want 2", remover.calls)
	}
}
