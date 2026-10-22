package projectprovisioning

// The application-data steps of a delete (#1243, #1244), without a database:
// which steps a journal row lists, what each one calls, how the engine's
// answers are classified, and how a call is bounded inside the run. The
// journal mechanics around them (marking, retrying, superseding) are the same
// for every step and are covered by the postgres integration tests of the
// delete.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/cleanupjournal"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type fakeRemover struct {
	calls    []int64
	summary  string
	err      error
	deadline []time.Duration
}

func (f *fakeRemover) DeleteProjectData(ctx context.Context, projectID int64) (string, error) {
	f.calls = append(f.calls, projectID)
	if deadline, ok := ctx.Deadline(); ok {
		f.deadline = append(f.deadline, time.Until(deadline))
	} else {
		f.deadline = append(f.deadline, 0)
	}
	return f.summary, f.err
}

func stepNames(steps []cleanupStep) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.name)
	}
	return names
}

func rowWithApps(apps ...string) *deletionCleanup {
	return &deletionCleanup{Done: map[string]bool{}, Apps: apps}
}

func TestARowWithNoApplicationsKeepsTheBaseSteps(t *testing.T) {
	p := New(nil, nil, nil)
	if got, want := stepNames(p.cleanupSteps(rowWithApps())), stepNames(cleanupSteps()); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want the base list %v", got, want)
	}
	if got, want := stepNames(p.cleanupSteps(nil)), stepNames(cleanupSteps()); !slices.Equal(got, want) {
		t.Fatalf("steps of a nil row = %v, want the base list %v", got, want)
	}
	if wired := p.ApplicationsWired(); len(wired) != 0 {
		t.Fatalf("ApplicationsWired = %v, want none", wired)
	}
}

// The deciding replica records its wired applications on the row (B), sorted.
func TestTheDecisionRecordsTheWiredApplicationsOnTheRow(t *testing.T) {
	p := New(nil, nil, nil,
		WithApplicationData("inventory", &fakeRemover{}),
		WithApplicationData("deepwiki", &fakeRemover{}),
	)
	cleanup := p.recordCleanup()
	if !slices.Equal(cleanup.Apps, []string{"deepwiki", "inventory"}) {
		t.Fatalf("Apps = %v", cleanup.Apps)
	}
	if got := p.ApplicationsWired(); !slices.Equal(got, []string{"deepwiki", "inventory"}) {
		t.Fatalf("ApplicationsWired = %v", got)
	}
}

// The step list comes from the ROW, not from the running process: a row that
// recorded two applications lists two steps on a replica that has one, and a
// row that recorded none lists none on a replica that has two.
func TestTheStepListComesFromTheRowNotTheReplica(t *testing.T) {
	oneWired := New(nil, nil, nil, WithApplicationData("inventory", &fakeRemover{}))
	want := append(stepNames(cleanupSteps()), StepDeepWikiData, StepInventoryData)
	if got := stepNames(oneWired.cleanupSteps(rowWithApps("deepwiki", "inventory"))); !slices.Equal(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	twoWired := New(nil, nil, nil,
		WithApplicationData("inventory", &fakeRemover{}), WithApplicationData("deepwiki", &fakeRemover{}))
	if got, base := stepNames(twoWired.cleanupSteps(rowWithApps())), stepNames(cleanupSteps()); !slices.Equal(got, base) {
		t.Fatalf("a row with no apps listed %v on a replica with two", got)
	}
	// The base list is untouched: the code and tests that address its steps by
	// index keep working.
	if got := stepNames(cleanupSteps()); len(got) != 2 || got[1] != StepProjectSchema {
		t.Fatalf("the base list changed: %v", got)
	}
}

func TestTheStepNamesAreTheApplicationNamePlusData(t *testing.T) {
	if ApplicationDataStep("deepwiki") != StepDeepWikiData || ApplicationDataStep("inventory") != StepInventoryData {
		t.Fatalf("ApplicationDataStep disagrees with the constants: %q %q",
			ApplicationDataStep("deepwiki"), ApplicationDataStep("inventory"))
	}
	// elitea-vector is wired as the application "vector" (infra/vectorclient.App;
	// spelled here so this package does not import the client).
	if ApplicationDataStep("vector") != StepVectorData {
		t.Fatalf("ApplicationDataStep(vector) = %q, want %q", ApplicationDataStep("vector"), StepVectorData)
	}
}

func lastStep(p *Provisioner, cleanup *deletionCleanup) cleanupStep {
	steps := p.cleanupSteps(cleanup)
	return steps[len(steps)-1]
}

func TestTheStepCallsTheRemoverAndRecordsItsSummary(t *testing.T) {
	remover := &fakeRemover{summary: "deleted 2 wiki index(es)"}
	p := New(nil, nil, nil, WithApplicationData("deepwiki", remover))
	row := rowWithApps("deepwiki")
	step := lastStep(p, row)
	if step.name != StepDeepWikiData || !errors.Is(step.notDone, ErrApplicationDataNotRemoved) {
		t.Fatalf("step = %+v", step)
	}
	note, err := step.run(context.Background(), p, 7, row)
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

// A replica without the client for a recorded application leaves the step
// pending (B): the outcome is notWiredError, and the remover is never
// consulted (there is none).
func TestAnUnwiredReplicaLeavesTheRecordedStepPending(t *testing.T) {
	p := New(nil, nil, nil, WithApplicationData("deepwiki", &fakeRemover{}))
	row := rowWithApps("deepwiki", "inventory")
	step := lastStep(p, row)
	if step.name != StepInventoryData {
		t.Fatalf("last step = %s", step.name)
	}
	note, err := step.run(context.Background(), p, 7, row)
	var notWired notWiredError
	if !errors.As(err, &notWired) || notWired.app != "inventory" {
		t.Fatalf("run = %v, want notWiredError for inventory", err)
	}
	if note != "" {
		t.Errorf("a pending step carried a note: %q", note)
	}
	var closed closedWithNote
	if errors.As(err, &closed) {
		t.Fatal("a pending step was classified as closed")
	}
}

// UNIMPLEMENTED closes the step with the unsupported note (C).
func TestAnUnsupportedDeletionClosesTheStepWithANote(t *testing.T) {
	remover := &fakeRemover{err: fmt.Errorf("%w: deepwiki delete_project", platformops.ErrNotSupported)}
	p := New(nil, nil, nil, WithApplicationData("deepwiki", remover))
	row := rowWithApps("deepwiki")
	_, err := lastStep(p, row).run(context.Background(), p, 7, row)
	var closed closedWithNote
	if !errors.As(err, &closed) || closed.note != NoteUnsupported {
		t.Fatalf("run = %v, want closedWithNote(%q)", err, NoteUnsupported)
	}
}

// A refused certificate or an invalid request is permanent (C): the step is
// closed rather than retried for ever, with the CLASS of the reason as its note.
// The note reaches the journal row and DELETE's response, so the engine's
// message (an identity, an address) is logged and never stored or reported.
func TestAPermanentFailureClosesTheStepWithTheReasonsClass(t *testing.T) {
	for cause, wantNote := range map[error]string{platformops.ErrNotAllowed: NoteRefused, platformops.ErrConfig: NoteInvalidRequest} {
		var log strings.Builder
		remover := &fakeRemover{err: fmt.Errorf("%w: inventory delete_project: CN=elitea-main not in allowlist at 10.0.7.3:9443", cause)}
		p := New(nil, nil, slog.New(slog.NewTextHandler(&log, nil)), WithApplicationData("inventory", remover))
		row := rowWithApps("inventory")
		_, err := lastStep(p, row).run(context.Background(), p, 7, row)
		var closed closedWithNote
		if !errors.As(err, &closed) {
			t.Fatalf("%v: run = %v, want closedWithNote", cause, err)
		}
		if closed.note != wantNote {
			t.Fatalf("%v: note = %q, want %q", cause, closed.note, wantNote)
		}
		for _, leak := range []string{"allowlist", "10.0.7.3", "CN="} {
			if strings.Contains(closed.note, leak) {
				t.Fatalf("%v: note %q carries the engine's message (%q)", cause, closed.note, leak)
			}
		}
		if !strings.Contains(log.String(), "10.0.7.3:9443") || !strings.Contains(log.String(), "project_id=7") {
			t.Fatalf("%v: the raw error and the project id are not in the log: %s", cause, log.String())
		}
	}
}

// A retry-later with a partial summary is a failure that KEEPS the summary as
// the note (E); a plain failure names the application and is retried.
func TestARetryLaterKeepsThePartialSummaryAndIsRetried(t *testing.T) {
	cause := fmt.Errorf("%w: inventory project 7: deleted 2 graph(s); still held: 1 busy toolkit(s)", platformops.ErrRetryLater)
	remover := &fakeRemover{summary: "deleted 2 graph(s)", err: cause}
	p := New(nil, nil, nil, WithApplicationData("inventory", remover))
	row := rowWithApps("inventory")
	step := lastStep(p, row)
	note, err := step.run(context.Background(), p, 7, row)
	if !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("run = %v, want the remover's retry-later", err)
	}
	if note != "deleted 2 graph(s)" {
		t.Errorf("the partial summary was dropped: note = %q", note)
	}
	var closed closedWithNote
	var notWired notWiredError
	if errors.As(err, &closed) || errors.As(err, &notWired) {
		t.Fatal("a retry-later was classified as closed or pending")
	}
	if !strings.Contains(err.Error(), "inventory") {
		t.Errorf("the error does not name the application: %v", err)
	}
	joined := step.failure(err)
	if !errors.Is(joined, ErrApplicationDataNotRemoved) || !errors.Is(joined, cause) {
		t.Fatalf("failure = %v, want ErrApplicationDataNotRemoved wrapping the cause", joined)
	}
}

// Each application call gets its share of what is left of the run (D): two
// steps still to run split the remaining bound in two; the last one takes all
// of it; a context with no deadline leaves the bound to the client.
func TestEachApplicationCallGetsItsShareOfTheRun(t *testing.T) {
	first, second := &fakeRemover{}, &fakeRemover{}
	p := New(nil, nil, nil, WithApplicationData("deepwiki", first), WithApplicationData("inventory", second))
	row := rowWithApps("deepwiki", "inventory")
	steps := p.cleanupSteps(row)
	deepwiki, inventory := steps[len(steps)-2], steps[len(steps)-1]

	runBound := 4*time.Minute + cleanupjournal.BookkeepingTimeout
	ctx, cancel := context.WithTimeout(context.Background(), runBound)
	defer cancel()
	if _, err := deepwiki.run(ctx, p, 7, row); err != nil {
		t.Fatal(err)
	}
	row.Done[StepDeepWikiData] = true
	if _, err := inventory.run(ctx, p, 7, row); err != nil {
		t.Fatal(err)
	}
	within := func(got, want time.Duration) bool { return got > want-5*time.Second && got <= want }
	if !within(first.deadline[0], 2*time.Minute) {
		t.Errorf("first of two calls got %v, want about half of 4m", first.deadline[0])
	}
	if !within(second.deadline[0], 4*time.Minute) {
		t.Errorf("last call got %v, want about the whole 4m left", second.deadline[0])
	}

	unbounded := &fakeRemover{}
	q := New(nil, nil, nil, WithApplicationData("deepwiki", unbounded))
	if _, err := lastStep(q, rowWithApps("deepwiki")).run(context.Background(), q, 7, rowWithApps("deepwiki")); err != nil {
		t.Fatal(err)
	}
	if unbounded.deadline[0] != 0 {
		t.Errorf("a call with no run deadline was bounded: %v", unbounded.deadline[0])
	}
}

func TestApplicationCallContextSharesTheRemainingBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute+cleanupjournal.BookkeepingTimeout)
	defer cancel()
	callCtx, cancelCall := applicationCallContext(ctx, 3)
	defer cancelCall()
	deadline, ok := callCtx.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	if left := time.Until(deadline); left > time.Minute || left < 55*time.Second {
		t.Fatalf("share = %v, want about a minute (3m / 3)", left)
	}
	spent, cancelSpent := context.WithTimeout(context.Background(), time.Second)
	defer cancelSpent()
	spentCall, cancelSpentCall := applicationCallContext(spent, 2)
	defer cancelSpentCall()
	if d, _ := spentCall.Deadline(); d.After(time.Now().Add(2 * time.Second)) {
		t.Fatalf("a spent run bound was extended: %v", time.Until(d))
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
	row := rowWithApps("deepwiki")
	steps := p.cleanupSteps(row)
	if len(steps) != 3 {
		t.Fatalf("steps = %v, want the base two plus one", stepNames(steps))
	}
	if _, err := steps[2].run(context.Background(), p, 7, row); err != nil {
		t.Fatal(err)
	}
	if len(first.calls) != 0 || len(second.calls) != 1 {
		t.Fatalf("first=%v second=%v, want the second only", first.calls, second.calls)
	}
}

// pendingResult reports the row's application steps too, so a 202 names them.
func TestPendingResultListsTheRowsApplicationSteps(t *testing.T) {
	p := New(nil, nil, nil)
	result := p.pendingResult(7, rowWithApps("inventory"), map[string]bool{StepArtifactBuckets: true})
	if !slices.Contains(result.Pending, StepInventoryData) {
		t.Fatalf("Pending = %v, want %s in it", result.Pending, StepInventoryData)
	}
	if slices.Contains(result.Pending, StepArtifactBuckets) {
		t.Fatalf("a done step is pending: %v", result.Pending)
	}
}

// A Rust-indexing replica without a client records the vector step
// (WithApplicationDataRecorded): it is on ApplicationsWired, and its run on
// this replica is notWiredError — pending, never closed.
func TestARustReplicaWithoutAClientRecordsTheVectorStepAndLeavesItPending(t *testing.T) {
	p := New(nil, nil, nil, WithApplicationDataRecorded(AppVector))
	if wired := p.ApplicationsWired(); !slices.Equal(wired, []string{AppVector}) {
		t.Fatalf("ApplicationsWired = %v, want [vector]", wired)
	}
	row := rowWithApps(AppVector)
	_, err := lastStep(p, row).run(context.Background(), p, 7, row)
	var notWired notWiredError
	if !errors.As(err, &notWired) || notWired.app != AppVector {
		t.Fatalf("run = %v, want notWiredError for vector", err)
	}
	// WithApplicationData for the same name adds the remover to the recorded
	// entry; the recorded-only option after a remover keeps the remover.
	remover := &fakeRemover{summary: "dropped"}
	for _, options := range [][]Option{
		{WithApplicationDataRecorded(AppVector), WithApplicationData(AppVector, remover)},
		{WithApplicationData(AppVector, remover), WithApplicationDataRecorded(AppVector)},
	} {
		p := New(nil, nil, nil, options...)
		if p.applicationRemover(AppVector) != remover || !slices.Equal(p.ApplicationsWired(), []string{AppVector}) {
			t.Fatalf("options %d: remover=%v wired=%v", len(options), p.applicationRemover(AppVector), p.ApplicationsWired())
		}
	}
	// A blank name records nothing.
	if wired := New(nil, nil, nil, WithApplicationDataRecorded(" ")).ApplicationsWired(); len(wired) != 0 {
		t.Fatalf("ApplicationsWired = %v, want none", wired)
	}
}

// No replica closes a recorded vector step on its own configuration: a replica
// without a client (Python indexing, or Rust mid-rollout) leaves it pending on
// every run, logged at Error naming the variable that wires the client.
func TestAReplicaWithoutAVectorClientNeverClosesTheVectorStep(t *testing.T) {
	var logs strings.Builder
	p := New(nil, nil, nil)
	p.logger = slog.New(slog.NewTextHandler(&logs, nil))
	row := rowWithApps(AppVector)
	for run := 0; run < 2; run++ {
		note, err := lastStep(p, row).run(context.Background(), p, 7, row)
		var notWired notWiredError
		var closed closedWithNote
		if !errors.As(err, &notWired) || errors.As(err, &closed) || note != "" {
			t.Fatalf("run %d = (%q, %v), want notWiredError and no note", run, note, err)
		}
	}
	if logged := logs.String(); strings.Count(logged, "level=ERROR") != 2 ||
		!strings.Contains(logged, VectorClientEnv) || !strings.Contains(logged, VectorCleanupAbandonEnv) {
		t.Fatalf("logs = %q, want an Error per run naming %s and %s", logged, VectorClientEnv, VectorCleanupAbandonEnv)
	}
}

// The operator's abandon switch closes a pending vector step on a replica
// without a client, with the note, at Warn; beside a remover it is ignored and
// the remover runs. It never touches an engine's step.
func TestTheAbandonSwitchClosesAnUnwiredVectorStepOnly(t *testing.T) {
	var logs strings.Builder
	p := New(nil, nil, nil, WithVectorCleanupAbandoned())
	p.logger = slog.New(slog.NewTextHandler(&logs, nil))
	row := rowWithApps("deepwiki", AppVector)
	steps := p.cleanupSteps(row)
	_, err := steps[len(steps)-1].run(context.Background(), p, 7, row)
	var closed closedWithNote
	if !errors.As(err, &closed) || closed.note != NoteVectorCleanupAbandoned {
		t.Fatalf("vector step = %v, want closed with %q", err, NoteVectorCleanupAbandoned)
	}
	if logged := logs.String(); !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, VectorCleanupAbandonEnv) {
		t.Fatalf("logs = %q, want a Warn naming the switch", logged)
	}
	// The engine's step is still merely unwired.
	var notWired notWiredError
	if _, err := steps[len(steps)-2].run(context.Background(), p, 7, row); !errors.As(err, &notWired) || notWired.app != "deepwiki" {
		t.Fatalf("deepwiki step = %v, want notWiredError", err)
	}
	// With a remover the switch is ignored.
	remover := &fakeRemover{summary: "dropped the project's vectors in 1 collection(s)"}
	wired := New(nil, nil, nil, WithVectorCleanupAbandoned(), WithApplicationData(AppVector, remover))
	note, err := lastStep(wired, rowWithApps(AppVector)).run(context.Background(), wired, 7, rowWithApps(AppVector))
	if err != nil || note != remover.summary || len(remover.calls) != 1 {
		t.Fatalf("wired run = (%q, %v) calls=%v, want the remover to run", note, err, remover.calls)
	}
}

// The row's next attempt is the EARLIEST schedule any undone step asks for:
// one refused step (the longest pause) never delays a step on its ordinary
// retry; a planned wait is neither a failure nor an attempt.
func TestARunsCloseTakesTheEarliestScheduleAcrossSteps(t *testing.T) {
	var closing runClose
	if !closing.complete() {
		t.Fatal("an empty close is not complete")
	}
	closing.fail(StepVectorData, deletionRetryMax)
	if closing.attemptBased || closing.fixed != deletionRetryMax || closing.complete() {
		t.Fatalf("after a refused step: %+v", closing)
	}
	closing.fail(StepInventoryData, 0)
	if !closing.attemptBased || closing.fixed != deletionRetryMax || !slices.Equal(closing.failed, []string{StepVectorData, StepInventoryData}) {
		t.Fatalf("after an ordinary failure beside it: %+v (the attempt-based schedule must apply too)", closing)
	}
	var waiting runClose
	waiting.wait(StepVectorData, 9*time.Second)
	if waiting.attemptBased || waiting.fixed != 9*time.Second || len(waiting.failed) != 0 || !slices.Equal(waiting.waiting, []string{StepVectorData}) || waiting.complete() {
		t.Fatalf("after a planned wait: %+v", waiting)
	}
	// A wait that has (just) run out still asks for a pause of at least a
	// second, never the attempt-based schedule.
	waiting.wait(StepVectorData, -time.Millisecond)
	if waiting.attemptBased || waiting.fixed != time.Second {
		t.Fatalf("after an expired wait: %+v", waiting)
	}
}
