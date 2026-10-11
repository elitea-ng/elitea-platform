package projectprovisioning

// The application-data steps of a delete (#1243, #1244), without a database:
// which steps a provisioner lists, what each one calls, and how a failure is
// reported. The journal mechanics around them (marking, retrying, superseding)
// are the same for every step and are covered by the postgres integration
// tests of the delete.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeRemover struct {
	calls   []int64
	summary string
	err     error
}

func (f *fakeRemover) DeleteProjectData(_ context.Context, projectID int64) (string, error) {
	f.calls = append(f.calls, projectID)
	return f.summary, f.err
}

func stepNames(steps []cleanupStep) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.name)
	}
	return names
}

func TestAProvisionerWithNoApplicationsKeepsTheBaseSteps(t *testing.T) {
	p := New(nil, nil, nil)
	if got, want := stepNames(p.cleanupSteps()), stepNames(cleanupSteps()); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want the base list %v", got, want)
	}
	if wired := p.ApplicationsWired(); len(wired) != 0 {
		t.Fatalf("ApplicationsWired = %v, want none", wired)
	}
}

func TestWiredApplicationsAddOneStepEachAfterTheBaseSteps(t *testing.T) {
	p := New(nil, nil, nil,
		WithApplicationData("inventory", &fakeRemover{}),
		WithApplicationData("deepwiki", &fakeRemover{}),
	)
	want := append(stepNames(cleanupSteps()), StepDeepWikiData, StepInventoryData)
	if got := stepNames(p.cleanupSteps()); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if got := p.ApplicationsWired(); !slices.Equal(got, []string{"deepwiki", "inventory"}) {
		t.Fatalf("ApplicationsWired = %v", got)
	}
	// The base list is untouched: the code and tests that address its steps by
	// index keep working.
	if got := stepNames(cleanupSteps()); len(got) != 3 || got[2] != StepProjectPgvectorDrop {
		t.Fatalf("the base list changed: %v", got)
	}
}

func TestTheStepNamesAreTheApplicationNamePlusData(t *testing.T) {
	if ApplicationDataStep("deepwiki") != StepDeepWikiData || ApplicationDataStep("inventory") != StepInventoryData {
		t.Fatalf("ApplicationDataStep disagrees with the constants: %q %q",
			ApplicationDataStep("deepwiki"), ApplicationDataStep("inventory"))
	}
}

func TestTheStepCallsTheRemoverAndRecordsItsSummary(t *testing.T) {
	remover := &fakeRemover{summary: "deleted 2 wiki index(es)"}
	p := New(nil, nil, nil, WithApplicationData("deepwiki", remover))
	steps := p.cleanupSteps()
	step := steps[len(steps)-1]
	if step.name != StepDeepWikiData || !errors.Is(step.notDone, ErrApplicationDataNotRemoved) {
		t.Fatalf("step = %+v", step)
	}
	note, err := step.run(context.Background(), p, 7, &deletionCleanup{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if note != "deleted 2 wiki index(es)" {
		t.Errorf("note = %q", note)
	}
	if !slices.Equal(remover.calls, []int64{7}) {
		t.Errorf("remover called with %v, want [7]", remover.calls)
	}
}

func TestARemoverFailureIsTheStepsFailureAndNamesTheApplication(t *testing.T) {
	cause := errors.New("the engine is still stopping an ingest; retry")
	remover := &fakeRemover{err: cause}
	p := New(nil, nil, nil, WithApplicationData("inventory", remover))
	steps := p.cleanupSteps()
	step := steps[len(steps)-1]
	note, err := step.run(context.Background(), p, 7, &deletionCleanup{})
	if !errors.Is(err, cause) {
		t.Fatalf("run = %v, want the remover's error", err)
	}
	if note != "" {
		t.Errorf("a failed step carried a note: %q", note)
	}
	if !strings.Contains(err.Error(), "inventory") {
		t.Errorf("the error does not name the application: %v", err)
	}
	joined := step.failure(&deletionCleanup{}, err)
	if !errors.Is(joined, ErrApplicationDataNotRemoved) || !errors.Is(joined, cause) {
		t.Fatalf("failure = %v, want ErrApplicationDataNotRemoved wrapping the cause", joined)
	}
}

func TestANilOrTypedNilRemoverIsIgnored(t *testing.T) {
	var typedNil *fakeRemover
	p := New(nil, nil, nil,
		WithApplicationData("deepwiki", nil),
		WithApplicationData("inventory", typedNil),
		WithApplicationData("", &fakeRemover{}),
	)
	if wired := p.ApplicationsWired(); len(wired) != 0 {
		t.Fatalf("ApplicationsWired = %v, want none", wired)
	}
}

func TestASecondOptionForTheSameApplicationReplacesTheFirst(t *testing.T) {
	first, second := &fakeRemover{}, &fakeRemover{}
	p := New(nil, nil, nil,
		WithApplicationData("deepwiki", first),
		WithApplicationData("deepwiki", second),
	)
	steps := p.cleanupSteps()
	if len(steps) != 4 {
		t.Fatalf("steps = %v, want the base three plus one", stepNames(steps))
	}
	if _, err := steps[3].run(context.Background(), p, 7, &deletionCleanup{}); err != nil {
		t.Fatal(err)
	}
	if len(first.calls) != 0 || len(second.calls) != 1 {
		t.Fatalf("first=%v second=%v, want the second only", first.calls, second.calls)
	}
}

// pendingResult reports the application steps too, so a 202 names them.
func TestPendingResultListsTheApplicationSteps(t *testing.T) {
	p := New(nil, nil, nil, WithApplicationData("inventory", &fakeRemover{}))
	result := p.pendingResult(7, map[string]bool{StepArtifactBuckets: true})
	if !slices.Contains(result.Pending, StepInventoryData) {
		t.Fatalf("Pending = %v, want %s in it", result.Pending, StepInventoryData)
	}
	if slices.Contains(result.Pending, StepArtifactBuckets) {
		t.Fatalf("a done step is pending: %v", result.Pending)
	}
}
