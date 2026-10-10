package projectprovisioning

// The rollback-versus-delete split of the PgVector drop (#1211).
//
// The irreversible drop is not part of the reverse step walk at all: it runs in
// Deprovision after the project row is proved gone. The walk's own pgvector
// step only removes the configuration row, for a rollback and a delete alike.

import (
	"context"
	"strings"
	"testing"
)

func TestTheStepWalkNeverDropsTheVectorStore(t *testing.T) {
	attempted := []StepStatus{{Step: StepProjectPgvector, Initialized: true}}
	store := &stubVectorStore{}
	p := New(nil, nil, nil, WithVectorStore(store))

	rollback := p.compensate(context.Background(), &provisionState{projectID: 7}, attempted)

	if len(store.removed) != 1 || store.removed[0] != 7 {
		t.Fatalf("removed %v, want the configuration row of [7]", store.removed)
	}
	if len(store.dropped) != 0 {
		t.Fatalf("the step walk dropped the vector store: %v", store.dropped)
	}
	if len(rollback) != 1 || rollback[0].OK == nil || !*rollback[0].OK {
		t.Fatalf("rollback = %+v", rollback)
	}
}

func TestActiveWorkCountsEveryCapabilityAndNoTerminalState(t *testing.T) {
	if strings.Contains(activeWorkSQL, "capability_id") {
		t.Fatalf("the active-work guard must cover every capability:\n%s", activeWorkSQL)
	}
	for _, state := range []string{"PENDING", "DISPATCHED", "CLAIMED", "RUNNING", "SETTLING"} {
		if !strings.Contains(activeWorkSQL, "'"+state+"'") {
			t.Errorf("state %s is not counted as active work", state)
		}
	}
	for _, state := range []string{"SUCCEEDED", "FAILED", "CANCELLED", "QUARANTINED"} {
		if strings.Contains(activeWorkSQL, "'"+state+"'") {
			t.Errorf("terminal state %s is counted as active work", state)
		}
	}
}

// The fence counts both sides of a job, the pair the admission trigger of
// shared/0160 guards (#1211).
func TestActiveWorkCountsResourceAndProjectionProject(t *testing.T) {
	for _, column := range []string{"resource_project_id = $1", "projection_project_id = $1"} {
		if !strings.Contains(activeWorkSQL, column) {
			t.Errorf("the active-work count does not look at %s:\n%s", column, activeWorkSQL)
		}
	}
	if !strings.Contains(activeWorkSQL, "resource_project_id = $1 OR projection_project_id = $1") {
		t.Errorf("the two project columns are not alternatives:\n%s", activeWorkSQL)
	}
}
