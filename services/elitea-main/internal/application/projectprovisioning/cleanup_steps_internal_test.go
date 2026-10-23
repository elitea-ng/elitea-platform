package projectprovisioning

// The pieces of the delete that need no database (#1211), and the retirement
// of the per-project PgVector store (#1359).

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
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

// A journal row written before #1359 carries the PgVector keys. It decodes;
// had_vector_store is read again and puts the legacy drop on the row, and a
// row whose drop an older build already finished does not run it again.
func TestAJournalRowWrittenBeforeThePgvectorRetirementDecodes(t *testing.T) {
	const open = `{"had_vector_store":true,"vector_database":"project_7",` +
		`"quiet_drop_failures":2,"done":{"artifact_buckets":true,"project_schema":true}}`
	var cleanup deletionCleanup
	if err := json.Unmarshal([]byte(open), &cleanup); err != nil {
		t.Fatalf("a pre-#1359 journal row no longer decodes: %v", err)
	}
	p := New(nil, nil, nil)
	if !slices.Contains(stepNames(p.cleanupSteps(&cleanup)), StepProjectPgvectorDrop) || cleanup.Done[StepProjectPgvectorDrop] {
		t.Fatalf("an open row that recorded a vector store must still owe the legacy drop: %v", stepNames(p.cleanupSteps(&cleanup)))
	}

	const finished = `{"had_vector_store":true,"done":{"artifact_buckets":true,"project_schema":true,"project_pgvector_drop":true},` +
		`"notes":{"project_pgvector_drop":"skipped: no vector store"}}`
	cleanup = deletionCleanup{}
	if err := json.Unmarshal([]byte(finished), &cleanup); err != nil {
		t.Fatal(err)
	}
	for _, step := range p.cleanupSteps(&cleanup) {
		if !cleanup.Done[step.name] {
			t.Errorf("step %s is undone on a row whose steps are all done", step.name)
		}
	}
}

type fakeLegacyDatabases struct {
	summary string
	err     error
	calls   int
}

func (f *fakeLegacyDatabases) DropProjectDatabase(context.Context, int64) (string, error) {
	f.calls++
	return f.summary, f.err
}

// The legacy step is on a row's list when the row recorded evidence or this
// replica can drop (and then the drop is attempted whatever the evidence
// says); a replica with neither leaves it off, so a fresh install's deletes
// are unchanged.
func TestTheLegacyDropIsOwedOnEvidenceOrWhereItCanRun(t *testing.T) {
	plain := New(nil, nil, nil)
	if slices.Contains(stepNames(plain.cleanupSteps(&deletionCleanup{})), StepProjectPgvectorDrop) {
		t.Fatal("a row with no evidence on a replica with no admin URL owes the legacy drop")
	}
	if !slices.Contains(stepNames(plain.cleanupSteps(&deletionCleanup{HadVectorStore: true})), StepProjectPgvectorDrop) {
		t.Fatal("a row with evidence does not owe the legacy drop")
	}
	wired := New(nil, nil, nil, WithLegacyVectorDatabases(&fakeLegacyDatabases{}))
	names := stepNames(wired.cleanupSteps(&deletionCleanup{Apps: []string{"deepwiki"}}))
	want := []string{StepArtifactBuckets, StepProjectSchema, StepProjectPgvectorDrop, "deepwiki_data"}
	if !slices.Equal(names, want) {
		t.Fatalf("steps = %v, want %v (the legacy drop after the base steps, before the engines)", names, want)
	}
	// A typed nil is absent.
	var none *fakeLegacyDatabases
	if New(nil, nil, nil, WithLegacyVectorDatabases(none)).legacyVectorDatabases != nil {
		t.Fatal("a typed nil dropper was taken")
	}
}

func TestTheLegacyDropStepRunsDropsClosesRetiredOrRefuses(t *testing.T) {
	ctx := context.Background()
	evidence := &deletionCleanup{HadVectorStore: true}

	databases := &fakeLegacyDatabases{summary: "dropped legacy PgVector database project_7 and role project_7_user"}
	note, err := cleanupLegacyVectorDatabase(ctx, New(nil, nil, nil, WithLegacyVectorDatabases(databases)), 7, evidence)
	if err != nil || note != databases.summary || databases.calls != 1 {
		t.Fatalf("with a dropper: note=%q err=%v calls=%d", note, err, databases.calls)
	}
	databases = &fakeLegacyDatabases{err: errors.New("connection refused")}
	if _, err := cleanupLegacyVectorDatabase(ctx, New(nil, nil, nil, WithLegacyVectorDatabases(databases)), 7, evidence); err == nil {
		t.Fatal("a failed drop was reported done")
	}

	note, err = cleanupLegacyVectorDatabase(ctx, New(nil, nil, nil, WithLegacyVectorDatabasesRetired()), 7, evidence)
	if err != nil || note != NoteLegacyVectorDatabasesRetired {
		t.Fatalf("retired: note=%q err=%v", note, err)
	}

	_, err = cleanupLegacyVectorDatabase(ctx, New(nil, nil, nil), 7, evidence)
	if err == nil || !strings.Contains(err.Error(), LegacyPgvectorAdminURLEnv) || !strings.Contains(err.Error(), "project_7") {
		t.Fatalf("no URL, not retired: err=%v, want a failure naming the database and %s", err, LegacyPgvectorAdminURLEnv)
	}
}
