package projectprovisioning

// The rollback-versus-delete split of the PgVector drop, and the pieces of the
// delete that need no database (#1211).
//
// The irreversible drop is not part of the create rollback at all: it is a
// cleanup-journal step of the delete, run after the project row is gone. The
// rollback's own pgvector step only removes the configuration row.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

func TestTheRollbackNeverDropsTheVectorStore(t *testing.T) {
	attempted := []StepStatus{{Step: StepProjectPgvector, Initialized: true}}
	store := &stubVectorStore{}
	p := New(nil, nil, nil, WithVectorStore(store))

	rollback := p.compensate(context.Background(), &provisionState{projectID: 7}, attempted)

	if len(store.removed) != 1 || store.removed[0] != 7 {
		t.Fatalf("removed %v, want the configuration row of [7]", store.removed)
	}
	if len(store.dropped) != 0 {
		t.Fatalf("the rollback dropped the vector store: %v", store.dropped)
	}
	if len(rollback) != 1 || rollback[0].OK == nil || !*rollback[0].OK {
		t.Fatalf("rollback = %+v", rollback)
	}
}

// The count covers every capability, both sides of a job, and exactly the
// non-terminal states of the one Go definition, bound as a parameter.
func TestActiveWorkCountsEveryCapabilityBothSidesAndTheSharedStates(t *testing.T) {
	if strings.Contains(activeWorkSQL, "capability_id") {
		t.Fatalf("the active-work count must cover every capability:\n%s", activeWorkSQL)
	}
	if !strings.Contains(activeWorkSQL, "resource_project_id = $1 OR projection_project_id = $1") {
		t.Errorf("the two project columns are not alternatives:\n%s", activeWorkSQL)
	}
	if !strings.Contains(activeWorkSQL, "state = ANY($2::text[])") || strings.Contains(activeWorkSQL, " IN (") {
		t.Errorf("the states are not bound as one array parameter:\n%s", activeWorkSQL)
	}
	want := make([]string, 0)
	for _, state := range execution.NonTerminalJobStates() {
		want = append(want, string(state))
	}
	if got := nonTerminalStates(); !slices.Equal(got, want) {
		t.Fatalf("nonTerminalStates() = %v, want %v", got, want)
	}
	for _, state := range []execution.JobState{execution.JobSucceeded, execution.JobFailed, execution.JobCancelled, execution.JobQuarantined} {
		if slices.Contains(nonTerminalStates(), string(state)) {
			t.Errorf("terminal state %s is counted as active work", state)
		}
	}
}

// The drop is ALWAYS attempted when a vector store is configured, whatever the
// probe recorded: a database can exist without its configuration row.
func TestCleanupVectorStoreAlwaysAttemptsTheDrop(t *testing.T) {
	store := &stubVectorStore{}
	p := New(nil, nil, nil, WithVectorStore(store))

	cleanup := &deletionCleanup{}
	if _, err := cleanupVectorStore(context.Background(), p, 7, cleanup); err != nil {
		t.Fatalf("cleanupVectorStore = %v", err)
	}
	if len(store.dropped) != 1 || store.droppedHadStore[0] {
		t.Fatalf("drops = %v (hadStore %v), want one drop with hadStore=false", store.dropped, store.droppedHadStore)
	}
	if cleanup.VectorDatabase != "" {
		t.Fatalf("a drop for an unrecorded store recorded database %q", cleanup.VectorDatabase)
	}

	// A bootstrap-less deployment: the store answers "nothing to drop".
	store.dropFn = func(context.Context, int64, bool) (string, error) { return "", nil }
	note, err := cleanupVectorStore(context.Background(), p, 7, &deletionCleanup{})
	if err != nil || note != "skipped: no vector store" {
		t.Fatalf("no bootstrap, no recorded store = %q, %v; want a skip", note, err)
	}
}

// Probe said no and the drop fails: the journal retries it, the client is not
// told (a quietRetry), and the database is not named as a leak.
func TestCleanupVectorStoreFailureForAnUnrecordedStoreIsQuiet(t *testing.T) {
	store := &stubVectorStore{dropErr: errors.New("server unreachable")}
	p := New(nil, nil, nil, WithVectorStore(store))

	cleanup := &deletionCleanup{}
	_, err := cleanupVectorStore(context.Background(), p, 7, cleanup)
	var quiet quietRetry
	if !errors.As(err, &quiet) {
		t.Fatalf("err = %v, want a quietRetry", err)
	}
	if cleanup.VectorDatabase != "" {
		t.Fatalf("an unrecorded store was named: %q", cleanup.VectorDatabase)
	}

	// The same failure for a RECORDED store is reported.
	recorded := &deletionCleanup{HadVectorStore: true}
	_, err = cleanupVectorStore(context.Background(), p, 7, recorded)
	if err == nil || errors.As(err, &quiet) {
		t.Fatalf("recorded-store failure = %v, want a reported error", err)
	}
	if recorded.VectorDatabase != "project_7" {
		t.Fatalf("recorded database = %q, want project_7", recorded.VectorDatabase)
	}
}

// No vector store configured on the provisioner: a skip when none was recorded,
// a reported leak when one was.
func TestCleanupVectorStoreWithoutAProvisionerStore(t *testing.T) {
	bare := New(nil, nil, nil)
	note, err := cleanupVectorStore(context.Background(), bare, 7, &deletionCleanup{})
	if err != nil || !strings.HasPrefix(note, "skipped") {
		t.Fatalf("unrecorded store, no collaborator = %q, %v; want a skip", note, err)
	}
	if _, err := cleanupVectorStore(context.Background(), bare, 7, &deletionCleanup{HadVectorStore: true}); err == nil {
		t.Fatal("a recorded store with no way to drop it was reported done")
	}
}

func TestStepErrorMapsEachStepToItsSentinel(t *testing.T) {
	cause := errors.New("boom")
	cleanup := &deletionCleanup{VectorDatabase: "project_7"}
	for step, want := range map[string]error{
		StepArtifactBuckets:     ErrArtifactsNotRemoved,
		StepProjectSchema:       ErrTenantSchemaNotRemoved,
		StepProjectPgvectorDrop: ErrVectorStoreNotDropped,
		StepProjectSecrets:      ErrCleanupIncomplete,
		StepSystemToken:         ErrCleanupIncomplete,
		StepSystemUser:          ErrCleanupIncomplete,
		StepProjectPermissions:  ErrCleanupIncomplete,
	} {
		err := stepError(step, cleanup, cause)
		if !errors.Is(err, want) || !errors.Is(err, cause) {
			t.Errorf("%s: %v, want %v wrapping the cause", step, err, want)
		}
	}
	if err := stepError(StepProjectPgvectorDrop, cleanup, cause); !strings.Contains(err.Error(), "project_7") {
		t.Errorf("the drop error does not name the database: %v", err)
	}
}

// The journal runs only the slow and external steps: the bytes first, then the
// tenant schema and the vector database.
func TestCleanupStepOrder(t *testing.T) {
	var names []string
	for _, step := range cleanupSteps() {
		names = append(names, step.name)
	}
	want := []string{StepArtifactBuckets, StepProjectSchema, StepProjectPgvectorDrop}
	if !slices.Equal(names, want) {
		t.Fatalf("cleanup steps = %v, want %v", names, want)
	}
	// The identity steps are the decision's, not the journal's.
	for _, name := range decisionSteps() {
		if slices.Contains(names, name) {
			t.Errorf("identity step %s is a journal step", name)
		}
	}
}
