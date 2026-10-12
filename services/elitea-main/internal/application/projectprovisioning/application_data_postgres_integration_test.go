package projectprovisioning_test

// The application-data steps of a project delete against a real PostgreSQL
// (#1243, #1244): the deciding replica records its applications on the journal
// row; a run on a replica without the client leaves the step pending and the
// row open; a partial deletion's summary is kept as the step's note while the
// step is retried; a permanent refusal closes the step with the reason. Uses
// the provisioning template TestMain builds (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

type scriptedRemover struct {
	calls   int
	summary string
	err     error
}

func (r *scriptedRemover) DeleteProjectData(_ context.Context, _ int64) (string, error) {
	r.calls++
	return r.summary, r.err
}

func newApplicationProvisioner(pool *pgxpool.Pool, options ...projectprovisioning.Option) *projectprovisioning.Provisioner {
	base := []projectprovisioning.Option{projectprovisioning.WithProjectVault(v2secrets.NewHandler(pool))}
	return projectprovisioning.New(pool, migrate.New(pool, platformmigrations.Files), nil, append(base, options...)...)
}

func journalNotes(entry journalEntry) map[string]any {
	notes, _ := entry.Cleanup["notes"].(map[string]any)
	return notes
}

func journalApps(entry journalEntry) []string {
	raw, _ := entry.Cleanup["apps"].([]any)
	apps := make([]string, 0, len(raw))
	for _, app := range raw {
		apps = append(apps, fmt.Sprint(app))
	}
	return apps
}

func journalDone(entry journalEntry, step string) bool {
	done, _ := entry.Cleanup["done"].(map[string]any)
	return done[step] == true
}

func TestTheRowRecordsTheAppsAndAnUnwiredReplicaLeavesTheStepPending(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The deciding replica has Inventory wired; the engine answers a partial
	// deletion (retry later, with a summary of what went).
	remover := &scriptedRemover{summary: "deleted 1 graph(s)",
		err: fmt.Errorf("%w: inventory project: deleted 1 graph(s); still held: 1 busy toolkit(s)", platformops.ErrRetryLater)}
	deciding := newApplicationProvisioner(pool, projectprovisioning.WithApplicationData("inventory", remover))
	created, err := deciding.Provision(ctx, projectprovisioning.Request{
		Name: "Apps On The Row", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := created.ProjectID

	result, err := deciding.Deprovision(ctx, projectID)
	if !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("err = %v, want ErrApplicationDataNotRemoved", err)
	}
	if !stepFailed(result.RollbackSteps, projectprovisioning.StepInventoryData) {
		t.Fatalf("result = %+v, want the inventory step failed", result.RollbackSteps)
	}
	if remover.calls != 1 {
		t.Fatalf("engine called %d times by the delete's run", remover.calls)
	}
	entry := readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Attempts != 1 {
		t.Fatalf("journal = %+v, want one failed run and the row open", entry)
	}
	if apps := journalApps(entry); len(apps) != 1 || apps[0] != "inventory" {
		t.Fatalf("apps on the row = %v, want [inventory]", apps)
	}
	// E: the partial summary is on the row although the step is not done.
	if notes := journalNotes(entry); notes[projectprovisioning.StepInventoryData] != "deleted 1 graph(s)" {
		t.Fatalf("notes = %v, want the partial summary under %s", notes, projectprovisioning.StepInventoryData)
	}
	if journalDone(entry, projectprovisioning.StepInventoryData) {
		t.Fatal("a retried step is marked done")
	}

	// B: a replica with NO Inventory client resumes the row: the step stays
	// pending, the row stays open, and the engine is not asked by anyone.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = clock_timestamp() WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	unwired := newApplicationProvisioner(pool)
	completed, err := unwired.ResumeDeletion(ctx, projectID)
	if completed || err == nil || !errors.Is(err, projectprovisioning.ErrApplicationDataNotRemoved) {
		t.Fatalf("unwired resume: completed=%v err=%v, want the step still owed", completed, err)
	}
	entry = readJournal(ctx, t, pool, projectID)
	if entry.Completed || entry.Attempts != 2 || entry.LastError == nil || !strings.Contains(*entry.LastError, projectprovisioning.StepInventoryData) {
		t.Fatalf("journal after the unwired run = %+v, want open, two attempts, the step named", entry)
	}
	if remover.calls != 1 {
		t.Fatalf("the unwired replica reached an engine: %d calls", remover.calls)
	}

	// The wired replica finishes it; the engine's summary is the step's note.
	if _, err := pool.Exec(ctx, `UPDATE centry.project_deletions SET next_attempt_at = clock_timestamp() WHERE project_id = $1`, projectID); err != nil {
		t.Fatal(err)
	}
	remover.summary, remover.err = "nothing to delete", nil
	if completed, err := deciding.ResumeDeletion(ctx, projectID); err != nil || !completed {
		t.Fatalf("wired resume: completed=%v err=%v", completed, err)
	}
	entry = readJournal(ctx, t, pool, projectID)
	if !entry.Completed || !journalDone(entry, projectprovisioning.StepInventoryData) {
		t.Fatalf("journal = %+v, want complete with the step done", entry)
	}
	if notes := journalNotes(entry); notes[projectprovisioning.StepInventoryData] != "nothing to delete" {
		t.Fatalf("notes = %v, want the engine's summary", notes)
	}
}

// C: a permanent refusal and an unsupported deletion close their steps with
// the reason, and the row completes; the delete itself reports no error for
// them (there is nothing a retry would change).
func TestTerminalEngineAnswersCloseTheStepWithTheReason(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	refused := &scriptedRemover{err: fmt.Errorf("%w: inventory delete_project: CN=elitea-main is not in the allowlist (PermissionDenied)", platformops.ErrNotAllowed)}
	unsupported := &scriptedRemover{err: fmt.Errorf("%w: deepwiki delete_project", platformops.ErrNotSupported)}
	provisioner := newApplicationProvisioner(pool,
		projectprovisioning.WithApplicationData("inventory", refused),
		projectprovisioning.WithApplicationData("deepwiki", unsupported))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Terminal Answers", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provisioner.Deprovision(ctx, created.ProjectID)
	if err != nil {
		t.Fatalf("Deprovision = %v, want no error: closed steps are not owed", err)
	}
	if len(result.Pending) != 0 {
		t.Fatalf("Pending = %v, want none", result.Pending)
	}
	entry := readJournal(ctx, t, pool, created.ProjectID)
	if !entry.Completed {
		t.Fatalf("journal = %+v, want complete", entry)
	}
	notes := journalNotes(entry)
	if note, _ := notes[projectprovisioning.StepInventoryData].(string); !strings.HasPrefix(note, "failed permanently: ") || !strings.Contains(note, "allowlist") {
		t.Fatalf("inventory note = %q, want the permanent reason", note)
	}
	if notes[projectprovisioning.StepDeepWikiData] != projectprovisioning.NoteUnsupported {
		t.Fatalf("deepwiki note = %v, want %q", notes[projectprovisioning.StepDeepWikiData], projectprovisioning.NoteUnsupported)
	}
	if refused.calls != 1 || unsupported.calls != 1 {
		t.Fatalf("calls: inventory=%d deepwiki=%d, want one each", refused.calls, unsupported.calls)
	}
}
