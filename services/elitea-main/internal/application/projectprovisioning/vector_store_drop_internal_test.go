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
	for _, state := range execution.TerminalJobStates() {
		if slices.Contains(nonTerminalStates(), string(state)) {
			t.Errorf("terminal state %s is counted as active work", state)
		}
	}
}

// A project that never had a vector store skips the drop without touching the
// store (and so without connecting to the PgVector server).
func TestCleanupVectorStoreSkipsWithoutAStore(t *testing.T) {
	store := &stubVectorStore{}
	p := New(nil, nil, nil, WithVectorStore(store))

	note, err := cleanupVectorStore(context.Background(), p, 7, &deletionCleanup{})
	if err != nil || note != "skipped: no vector store" {
		t.Fatalf("cleanupVectorStore = %q, %v", note, err)
	}
	if len(store.dropped) != 0 {
		t.Fatalf("the skip called the drop: %v", store.dropped)
	}

	// The journal says it had one, and this provisioner has no store to drop it
	// with: that is a failure, not a skip.
	bare := New(nil, nil, nil)
	if _, err := cleanupVectorStore(context.Background(), bare, 7, &deletionCleanup{HadVectorStore: true, VectorDatabase: "project_7"}); err == nil {
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

// Every step the journal runs is a step a caller can name, and the order puts
// the bytes first and the tenant schema and the vector database last.
func TestCleanupStepOrder(t *testing.T) {
	var names []string
	for _, step := range cleanupSteps() {
		names = append(names, step.name)
	}
	want := []string{
		StepArtifactBuckets, StepProjectSecrets, StepSystemToken, StepSystemUser,
		StepProjectPermissions, StepProjectSchema, StepProjectPgvectorDrop,
	}
	if !slices.Equal(names, want) {
		t.Fatalf("cleanup steps = %v, want %v", names, want)
	}
}
