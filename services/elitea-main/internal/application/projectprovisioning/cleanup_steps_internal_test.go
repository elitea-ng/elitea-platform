package projectprovisioning

// The pieces of the delete that need no database (#1211), and the retirement
// of the per-project PgVector store (#1359).

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

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
	if got := execution.NonTerminalJobStateStrings(); !slices.Equal(got, want) {
		t.Fatalf("NonTerminalJobStateStrings() = %v, want %v", got, want)
	}
	for _, state := range []execution.JobState{execution.JobSucceeded, execution.JobFailed, execution.JobCancelled, execution.JobQuarantined} {
		if slices.Contains(execution.NonTerminalJobStateStrings(), string(state)) {
			t.Errorf("terminal state %s is counted as active work", state)
		}
	}
}

func TestEveryCleanupStepNamesItsSentinel(t *testing.T) {
	cause := errors.New("boom")
	want := map[string]error{
		StepArtifactBuckets: ErrArtifactsNotRemoved,
		StepProjectSchema:   ErrTenantSchemaNotRemoved,
	}
	steps := cleanupSteps()
	if len(steps) != len(want) {
		t.Fatalf("%d cleanup steps, %d sentinels: a step has no mapping", len(steps), len(want))
	}
	for _, step := range steps {
		if step.notDone == nil || !errors.Is(step.notDone, want[step.name]) {
			t.Errorf("%s: sentinel %v, want %v", step.name, step.notDone, want[step.name])
			continue
		}
		err := step.failure(cause)
		if !errors.Is(err, want[step.name]) || !errors.Is(err, cause) {
			t.Errorf("%s: %v, want %v wrapping the cause", step.name, err, want[step.name])
		}
	}
}

// The journal runs only the slow and external steps: the bytes first, then the
// tenant schema. The PgVector drop is not one of them any more (#1359).
func TestCleanupStepOrder(t *testing.T) {
	var names []string
	for _, step := range cleanupSteps() {
		names = append(names, step.name)
	}
	want := []string{StepArtifactBuckets, StepProjectSchema}
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

// Project creation provisions no vector store (#1359): every vector is in
// elitea-vector, keyed by project, and needs nothing per project.
func TestCreateStepsProvisionNoVectorStore(t *testing.T) {
	for _, step := range createSteps() {
		if strings.Contains(step.name, "pgvector") || strings.Contains(step.name, "vector") {
			t.Fatalf("create step %q provisions a vector store", step.name)
		}
	}
}

// A journal row written before #1359 carries the PgVector keys. It decodes,
// and no step a run takes is the retired drop, so the row cannot wait on it.
func TestAJournalRowWrittenBeforeThePgvectorRetirementDecodes(t *testing.T) {
	const legacy = `{"had_vector_store":true,"vector_database":"project_7",` +
		`"quiet_drop_failures":2,"done":{"artifact_buckets":true,"project_schema":true},` +
		`"notes":{"project_pgvector_drop":"skipped: no vector store"}}`
	var cleanup deletionCleanup
	if err := json.Unmarshal([]byte(legacy), &cleanup); err != nil {
		t.Fatalf("a pre-#1359 journal row no longer decodes: %v", err)
	}
	p := New(nil, nil, nil)
	for _, step := range p.cleanupSteps(&cleanup) {
		if step.name == "project_pgvector_drop" {
			t.Fatal("the retired PgVector drop is still a journal step")
		}
		if !cleanup.Done[step.name] {
			t.Errorf("step %s is undone on a row whose base steps are all done", step.name)
		}
	}
}
