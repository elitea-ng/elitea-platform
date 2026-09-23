package runtimecomposition

// The adapter's refusal rule. The pass itself is tested in
// internal/application/patexpiry and end to end against Postgres in
// internal/api/v2/admin; what is left here is the guard every scheduling
// handler in this package carries, and which is the difference between "the
// scheduler asked us to run" and "something called Execute".

import (
	"context"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/patexpiry"
	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
)

type recordingPATExpiryRunner struct {
	runs   int
	within time.Duration
}

func (r *recordingPATExpiryRunner) Run(
	_ context.Context, _ time.Time, within time.Duration,
) (patexpiry.Result, error) {
	r.runs++
	r.within = within
	return patexpiry.Result{}, nil
}

func validPATExpiryOccurrence() schedulingapp.Occurrence {
	return schedulingapp.Occurrence{
		InvocationID:     "invocation-1",
		JobID:            patExpirySweepCapability,
		ScheduleRevision: patExpirySweepRevision,
		DueAt:            time.Now(),
		LeaseEpoch:       1,
		ClaimFence:       "fence-1",
	}
}

func TestPATExpirySweepRunsTheProductionWindow(t *testing.T) {
	runner := &recordingPATExpiryRunner{}
	sweep, err := newPATExpirySweep(runner)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := sweep.Execute(context.Background(), validPATExpiryOccurrence())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome != schedulingapp.OutcomeLocalCompleted {
		t.Errorf("outcome = %q, want %q", outcome, schedulingapp.OutcomeLocalCompleted)
	}
	// A scheduled tick has nobody to have asked for a different window, so the
	// operator control must not leak into it.
	if runner.runs != 1 || runner.within != patexpiry.DefaultWindow {
		t.Errorf("ran %d times with window %v, want 1 run at %v", runner.runs, runner.within, patexpiry.DefaultWindow)
	}
}

func TestPATExpirySweepRefusesAnOccurrenceThatIsNotItsOwn(t *testing.T) {
	cases := map[string]func(*schedulingapp.Occurrence){
		"no invocation id": func(o *schedulingapp.Occurrence) { o.InvocationID = "" },
		"another job":      func(o *schedulingapp.Occurrence) { o.JobID = "artifact.retention.sweep.v1" },
		"another revision": func(o *schedulingapp.Occurrence) { o.ScheduleRevision = "pat-expiry-notice-r0" },
		"no due time":      func(o *schedulingapp.Occurrence) { o.DueAt = time.Time{} },
		"no lease":         func(o *schedulingapp.Occurrence) { o.LeaseEpoch = 0 },
		"no claim fence":   func(o *schedulingapp.Occurrence) { o.ClaimFence = "" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &recordingPATExpiryRunner{}
			sweep, err := newPATExpirySweep(runner)
			if err != nil {
				t.Fatal(err)
			}
			occurrence := validPATExpiryOccurrence()
			breakIt(&occurrence)
			if _, err := sweep.Execute(context.Background(), occurrence); err == nil {
				t.Fatal("Execute accepted an occurrence that is not this job's")
			}
			if runner.runs != 0 {
				t.Errorf("the pass ran %d times on a refused occurrence, want 0", runner.runs)
			}
		})
	}
}

func TestNewPATExpirySweepRefusesANilNotifier(t *testing.T) {
	if _, err := newPATExpirySweep(nil); err == nil {
		t.Fatal("a sweep was built over no notifier")
	}
}
